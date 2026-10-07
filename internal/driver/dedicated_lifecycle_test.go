package driver

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

func TestQEMURoutedHardStopCleansPersistentOwnedResources(t *testing.T) {
	calls := []string{}
	runner := dedicatedRunner(t, &calls)
	ctx := WithCommandRunner(context.Background(), func(ctx context.Context, n string, a ...string) ([]byte, error) {
		if n == "ip" && len(a) > 1 && a[0] == "-j" && a[1] == "link" {
			return []byte(`[{"ifname":"vb8","ifalias":"virtualis:8","linkinfo":{"info_kind":"bridge"}}]`), nil
		}
		if n == "virsh" && a[0] == "domstate" {
			return []byte("shut off"), nil
		}
		return runner(ctx, n, a...)
	})
	dir := t.TempDir()
	d := NewQEMUWithDataDir(dir)
	inst := &protocol.Instance{ID: 8, Name: "routed", Network: protocol.NetworkConfig{Mode: "dedicated", Bridge: "eth0", IPv4: "192.0.2.20/24"}}
	if e := d.ensureNetwork(ctx, inst); e != nil {
		t.Fatal(e)
	}
	files, e := os.ReadDir(dir + "/network")
	if e != nil || len(files) != 1 {
		t.Fatalf("routed ownership is not persistent: %v %v", files, e)
	}
	// A fresh driver and missing desired network must still clean only its record.
	calls = nil
	stopped := *inst
	stopped.Network = protocol.NetworkConfig{Mode: "none"}
	if e := NewQEMUWithDataDir(dir).HardStop(ctx, &stopped); e != nil {
		t.Fatal(e)
	}
	joined := strings.Join(calls, "\n")
	for _, want := range []string{"ip route del 192.0.2.20/32 dev vb8 proto 186", "ip neigh del proxy 192.0.2.20 dev eth0", "ip link delete dev vb8", "-X VIRTIS-AS-8"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing cleanup %s in %s", want, joined)
		}
	}
	if strings.Contains(joined, "link delete dev eth0") || strings.Contains(joined, "-F FORWARD") {
		t.Fatal("foreign resource cleanup", joined)
	}
	files, e = os.ReadDir(dir + "/network")
	if e != nil || len(files) != 0 {
		t.Fatal("owned record retained after proven cleanup", files, e)
	}
}

func TestQEMURoutedRefusesForeignRouteBeforeWrites(t *testing.T) {
	calls := []string{}
	runner := dedicatedRunner(t, &calls)
	ctx := WithCommandRunner(context.Background(), func(ctx context.Context, n string, a ...string) ([]byte, error) {
		if n == "ip" && len(a) > 1 && a[0] == "-j" && a[1] == "route" {
			return []byte(`[{"dst":"192.0.2.20","dev":"docker0","protocol":"static"}]`), nil
		}
		return runner(ctx, n, a...)
	})
	inst := &protocol.Instance{ID: 8, Network: protocol.NetworkConfig{Mode: "dedicated", Bridge: "eth0", IPv4: "192.0.2.20/24"}}
	if e := NewQEMUWithDataDir(t.TempDir()).ensureNetwork(ctx, inst); e == nil {
		t.Fatal("foreign route silently overwritten")
	}
	for _, c := range calls {
		if strings.Contains(c, "replace") || strings.Contains(c, "link add") || strings.Contains(c, "iptables-restore") {
			t.Fatal("conflict mutated host", calls)
		}
	}
}
