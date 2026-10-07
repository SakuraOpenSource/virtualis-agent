package driver

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

const (
	StatusRunning = "running"
	StatusStopped = "stopped"
)

// Driver 是被控上的虚拟化后端抽象。所有实现都必须无状态、可并发使用：
// 实例清单在主控落库，被控只按请求操作本机资源。
type Driver interface {
	Name() string
	Probe(context.Context) error
	Create(context.Context, *protocol.Instance) error
	Delete(context.Context, *protocol.Instance) error
	Start(context.Context, *protocol.Instance) error
	Stop(context.Context, *protocol.Instance) error
	Restart(context.Context, *protocol.Instance) error
	HardStart(context.Context, *protocol.Instance) error
	HardStop(context.Context, *protocol.Instance) error
	HardRestart(context.Context, *protocol.Instance) error
	Reinstall(context.Context, *protocol.Instance) error
	Status(context.Context, *protocol.Instance) (string, error)
	Metrics(context.Context, *protocol.Instance) (protocol.Metrics, error)
	Network(context.Context, *protocol.Instance) (protocol.NetworkStatus, error)
	// ConfigureNetwork reapplies the instance profile/device configuration.
	ConfigureNetwork(context.Context, *protocol.Instance) error
	VNC(context.Context, *protocol.Instance, string) (protocol.VNCInfo, error)
	// SetRootPassword 尝试把 root 密码注入运行中的实例。容器走 exec
	// chpasswd 立即生效；QEMU 依赖 guest agent，客户机启动完成前会失败，
	// 实现内部做有限重试。注入失败返回错误，由调用方决定是否重试。
	SetRootPassword(context.Context, *protocol.Instance, string) error
	// CreateNetwork 创建 VPC 虚拟网络（托管网络 / libvirt 命名网络），
	// DeleteNetwork 删除同名网络。
	CreateNetwork(context.Context, protocol.NetworkSpec) error
	DeleteNetwork(context.Context, string) error
}

// NATIdentityReconciler 由支持 NAT 的驱动可选实现：按域里网卡的"真实 MAC"
// 重对账 DHCP 静态保留。历史实例的域可能由旧版本代码定义（当时 MAC 为空，
// libvirt 派随机地址），代码里派生的 MAC 与实际网卡不一致会让静态保留
// 永远等不到匹配的客户端，实例拿不到保留 IP。
type NATIdentityReconciler interface {
	EnsureNATIdentity(ctx context.Context, inst *protocol.Instance)
}

type Capability struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Error     string `json:"error,omitempty"`
	// FirewallPolicy 表示驱动支持事务化默认策略下发（iptables-restore 单事务
	// 替换 + 失败回滚）。主控在实例携带 firewall_policy 时会校验该位，旧被控
	// 缺字段即 false，创建被拒绝而不是静默忽略默认拒绝语义。
	FirewallPolicy bool `json:"firewall_policy"`
}

// policyCapable 由实现 FirewallPolicy 事务下发的驱动声明。incus/qemu 均已实现；
// probe 失败（二进制缺失）时 Available=false，本标志不参与该判断。
type policyCapable interface {
	firewallPolicyCapable() bool
}

// Registry 按名字管理驱动，并支持 auto 时按 Probe 顺序自动选择。
type Registry struct {
	mu      sync.RWMutex
	drivers map[string]Driver
}

// NewRegistryWithDataDir 注册全部真实驱动：incus、qemu。
// dataDir 是被控数据目录，驱动把磁盘与镜像放在 <dataDir>/images 下，
// 累计流量文件放在 <dataDir>/traffic_<id>.json。
func NewRegistryWithDataDir(dataDir string) *Registry {
	r := &Registry{drivers: make(map[string]Driver)}
	r.Register(NewIncusWithDataDir(dataDir))
	r.Register(NewQEMUWithDataDir(dataDir))
	return r
}

func (r *Registry) Register(d Driver) {
	if d == nil || d.Name() == "" {
		return
	}
	r.mu.Lock()
	r.drivers[d.Name()] = d
	r.mu.Unlock()
}

func (r *Registry) Get(name string) (Driver, bool) {
	r.mu.RLock()
	d, ok := r.drivers[strings.ToLower(strings.TrimSpace(name))]
	r.mu.RUnlock()
	return d, ok
}

func (r *Registry) Names() []string {
	r.mu.RLock()
	names := make([]string, 0, len(r.drivers))
	for name := range r.drivers {
		names = append(names, name)
	}
	r.mu.RUnlock()
	sort.Slice(names, func(i, j int) bool {
		return driverRank(names[i]) < driverRank(names[j])
	})
	return names
}

func driverRank(name string) int {
	switch name {
	case "incus":
		return 0
	case "qemu":
		return 1
	default:
		return 99
	}
}

// Resolve 选出要用的驱动：显式指定时必须已注册且可用；auto 时按
// incus → qemu → lxc 顺序 Probe，取第一个可用的。
func (r *Registry) Resolve(ctx context.Context, preferred string) (Driver, error) {
	preferred = strings.ToLower(strings.TrimSpace(preferred))
	if preferred != "" && preferred != "auto" {
		d, ok := r.Get(preferred)
		if !ok {
			return nil, fmt.Errorf("driver %q is not registered", preferred)
		}
		if err := d.Probe(ctx); err != nil {
			return nil, fmt.Errorf("driver %q unavailable: %w", preferred, err)
		}
		return d, nil
	}
	for _, name := range r.Names() {
		d, _ := r.Get(name)
		if err := d.Probe(ctx); err == nil {
			return d, nil
		}
	}
	return nil, fmt.Errorf("no virtualization driver is installed")
}

func (r *Registry) Capabilities(ctx context.Context) []Capability {
	items := make([]Capability, 0, len(r.drivers))
	for _, name := range r.Names() {
		d, _ := r.Get(name)
		item := Capability{Name: name}
		if pc, ok := d.(policyCapable); ok {
			item.FirewallPolicy = pc.firewallPolicyCapable()
		}
		probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := d.Probe(probeCtx)
		cancel()
		item.Available = err == nil
		if err != nil {
			item.Error = err.Error()
		}
		items = append(items, item)
	}
	return items
}

func run(ctx context.Context, name string, args ...string) error {
	out, err := output(ctx, name, args...)
	if err != nil {
		return fmt.Errorf("%s %v: %w: %s", name, args, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func output(ctx context.Context, name string, args ...string) ([]byte, error) {
	if runner, ok := ctx.Value(commandRunnerKey{}).(CommandRunner); ok {
		return runner(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// CommandRunner is a request-scoped command seam. Production defaults to real
// exec; tests can exercise host workflows without fake POSIX shell executables
// or a racy process-global override.
type CommandRunner func(context.Context, string, ...string) ([]byte, error)
type commandRunnerKey struct{}

func WithCommandRunner(ctx context.Context, runner CommandRunner) context.Context {
	return context.WithValue(ctx, commandRunnerKey{}, runner)
}

func hasCommand(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// resourceName 是实例在本机的资源名（域名/容器名），主键 ID 保证唯一，
// 展示名清洗后拼在后面便于管理员在 virsh/incus 里辨认。
func resourceName(prefix string, inst *protocol.Instance) string {
	return fmt.Sprintf("virtualis-%d-%s", inst.ID, sanitizeName(inst.Name))
}

func sanitizeName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "instance"
	}
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "instance"
	}
	return b.String()
}

func contains(s, part string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(part))
}
