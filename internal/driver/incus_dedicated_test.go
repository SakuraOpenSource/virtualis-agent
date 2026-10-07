package driver

import (
	"context"
	"strings"
	"testing"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

func TestIncusPhysicalUplinkUsesRoutedNICForContainerAndVM(t *testing.T) {
	for _, typ := range []string{"container", "vm"} {
		t.Run(typ, func(t *testing.T) {
			calls := []string{}
			baseRunner := dedicatedRunner(t, &calls)
			ctx := WithCommandRunner(context.Background(), func(ctx context.Context, name string, a ...string) ([]byte, error) {
				if len(a) > 1 && a[0] == "query" && strings.Contains(a[1], "/profiles/") {
					return []byte(`{"devices":{"root":{"type":"disk"}}}`), nil
				}
				return baseRunner(ctx, name, a...)
			})
			inst := &protocol.Instance{ID: 8, Type: typ, Network: protocol.NetworkConfig{Mode: "dedicated", Bridge: "eth0", IPv4: "192.0.2.20/24", Gateway: "192.0.2.1"}}
			if e := NewIncusWithDataDir(t.TempDir()).ensureProfile(ctx, "virtualis-p-8", inst.Network, inst, false); e != nil {
				t.Fatal(e)
			}
			joined := strings.Join(calls, "\n")
			for _, want := range []string{"nictype=routed", "parent=eth0", "ipv4.address=192.0.2.20", "ipv4.host_address=169.254.0.1", "host_name=vt8"} {
				if !strings.Contains(joined, want) {
					t.Errorf("missing %s in %s", want, joined)
				}
			}
			if strings.Contains(joined, "ipv4.gateway=169.") || strings.Contains(joined, "nictype=bridged") {
				t.Fatal("unsupported routed NIC configuration", joined)
			}
		})
	}
}
