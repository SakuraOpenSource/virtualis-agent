package driver

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

func TestIncusDedicatedDeleteCleansOwnershipRecord(t *testing.T) {
	dir := t.TempDir()
	// Seed ownership records as a running start would.
	for _, id := range []uint{8} {
		rec := dedicatedRecord{ID: id, Driver: "incus", Attachment: dedicatedAttachment{Uplink: "eth0", Mode: "routed", IP: "192.0.2.20"}, Network: protocol.NetworkConfig{Mode: "dedicated"}}
		if e := saveDedicatedRecord(dir, rec); e != nil {
			t.Fatal(e)
		}
	}
	inst := &protocol.Instance{ID: 8, Name: "routed", Driver: "incus", Network: protocol.NetworkConfig{Mode: "dedicated", Bridge: "eth0", IPv4: "192.0.2.20/24"}}
	calls := []string{}
	runner := dedicatedRunner(t, &calls)
	ctx := WithCommandRunner(context.Background(), runner)
	if e := NewIncusWithDataDir(dir).Delete(ctx, inst); e != nil {
		t.Fatal(e)
	}
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, "-X VIRTIS-AS-8") {
		t.Fatal("delete left anti-spoof chain", joined)
	}
	if strings.Contains(joined, "ip link delete") || strings.Contains(joined, "ip route del") {
		t.Fatal("agent removed Incus-owned interfaces", joined)
	}
	if _, e := os.Stat(networkRecordPath(dir, 8)); !os.IsNotExist(e) {
		t.Fatal("ownership record survived delete", e)
	}
}

func TestQEMUDedicatedDeleteCleansOwnedHostResources(t *testing.T) {
	dir := t.TempDir()
	calls := []string{}
	runner := dedicatedRunner(t, &calls)
	ctx := WithCommandRunner(context.Background(), func(ctx context.Context, n string, a ...string) ([]byte, error) {
		if n == "virsh" && a[0] == "domstate" {
			return []byte("shut off"), nil
		}
		return runner(ctx, n, a...)
	})
	inst := &protocol.Instance{ID: 8, Name: "routed", Network: protocol.NetworkConfig{Mode: "dedicated", Bridge: "eth0", IPv4: "192.0.2.20/24"}}
	if e := NewQEMUWithDataDir(dir).ensureNetwork(ctx, inst); e != nil {
		t.Fatal(e)
	}
	calls = nil
	if e := NewQEMUWithDataDir(dir).Delete(ctx, inst); e != nil {
		t.Fatal(e)
	}
	joined := strings.Join(calls, "\n")
	for _, want := range []string{"ip route del 192.0.2.20/32 dev vb8 proto 186", "ip neigh del proxy 192.0.2.20 dev eth0", "ip link delete dev vb8", "-X VIRTIS-AS-8"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing cleanup %s in %s", want, joined)
		}
	}
	if strings.Contains(joined, "link delete dev eth0") {
		t.Fatal("primary interface deleted", joined)
	}
	if _, e := os.Stat(networkRecordPath(dir, 8)); !os.IsNotExist(e) {
		t.Fatal("record survived delete", e)
	}
}
