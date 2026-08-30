package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

// Incus 驱动：离线导入镜像后 launch，网络经 device 配置。
type Incus struct {
	mu      sync.Mutex
	samples map[uint]incusSample
}

type incusSample struct {
	cpuTime uint64
	rx, tx  uint64
	at      time.Time
}

func NewIncus() *Incus {
	return &Incus{samples: make(map[uint]incusSample)}
}
func (d *Incus) Name() string { return "incus" }

func (d *Incus) Probe(ctx context.Context) error {
	if !hasCommand("incus") {
		return fmt.Errorf("Incus 未安装")
	}
	return run(ctx, "incus", "version")
}

func (d *Incus) cli() string {
	if hasCommand("incus") {
		return "incus"
	}
	return "lxc"
}

func (d *Incus) Create(ctx context.Context, inst *protocol.Instance) error {
	if inst.Image == nil || inst.Image.Path == "" {
		return fmt.Errorf("Incus 离线模式需要先上传镜像到 data/images，请在镜像管理上传镜像后重试")
	}
	name := resourceName("incus", inst)
	alias := fmt.Sprintf("virtualis-img-%d", inst.ID)
	// 上次失败可能残留同名 alias 或同 fingerprint 镜像：先幂等清理。
	_ = run(ctx, d.cli(), "image", "delete", alias)
	// 分割镜像（meta.tar.xz + rootfs.tar.xz / disk.qcow2）传两个文件；
	// 统一镜像（单 tar）只传 Path。
	importArgs := []string{"image", "import", inst.Image.Path, "--alias", alias}
	if inst.Image.ExtraPath != "" {
		importArgs = []string{"image", "import", inst.Image.ExtraPath, inst.Image.Path, "--alias", alias}
	}
	if err := run(ctx, d.cli(), importArgs...); err != nil {
		if !contains(err.Error(), "already exists") {
			return fmt.Errorf("导入离线镜像失败: %w", err)
		}
		// 同 fingerprint 镜像已在本地库（上次失败残留）：把挂在该镜像上的
		// virtualis alias 指回本次 alias，launch 才有入口。
		out, listErr := output(ctx, d.cli(), "image", "list", "--format", "csv")
		if listErr != nil {
			return fmt.Errorf("导入离线镜像失败: %w", err)
		}
		fingerprint := ""
		for _, line := range strings.Split(string(out), "\n") {
			// CSV 列：alias,fingerprint,public,description,...
			cols := strings.Split(line, ",")
			if len(cols) < 2 || !strings.Contains(cols[0], "virtualis-img-") {
				continue
			}
			fingerprint = cols[1]
			break
		}
		if fingerprint == "" || run(ctx, d.cli(), "image", "alias", "create", alias, fingerprint) != nil {
			return fmt.Errorf("导入离线镜像失败: %w", err)
		}
	}
	// NAT 模式保留静态地址（dnsmasq 静态租约），NAT 映射目标才稳定。
	if NormalizeNetworkMode(inst.Network.Mode) == NetworkModeNat {
		if inst.Network.IPv4 == "" {
			// Incus 容器挂 incusbr0，保留地址必须落在它的子网内。
			reserved, _ := natSlotIPOn("incusbr0", inst)
			if reserved == "" {
				reserved = fmt.Sprintf("10.10.10.%d", 100+int(inst.ID%140))
			}
			inst.Network.IPv4 = reserved
		}
	}
	// 每实例一个专用 profile 承载网络/资源限制：launch 的 -d 简写在
	// Incus 6 上解析不可靠，profile device add 的位置参数语法最稳。
	profile := fmt.Sprintf("virtualis-p-%d", inst.ID)
	if err := d.ensureProfile(ctx, profile, inst.Network, inst); err != nil {
		return err
	}
	args := []string{"launch", alias, name, "-p", profile}
	if inst.Type == "vm" {
		args = append(args, "--vm")
	}
	if inst.Spec.CPU > 0 {
		args = append(args, "-c", fmt.Sprintf("limits.cpu=%d", inst.Spec.CPU))
	}
	if inst.Spec.MemoryMB > 0 {
		args = append(args, "-c", fmt.Sprintf("limits.memory=%dMiB", inst.Spec.MemoryMB))
	}
	if err := run(ctx, d.cli(), args...); err != nil {
		return err
	}
	d.ensureImageAliasCleaned(ctx, alias)
	return nil
}

// ensureProfile 建立实例专用 profile 并按网络模式写入 eth0/root 设备。
// 幂等：profile 已存在时仅重建设备定义。
func (d *Incus) ensureProfile(ctx context.Context, profile string, network protocol.NetworkConfig, inst *protocol.Instance) error {
	if err := run(ctx, d.cli(), "profile", "create", profile); err != nil && !contains(err.Error(), "already exists") {
		return fmt.Errorf("创建实例 profile 失败: %w", err)
	}
	// 从默认 profile 继承 root 磁盘；已存在先移除再添加（幂等重建）。
	// size 是磁盘配额：需要 btrfs/zfs 存储池才生效（dir 池不支持）。
	_ = run(ctx, d.cli(), "profile", "device", "remove", profile, "root")
	rootArgs := []string{"profile", "device", "add", profile, "root", "disk", "path=/", "pool=default"}
	if size := inst.Spec.DiskGB; size > 0 {
		rootArgs = append(rootArgs, fmt.Sprintf("size=%dGiB", size))
	}
	if err := run(ctx, d.cli(), rootArgs...); err != nil {
		return fmt.Errorf("配置 root 磁盘失败: %w", err)
	}
	_ = run(ctx, d.cli(), "profile", "device", "remove", profile, "eth0")
	mode := NormalizeNetworkMode(network.Mode)
	if mode == NetworkModeNone {
		if err := run(ctx, d.cli(), "profile", "device", "add", profile, "eth0", "nic", "nictype=none"); err != nil {
			return fmt.Errorf("配置网络设备失败: %w", err)
		}
		return nil
	}
	parent := "incusbr0"
	if mode == NetworkModeDedicated {
		target, _, err := dedicatedTarget(network)
		if err == nil && target != "" {
			parent = target
		}
	}
	spec := []string{"profile", "device", "add", profile, "eth0", "nic", "nictype=bridged", "parent=" + parent}
	if network.IPv4 != "" {
		spec = append(spec, "ipv4.address="+strings.Split(network.IPv4, "/")[0])
	}
	if network.MAC != "" {
		spec = append(spec, "hwaddr="+network.MAC)
	}
	if err := run(ctx, d.cli(), spec...); err != nil {
		return fmt.Errorf("配置网络设备失败: %w", err)
	}
	return nil
}

// incusDeviceArgs 把网络配置翻译成 launch 可用的 -d 参数。
//
// NAT：nic 默认网络（incusbr0），DHCP 自动发地址，共享主机出口 IP。
// 独立 IP：nic bridged 挂到主机网桥；IPv4/gateway/DNS 显式下发给容器。
// 关闭：nic none。
func incusDeviceArgs(network protocol.NetworkConfig, inst *protocol.Instance) []string {
	switch NormalizeNetworkMode(network.Mode) {
	case NetworkModeNone:
		return []string{"-d", "eth0,nic,nictype=none"}
	case NetworkModeNat:
		// 有保留地址时显式声明 bridged 设备，让 dnsmasq 发固定租约。
		if network.IPv4 != "" {
			spec := "eth0,nic,nictype=bridged,parent=incusbr0,ipv4.address=" + strings.Split(network.IPv4, "/")[0]
			if network.MAC != "" {
				spec += ",hwaddr=" + network.MAC
			}
			return []string{"-d", spec}
		}
		return nil // 不指定时 Incus 用 default 网络的 eth0
	}
	parent := "incusbr0"
	if value := strings.TrimSpace(network.Bridge); value != "" {
		parent = value
	}
	spec := fmt.Sprintf("eth0,nic,nictype=bridged,parent=%s", parent)
	if network.MAC != "" {
		spec += ",hwaddr=" + network.MAC
	}
	if network.IPv4 != "" {
		spec += ",ipv4.address=" + network.IPv4
	}
	if network.Gateway != "" {
		spec += ",ipv4.gateway=" + network.Gateway
	}
	if network.BandwidthMbps > 0 {
		limit := fmt.Sprintf("%dMbit", network.BandwidthMbps)
		spec += ",limits.ingress=" + limit + ",limits.egress=" + limit
	}
	return []string{"-d", spec}
}

func (d *Incus) ensureImageAliasCleaned(ctx context.Context, alias string) {
	// 镜像已随实例 launch 挂载，别名只是导入时的临时名字，删掉防堆积。
	_ = run(context.WithoutCancel(ctx), d.cli(), "image", "delete", alias)
}

func (d *Incus) Delete(ctx context.Context, inst *protocol.Instance) error {
	containerVNC.stop(d.Name(), inst)
	err := run(ctx, d.cli(), "delete", resourceName("incus", inst), "--force")
	if err != nil && !contains(err.Error(), "not found") {
		return err
	}
	return nil
}

func (d *Incus) Start(ctx context.Context, inst *protocol.Instance) error {
	err := run(ctx, d.cli(), "start", resourceName("incus", inst))
	// launch 创建的实例一落地就在运行，"already running" 视为成功。
	if err != nil && !contains(err.Error(), "already running") {
		return err
	}
	return nil
}
func (d *Incus) Stop(ctx context.Context, inst *protocol.Instance) error {
	containerVNC.stop(d.Name(), inst)
	err := run(ctx, d.cli(), "stop", resourceName("incus", inst))
	if err != nil && !contains(err.Error(), "not running") {
		return err
	}
	return nil
}
func (d *Incus) Restart(ctx context.Context, inst *protocol.Instance) error {
	return run(ctx, d.cli(), "restart", resourceName("incus", inst))
}
func (d *Incus) HardStart(ctx context.Context, inst *protocol.Instance) error {
	return d.Start(ctx, inst)
}
func (d *Incus) HardStop(ctx context.Context, inst *protocol.Instance) error {
	containerVNC.stop(d.Name(), inst)
	return run(ctx, d.cli(), "stop", resourceName("incus", inst), "--force")
}
func (d *Incus) HardRestart(ctx context.Context, inst *protocol.Instance) error {
	if err := d.HardStop(ctx, inst); err != nil {
		return err
	}
	return d.HardStart(ctx, inst)
}
func (d *Incus) Reinstall(ctx context.Context, inst *protocol.Instance) error {
	if err := d.Delete(ctx, inst); err != nil {
		return err
	}
	return d.Create(ctx, inst)
}

func (d *Incus) Status(ctx context.Context, inst *protocol.Instance) (string, error) {
	out, err := output(ctx, d.cli(), "list", resourceName("incus", inst), "--format", "csv", "-c", "ns")
	if err != nil {
		return StatusStopped, nil
	}
	if strings.Contains(strings.ToLower(string(out)), "running") {
		return StatusRunning, nil
	}
	return StatusStopped, nil
}

// incusState 是 incus query .../state 里采集需要字段的子集。
type incusState struct {
	CPU struct {
		Usage uint64 `json:"usage"`
	} `json:"cpu"`
	Memory struct {
		Usage uint64 `json:"usage"`
		Total uint64 `json:"total"`
	} `json:"memory"`
	Network map[string]struct {
		Addresses []struct {
			Family  string `json:"family"`
			Address string `json:"address"`
			Scope   string `json:"scope"`
		} `json:"addresses"`
		Counters struct {
			BytesReceived uint64 `json:"bytes_received"`
			BytesSent     uint64 `json:"bytes_sent"`
		} `json:"counters"`
		HWAddr string `json:"hwaddr"`
		State  string `json:"state"`
	} `json:"network"`
}

// queryState 拉取实例的运行状态 JSON（incus query 的权威数据源）。
func (d *Incus) queryState(ctx context.Context, name string) (*incusState, error) {
	out, err := output(ctx, d.cli(), "query", "/1.0/instances/"+name+"/state")
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Metadata *incusState `json:"metadata"`
	}
	if err := json.Unmarshal(out, &envelope); err != nil || envelope.Metadata == nil {
		// 某些版本直接返回状态对象。
		var direct incusState
		if err2 := json.Unmarshal(out, &direct); err2 != nil {
			return nil, fmt.Errorf("解析实例状态失败: %w", err)
		}
		return &direct, nil
	}
	return envelope.Metadata, nil
}

func (d *Incus) Metrics(ctx context.Context, inst *protocol.Instance) (protocol.Metrics, error) {
	name := resourceName("incus", inst)
	state, err := d.queryState(ctx, name)
	if err != nil {
		return protocol.Metrics{}, err
	}
	metrics := defaultMetrics(inst)
	metrics.CollectedAt = time.Now().UTC()
	if state.Memory.Total > 0 {
		metrics.MemoryTotalMB = int64(state.Memory.Total / 1024 / 1024)
	}
	metrics.MemoryUsedMB = int64(state.Memory.Usage / 1024 / 1024)
	var rxBytes, txBytes uint64
	for _, net := range state.Network {
		rxBytes += net.Counters.BytesReceived
		txBytes += net.Counters.BytesSent
	}
	metrics.NetworkRxBytes = rxBytes
	metrics.NetworkTxBytes = txBytes

	// CPU 与带宽都是累计值：与上次采样差分出速率。
	d.mu.Lock()
	previous, ok := d.samples[inst.ID]
	d.samples[inst.ID] = incusSample{cpuTime: state.CPU.Usage, rx: rxBytes, tx: txBytes, at: metrics.CollectedAt}
	d.mu.Unlock()
	if ok {
		seconds := metrics.CollectedAt.Sub(previous.at).Seconds()
		if seconds > 0 && state.CPU.Usage >= previous.cpuTime {
			cores := inst.Spec.CPU
			if cores < 1 {
				cores = 1
			}
			metrics.CPUPercent = float64(state.CPU.Usage-previous.cpuTime) / (seconds * 1e9 * float64(cores)) * 100
		}
		if seconds > 0 && rxBytes >= previous.rx {
			metrics.BandwidthRxBps = float64(rxBytes-previous.rx) / seconds
		}
		if seconds > 0 && txBytes >= previous.tx {
			metrics.BandwidthTxBps = float64(txBytes-previous.tx) / seconds
		}
	}
	if metrics.CPUPercent < 0 {
		metrics.CPUPercent = 0
	}
	if metrics.CPUPercent > 100 {
		metrics.CPUPercent = 100
	}
	return metrics, nil
}

func (d *Incus) Network(ctx context.Context, inst *protocol.Instance) (protocol.NetworkStatus, error) {
	name := resourceName("incus", inst)
	status := protocol.NetworkStatus{CheckedAt: time.Now().UTC()}
	state, err := d.queryState(ctx, name)
	if err != nil {
		status.Error = "实例未运行或状态不可读"
		return status, nil
	}
	reachable := false
	for ifaceName, net := range state.Network {
		item := protocol.NetworkInterface{Name: ifaceName, MAC: net.HWAddr, State: net.State}
		if item.State == "" {
			item.State = "up"
		}
		for _, addr := range net.Addresses {
			// 只显示全局地址：local/link scope 是环回与链路本地。
			if addr.Scope != "global" {
				continue
			}
			if addr.Family == "inet" {
				item.IPv4 = append(item.IPv4, addr.Address)
			} else {
				item.IPv6 = append(item.IPv6, addr.Address)
			}
		}
		item.RxBytes = net.Counters.BytesReceived
		item.TxBytes = net.Counters.BytesSent
		if len(item.IPv4) > 0 || len(item.IPv6) > 0 {
			reachable = true
		}
		status.Interfaces = append(status.Interfaces, item)
	}
	status.Reachable = reachable
	if !reachable {
		status.Error = "实例网卡已连接但未获取到全局 IP"
	}
	return status, nil
}

// SetRootPassword 注入 root 密码并确保 SSH 可用：
// 1) 容器就绪前 exec 会失败，做有限重试；
// 2) 精简镜像（如 TUNA default 变体）不带 sshd，检测缺失时经 apt 安装；
// 3) 放行 root 密码登录并启动 sshd。
// 任一环境步骤失败不阻塞密码设置本身（用户可稍后重试/自行安装）。
func (d *Incus) SetRootPassword(ctx context.Context, inst *protocol.Instance, password string) error {
	name := resourceName("incus", inst)
	exec := func(timeout context.Context, args ...string) error {
		full := append([]string{"exec", name, "--"}, args...)
		return run(timeout, d.cli(), full...)
	}
	// 容器 init 未完成时 exec 报错：最多等 30 秒。
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(8 * time.Second):
			}
		}
		lastErr = exec(ctx, "true")
		if lastErr == nil {
			break
		}
	}
	if lastErr != nil {
		return fmt.Errorf("设置密码失败（实例可能尚未启动完成）: %w", lastErr)
	}
	// sshd 缺失时安装（Debian 系）。失败仅记录，不影响 chpasswd。
	if exec(ctx, "sh", "-c", "command -v sshd || test -x /usr/sbin/sshd") != nil {
		if exec(ctx, "sh", "-c", "export DEBIAN_FRONTEND=noninteractive; apt-get update -qq && apt-get install -y -qq openssh-server") != nil {
			log.Printf("实例 %s 安装 openssh-server 失败（镜像可能非 Debian 系）", name)
		}
	}
	if err := exec(ctx, "sh", "-c", "echo root:"+password+" | chpasswd"); err != nil {
		return fmt.Errorf("设置密码失败: %w", err)
	}
	// 放行 root 密码登录并拉起 sshd；drop-in 不支持的老配置退回主配置追加。
	exec(ctx, "sh", "-c", "mkdir -p /etc/ssh/sshd_config.d && echo 'PermitRootLogin yes' > /etc/ssh/sshd_config.d/00-virtualis.conf")
	exec(ctx, "sh", "-c", "systemctl enable --now ssh 2>/dev/null || service ssh start 2>/dev/null || /usr/sbin/sshd")
	return nil
}

// VNC 返回容器控制台的本地 VNC 端口（Xvfb + xterm + x11vnc 桥接）。
func (d *Incus) VNC(ctx context.Context, inst *protocol.Instance, _ string) (protocol.VNCInfo, error) {
	port, err := containerVNC.ensure(ctx, d.Name(), inst,
		func() bool { s, _ := d.Status(ctx, inst); return s == StatusRunning },
		func(name string) []string { return []string{d.cli(), "exec", name, "--", "/bin/bash", "-l"} })
	if err != nil {
		return protocol.VNCInfo{Available: false, Message: err.Error()}, nil
	}
	return protocol.VNCInfo{
		Available: true, Protocol: "vnc", Host: "127.0.0.1", Port: port,
		Display: ":" + strconv.Itoa(port-5900), URL: fmt.Sprintf("vnc://127.0.0.1:%d", port),
	}, nil
}

func parseMiB(line string) int64 {
	fields := strings.Fields(line)
	for _, field := range fields {
		field = strings.TrimSuffix(strings.TrimSuffix(field, "MiB"), "MB")
		if value, err := strconv.ParseInt(field, 10, 64); err == nil {
			return value
		}
	}
	return 0
}
