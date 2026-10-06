package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SakuraOpenSource/virtualis-agent/internal/driver"
	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

type testDriver struct {
	driver.Driver
	status     func(context.Context, *protocol.Instance) (string, error)
	export     func(context.Context, *protocol.Instance, string) error
	importFn   func(context.Context, *protocol.Instance, string) error
	deleteFn   func(context.Context, *protocol.Instance) error
	validate   func(context.Context, *protocol.Instance, string) error
	resize     func(context.Context, *protocol.Instance, protocol.InstanceSpec, *protocol.NetworkConfig) error
	networkFn  func(context.Context, *protocol.Instance) (protocol.NetworkStatus, error)
	metricsFn  func(context.Context, *protocol.Instance) (protocol.Metrics, error)
	passwordFn func(context.Context, *protocol.Instance, string) error
}

func (*testDriver) Name() string                { return "test" }
func (*testDriver) Probe(context.Context) error { return nil }
func (d *testDriver) Status(ctx context.Context, i *protocol.Instance) (string, error) {
	if d.status != nil {
		return d.status(ctx, i)
	}
	return driver.StatusStopped, nil
}
func (d *testDriver) Export(ctx context.Context, i *protocol.Instance, p string) error {
	if d.export != nil {
		return d.export(ctx, i, p)
	}
	return os.WriteFile(p, []byte("original"), 0600)
}
func (d *testDriver) Import(ctx context.Context, i *protocol.Instance, p string) error {
	if d.importFn != nil {
		return d.importFn(ctx, i, p)
	}
	return nil
}
func (d *testDriver) Delete(ctx context.Context, i *protocol.Instance) error {
	if d.deleteFn != nil {
		return d.deleteFn(ctx, i)
	}
	return nil
}
func (*testDriver) ConfigureNetwork(context.Context, *protocol.Instance) error { return nil }
func (d *testDriver) Network(ctx context.Context, i *protocol.Instance) (protocol.NetworkStatus, error) {
	if d.networkFn != nil {
		return d.networkFn(ctx, i)
	}
	return protocol.NetworkStatus{}, nil
}
func (d *testDriver) Metrics(ctx context.Context, i *protocol.Instance) (protocol.Metrics, error) {
	if d.metricsFn != nil {
		return d.metricsFn(ctx, i)
	}
	return protocol.Metrics{}, nil
}
func (d *testDriver) SetRootPassword(ctx context.Context, i *protocol.Instance, p string) error {
	if d.passwordFn != nil {
		return d.passwordFn(ctx, i, p)
	}
	return nil
}
func (*testDriver) Create(context.Context, *protocol.Instance) error { return nil }
func (*testDriver) Start(context.Context, *protocol.Instance) error  { return nil }
func (*testDriver) CreateSnapshot(context.Context, *protocol.Instance, string) (int64, error) {
	return 7, nil
}
func (*testDriver) RestoreSnapshot(context.Context, *protocol.Instance, string) error { return nil }
func (*testDriver) DeleteSnapshot(context.Context, *protocol.Instance, string) error  { return nil }
func (d *testDriver) ValidateImport(ctx context.Context, i *protocol.Instance, p string) error {
	if d.validate != nil {
		return d.validate(ctx, i, p)
	}
	return nil
}
func (d *testDriver) Resize(ctx context.Context, i *protocol.Instance, s protocol.InstanceSpec, n *protocol.NetworkConfig) error {
	if d.resize != nil {
		return d.resize(ctx, i, s, n)
	}
	i.Spec = s
	if n != nil {
		i.Network = *n
	}
	return nil
}
func agentForTest(t *testing.T, d *testDriver) *agentServer {
	t.Helper()
	s := newAgentServer("token", "test", "dev", t.TempDir())
	s.registry.Register(d)
	return s
}
func testInstance(id uint) protocol.Instance {
	return protocol.Instance{ID: id, Name: "test", Driver: "test", Type: "vm", Spec: protocol.InstanceSpec{CPU: 1, MemoryMB: 512, DiskGB: 20, Arch: "x86_64"}, Network: protocol.NetworkConfig{Mode: "none"}}
}
func requestJSON(s *agentServer, path string, value any) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(value)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw)))
	req.Header.Set("X-Agent-Token", "token")
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)
	return rec
}

func TestExportExcludesOverlappingMutationAndReconciliation(t *testing.T) {
	entered, leave := make(chan struct{}), make(chan struct{})
	d := &testDriver{export: func(_ context.Context, _ *protocol.Instance, p string) error {
		close(entered)
		<-leave
		return os.WriteFile(p, []byte("archive"), 0600)
	}}
	s := agentForTest(t, d)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- requestJSON(s, "/api/instances/1/export", map[string]any{"instance": testInstance(1)}) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("export never began")
	}
	defer close(leave)
	for _, action := range []string{"status", "snapshots", "power", "network/configure", "firewall", "nat", "password", "import", "resize", "metrics"} {
		rec := requestJSON(s, "/api/instances/1/"+action, map[string]any{"instance": testInstance(1), "name": "snap", "action": "create"})
		if rec.Code != 409 {
			t.Errorf("%s during export = %d; want 409: %s", action, rec.Code, rec.Body.String())
		}
	}
	rec := requestJSON(s, "/api/instances/2/status", map[string]any{"instance": testInstance(2)})
	if rec.Code != 200 {
		t.Fatalf("unrelated instance blocked: %d %s", rec.Code, rec.Body.String())
	}
}
