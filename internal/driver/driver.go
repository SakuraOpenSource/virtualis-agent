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
	VNC(context.Context, *protocol.Instance, string) (protocol.VNCInfo, error)
}

type Capability struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Error     string `json:"error,omitempty"`
}

type Registry struct {
	mu      sync.RWMutex
	drivers map[string]Driver
}

func NewRegistry() *Registry {
	r := &Registry{drivers: make(map[string]Driver)}
	r.Register(NewIncus())
	r.Register(NewQEMU())
	r.Register(NewLXC())
	r.Register(NewMock())
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
	case "lxc":
		return 2
	case "mock":
		return 3
	default:
		return 99
	}
}

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
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %w: %s", name, args, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func output(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func hasCommand(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

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

type Mock struct {
	mu        sync.Mutex
	instances map[uint]string
}

func NewMock() *Mock { return &Mock{instances: make(map[uint]string)} }

func (m *Mock) Name() string                { return "mock" }
func (m *Mock) Probe(context.Context) error { return nil }
func (m *Mock) Create(ctx context.Context, inst *protocol.Instance) error {
	if err := shortDelay(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	m.instances[inst.ID] = StatusStopped
	m.mu.Unlock()
	return nil
}
func (m *Mock) Delete(ctx context.Context, inst *protocol.Instance) error {
	if err := shortDelay(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.instances, inst.ID)
	m.mu.Unlock()
	return nil
}
func (m *Mock) Start(ctx context.Context, inst *protocol.Instance) error {
	return m.set(ctx, inst.ID, StatusRunning)
}
func (m *Mock) Stop(ctx context.Context, inst *protocol.Instance) error {
	return m.set(ctx, inst.ID, StatusStopped)
}
func (m *Mock) Restart(ctx context.Context, inst *protocol.Instance) error {
	if err := m.Stop(ctx, inst); err != nil {
		return err
	}
	return m.Start(ctx, inst)
}
func (m *Mock) HardStart(ctx context.Context, inst *protocol.Instance) error {
	return m.Start(ctx, inst)
}
func (m *Mock) HardStop(ctx context.Context, inst *protocol.Instance) error { return m.Stop(ctx, inst) }
func (m *Mock) HardRestart(ctx context.Context, inst *protocol.Instance) error {
	return m.Restart(ctx, inst)
}
func (m *Mock) Reinstall(ctx context.Context, inst *protocol.Instance) error {
	return m.set(ctx, inst.ID, StatusStopped)
}
func (m *Mock) Status(ctx context.Context, inst *protocol.Instance) (string, error) {
	if err := shortDelay(ctx); err != nil {
		return "", err
	}
	m.mu.Lock()
	status, ok := m.instances[inst.ID]
	m.mu.Unlock()
	if !ok {
		return StatusStopped, nil
	}
	return status, nil
}
func (m *Mock) Metrics(ctx context.Context, inst *protocol.Instance) (protocol.Metrics, error) {
	if err := shortDelay(ctx); err != nil {
		return protocol.Metrics{}, err
	}
	return defaultMetrics(inst), nil
}
func (m *Mock) Network(ctx context.Context, inst *protocol.Instance) (protocol.NetworkStatus, error) {
	if err := shortDelay(ctx); err != nil {
		return protocol.NetworkStatus{}, err
	}
	status := collectHostNetwork(ctx, inst.Network)
	if inst.Network.Mode == "" {
		status.Reachable = true
	}
	return status, nil
}
func (m *Mock) VNC(context.Context, *protocol.Instance, string) (protocol.VNCInfo, error) {
	return unsupportedVNC("mock")
}
func (m *Mock) set(ctx context.Context, id uint, status string) error {
	if err := shortDelay(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	m.instances[id] = status
	m.mu.Unlock()
	return nil
}
func shortDelay(ctx context.Context) error {
	t := time.NewTimer(10 * time.Millisecond)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
