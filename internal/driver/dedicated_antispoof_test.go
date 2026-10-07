package driver

import (
	"context"
	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
	"os"
	"strings"
	"testing"
)

func TestQEMUDedicatedStartInstallsScopedAntiSpoofBeforePower(t *testing.T) {
	calls := []string{}
	baseRunner := dedicatedRunner(t, &calls)
	raw := ""
	started := false
	ctx := WithCommandRunner(context.Background(), func(ctx context.Context, name string, a ...string) ([]byte, error) {
		if name == "iptables-restore" {
			b, e := os.ReadFile(a[len(a)-1])
			if e != nil {
				return nil, e
			}
			if strings.Contains(string(b), "*raw") {
				raw = string(b)
			}
		}
		if name == "virsh" && a[0] == "start" {
			started = true
			if raw == "" {
				t.Fatal("guest started before anti-spoof protection")
			}
		}
		return baseRunner(ctx, name, a...)
	})
	inst := &protocol.Instance{ID: 8, Name: "routed", Network: protocol.NetworkConfig{Mode: "dedicated", Bridge: "eth0", IPv4: "192.0.2.20/24"}, Firewall: []protocol.FirewallRule{{Direction: "out", Protocol: "any", Action: "accept", Enabled: true}}}
	if e := NewQEMUWithDataDir(t.TempDir()).Start(ctx, inst); e != nil {
		t.Fatal(e)
	}
	if !started {
		t.Fatal("guest never started")
	}
	for _, want := range []string{"*raw", "-A VIRTIS-AS-8 -s 192.0.2.20 -j RETURN", "-A VIRTIS-AS-8 -j DROP", "-I PREROUTING 1 -i vb8 -j VIRTIS-AS-8", "COMMIT"} {
		if !strings.Contains(raw, want) {
			t.Errorf("missing %s in %s", want, raw)
		}
	}
	if strings.Contains(raw, "-i eth0") || strings.Contains(raw, "ACCEPT") || strings.Contains(raw, "-F PREROUTING") {
		t.Fatal("anti-spoof affects primary/foreign interface", raw)
	}
}
