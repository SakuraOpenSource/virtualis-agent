package driver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

// LXC 驱动（经典 lxc 工具链）：lxc-create 从本地磁盘模板创建，
// 网络写入 /var/lib/lxc/<name>/config。
type LXC struct{}

func NewLXC() *LXC          { return &LXC{} }
func (d *LXC) Name() string { return "lxc" }

func (d *LXC) Probe(_ context.Context) error {
	for _, name := range []string{"lxc-create", "lxc-info", "lxc"} {
		if hasCommand(name) {
			return nil
		}
	}
	return fmt.Errorf("LXC 未安装")
}

func (d *LXC) Create(ctx context.Context, inst *protocol.Instance) error {
	if inst.Image == nil || inst.Image.Path == "" {
		return fmt.Errorf("LXC 离线模式需要先上传镜像到 data/images，请在镜像管理上传镜像后重试")
	}
	if !strings.EqualFold(inst.Image.Type, "disk") {
		return fmt.Errorf("LXC 仅支持磁盘镜像类型")
	}
	name := resourceName("lxc", inst)
	if hasCommand("lxc-create") {
		if err := run(ctx, "lxc-create", "-n", name, "-t", "local", "--", "-f", inst.Image.Path); err != nil {
			return err
		}
		return configureLXCNetwork(name, inst.Network)
	}
	return run(ctx, "lxc", "create", name)
}

// configureLXCNetwork 把网络配置追加到容器 config。
//
// NAT：veth 挂 lxcbr0，由宿主 dnsmasq 发地址，共享主机出口 IP。
// 独立 IP：veth 挂指定网桥，静态下发 IPv4/网关；主机需有至少 2 个 IPv4。
// 关闭：不写网络配置（使用镜像默认）。
func configureLXCNetwork(name string, network protocol.NetworkConfig) error {
	mode := NormalizeNetworkMode(network.Mode)
	if mode == NetworkModeNone {
		return nil
	}
	bridge := "lxcbr0"
	if mode == NetworkModeDedicated {
		target, _, err := dedicatedTarget(network)
		if err != nil {
			return err
		}
		if !DedicatedReady() {
			return fmt.Errorf("独立 IP 模式要求主机拥有至少 2 个 IPv4 地址，当前不满足")
		}
		bridge = target
	}
	path := filepath.Join("/var/lib/lxc", name, "config")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()
	lines := []string{"", "# Virtualis network", "lxc.net.0.type = veth", "lxc.net.0.link = " + bridge}
	if network.MAC != "" {
		lines = append(lines, "lxc.net.0.hwaddr = "+network.MAC)
	}
	if mode == NetworkModeDedicated && network.IPv4 != "" {
		lines = append(lines, "lxc.net.0.ipv4.address = "+network.IPv4)
	}
	if mode == NetworkModeDedicated && network.Gateway != "" {
		lines = append(lines, "lxc.net.0.ipv4.gateway = "+network.Gateway)
	}
	_, err = file.WriteString(strings.Join(lines, "\n") + "\n")
	return err
}

func (d *LXC) Delete(ctx context.Context, inst *protocol.Instance) error {
	name := resourceName("lxc", inst)
	if hasCommand("lxc-destroy") {
		err := run(ctx, "lxc-destroy", "-n", name, "-f")
		if err != nil && !contains(err.Error(), "does not exist") {
			return err
		}
		return nil
	}
	return run(ctx, "lxc", "delete", name, "--force")
}

func (d *LXC) Start(ctx context.Context, inst *protocol.Instance) error {
	name := resourceName("lxc", inst)
	if hasCommand("lxc-start") {
		return run(ctx, "lxc-start", "-n", name, "-d")
	}
	return run(ctx, "lxc", "start", name)
}

func (d *LXC) Stop(ctx context.Context, inst *protocol.Instance) error {
	name := resourceName("lxc", inst)
	if hasCommand("lxc-stop") {
		return run(ctx, "lxc-stop", "-n", name)
	}
	return run(ctx, "lxc", "stop", name)
}

func (d *LXC) Restart(ctx context.Context, inst *protocol.Instance) error {
	if err := d.Stop(ctx, inst); err != nil {
		return err
	}
	return d.Start(ctx, inst)
}
func (d *LXC) HardStart(ctx context.Context, inst *protocol.Instance) error {
	return d.Start(ctx, inst)
}
func (d *LXC) HardStop(ctx context.Context, inst *protocol.Instance) error {
	name := resourceName("lxc", inst)
	if hasCommand("lxc-stop") {
		return run(ctx, "lxc-stop", "-n", name, "-k")
	}
	return run(ctx, "lxc", "stop", name, "--force")
}
func (d *LXC) HardRestart(ctx context.Context, inst *protocol.Instance) error {
	if err := d.HardStop(ctx, inst); err != nil {
		return err
	}
	return d.HardStart(ctx, inst)
}
func (d *LXC) Reinstall(ctx context.Context, inst *protocol.Instance) error {
	if err := d.Delete(ctx, inst); err != nil {
		return err
	}
	return d.Create(ctx, inst)
}

func (d *LXC) Status(ctx context.Context, inst *protocol.Instance) (string, error) {
	name := resourceName("lxc", inst)
	if hasCommand("lxc-info") {
		out, err := output(ctx, "lxc-info", "-n", name)
		if err != nil {
			return StatusStopped, nil
		}
		if contains(string(out), "RUNNING") {
			return StatusRunning, nil
		}
		return StatusStopped, nil
	}
	out, err := output(ctx, "lxc", "list", name, "--format", "csv")
	if err != nil || !contains(string(out), "running") {
		return StatusStopped, nil
	}
	return StatusRunning, nil
}

func (d *LXC) Metrics(ctx context.Context, inst *protocol.Instance) (protocol.Metrics, error) {
	// LXC 的 cgroup 计数在 cgroup v1/v2 与发行版之间差异很大，这里只给
	// 配置内存与宿主侧网络计数，不伪造容器 CPU。
	metrics := collectHostMetrics(inst)
	_ = ctx
	return metrics, nil
}

func (d *LXC) Network(ctx context.Context, inst *protocol.Instance) (protocol.NetworkStatus, error) {
	status := collectHostNetwork(ctx, inst.Network)
	name := resourceName("lxc", inst)
	if hasCommand("lxc-attach") {
		if out, err := output(ctx, "lxc-attach", "-n", name, "--", "ip", "-o", "addr", "show"); err == nil {
			guest := parseIPCommand(string(out))
			if len(guest) > 0 {
				status.Interfaces = guest
				status.Reachable = true
				status.Error = ""
			}
		}
	} else if out, err := output(ctx, "lxc-info", "-n", name, "-iH"); err == nil {
		ip := strings.TrimSpace(string(out))
		if ip != "" {
			status.Interfaces = []protocol.NetworkInterface{{Name: "eth0", State: "up", IPv4: []string{ip}}}
			status.Reachable = true
			status.Error = ""
		}
	}
	return status, nil
}

// VNC：LXC 容器无图形控制台。
func (d *LXC) VNC(context.Context, *protocol.Instance, string) (protocol.VNCInfo, error) {
	return unsupportedVNC("lxc")
}
