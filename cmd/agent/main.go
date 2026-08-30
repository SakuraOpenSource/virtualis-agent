package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/SakuraOpenSource/virtualis-agent/internal/driver"
	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

const maxImageSize = int64(64 << 30)

type agentServer struct {
	token     string
	name      string
	version   string
	dataDir   string
	registry  *driver.Registry
	mu        sync.RWMutex
	instances map[uint]protocol.Instance
	metrics   map[uint]protocol.Metrics
}

func newAgentServer(token, name, version, dataDir string) *agentServer {
	return &agentServer{
		token:     token,
		name:      name,
		version:   version,
		dataDir:   dataDir,
		registry:  driver.NewRegistryWithDataDir(dataDir),
		instances: make(map[uint]protocol.Instance),
		metrics:   make(map[uint]protocol.Metrics),
	}
}

func (s *agentServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/api/health", s.auth(http.HandlerFunc(s.health)))
	mux.Handle("/api/drivers", s.auth(http.HandlerFunc(s.drivers)))
	mux.Handle("/api/host/network", s.auth(http.HandlerFunc(s.hostNetwork)))
	mux.Handle("/api/instances", s.auth(http.HandlerFunc(s.createInstance)))
	mux.Handle("/api/instances/", s.auth(http.HandlerFunc(s.instanceRoute)))
	return mux
}

func (s *agentServer) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided := r.Header.Get("X-Agent-Token")
		if provided == "" {
			provided = r.URL.Query().Get("token")
		}
		if subtle.ConstantTimeCompare([]byte(provided), []byte(s.token)) != 1 {
			writeError(w, http.StatusUnauthorized, "agent token 无效")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *agentServer) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"name":    s.name,
		"version": s.version,
		"drivers": s.registry.Capabilities(r.Context()),
	})
}

func (s *agentServer) drivers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": s.registry.Capabilities(r.Context())})
}

// hostNetwork 返回主机网卡清单与 IPv4 总数。独立 IP 模式的可用性判断
// （主机至少 2 个 IPv4）与挂载接口选择都以此为数据源。
func (s *agentServer) hostNetwork(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	summary := driver.CollectHostNetwork()
	writeJSON(w, http.StatusOK, map[string]any{"network": summary})
}

func (s *agentServer) createInstance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	instance, upload, filename, extraUpload, extraName, cleanup, err := parseInstance(r)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if instance.ID == 0 || strings.TrimSpace(instance.Name) == "" {
		writeError(w, http.StatusBadRequest, "instance id/name required")
		return
	}
	if instance.Image == nil && upload != nil {
		instance.Image = &protocol.Image{OriginalName: filename, Driver: instance.Driver, Type: "disk"}
	}
	if upload != nil {
		localPath, saveErr := s.saveImage(upload, filename)
		if saveErr != nil {
			writeError(w, http.StatusBadRequest, saveErr.Error())
			return
		}
		if instance.Image == nil {
			instance.Image = &protocol.Image{}
		}
		instance.Image.Path = localPath
	}
	if extraUpload != nil {
		extraPath, saveErr := s.saveImage(extraUpload, extraName)
		if saveErr != nil {
			writeError(w, http.StatusBadRequest, saveErr.Error())
			return
		}
		if instance.Image == nil {
			instance.Image = &protocol.Image{}
		}
		instance.Image.ExtraPath = extraPath
	}
	d, err := s.registry.Resolve(r.Context(), instance.Driver)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := d.Create(r.Context(), &instance); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	// 面板生成的 root 密码：QEMU 在 Create 内写盘；容器直接 exec 注入。
	// 失败只记日志，不阻塞创建（可稍后经 /password 重试）。
	if instance.RootPassword != "" && d.Name() != "qemu" {
		if err := d.SetRootPassword(r.Context(), &instance, instance.RootPassword); err != nil {
			log.Printf("实例 %d 初始 root 密码注入失败: %v", instance.ID, err)
		}
	}
	instance.Driver = d.Name()
	instance.Status = driver.StatusStopped
	instance.RootPassword = ""
	s.mu.Lock()
	s.instances[instance.ID] = instance
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"instance": instance})
}

func (s *agentServer) instanceRoute(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/instances/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusBadRequest, "instance id required")
		return
	}
	var id uint
	if _, err := fmt.Sscanf(parts[0], "%d", &id); err != nil || id == 0 {
		writeError(w, http.StatusBadRequest, "invalid instance id")
		return
	}
	switch {
	case r.Method == http.MethodDelete && len(parts) == 1:
		s.deleteInstance(w, r, id)
	case r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "power":
		s.powerInstance(w, r, id)
	case r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "status":
		s.statusInstance(w, r, id)
	case r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "metrics":
		s.metricsInstance(w, r, id)
	case r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "network":
		s.networkInstance(w, r, id)
	case r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "vnc":
		s.vncInstance(w, r, id)
	case r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "nat":
		s.updateNATMappings(w, r, id)
	case r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "password":
		s.setPassword(w, r, id)
	case r.Method == http.MethodGet && len(parts) == 3 && parts[1] == "vnc" && parts[2] == "ws":
		s.vncWebSocket(w, r, id)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (s *agentServer) deleteInstance(w http.ResponseWriter, r *http.Request, id uint) {
	instance, err := s.requestInstance(r, id)
	if err != nil {
		instance, err = s.storedInstance(id)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	d, err := s.registry.Resolve(r.Context(), instance.Driver)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := d.Delete(r.Context(), &instance); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	driver.ClearNATRules(r.Context(), id)
	if instance.Image != nil && instance.Image.Path != "" {
		_ = os.Remove(instance.Image.Path)
	}
	s.mu.Lock()
	delete(s.instances, id)
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *agentServer) powerInstance(w http.ResponseWriter, r *http.Request, id uint) {
	instance, action, upload, filename, extraUpload, extraName, cleanup, err := parsePower(r)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if instance.ID == 0 {
		instance, err = s.storedInstance(id)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if instance.ID != id {
		writeError(w, http.StatusBadRequest, "instance id mismatch")
		return
	}
	if upload != nil {
		localPath, saveErr := s.saveImage(upload, filename)
		if saveErr != nil {
			writeError(w, http.StatusBadRequest, saveErr.Error())
			return
		}
		if instance.Image == nil {
			instance.Image = &protocol.Image{Driver: instance.Driver, Type: "disk"}
		}
		instance.Image.Path = localPath
	}
	if extraUpload != nil {
		extraPath, saveErr := s.saveImage(extraUpload, extraName)
		if saveErr != nil {
			writeError(w, http.StatusBadRequest, saveErr.Error())
			return
		}
		if instance.Image == nil {
			instance.Image = &protocol.Image{}
		}
		instance.Image.ExtraPath = extraPath
	}
	d, err := s.registry.Resolve(r.Context(), instance.Driver)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := runAction(r.Context(), d, action, &instance); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	instance.Driver = d.Name()
	instance.Status = statusForAction(action)
	applyOrClearNAT(r.Context(), d, &instance, s, false)
	s.mu.Lock()
	s.instances[id] = instance
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"instance": instance})
}

func (s *agentServer) statusInstance(w http.ResponseWriter, r *http.Request, id uint) {
	instance, err := s.requestInstance(r, id)
	if err != nil {
		instance, err = s.storedInstance(id)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	d, err := s.registry.Resolve(r.Context(), instance.Driver)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	status, err := d.Status(r.Context(), &instance)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	instance.Driver = d.Name()
	instance.Status = status
	// 自愈：域可能在被控升级/重启前就处于运行状态（那时没有 NAT 规则
	// 逻辑），状态查询是最频繁的请求，借它幂等对账规则，无需重启实例。
	applyOrClearNAT(r.Context(), d, &instance, s, true)
	s.mu.Lock()
	s.instances[id] = instance
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"instance": instance})
}

func (s *agentServer) metricsInstance(w http.ResponseWriter, r *http.Request, id uint) {
	instance, err := s.requestInstance(r, id)
	if err != nil {
		instance, err = s.storedInstance(id)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	d, err := s.registry.Resolve(r.Context(), instance.Driver)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	metrics, err := d.Metrics(r.Context(), &instance)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	metrics = s.addBandwidthSample(id, metrics)
	writeJSON(w, http.StatusOK, map[string]any{"metrics": metrics})
}

func (s *agentServer) addBandwidthSample(id uint, metrics protocol.Metrics) protocol.Metrics {
	if metrics.CollectedAt.IsZero() {
		metrics.CollectedAt = time.Now().UTC()
	}
	s.mu.Lock()
	previous, exists := s.metrics[id]
	s.metrics[id] = metrics
	s.mu.Unlock()
	if !exists {
		return metrics
	}
	seconds := metrics.CollectedAt.Sub(previous.CollectedAt).Seconds()
	if seconds <= 0 {
		return metrics
	}
	if metrics.BandwidthRxBps == 0 && metrics.NetworkRxBytes >= previous.NetworkRxBytes {
		metrics.BandwidthRxBps = float64(metrics.NetworkRxBytes-previous.NetworkRxBytes) / seconds
	}
	if metrics.BandwidthTxBps == 0 && metrics.NetworkTxBytes >= previous.NetworkTxBytes {
		metrics.BandwidthTxBps = float64(metrics.NetworkTxBytes-previous.NetworkTxBytes) / seconds
	}
	return metrics
}

func (s *agentServer) networkInstance(w http.ResponseWriter, r *http.Request, id uint) {
	instance, err := s.requestInstance(r, id)
	if err != nil {
		instance, err = s.storedInstance(id)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	d, err := s.registry.Resolve(r.Context(), instance.Driver)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	network, err := d.Network(r.Context(), &instance)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"network": network})
}

func (s *agentServer) vncInstance(w http.ResponseWriter, r *http.Request, id uint) {
	instance, err := s.requestInstance(r, id)
	if err != nil {
		instance, err = s.storedInstance(id)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	d, err := s.registry.Resolve(r.Context(), instance.Driver)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	vnc, err := d.VNC(r.Context(), &instance, requestHost(r))
	if err != nil {
		log.Printf("VNC 查询实例 %d 失败: %v", id, err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if !vnc.Available {
		log.Printf("VNC 实例 %d 不可用: %s", id, vnc.Message)
	}
	writeJSON(w, http.StatusOK, map[string]any{"vnc": vnc})
}

// applyOrClearNAT 在电源动作/状态查询后同步 NAT 规则：启动类动作应用
// 全量清单，停止类动作清除。失败只记日志不阻塞开机。quick 用于高频的
// 状态查询：IP 解析只试一次，避免拖慢接口。
func applyOrClearNAT(ctx context.Context, d driver.Driver, instance *protocol.Instance, s *agentServer, quick bool) {
	switch instance.Status {
	case driver.StatusRunning:
		retries, interval := 5, 3
		if quick {
			retries, interval = 1, 1
		}
		if _, err := driver.ApplyNATRules(ctx, d, instance, retries, interval); err != nil {
			log.Printf("实例 %d 应用 NAT 映射失败: %v", instance.ID, err)
		}
	default:
		driver.ClearNATRules(ctx, instance.ID)
	}
}

// updateNATMappings 让被控按主控下发的全量清单对账 NAT 规则。实例运行中
// 立即生效；关机状态只清除已移除映射的规则并保存清单。
func (s *agentServer) updateNATMappings(w http.ResponseWriter, r *http.Request, id uint) {
	var payload struct {
		Instance protocol.Instance     `json:"instance"`
		Mappings []protocol.NATMapping `json:"mappings"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.Instance.ID != id {
		writeError(w, http.StatusBadRequest, "invalid payload")
		return
	}
	instance, err := s.storedInstance(id)
	if err != nil {
		instance = payload.Instance
	}
	d, err := s.registry.Resolve(r.Context(), instance.Driver)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	instance.NATMappings = payload.Mappings
	if running, _ := d.Status(r.Context(), &instance); running == driver.StatusRunning {
		instance.Status = driver.StatusRunning
		applyOrClearNAT(r.Context(), d, &instance, s, false)
	} else {
		driver.ClearNATRules(r.Context(), id)
	}
	s.mu.Lock()
	s.instances[id] = instance
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"instance": instance})
}

// setPassword 向运行中的实例注入 root 密码（容器 exec / QEMU guest agent）。
func (s *agentServer) setPassword(w http.ResponseWriter, r *http.Request, id uint) {
	var payload struct {
		Instance protocol.Instance `json:"instance"`
		Password string            `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.Instance.ID != id {
		writeError(w, http.StatusBadRequest, "invalid payload")
		return
	}
	if payload.Password == "" {
		writeError(w, http.StatusBadRequest, "password required")
		return
	}
	instance, err := s.storedInstance(id)
	if err != nil {
		instance = payload.Instance
	}
	d, err := s.registry.Resolve(r.Context(), instance.Driver)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := d.SetRootPassword(r.Context(), &instance, payload.Password); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

var vncUpgrader = websocket.Upgrader{
	ReadBufferSize:  32 << 10,
	WriteBufferSize: 32 << 10,
	Subprotocols:    []string{"binary"},
	CheckOrigin:     func(*http.Request) bool { return true },
}

func (s *agentServer) vncWebSocket(w http.ResponseWriter, r *http.Request, id uint) {
	instance, err := s.storedInstance(id)
	if err != nil {
		// The master supplies the identity in the query so VNC can still work
		// immediately after an agent restart, before the in-memory map is rebuilt.
		instance = protocol.Instance{ID: id, Name: r.URL.Query().Get("name"), Driver: r.URL.Query().Get("driver")}
		if instance.Name == "" || instance.Driver == "" {
			writeError(w, http.StatusBadRequest, "instance identity required")
			return
		}
	}
	d, err := s.registry.Resolve(r.Context(), instance.Driver)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	vnc, err := d.VNC(r.Context(), &instance, "127.0.0.1")
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if !vnc.Available || vnc.Port == 0 {
		log.Printf("VNC WS 实例 %d 不可用: %s", id, vnc.Message)
		writeError(w, http.StatusBadRequest, vnc.Message)
		return
	}
	raw, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(vnc.Port)), 5*time.Second)
	if err != nil {
		log.Printf("VNC WS 实例 %d 连接 127.0.0.1:%d 失败: %v", id, vnc.Port, err)
		writeError(w, http.StatusBadGateway, "连接 QEMU VNC 失败: "+err.Error())
		return
	}
	log.Printf("VNC WS 实例 %d 已建立，目标 127.0.0.1:%d", id, vnc.Port)
	defer raw.Close()
	conn, err := vncUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("VNC WS 实例 %d 升级失败: %v", id, err)
		return
	}
	defer conn.Close()
	conn.SetReadLimit(16 << 20)
	result := make(chan error, 2)
	// 双向首包与字节计数：RFB 握手卡住或秒断时，journal 能直接看出
	// 数据断在哪个方向、断前传了多少（几十字节=安全协商崩，KB 级=会话
	// 已建立后被杀）。
	var toGuest, toBrowser logFirst
	var bytesToGuest, bytesToBrowser uint64
	go func() {
		for {
			_, reader, readErr := conn.NextReader()
			if readErr != nil {
				result <- readErr
				return
			}
			n, copyErr := io.Copy(raw, reader)
			atomic.AddUint64(&bytesToGuest, uint64(n))
			if copyErr != nil {
				result <- copyErr
				return
			}
			toGuest.once(func() { log.Printf("VNC WS 实例 %d 收到浏览器首包 → QEMU", id) })
		}
	}()
	go func() {
		buffer := make([]byte, 32<<10)
		for {
			n, readErr := raw.Read(buffer)
			if n > 0 {
				atomic.AddUint64(&bytesToBrowser, uint64(n))
				toBrowser.once(func() { log.Printf("VNC WS 实例 %d 收到 QEMU 首包（RFB banner）→ 浏览器", id) })
				writer, writeErr := conn.NextWriter(websocket.BinaryMessage)
				if writeErr != nil {
					result <- writeErr
					return
				}
				if _, writeErr = writer.Write(buffer[:n]); writeErr == nil {
					writeErr = writer.Close()
				} else {
					_ = writer.Close()
				}
				if writeErr != nil {
					result <- writeErr
					return
				}
			}
			if readErr != nil {
				result <- readErr
				return
			}
		}
	}()
	if err := <-result; err != nil {
		log.Printf("VNC WS 实例 %d 断开: %v（QEMU→浏览器 %d B / 浏览器→QEMU %d B）",
			id, err, atomic.LoadUint64(&bytesToBrowser), atomic.LoadUint64(&bytesToGuest))
	}
}

// logFirst 让一段日志只打一次。
type logFirst struct {
	do sync.Once
}

func (l *logFirst) once(fn func()) { l.do.Do(fn) }

func requestHost(r *http.Request) string {
	host := r.Host
	if value, _, err := net.SplitHostPort(host); err == nil {
		return value
	}
	return host
}

func (s *agentServer) requestInstance(r *http.Request, id uint) (protocol.Instance, error) {
	var payload struct {
		Instance protocol.Instance `json:"instance"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		return protocol.Instance{}, err
	}
	if payload.Instance.ID == 0 {
		return protocol.Instance{}, errors.New("instance missing")
	}
	if payload.Instance.ID != id {
		return protocol.Instance{}, errors.New("instance id mismatch")
	}
	return payload.Instance, nil
}

func (s *agentServer) storedInstance(id uint) (protocol.Instance, error) {
	s.mu.RLock()
	instance, ok := s.instances[id]
	s.mu.RUnlock()
	if !ok {
		return protocol.Instance{}, errors.New("instance not found on agent")
	}
	return instance, nil
}

func (s *agentServer) saveImage(src io.Reader, filename string) (string, error) {
	if err := os.MkdirAll(filepath.Join(s.dataDir, "images"), 0o755); err != nil {
		return "", fmt.Errorf("创建镜像目录失败: %w", err)
	}
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	ext := safeExtension(filename)
	path := filepath.Join(s.dataDir, "images", hex.EncodeToString(raw)+ext)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	defer f.Close()
	written, err := io.Copy(f, io.LimitReader(src, maxImageSize+1))
	if err != nil {
		_ = os.Remove(path)
		return "", err
	}
	if written > maxImageSize {
		_ = os.Remove(path)
		return "", errors.New("镜像文件超过 64 GiB")
	}
	// libvirt 的 QEMU 进程不是 root，落盘后立刻放开读取权限。
	driver.PrepareDiskFile(path)
	return path, nil
}

func safeExtension(filename string) string {
	name := strings.ToLower(filepath.Base(filename))
	for _, ext := range []string{".tar.gz", ".qcow2", ".vmdk", ".vdi", ".raw", ".img", ".iso", ".gz"} {
		if strings.HasSuffix(name, ext) {
			return ext
		}
	}
	return ""
}

func parseInstance(r *http.Request) (protocol.Instance, io.Reader, string, io.Reader, string, func(), error) {
	if strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/form-data") {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			return protocol.Instance{}, nil, "", nil, "", nil, err
		}
		var instance protocol.Instance
		if err := json.Unmarshal([]byte(r.FormValue("instance")), &instance); err != nil {
			return protocol.Instance{}, nil, "", nil, "", cleanupMultipart(r), err
		}
		file, header, err := r.FormFile("image")
		if err != nil {
			return instance, nil, "", nil, "", cleanupMultipart(r), nil
		}
		extraFile, extraHeader, extraErr := r.FormFile("extra")
		if extraErr != nil {
			extraFile, extraHeader = nil, nil
		}
		cleanup := func() {
			file.Close()
			if extraFile != nil {
				extraFile.Close()
			}
			cleanupMultipart(r)
		}
		extraName := ""
		if extraHeader != nil {
			extraName = extraHeader.Filename
		}
		return instance, file, header.Filename, extraFile, extraName, cleanup, nil
	}
	var payload struct {
		Instance protocol.Instance `json:"instance"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		return protocol.Instance{}, nil, "", nil, "", nil, err
	}
	return payload.Instance, nil, "", nil, "", nil, nil
}

func parsePower(r *http.Request) (protocol.Instance, string, io.Reader, string, io.Reader, string, func(), error) {
	if strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/form-data") {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			return protocol.Instance{}, "", nil, "", nil, "", nil, err
		}
		var payload struct {
			Action   string            `json:"action"`
			Instance protocol.Instance `json:"instance"`
		}
		if err := json.Unmarshal([]byte(r.FormValue("power")), &payload); err != nil {
			return protocol.Instance{}, "", nil, "", nil, "", cleanupMultipart(r), err
		}
		file, header, err := r.FormFile("image")
		if err != nil {
			return payload.Instance, payload.Action, nil, "", nil, "", cleanupMultipart(r), nil
		}
		extraFile, extraHeader, extraErr := r.FormFile("extra")
		if extraErr != nil {
			extraFile, extraHeader = nil, nil
		}
		cleanup := func() {
			file.Close()
			if extraFile != nil {
				extraFile.Close()
			}
			cleanupMultipart(r)
		}
		extraName := ""
		if extraHeader != nil {
			extraName = extraHeader.Filename
		}
		return payload.Instance, payload.Action, file, header.Filename, extraFile, extraName, cleanup, nil
	}
	var payload struct {
		Action   string            `json:"action"`
		Instance protocol.Instance `json:"instance"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		return protocol.Instance{}, "", nil, "", nil, "", nil, err
	}
	return payload.Instance, payload.Action, nil, "", nil, "", nil, nil
}

func cleanupMultipart(r *http.Request) func() {
	return func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}
}

func runAction(ctx context.Context, d driver.Driver, action string, instance *protocol.Instance) error {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "start":
		return d.Start(ctx, instance)
	case "stop":
		return d.Stop(ctx, instance)
	case "restart":
		return d.Restart(ctx, instance)
	case "hard_start":
		return d.HardStart(ctx, instance)
	case "hard_stop":
		return d.HardStop(ctx, instance)
	case "hard_restart":
		return d.HardRestart(ctx, instance)
	case "reinstall":
		return d.Reinstall(ctx, instance)
	default:
		return fmt.Errorf("不支持的操作 %q", action)
	}
}

func statusForAction(action string) string {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "start", "restart", "hard_start", "hard_restart":
		return driver.StatusRunning
	default:
		return driver.StatusStopped
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

type registration struct {
	IP       string   `json:"ip"`
	Endpoint string   `json:"endpoint"`
	Driver   string   `json:"driver"`
	Drivers  []string `json:"drivers"`
	OS       string   `json:"os"`
	Arch     string   `json:"arch"`
	Version  string   `json:"version"`
}

func (s *agentServer) register(ctx context.Context, master, endpoint string) error {
	items := s.registry.Capabilities(ctx)
	drivers := make([]string, 0, len(items))
	primary := ""
	for _, item := range items {
		if item.Available {
			drivers = append(drivers, item.Name)
			if primary == "" {
				primary = item.Name
			}
		}
	}
	ip := endpointHost(endpoint)
	payload := registration{IP: ip, Endpoint: endpoint, Driver: primary, Drivers: drivers, OS: goruntime.GOOS, Arch: goruntime.GOARCH, Version: s.version}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(master, "/")+"/api/agent/register", strings.NewReader(string(raw)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agent-Token", s.token)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("主控拒绝注册 (401)：接入 token 已失效或节点已被删除，请在主控重新生成接入指令并更新 --token 后重启被控")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("主控拒绝注册 (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func endpointHost(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func discoverEndpoint(advertise string, listen net.Addr, master string) (string, error) {
	if advertise = strings.TrimRight(strings.TrimSpace(advertise), "/"); advertise != "" {
		u, err := url.Parse(advertise)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return "", errors.New("--advertise 必须是 http:// 或 https:// 地址")
		}
		return advertise, nil
	}
	tcp, ok := listen.(*net.TCPAddr)
	if !ok {
		return "", errors.New("无法解析监听地址")
	}
	host := tcp.IP.String()
	if tcp.IP == nil || tcp.IP.IsUnspecified() {
		host = localAddress(master)
	}
	if host == "" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, fmt.Sprint(tcp.Port)), nil
}

func localAddress(master string) string {
	u, err := url.Parse(master)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	port := u.Port()
	if port == "" {
		port = "80"
	}
	conn, err := net.DialTimeout("udp", net.JoinHostPort(u.Hostname(), port), 2*time.Second)
	if err != nil {
		return ""
	}
	defer conn.Close()
	addr, _ := net.ResolveUDPAddr("udp", conn.LocalAddr().String())
	if addr == nil {
		return ""
	}
	return addr.IP.String()
}

func main() {
	var (
		master    = flag.String("master", "", "主控地址，例如 http://MASTER:8080")
		token     = flag.String("token", "", "主控生成的接入 token")
		name      = flag.String("name", "", "被控名称")
		listen    = flag.String("listen", ":8081", "被控 RPC 监听地址")
		advertise = flag.String("advertise", "", "主控可访问的被控地址，例如 http://10.0.0.2:8081")
		dataDir   = flag.String("data", "/var/lib/virtualis-agent", "被控数据目录")
		version   = flag.String("version", "dev", "版本")
	)
	flag.Parse()
	if strings.TrimSpace(*master) == "" || strings.TrimSpace(*token) == "" {
		fmt.Println("用法: virtualis-agent --master http://MASTER:8080 --token TOKEN --name node-01 [--advertise http://AGENT:8081]")
		flag.PrintDefaults()
		os.Exit(2)
	}
	if *name == "" {
		*name, _ = os.Hostname()
		if *name == "" {
			*name = "agent"
		}
	}
	if err := os.MkdirAll(filepath.Join(*dataDir, "images"), 0o755); err != nil {
		log.Fatalf("创建数据目录失败: %v", err)
	}
	driver.PrepareDiskDir(*dataDir)
	// 修复旧版本以 0600 落盘的存量镜像，升级重启后即可直接开机。
	driver.PrepareAllDiskFiles(*dataDir)
	state := newAgentServer(*token, *name, *version, *dataDir)
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("监听失败: %v", err)
	}
	endpoint, err := discoverEndpoint(*advertise, listener.Addr(), *master)
	if err != nil {
		listener.Close()
		log.Fatal(err)
	}
	httpServer := &http.Server{Handler: state.handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("RPC 服务停止: %v", err)
		}
	}()

	registerCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	err = state.register(registerCtx, *master, endpoint)
	cancel()
	if err != nil {
		_ = httpServer.Shutdown(context.Background())
		log.Fatalf("注册失败: %v", err)
	}
	log.Printf("Virtualis Agent %s online: name=%s endpoint=%s", *version, *name, endpoint)

	ctx, stop := signalContext()
	defer stop()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			heartbeatCtx, heartbeatCancel := context.WithTimeout(context.Background(), 20*time.Second)
			if err := state.register(heartbeatCtx, *master, endpoint); err != nil {
				log.Printf("心跳失败: %v", err)
			}
			heartbeatCancel()
		case <-ctx.Done():
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = httpServer.Shutdown(shutdownCtx)
			shutdownCancel()
			return
		}
	}
}

func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	// Keep the agent dependency-free while handling the common termination
	// signals used by systemd, launchd, and Windows service wrappers.
	go func() {
		<-ch
		cancel()
	}()
	// os/signal.Notify is isolated here to keep startup code readable.
	// The channel is registered for SIGINT and SIGTERM on Unix; on Windows the
	// runtime still delivers the interrupt signal.
	signal.Notify(ch, os.Interrupt)
	return ctx, cancel
}
