package driver

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

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
	name := resourceName("incus", inst)
	args := []string{"launch", "images:ubuntu/22.04", name}
	if inst.Type == "vm" {
		args = append(args, "--vm")
	}
	if inst.Spec.CPU > 0 {
		args = append(args, "-c", fmt.Sprintf("limits.cpu=%d", inst.Spec.CPU))
	}
	if inst.Spec.MemoryMB > 0 {
		args = append(args, "-c", fmt.Sprintf("limits.memory=%dMiB", inst.Spec.MemoryMB))
	}
	if inst.Image != nil && inst.Image.Path != "" {
		alias := name + "-image"
		if err := run(ctx, d.cli(), "image", "import", inst.Image.Path, alias); err != nil {
			return err
		}
		args[1] = alias
	}
	if err := run(ctx, d.cli(), args...); err != nil {
		return err
	}
	return configureIncusNetwork(ctx, d.cli(), name, inst.Network)
}

func (d *Incus) Delete(ctx context.Context, inst *protocol.Instance) error {
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
	return run(ctx, d.cli(), "stop", resourceName("incus", inst))
}
func (d *Incus) Restart(ctx context.Context, inst *protocol.Instance) error {
	return run(ctx, d.cli(), "restart", resourceName("incus", inst))
}
func (d *Incus) HardStart(ctx context.Context, inst *protocol.Instance) error {
	return d.Start(ctx, inst)
}
func (d *Incus) HardStop(ctx context.Context, inst *protocol.Instance) error {
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
				// Incus output is human-readable; the configured total remains
				// authoritative when the running value cannot be parsed safely.
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

func (d *Incus) VNC(context.Context, *protocol.Instance, string) (protocol.VNCInfo, error) {
	return unsupportedVNC("incus")
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

func configureIncusNetwork(ctx context.Context, cli, name string, network protocol.NetworkConfig) error {
	mode := strings.ToLower(strings.TrimSpace(network.Mode))
	if mode == "none" {
		return run(ctx, cli, "config", "device", "remove", name, "eth0")
	}
	if mode == "bridge" && network.Bridge != "" {
		if err := run(ctx, cli, "config", "device", "set", name, "eth0", "nictype", "bridged"); err != nil {
			return err
		}
		if err := run(ctx, cli, "config", "device", "set", name, "eth0", "parent", network.Bridge); err != nil {
			return err
		}
	}
	if network.MAC != "" {
		if err := run(ctx, cli, "config", "device", "set", name, "eth0", "hwaddr", network.MAC); err != nil {
			return err
		}
	}
	if network.IPv4 != "" {
		if err := run(ctx, cli, "config", "device", "set", name, "eth0", "ipv4.address", network.IPv4); err != nil {
			return err
		}
	}
	if network.BandwidthMbps > 0 {
		limit := fmt.Sprintf("%dMbit", network.BandwidthMbps)
		if err := run(ctx, cli, "config", "device", "set", name, "eth0", "limits.ingress", limit); err != nil {
			return err
		}
		if err := run(ctx, cli, "config", "device", "set", name, "eth0", "limits.egress", limit); err != nil {
			return err
		}
	}
	return nil
}
