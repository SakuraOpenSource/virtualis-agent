package driver

import (
	"context"
	"fmt"
	"html"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

// QEMU 驱动：经 libvirt（virsh）管理 KVM 虚拟机。
//
// 网络：NAT 模式自动确保 libvirt default NAT 网络存在（virbr0 + DHCP）；
// 独立 IP 模式把网卡直接挂到主机网卡 —— 软件网桥用 bridge 型接口，
// 物理网卡用 macvtap（direct/bridge），实例以自己的 MAC 出现在网段上。
type QEMU struct {
	mu      sync.Mutex
	samples map[uint]qemuSample
	dataDir string
}

type qemuSample struct {
	cpuTime uint64
	rxBytes uint64
	txBytes uint64
	at      time.Time
}

func NewQEMU() *QEMU { return NewQEMUWithDataDir("") }

func NewQEMUWithDataDir(dataDir string) *QEMU {
	if dataDir == "" {
		dataDir = "/var/lib/virtualis-agent"
	}
	return &QEMU{samples: make(map[uint]qemuSample), dataDir: dataDir}
}

func (d *QEMU) imagesDir() string {
	return filepath.Join(d.dataDir, "images")
}

func (d *QEMU) Name() string { return "qemu" }

func (d *QEMU) Probe(_ context.Context) error {
	if !hasCommand("virsh") {
		return fmt.Errorf("virsh 未安装")
	}
	return nil
}

// Create 定义并准备虚拟机：确保网络就绪、准备系统盘（qcow2）与安装介质
// （ISO 走 cdrom），最后生成 domain XML 写入 libvirt。启动由 Start 完成，
// ISO 存在时从光驱优先引导。
func (d *QEMU) Create(ctx context.Context, inst *protocol.Instance) error {
	if !hasCommand("virsh") {
		return fmt.Errorf("virsh 未安装，无法创建 QEMU 实例")
	}
	if err := d.ensureNetwork(ctx, inst); err != nil {
		return err
	}
	name := resourceName("qemu", inst)
	if d.exists(ctx, name) {
		// 幂等：重装/重复投递时已有的域直接复用，由调用方继续 Start。
		return nil
	}

	// 系统盘：磁盘镜像直接挂为 vda；ISO 引导时新建空白 qcow2 承载系统。
	diskPath := filepath.Join(d.imagesDir(), name+".qcow2")
	var isoPath string
	if inst.Image != nil && inst.Image.Path != "" {
		if strings.EqualFold(inst.Image.Type, "iso") {
			isoPath = inst.Image.Path
		} else {
			diskPath = inst.Image.Path
		}
	}
	PrepareDiskDir(d.dataDir)
	if _, err := os.Stat(diskPath); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(diskPath), 0o755); err != nil {
			return fmt.Errorf("创建磁盘目录失败: %w", err)
		}
		if !hasCommand("qemu-img") {
			return fmt.Errorf("qemu-img 未安装")
		}
		size := inst.Spec.DiskGB
		if size < 1 {
			size = 20
		}
		if err := run(ctx, "qemu-img", "create", "-f", "qcow2", diskPath, fmt.Sprintf("%dG", size)); err != nil {
			return err
		}
	}
	// libvirt 以非 root 用户拉起 QEMU，root 落盘的镜像必须放开访问。
	PrepareDiskFile(diskPath)
	if isoPath != "" {
		PrepareDiskFile(isoPath)
	}

	// NAT 模式且有本地系统盘：注入 guest 引导（运行时自动探测网卡 + DHCP、
	// 面板生成的 root 密码）。ISO 引导的空盘挂不出根分区，内部静默跳过。
	if NormalizeNetworkMode(inst.Network.Mode) == NetworkModeNat && isoPath == "" {
		if err := InjectGuestBootstrap(ctx, diskPath, inst.RootPassword); err != nil {
			log.Printf("实例 %d guest 引导注入跳过: %v", inst.ID, err)
		}
	}

	// NAT 模式：派生确定性 MAC 并在 libvirt 网络里做静态 DHCP 保留，
	// 让实例每次都拿到同一 IP，NAT 映射的目标地址才稳定。保留失败
	// （老版本 libvirt 等）不阻塞创建，映射走动态解析回退。
	if NormalizeNetworkMode(inst.Network.Mode) == NetworkModeNat {
		if inst.Network.MAC == "" {
			inst.Network.MAC = natMAC(inst)
		}
		reserved, _ := natSlotIP(inst)
		if reserved != "" && d.reserveNATIP(ctx, inst.Network.MAC, reserved) {
			inst.Network.IPv4 = reserved
		}
	}

	xml := domainXML(name, inst, diskPath, isoPath)
	tmp, err := os.CreateTemp("", "virtualis-domain-*.xml")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(xml); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return run(ctx, "virsh", "define", tmpName)
}

func (d *QEMU) Delete(ctx context.Context, inst *protocol.Instance) error {
	name := resourceName("qemu", inst)
	if !d.exists(ctx, name) {
		return nil
	}
	_ = d.HardStop(ctx, inst)
	// --remove-all-storage 连同 qcow2 一起清理；镜像文件由上层按需删除。
	if err := run(ctx, "virsh", "undefine", name, "--remove-all-storage", "--nvram"); err != nil && !contains(err.Error(), "not found") {
		return err
	}
	d.mu.Lock()
	delete(d.samples, inst.ID)
	d.mu.Unlock()
	return nil
}

func (d *QEMU) Start(ctx context.Context, inst *protocol.Instance) error {
	if err := d.ensureNetwork(ctx, inst); err != nil {
		return err
	}
	err := d.action(ctx, "start", inst, "already active")
	if err == nil {
		return nil
	}
	// 自愈：宿主机重启后 libvirt default 网络可能仍未自启，libvirt 会在
	// 域启动时报 "network ... is not active"。这里强制拉起网络后重试一次。
	if contains(err.Error(), "network") && contains(err.Error(), "not active") {
		if fixErr := d.ensureDefaultNetwork(ctx); fixErr == nil {
			if retryErr := d.action(ctx, "start", inst, "already active"); retryErr == nil {
				return nil
			}
		}
	}
	return err
}
func (d *QEMU) Stop(ctx context.Context, inst *protocol.Instance) error {
	return d.action(ctx, "shutdown", inst, "not active", "Domain not found")
}
func (d *QEMU) Restart(ctx context.Context, inst *protocol.Instance) error {
	return d.action(ctx, "reboot", inst)
}
func (d *QEMU) HardStart(ctx context.Context, inst *protocol.Instance) error {
	return d.Start(ctx, inst)
}
func (d *QEMU) HardStop(ctx context.Context, inst *protocol.Instance) error {
	return d.action(ctx, "destroy", inst, "not active", "Domain not found")
}
func (d *QEMU) HardRestart(ctx context.Context, inst *protocol.Instance) error {
	if err := d.HardStop(ctx, inst); err != nil {
		return err
	}
	return d.HardStart(ctx, inst)
}
func (d *QEMU) Reinstall(ctx context.Context, inst *protocol.Instance) error {
	if err := d.Delete(ctx, inst); err != nil {
		return err
	}
	return d.Create(ctx, inst)
}

func (d *QEMU) Status(ctx context.Context, inst *protocol.Instance) (string, error) {
	out, err := output(ctx, "virsh", "domstate", resourceName("qemu", inst))
	if err != nil {
		return StatusStopped, nil
	}
	state := strings.ToLower(strings.TrimSpace(string(out)))
	if strings.Contains(state, "running") || strings.Contains(state, "paused") {
		return StatusRunning, nil
	}
	return StatusStopped, nil
}

func (d *QEMU) Metrics(ctx context.Context, inst *protocol.Instance) (protocol.Metrics, error) {
	out, err := output(ctx, "virsh", "domstats", resourceName("qemu", inst), "--vcpu", "--balloon", "--interface")
	if err != nil {
		return protocol.Metrics{}, err
	}
	values := parseKeyValues(string(out))
	metrics := defaultMetrics(inst)
	metrics.CollectedAt = time.Now().UTC()
	metrics.MemoryUsedMB = int64(values["balloon.current"] / 1024)
	if metrics.MemoryTotalMB == 0 {
		metrics.MemoryTotalMB = int64(inst.Spec.MemoryMB)
	}
	var cpuTime uint64
	var rxBytes, txBytes uint64
	for key, value := range values {
		if strings.HasSuffix(key, ".time") {
			cpuTime += value
		}
		if strings.HasSuffix(key, ".rx.bytes") {
			rxBytes += value
		}
		if strings.HasSuffix(key, ".tx.bytes") {
			txBytes += value
		}
	}
	metrics.NetworkRxBytes = rxBytes
	metrics.NetworkTxBytes = txBytes
	d.mu.Lock()
	previous, ok := d.samples[inst.ID]
	d.samples[inst.ID] = qemuSample{cpuTime: cpuTime, rxBytes: rxBytes, txBytes: txBytes, at: metrics.CollectedAt}
	d.mu.Unlock()
	if ok {
		seconds := metrics.CollectedAt.Sub(previous.at).Seconds()
		if seconds > 0 {
			if cpuTime >= previous.cpuTime {
				cores := inst.Spec.CPU
				if cores < 1 {
					cores = 1
				}
				metrics.CPUPercent = float64(cpuTime-previous.cpuTime) / (seconds * 1e9 * float64(cores)) * 100
			}
			if rxBytes >= previous.rxBytes {
				metrics.BandwidthRxBps = float64(rxBytes-previous.rxBytes) / seconds
			}
			if txBytes >= previous.txBytes {
				metrics.BandwidthTxBps = float64(txBytes-previous.txBytes) / seconds
			}
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

// ConfigureNetwork ensures the requested network infrastructure and restarts
// the guest so the persisted domain definition is active.
func (d *QEMU) ConfigureNetwork(ctx context.Context, inst *protocol.Instance) error {
	if err := d.ensureNetwork(ctx, inst); err != nil {
		return err
	}
	status, err := d.Status(ctx, inst)
	if err != nil {
		return err
	}
	if status == StatusRunning {
		if err := d.Restart(ctx, inst); err != nil {
			return fmt.Errorf("重启实例使网络配置生效失败: %w", err)
		}
	}
	return nil
}

func (d *QEMU) Network(ctx context.Context, inst *protocol.Instance) (protocol.NetworkStatus, error) {
	name := resourceName("qemu", inst)
	status := protocol.NetworkStatus{CheckedAt: time.Now().UTC()}
	if out, err := output(ctx, "virsh", "domiflist", name); err == nil {
		status.Interfaces = parseQEMUInterfaces(string(out))
	}
	// IP 优先问 guest agent（qemu-guest-agent），不可用再回落 DHCP lease。
	ipOutput, ipErr := output(ctx, "virsh", "domifaddr", name, "--source", "agent")
	if ipErr != nil || !strings.Contains(string(ipOutput), "ipv4") {
		if leaseOutput, leaseErr := output(ctx, "virsh", "domifaddr", name, "--source", "lease"); leaseErr == nil {
			ipOutput = leaseOutput
		}
	}
	// 按 MAC 归位地址：agent 源会列出发客户机内部的所有网卡（含 lo 的
	// 127.0.0.1），与 domiflist 的 vnetX 顺序对不上，按索引填会张冠李戴。
	for _, addr := range parseQEMUAddresses(string(ipOutput)) {
		matched := false
		for i := range status.Interfaces {
			if strings.EqualFold(status.Interfaces[i].MAC, addr.MAC) {
				status.Interfaces[i].IPv4 = append(status.Interfaces[i].IPv4, addr.IPv4...)
				status.Interfaces[i].IPv6 = append(status.Interfaces[i].IPv6, addr.IPv6...)
				matched = true
				break
			}
		}
		// MAC 对不上（lease 源行 MAC 即 domiflist MAC，正常都能对上）时
		// 兜底塞给第一个还没有地址的接口。
		if !matched {
			for i := range status.Interfaces {
				if len(status.Interfaces[i].IPv4) == 0 && len(status.Interfaces[i].IPv6) == 0 {
					status.Interfaces[i].IPv4 = append(status.Interfaces[i].IPv4, addr.IPv4...)
					status.Interfaces[i].IPv6 = append(status.Interfaces[i].IPv6, addr.IPv6...)
					break
				}
			}
		}
	}
	for i := range status.Interfaces {
		if out, err := output(ctx, "virsh", "domifstat", name, status.Interfaces[i].Name); err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				fields := strings.Fields(line)
				if len(fields) != 2 {
					continue
				}
				value, parseErr := strconv.ParseUint(fields[1], 10, 64)
				if parseErr != nil {
					continue
				}
				switch fields[0] {
				case "rx_bytes":
					status.Interfaces[i].RxBytes = value
				case "tx_bytes":
					status.Interfaces[i].TxBytes = value
				}
			}
		}
	}
	if len(status.Interfaces) > 0 {
		status.Reachable = false
		for _, item := range status.Interfaces {
			if len(item.IPv4) > 0 || len(item.IPv6) > 0 {
				status.Reachable = true
				break
			}
		}
		if !status.Reachable {
			// vnet 在、没地址：客户机侧问题（DHCP 未完成/未配置网络），
			// 或没装 qemu-guest-agent 导致 agent/lease 两个来源都查不到。
			status.Error = "实例网卡已连接但未获取到 IP：请确认客户机已配置网络（DHCP），安装 qemu-guest-agent 可提升 IP 可见性"
		}
	} else {
		status.Error = "未找到虚拟网卡或虚拟机尚未启动"
	}
	return status, nil
}

// VNC 返回实例的 VNC 连接信息。libvirt autoport 分配端口，运行时用
// virsh vncdisplay 查询；主程序经 WebSocket 反代连接，端口不对外暴露。
func (d *QEMU) VNC(ctx context.Context, inst *protocol.Instance, host string) (protocol.VNCInfo, error) {
	out, err := output(ctx, "virsh", "vncdisplay", resourceName("qemu", inst))
	if err != nil {
		// 带上 virsh 的原始错误：域不存在、未运行、权限问题一眼可辨。
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			detail = err.Error()
		}
		return protocol.VNCInfo{Available: false, Message: "查询 VNC 失败（域可能不存在或未运行）: " + detail}, nil
	}
	display := strings.TrimSpace(string(out))
	port := 0
	if strings.HasPrefix(display, ":") {
		n, parseErr := strconv.Atoi(strings.TrimPrefix(display, ":"))
		if parseErr == nil {
			port = 5900 + n
		}
	} else if _, portText, splitErr := net.SplitHostPort(display); splitErr == nil {
		value, _ := strconv.Atoi(portText)
		if value >= 0 && value < 100 {
			port = 5900 + value
		} else {
			port = value
		}
	}
	if port == 0 {
		return protocol.VNCInfo{Available: false, Display: display, Message: "无法解析 VNC 端口"}, nil
	}
	host = strings.TrimSpace(host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "" {
		host = "127.0.0.1"
	}
	return protocol.VNCInfo{Available: true, Protocol: "vnc", Host: host, Port: port, Display: display, URL: fmt.Sprintf("vnc://%s:%d", host, port)}, nil
}

// reserveNATIP 在 libvirt default 网络里登记 MAC→IP 的静态 DHCP 条目。
// 返回是否成功；失败时调用方不写静态值，让映射回退到动态解析。
func (d *QEMU) reserveNATIP(ctx context.Context, mac, ip string) bool {
	hostXML := fmt.Sprintf("<host mac='%s' ip='%s'/>", html.EscapeString(mac), html.EscapeString(ip))
	err := run(ctx, "virsh", "net-update", "default", "add-last", "ip-dhcp-host", hostXML, "--live", "--config")
	return err == nil || contains(err.Error(), "exists")
}

// unreserveNATIP 删除一条 MAC→IP 的静态 DHCP 条目，不存在时静默成功。
func (d *QEMU) unreserveNATIP(ctx context.Context, mac, ip string) {
	hostXML := fmt.Sprintf("<host mac='%s' ip='%s'/>", html.EscapeString(mac), html.EscapeString(ip))
	_ = run(ctx, "virsh", "net-update", "default", "delete", "ip-dhcp-host", hostXML, "--live", "--config")
}

// EnsureNATIdentity 让 NAT 静态保留与域里网卡的"真实 MAC"对齐
// （NATIdentityReconciler）。
//
// 旧版本代码创建的实例域里 MAC 字段可能为空——macXML 对空值不输出
// <mac>，libvirt 会派一个随机地址；而 DHCP 静态保留是按派生 MAC 写的，
// 永远等不到匹配的客户端，实例就拿不到保留 IP。这里以 domiflist 查到的
// 实际 MAC 为权威改写保留条目，并把真实 MAC/保留 IP 回填进 inst.Network，
// 由上层同步回主控。域未定义时直接返回（创建流程自己会做首次保留）。
func (d *QEMU) EnsureNATIdentity(ctx context.Context, inst *protocol.Instance) {
	if NormalizeNetworkMode(inst.Network.Mode) != NetworkModeNat {
		return
	}
	name := resourceName("qemu", inst)
	if !d.exists(ctx, name) {
		return
	}
	out, err := output(ctx, "virsh", "domiflist", name)
	if err != nil {
		return
	}
	ifaces := parseQEMUInterfaces(string(out))
	if len(ifaces) == 0 {
		return
	}
	actualMAC := strings.ToLower(ifaces[0].MAC)
	if _, parseErr := net.ParseMAC(actualMAC); parseErr != nil {
		return
	}
	reservedIP, _ := natSlotIP(inst)
	if reservedIP == "" {
		return
	}
	// DB 里 MAC 可能为空：此时按派生 MAC 反查旧保留条目一并清理。
	oldMAC := strings.ToLower(strings.TrimSpace(inst.Network.MAC))
	if oldMAC == "" {
		oldMAC = strings.ToLower(natMAC(inst))
	}
	oldIP := strings.Split(inst.Network.IPv4, "/")[0]
	if oldMAC == actualMAC && oldIP == reservedIP {
		return
	}
	if oldMAC != actualMAC {
		d.unreserveNATIP(ctx, oldMAC, oldIP)
	}
	if oldIP != "" && oldIP != reservedIP {
		d.unreserveNATIP(ctx, actualMAC, oldIP)
	}
	if !d.reserveNATIP(ctx, actualMAC, reservedIP) {
		log.Printf("NAT 实例 %d 写入 DHCP 保留失败: MAC %s → %s", inst.ID, actualMAC, reservedIP)
		return
	}
	inst.Network.MAC = actualMAC
	inst.Network.IPv4 = reservedIP
	log.Printf("NAT 实例 %d 身份已对账: 网卡 MAC %s ↔ 保留 IP %s", inst.ID, actualMAC, reservedIP)
}

// SetRootPassword 经 guest agent 设置 root 密码。客户机的
// qemu-guest-agent 启动完成前该调用会失败，做约 40 秒的有限重试。
func (d *QEMU) SetRootPassword(ctx context.Context, inst *protocol.Instance, password string) error {
	name := resourceName("qemu", inst)
	var lastErr error
	for attempt := 0; attempt < 8; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
			}
		}
		lastErr = run(ctx, "virsh", "set-user-password", "--domain", name, "--user", "root", "--password", password)
		if lastErr == nil {
			return nil
		}
	}
	return fmt.Errorf("设置密码失败（确认客户机已安装并启动 qemu-guest-agent）: %w", lastErr)
}

func parseKeyValues(text string) map[string]uint64 {
	values := make(map[string]uint64)
	for _, line := range strings.Split(text, "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "=", 2)
		if len(parts) != 2 {
			continue
		}
		value, err := strconv.ParseUint(strings.TrimSpace(parts[1]), 10, 64)
		if err == nil {
			values[strings.TrimSpace(parts[0])] = value
		}
	}
	return values
}

func parseQEMUInterfaces(text string) []protocol.NetworkInterface {
	var result []protocol.NetworkInterface
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] == "Interface" || strings.HasPrefix(fields[0], "-") {
			continue
		}
		result = append(result, protocol.NetworkInterface{Name: fields[0], State: "up", MAC: fields[len(fields)-1]})
	}
	return result
}

// qemuAddress 是 domifaddr 输出里一条 MAC→地址 的映射。
type qemuAddress struct {
	MAC        string
	IPv4, IPv6 []string
}

// parseQEMUAddresses 解析 domifaddr 输出（agent/lease 两个源的表格同构）：
// 每行 "接口 MAC 协议 地址/前缀"。跳过环回（127.0.0.0/8、::1）与链路本地
// （fe80::/10），它们会污染"实例是否拿到地址"的判断。
func parseQEMUAddresses(text string) []qemuAddress {
	var order []string
	byMAC := make(map[string]*qemuAddress)
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[0] == "Name" || strings.HasPrefix(fields[0], "-") {
			continue
		}
		mac := strings.ToLower(fields[1])
		if _, err := net.ParseMAC(mac); err != nil {
			continue
		}
		value := strings.Split(fields[len(fields)-1], "/")[0]
		ip := net.ParseIP(value)
		if ip == nil {
			continue
		}
		if v4 := ip.To4(); v4 != nil {
			if v4.IsLoopback() {
				continue
			}
		} else if ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue
		}
		entry, ok := byMAC[mac]
		if !ok {
			entry = &qemuAddress{MAC: mac}
			byMAC[mac] = entry
			order = append(order, mac)
		}
		if ip.To4() != nil {
			entry.IPv4 = append(entry.IPv4, value)
		} else {
			entry.IPv6 = append(entry.IPv6, value)
		}
	}
	result := make([]qemuAddress, 0, len(order))
	for _, mac := range order {
		result = append(result, *byMAC[mac])
	}
	return result
}

func parseQEMUIPs(text string) []string {
	var result []string
	for _, line := range strings.Split(text, "\n") {
		for _, field := range strings.Fields(line) {
			if strings.Contains(field, "/") {
				value := strings.Split(field, "/")[0]
				if ip := net.ParseIP(value); ip != nil {
					result = append(result, value)
				}
			}
		}
	}
	return result
}

func (d *QEMU) action(ctx context.Context, action string, inst *protocol.Instance, ignored ...string) error {
	err := run(ctx, "virsh", action, resourceName("qemu", inst))
	if err == nil {
		return nil
	}
	text := err.Error()
	for _, value := range ignored {
		if contains(text, value) {
			return nil
		}
	}
	return err
}

func (d *QEMU) exists(ctx context.Context, name string) bool {
	return exec.CommandContext(ctx, "virsh", "dominfo", name).Run() == nil
}

// ensureNetwork 保证所选网络模式的基础设施就绪。
//
// NAT：确保 libvirt default NAT 网络存在并启动（virbr0 + DHCP 段）。
// 独立 IP：校验挂载目标（网卡/网桥）存在，且主机有至少 2 个 IPv4 地址 ——
// 只有一个地址说明主机自身都不宽裕，不允许再往同一网段放独立 IP 实例。
// 关闭：无事可做。
func (d *QEMU) ensureNetwork(ctx context.Context, inst *protocol.Instance) error {
	switch NormalizeNetworkMode(inst.Network.Mode) {
	case NetworkModeNone:
		return nil
	case NetworkModeNat:
		return d.ensureDefaultNetwork(ctx)
	case NetworkModeDedicated:
		if _, _, err := dedicatedTarget(inst.Network); err != nil {
			return err
		}
		if !DedicatedReady() {
			return fmt.Errorf("独立 IP 模式要求主机拥有至少 2 个 IPv4 地址，当前不满足")
		}
		return nil
	}
	return nil
}

// ensureDefaultNetwork 定义并启动 libvirt default NAT 网络（幂等）。
//
// 不用 net-info 的 Active 字段判断是否需要启动：该输出是表格对齐格式，
// 不同 libvirt 版本冒号与值的间隔空格数不同，字符串解析不可靠，误判
// "已激活" 会导致跳过启动、域开机时报 network not active。net-start
// 本身幂等且廉价，网络已激活时报错含 "already active"，直接忽略。
func (d *QEMU) ensureDefaultNetwork(ctx context.Context) error {
	if _, err := output(ctx, "virsh", "net-info", "default"); err != nil {
		xml := `<network>
  <name>default</name>
  <forward mode='nat'/>
  <bridge name='virbr0' stp='on' delay='0'/>
  <ip address='192.168.122.1' netmask='255.255.255.0'>
    <dhcp>
      <range start='192.168.122.2' end='192.168.122.254'/>
    </dhcp>
  </ip>
</network>
`
		tmp, createErr := os.CreateTemp("", "virtualis-network-*.xml")
		if createErr != nil {
			return fmt.Errorf("创建 libvirt default 网络配置失败: %w", createErr)
		}
		tmpName := tmp.Name()
		defer os.Remove(tmpName)
		if _, writeErr := tmp.WriteString(xml); writeErr != nil {
			tmp.Close()
			return fmt.Errorf("写入 libvirt default 网络配置失败: %w", writeErr)
		}
		if closeErr := tmp.Close(); closeErr != nil {
			return closeErr
		}
		if defineErr := run(ctx, "virsh", "net-define", tmpName); defineErr != nil && !contains(defineErr.Error(), "already exists") {
			return fmt.Errorf("定义 libvirt default 网络失败: %w", defineErr)
		}
	}
	if err := run(ctx, "virsh", "net-start", "default"); err != nil && !contains(err.Error(), "already active") {
		return fmt.Errorf("启动 libvirt default 网络失败: %w", err)
	}
	// 自启动尽力而为：失败只影响宿主机重启后的第一台虚拟机，本次开机不受影响。
	_ = run(ctx, "virsh", "net-autostart", "default")
	return nil
}

// domainXML 生成 libvirt 域描述，设备模型对齐生产级 KVM 管理面的常用配置：
// host-passthrough CPU、virtio 磁盘/网卡、guest agent 通道、VGA 显卡、
// USB tablet（VNC 指针跟随）。ISO 存在时优先从光驱引导。
func domainXML(name string, inst *protocol.Instance, diskPath, isoPath string) string {
	memory := inst.Spec.MemoryMB
	if memory < 128 {
		memory = 1024
	}
	cpu := inst.Spec.CPU
	if cpu < 1 {
		cpu = 1
	}
	arch := html.EscapeString(inst.Spec.Arch)
	if arch == "" {
		arch = "x86_64"
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<domain type='kvm'>
  <name>%s</name>
  <memory unit='MiB'>%d</memory>
  <currentMemory unit='MiB'>%d</currentMemory>
  <vcpu placement='static'>%d</vcpu>
  <os>
    <type arch='%s' machine='pc'>hvm</type>
`, html.EscapeString(name), memory, memory, cpu, arch)
	if isoPath != "" {
		b.WriteString("    <boot dev='cdrom'/>\n")
	}
	b.WriteString("    <boot dev='hd'/>\n  </os>\n")
	b.WriteString("  <features>\n    <acpi/>\n    <apic/>\n  </features>\n")
	b.WriteString("  <cpu mode='host-passthrough' check='none'>\n    <cache mode='passthrough'/>\n  </cpu>\n")
	b.WriteString(`  <clock offset='utc'>
    <timer name='rtc' tickpolicy='catchup'/>
    <timer name='pit' tickpolicy='delay'/>
    <timer name='hpet' present='no'/>
  </clock>
`)
	b.WriteString("  <on_poweroff>destroy</on_poweroff>\n  <on_reboot>restart</on_reboot>\n  <on_crash>destroy</on_crash>\n")
	b.WriteString("  <pm>\n    <suspend-to-mem enabled='no'/>\n    <suspend-to-disk enabled='no'/>\n  </pm>\n")
	b.WriteString("  <devices>\n")
	if diskPath != "" {
		fmt.Fprintf(&b, `    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2' cache='none' discard='unmap'/>
      <source file='%s'/>
      <target dev='vda' bus='virtio'/>
    </disk>
`, html.EscapeString(diskPath))
	}
	if isoPath != "" {
		fmt.Fprintf(&b, `    <disk type='file' device='cdrom'>
      <driver name='qemu' type='raw'/>
      <source file='%s'/>
      <target dev='sda' bus='sata'/>
      <readonly/>
    </disk>
`, html.EscapeString(isoPath))
	}
	b.WriteString(qemuInterfaceXML(inst.Network))
	b.WriteString(`    <controller type='scsi' index='0' model='virtio-scsi'/>
    <channel type='unix'>
      <target type='virtio' name='org.qemu.guest_agent.0'/>
    </channel>
    <input type='tablet' bus='usb'/>
    <video>
      <model type='vga' vram='16384' heads='1' primary='yes'/>
    </video>
    <memballoon model='virtio'/>
    <graphics type='vnc' autoport='yes' listen='0.0.0.0'/>
    <console type='pty'>
      <target type='serial' port='0'/>
    </console>
  </devices>
</domain>
`)
	return b.String()
}

// qemuInterfaceXML 按网络模式生成 <interface>。
//
// NAT：挂 libvirt default NAT 网络，DHCP 自动发地址。
// 独立 IP：挂主机网桥（bridge 型）或物理网卡（macvtap direct/bridge），
// 实例以自己的 MAC 直接出现在局域网，拿到独立 IP。
// 关闭：不生成网卡。
func qemuInterfaceXML(network protocol.NetworkConfig) string {
	switch NormalizeNetworkMode(network.Mode) {
	case NetworkModeNone:
		return ""
	case NetworkModeNat:
		mac := macXML(network.MAC)
		bandwidth := bandwidthXML(network.BandwidthMbps)
		return fmt.Sprintf("    <interface type='network'>%s<source network='default'/>%s<model type='virtio'/></interface>\n", mac, bandwidth)
	}
	target, isBridge, err := dedicatedTarget(network)
	if err != nil {
		// Create 前的 ensureNetwork 已校验过；这里防御性降级为 NAT。
		mac := macXML(network.MAC)
		return fmt.Sprintf("    <interface type='network'><source network='default'/>%s<model type='virtio'/></interface>\n", mac)
	}
	mac := macXML(network.MAC)
	bandwidth := bandwidthXML(network.BandwidthMbps)
	if isBridge {
		return fmt.Sprintf("    <interface type='bridge'>%s<source bridge='%s'/>%s<model type='virtio'/></interface>\n", mac, html.EscapeString(target), bandwidth)
	}
	return fmt.Sprintf("    <interface type='direct'>%s<source dev='%s' mode='bridge'/>%s<model type='virtio'/></interface>\n", mac, html.EscapeString(target), bandwidth)
}

func macXML(raw string) string {
	parsed, err := net.ParseMAC(raw)
	if err != nil || len(parsed) != 6 {
		return ""
	}
	return fmt.Sprintf("<mac address='%s'/>", html.EscapeString(parsed.String()))
}

func bandwidthXML(mbps int) string {
	if mbps <= 0 {
		return ""
	}
	average := mbps * 1000
	return fmt.Sprintf("<bandwidth><inbound average='%d' peak='%d'/><outbound average='%d' peak='%d'/></bandwidth>", average, average, average, average)
}
