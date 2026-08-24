package driver

import (
	"context"
	"fmt"
	"html"
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

type QEMU struct {
	mu      sync.Mutex
	samples map[uint]qemuSample
}

type qemuSample struct {
	cpuTime uint64
	rxBytes uint64
	txBytes uint64
	at      time.Time
}

func NewQEMU() *QEMU         { return &QEMU{samples: make(map[uint]qemuSample)} }
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
	if err := d.ensureNetwork(ctx, inst); err != nil {
		return err
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
	d.mu.Lock()
	delete(d.samples, inst.ID)
	d.mu.Unlock()
	return nil
}

func (d *QEMU) Start(ctx context.Context, inst *protocol.Instance) error {
	if err := d.ensureNetwork(ctx, inst); err != nil {
		return err
	}
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

func (d *QEMU) Network(ctx context.Context, inst *protocol.Instance) (protocol.NetworkStatus, error) {
	name := resourceName("qemu", inst)
	status := protocol.NetworkStatus{CheckedAt: time.Now().UTC()}
	if out, err := output(ctx, "virsh", "domiflist", name); err == nil {
		status.Interfaces = parseQEMUInterfaces(string(out))
	}
	ipOutput, ipErr := output(ctx, "virsh", "domifaddr", name, "--source", "agent")
	if ipErr != nil {
		ipOutput, _ = output(ctx, "virsh", "domifaddr", name, "--source", "lease")
	}
	if len(ipOutput) > 0 {
		ips := parseQEMUIPs(string(ipOutput))
		for i := range ips {
			if i < len(status.Interfaces) {
				status.Interfaces[i].IPv4 = append(status.Interfaces[i].IPv4, ips[i])
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
	} else {
		status.Error = "未找到虚拟网卡或虚拟机尚未启动"
	}
	return status, nil
}

func (d *QEMU) VNC(ctx context.Context, inst *protocol.Instance, host string) (protocol.VNCInfo, error) {
	out, err := output(ctx, "virsh", "vncdisplay", resourceName("qemu", inst))
	if err != nil {
		return protocol.VNCInfo{Available: false, Message: "实例尚未启用 VNC 或尚未启动"}, nil
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

func (d *QEMU) ensureNetwork(ctx context.Context, inst *protocol.Instance) error {
	mode := strings.ToLower(strings.TrimSpace(inst.Network.Mode))
	if mode == "none" || mode == "bridge" {
		return nil
	}
	info, err := output(ctx, "virsh", "net-info", "default")
	if err != nil {
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
		tmp, createErr := os.CreateTemp("", "virtualis-default-network-*.xml")
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
		info, err = output(ctx, "virsh", "net-info", "default")
	}
	if err != nil {
		return fmt.Errorf("读取 libvirt default 网络失败: %w", err)
	}
	if contains(string(info), "active: yes") {
		return nil
	}
	if err := run(ctx, "virsh", "net-start", "default"); err != nil && !contains(err.Error(), "already active") {
		return fmt.Errorf("启动 libvirt default 网络失败: %w", err)
	}
	if err := run(ctx, "virsh", "net-autostart", "default"); err != nil && !contains(err.Error(), "already active") {
		return fmt.Errorf("设置 libvirt default 网络开机启动失败: %w", err)
	}
	return nil
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
	b.WriteString(networkXML(inst.Network))
	b.WriteString("<graphics type='vnc' autoport='yes' listen='0.0.0.0'/><console type='pty'/></devices></domain>")
	return b.String()
}

func networkXML(network protocol.NetworkConfig) string {
	mode := strings.ToLower(strings.TrimSpace(network.Mode))
	if mode == "none" {
		return ""
	}
	interfaceType := "network"
	source := "<source network='default'/>"
	if mode == "bridge" && network.Bridge != "" {
		interfaceType = "bridge"
		source = fmt.Sprintf("<source bridge='%s'/>", html.EscapeString(network.Bridge))
	}
	mac := ""
	if parsed, err := net.ParseMAC(network.MAC); err == nil && len(parsed) == 6 {
		mac = fmt.Sprintf("<mac address='%s'/>", html.EscapeString(parsed.String()))
	}
	bandwidth := ""
	if network.BandwidthMbps > 0 {
		average := network.BandwidthMbps * 1000
		bandwidth = fmt.Sprintf("<bandwidth><inbound average='%d'/><outbound average='%d'/></bandwidth>", average, average)
	}
	return fmt.Sprintf("<interface type='%s'>%s%s<model type='virtio'/>%s</interface>", interfaceType, source, mac, bandwidth)
}

func contains(s, part string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(part))
}
