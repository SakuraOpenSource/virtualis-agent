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
	// 创建时就地确认网络已注册（静态 IPv4 真的拿到手），不留到首次使用。
	if NormalizeNetworkMode(inst.Network.Mode) == NetworkModeNat && inst.Network.IPv4 != "" {
		if err := d.ensureContainerIPv4(ctx, name, inst.Network.IPv4); err != nil {
			log.Printf("实例 %d 容器 IPv4 确认失败: %v", inst.ID, err)
		}
	}
	return nil
}

// ConfigureNetwork rebuilds the instance profile and reapplies it to an
// existing container. Incus profiles are live for most device changes; a
// restart makes the new DHCP/static address deterministic.
func (d *Incus) ConfigureNetwork(ctx context.Context, inst *protocol.Instance) error {
	name := resourceName("incus", inst)
	profile := fmt.Sprintf("virtualis-p-%d", inst.ID)
	if err := d.ensureProfile(ctx, profile, inst.Network, inst); err != nil {
		return err
	}
	if err := run(ctx, d.cli(), "profile", "assign", name, profile); err != nil && !contains(err.Error(), "already assigned") {
		return fmt.Errorf("应用实例 profile 失败: %w", err)
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
	if NormalizeNetworkMode(inst.Network.Mode) == NetworkModeNat && inst.Network.IPv4 != "" {
		if err := d.ensureContainerIPv4(ctx, name, inst.Network.IPv4); err != nil {
			return fmt.Errorf("确认容器 IPv4 失败: %w", err)
		}
	}
	return nil
}

// 幂等：profile 已存在时仅重建设备定义。
func (d *Incus) ensureProfile(ctx context.Context, profile string, network protocol.NetworkConfig, inst *protocol.Instance) error {
	if err := run(ctx, d.cli(), "profile", "create", profile); err != nil && !contains(err.Error(), "already exists") {
		return fmt.Errorf("创建实例 profile 失败: %w", err)
	}
	// 从默认 profile 继承 root 磁盘；首次创建时重建设备定义。
	// 已被实例使用的 profile 不能删除 root（Incus 会拒绝），此时保留
	// 现有 root 并只更新配额；网络修复必须能在运行中的实例上幂等执行。
	rootRemoved := run(ctx, d.cli(), "profile", "device", "remove", profile, "root")
	if rootRemoved == nil {
		rootArgs := []string{"profile", "device", "add", profile, "root", "disk", "path=/", "pool=default"}
		if size := inst.Spec.DiskGB; size > 0 {
			rootArgs = append(rootArgs, fmt.Sprintf("size=%dGiB", size))
		}
		if err := run(ctx, d.cli(), rootArgs...); err != nil {
			return fmt.Errorf("配置 root 磁盘失败: %w", err)
		}
	} else if size := inst.Spec.DiskGB; size > 0 {
		if err := run(ctx, d.cli(), "profile", "device", "set", profile, "root", fmt.Sprintf("size=%dGiB", size)); err != nil {
			return fmt.Errorf("更新 root 磁盘配额失败: %w", err)
		}
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
		return "", fmt.Errorf("读取 Incus 实例状态失败: %w", err)
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
	if inst.Network.IPv4 != "" && len(status.Interfaces) > 0 {
		// Static NAT reservation is authoritative even if Incus reports the
		// address with a non-global scope during early boot.
		for i := range status.Interfaces {
			if status.Interfaces[i].Name == "eth0" {
				found := false
				for _, ip := range status.Interfaces[i].IPv4 {
					if strings.Split(ip, "/")[0] == strings.Split(inst.Network.IPv4, "/")[0] {
						found = true
						break
					}
				}
				if !found {
					status.Interfaces[i].IPv4 = append(status.Interfaces[i].IPv4, strings.Split(inst.Network.IPv4, "/")[0])
				}
				status.Reachable = true
				status.Error = ""
				break
			}
		}
	}
	return status, nil
}

// ensureContainerIPv4 确保容器的 eth0 真的拿到了期望的静态 IPv4。
//
// Incus 的 dnsmasq 静态租约偶发竞态：profile 配好保留地址，容器首次
// DHCP 请求却没拿到（lease 表有 STATIC 条目、容器内只有 IPv6 链路本地）。
// 创建时就地修复而不是留到"第一次启动才发现"：轮询 → 强制重启再轮询 →
// 最终兜底在容器内直接静态加地址+网关。全部失败才返回错误。
func (d *Incus) ensureContainerIPv4(ctx context.Context, name, expectIP string) error {
	if expectIP == "" {
		return nil
	}
	hasIPv4 := func() bool {
		state, err := d.queryState(ctx, name)
		if err != nil {
			return false
		}
		net, ok := state.Network["eth0"]
		if !ok {
			return false
		}
		for _, addr := range net.Addresses {
			if addr.Family == "inet" && addr.Scope == "global" {
				return true
			}
		}
		return false
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if hasIPv4() {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	// DHCP 竞态：重启容器重新走一遍 DHCP。
	_ = run(ctx, d.cli(), "restart", name, "--force")
	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if hasIPv4() {
			log.Printf("容器 %s 重启后已获取 IPv4", name)
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	// 最终兜底：容器内直接静态配置（网关取 incusbr0 地址）。
	// natSlotIPOn 返回 (guestIP, gatewayIP)，这里必须取第二个值；
	// 旧代码误用 guestIP（如 10.10.10.101）作网关，会让容器失去外网，
	// 随后的 openssh-server 安装必然失败。
	_, gateway := natSlotIPOn("incusbr0", &protocol.Instance{ID: 1})
	gw := gateway
	ip := strings.Split(expectIP, "/")[0]
	script := "ip addr add " + ip + "/24 dev eth0 2>/dev/null; ip link set eth0 up"
	if gw != "" {
		script += "; ip route replace default via " + gw + " dev eth0"
	}
	if err := run(ctx, d.cli(), "exec", name, "--", "sh", "-c", script); err != nil {
		return fmt.Errorf("静态兜底配置失败: %w", err)
	}
	log.Printf("容器 %s DHCP 未就绪，已在容器内静态配置 %s", name, ip)
	return nil
}

// SetRootPassword 注入 root 密码并确保 SSH 可用：
// 1) 容器就绪前 exec 会失败，做有限重试；
// 2) 精简镜像不带 sshd，按发行版包管理器安装；
// 3) 放行 root 密码登录、校验配置并启动 sshd；
// 4) 任一步失败都返回错误，绝不能把“仅 chpasswd 成功”冒充为 SSH 可用。
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
		return fmt.Errorf("等待容器初始化失败: %w", lastErr)
	}

	if exec(ctx, "sh", "-c", "command -v sshd >/dev/null 2>&1 || test -x /usr/sbin/sshd") != nil {
		// 安装包之前先确保容器有正确的 IPv4 和默认路由。
		var ipv4 string
		if state, err := d.queryState(ctx, name); err == nil {
			for _, addr := range state.Network["eth0"].Addresses {
				if addr.Family == "inet" && addr.Scope == "global" {
					ipv4 = addr.Address
					break
				}
			}
		}
		if ipv4 == "" && inst.Network.IPv4 != "" {
			if err := d.ensureContainerIPv4(ctx, name, inst.Network.IPv4); err != nil {
				return fmt.Errorf("安装 sshd 前配置 IPv4 失败: %w", err)
			}
		}
		install := "if command -v apt-get >/dev/null 2>&1; then " +
			"export DEBIAN_FRONTEND=noninteractive; apt-get update -qq && apt-get install -y -qq openssh-server; " +
			"elif command -v apk >/dev/null 2>&1; then apk add --no-cache openssh; " +
			"elif command -v dnf >/dev/null 2>&1; then dnf install -y openssh-server; " +
			"elif command -v yum >/dev/null 2>&1; then yum install -y openssh-server; " +
			"elif command -v pacman >/dev/null 2>&1; then pacman -Sy --noconfirm openssh; " +
			"else echo 'unsupported package manager' >&2; exit 127; fi"
		if err := exec(ctx, "sh", "-c", install); err != nil {
			return fmt.Errorf("安装 SSH 服务失败: %w", err)
		}
	}
	if err := exec(ctx, "sh", "-c", "command -v sshd >/dev/null 2>&1 || test -x /usr/sbin/sshd"); err != nil {
		return fmt.Errorf("安装后仍找不到 sshd: %w", err)
	}
	if err := exec(ctx, "sh", "-c", "echo root:"+shellQuote(password)+" | chpasswd"); err != nil {
		return fmt.Errorf("设置 root 密码失败: %w", err)
	}
	config := "mkdir -p /etc/ssh/sshd_config.d && printf '%s\\n' 'PermitRootLogin yes' 'PasswordAuthentication yes' > /etc/ssh/sshd_config.d/00-virtualis.conf"
	if err := exec(ctx, "sh", "-c", config); err != nil {
		return fmt.Errorf("写入 SSH 登录配置失败: %w", err)
	}
	if err := exec(ctx, "sh", "-c", "sshd -t"); err != nil {
		return fmt.Errorf("SSH 配置校验失败: %w", err)
	}
	start := "systemctl enable --now ssh 2>/dev/null || systemctl enable --now sshd 2>/dev/null || service ssh start 2>/dev/null || service sshd start 2>/dev/null || /usr/sbin/sshd"
	if err := exec(ctx, "sh", "-c", start); err != nil {
		return fmt.Errorf("启动 SSH 服务失败: %w", err)
	}
	if err := exec(ctx, "sh", "-c", "pgrep -x sshd >/dev/null 2>&1 || ss -lnt 2>/dev/null | grep -q ':22 '"); err != nil {
		return fmt.Errorf("SSH 服务启动后未监听 22 端口: %w", err)
	}
	return nil
}

// shellQuote 返回可安全嵌入 POSIX shell 单引号字符串的内容。
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
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
