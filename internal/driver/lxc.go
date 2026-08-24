package driver

import (
	"context"
	"fmt"
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
		return run(ctx, "lxc-create", args...)
	}
	return run(ctx, "lxc", "create", name)
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

func lxcArch(arch string) string {
	if strings.EqualFold(arch, "arm64") || strings.EqualFold(arch, "aarch64") {
		return "arm64"
	}
	return "amd64"
}
