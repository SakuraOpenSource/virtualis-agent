package driver

import (
	"context"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

type QEMU struct{}

func NewQEMU() *QEMU         { return &QEMU{} }
func (d *QEMU) Name() string { return "qemu" }

func (d *QEMU) Probe(_ context.Context) error {
	if !hasCommand("virsh") {
		return fmt.Errorf("virsh 未安装")
	}
	return nil
}

func (d *QEMU) Create(ctx context.Context, inst *protocol.Instance) error {
	if !hasCommand("virsh") {
		return fmt.Errorf("virsh 未安装，无法创建 QEMU 实例")
	}
	name := resourceName("qemu", inst)
	if d.exists(ctx, name) {
		return nil
	}
	diskPath := filepath.Join("/var/lib/virtualis-agent/images", name+".qcow2")
	var isoPath string
	if inst.Image != nil && inst.Image.Path != "" {
		if strings.EqualFold(inst.Image.Type, "iso") {
			isoPath = inst.Image.Path
		} else {
			diskPath = inst.Image.Path
		}
	}
	if isoPath != "" || diskPath != "" {
		if _, err := os.Stat(diskPath); os.IsNotExist(err) {
			if err := os.MkdirAll(filepath.Dir(diskPath), 0o700); err != nil {
				return fmt.Errorf("创建 QEMU 磁盘目录失败: %w", err)
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
	}
	xml := domainXML(name, inst, diskPath, isoPath)
	tmp, err := os.CreateTemp("", "virtualis-qemu-*.xml")
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
	if err := run(ctx, "virsh", "undefine", name, "--remove-all-storage"); err != nil && !contains(err.Error(), "not found") {
		return err
	}
	return nil
}

func (d *QEMU) Start(ctx context.Context, inst *protocol.Instance) error {
	return d.action(ctx, "start", inst, "already active")
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
	if strings.Contains(state, "running") {
		return StatusRunning, nil
	}
	return StatusStopped, nil
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
	fmt.Fprintf(&b, "<domain type='kvm'><name>%s</name><memory unit='MiB'>%d</memory><currentMemory unit='MiB'>%d</currentMemory><vcpu placement='static'>%d</vcpu><os><type arch='%s'>hvm</type></os><devices>", html.EscapeString(name), memory, memory, cpu, arch)
	if diskPath != "" {
		fmt.Fprintf(&b, "<disk type='file' device='disk'><driver name='qemu' type='qcow2'/><source file='%s'/><target dev='vda' bus='virtio'/></disk>", html.EscapeString(diskPath))
	}
	if isoPath != "" {
		fmt.Fprintf(&b, "<disk type='file' device='cdrom'><driver name='qemu' type='raw'/><source file='%s'/><target dev='sda' bus='sata'/><readonly/></disk>", html.EscapeString(isoPath))
	}
	b.WriteString("<interface type='user'><model type='virtio'/></interface><console type='pty'/></devices></domain>")
	return b.String()
}

func contains(s, part string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(part))
}
