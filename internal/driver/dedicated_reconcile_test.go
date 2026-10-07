package driver

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

func TestRoutedReconcileRepairsResourcesWithoutRebootAndCleansStopped(t *testing.T) {
	dir := t.TempDir()
	calls := []string{}
	runner := dedicatedRunner(t, &calls)
	ctx := WithCommandRunner(context.Background(), runner)
	d := NewQEMUWithDataDir(dir)
	inst := &protocol.Instance{ID: 8, Name: "routed", Status: StatusRunning, Network: protocol.NetworkConfig{Mode: "dedicated", Bridge: "eth0", IPv4: "192.0.2.20/24"}}
	// Public reconciliation seam is optional for test/legacy backends.
	reconciler, ok := any(d).(interface {
		ReconcileNetworkResources(context.Context, *protocol.Instance) error
	})
	if !ok {
		t.Fatal("driver has no owned resource reconcile lifecycle")
	}
	if e := reconciler.ReconcileNetworkResources(ctx, inst); e != nil {
		t.Fatal(e)
	}
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, "ip route replace 192.0.2.20/32") || strings.Contains(joined, "virsh reboot") {
		t.Fatal("reconcile must repair without reboot", joined)
	}
	calls = nil
	inst.Status = StatusStopped
	if e := reconciler.ReconcileNetworkResources(ctx, inst); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(strings.Join(calls, "\n"), "ip neigh del proxy 192.0.2.20") {
		t.Fatal("stopped poll did not clean owned resources", calls)
	}
	if _, e := os.Stat(networkRecordPath(dir, inst.ID)); !os.IsNotExist(e) {
		t.Fatal("cleanup record not removed", e)
	}
}

func TestIncusDedicatedProtectionBeforeStartAndCleanupAfterStop(t *testing.T) {
	dir := t.TempDir()
	calls := []string{}
	runner := dedicatedRunner(t, &calls)
	protected := false
	ctx := WithCommandRunner(context.Background(), func(ctx context.Context, n string, a ...string) ([]byte, error) {
		if n == "iptables-restore" {
			b, e := os.ReadFile(a[len(a)-1])
			if e != nil {
				return nil, e
			}
			if strings.Contains(string(b), "-I PREROUTING 1 -i vt8 -j VIRTIS-AS-8") {
				protected = true
			}
		}
		if len(a) > 1 && a[0] == "query" && strings.Contains(a[1], "/profiles/") {
			return []byte(`{"devices":{"root":{"type":"disk"}}}`), nil
		}
		if len(a) > 0 && a[0] == "start" && !protected {
			t.Fatal("Incus started without independent anti-spoof")
		}
		return runner(ctx, n, a...)
	})
	inst := &protocol.Instance{ID: 8, Name: "routed", Network: protocol.NetworkConfig{Mode: "dedicated", Bridge: "eth0", IPv4: "192.0.2.20/24"}}
	d := NewIncusWithDataDir(dir)
	if e := d.Start(ctx, inst); e != nil {
		t.Fatal(e)
	}
	if !protected {
		t.Fatal("protection absent")
	}
	if _, e := os.Stat(networkRecordPath(dir, inst.ID)); e != nil {
		t.Fatal("Incus ownership not persistent", e)
	}
	calls = nil
	if e := d.Stop(ctx, inst); e != nil {
		t.Fatal(e)
	}
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, "-X VIRTIS-AS-8") {
		t.Fatal("anti-spoof leaked after stop", joined)
	}
	if strings.Contains(joined, "ip route del") || strings.Contains(joined, "ip link delete") {
		t.Fatal("Agent removed Incus daemon-owned resources", joined)
	}
}
