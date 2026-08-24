package driver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

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
	name := resourceName("lxc", inst)
	if hasCommand("lxc-create") {
		args := []string{"-n", name, "-t", "download", "--", "-d", "ubuntu", "-r", "jammy", "-a", lxcArch(inst.Spec.Arch)}
		if inst.Image != nil && inst.Image.Path != "" && strings.EqualFold(inst.Image.Type, "disk") {
			// A local LXC tarball is accepted by the local template when present.
			args = []string{"-n", name, "-t", "local", "--", "-f", inst.Image.Path}
		}
		if err := run(ctx, "lxc-create", args...); err != nil {
			return err
		}
		return configureLXCNetwork(name, inst.Network)
	}
	if err := run(ctx, "lxc", "create", name); err != nil {
		return err
	}
	return nil
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
	metrics := collectHostMetrics(inst)
	// LXC exposes cgroup counters differently across cgroup v1/v2 and distro
	// versions. Return the configured memory and live network counters while
	// leaving unavailable container CPU counters explicit as zero.
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

func (d *LXC) VNC(context.Context, *protocol.Instance, string) (protocol.VNCInfo, error) {
	return unsupportedVNC("lxc")
}

func lxcArch(arch string) string {
	if strings.EqualFold(arch, "arm64") || strings.EqualFold(arch, "aarch64") {
		return "arm64"
	}
	return "amd64"
}

func configureLXCNetwork(name string, network protocol.NetworkConfig) error {
	if strings.EqualFold(network.Mode, "none") {
		return nil
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
	bridge := network.Bridge
	if bridge == "" {
		bridge = "lxcbr0"
	}
	lines := []string{"", "# Virtualis network", "lxc.net.0.type = veth", "lxc.net.0.link = " + bridge}
	if network.MAC != "" {
		lines = append(lines, "lxc.net.0.hwaddr = "+network.MAC)
	}
	if network.IPv4 != "" {
		lines = append(lines, "lxc.net.0.ipv4.address = "+network.IPv4)
	}
	if network.Gateway != "" {
		lines = append(lines, "lxc.net.0.ipv4.gateway = "+network.Gateway)
	}
	_, err = file.WriteString(strings.Join(lines, "\n") + "\n")
	return err
}
