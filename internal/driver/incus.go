package driver

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

// Incus 驱动：离线导入镜像后 launch，网络经 device 配置。
type Incus struct{}

func NewIncus() *Incus        { return &Incus{} }
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
	if err := d.ensureProfile(ctx, profile, inst.Network); err != nil {
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
func (d *Incus) ensureProfile(ctx context.Context, profile string, network protocol.NetworkConfig) error {
	if err := run(ctx, d.cli(), "profile", "create", profile); err != nil && !contains(err.Error(), "already exists") {
		return fmt.Errorf("创建实例 profile 失败: %w", err)
	}
	// 从默认 profile 继承 root 磁盘；已存在先移除再添加（幂等重建）。
	_ = run(ctx, d.cli(), "profile", "device", "remove", profile, "root")
	if err := run(ctx, d.cli(), "profile", "device", "add", profile, "root", "disk", "path=/", "pool=default"); err != nil {
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
	return run(ctx, d.cli(), "start", resourceName("incus", inst))
}
func (d *Incus) Stop(ctx context.Context, inst *protocol.Instance) error {
	containerVNC.stop(d.Name(), inst)
	return run(ctx, d.cli(), "stop", resourceName("incus", inst))
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

func (d *Incus) Metrics(ctx context.Context, inst *protocol.Instance) (protocol.Metrics, error) {
	metrics := collectHostMetrics(inst)
	name := resourceName("incus", inst)
	if out, err := output(ctx, d.cli(), "info", name, "--resources"); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			lower := strings.ToLower(strings.TrimSpace(line))
			if strings.HasPrefix(lower, "memory:") && strings.Contains(lower, "used") {
				// Incus 输出是人类可读格式；解析不出时保留配置值作为总量。
				metrics.MemoryUsedMB = parseMiB(lower)
			}
		}
	}
	return metrics, nil
}

func (d *Incus) Network(ctx context.Context, inst *protocol.Instance) (protocol.NetworkStatus, error) {
	status := collectHostNetwork(ctx, inst.Network)
	name := resourceName("incus", inst)
	if out, err := output(ctx, d.cli(), "exec", name, "--", "ip", "-o", "addr", "show"); err == nil {
		guest := parseIPCommand(string(out))
		if len(guest) > 0 {
			status.Interfaces = guest
			status.Reachable = true
			status.Error = ""
		}
	}
	return status, nil
}

// SetRootPassword 经 exec 注入 root 密码，容器启动后立即可用。
func (d *Incus) SetRootPassword(ctx context.Context, inst *protocol.Instance, password string) error {
	name := resourceName("incus", inst)
	if err := run(ctx, d.cli(), "exec", name, "--", "sh", "-c", "echo root:"+password+" | chpasswd"); err != nil {
		return fmt.Errorf("设置密码失败（实例可能尚未启动完成）: %w", err)
	}
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
