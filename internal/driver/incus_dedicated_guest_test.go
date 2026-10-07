package driver

import (
	"context"
	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
	"strings"
	"testing"
)

func TestIncusDedicatedStartPersistsStaticNetworkAndSurfacesGuestFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "persistent", true: "guest-rejected"}[fail], func(t *testing.T) {
			calls := []string{}
			baseRunner := dedicatedRunner(t, &calls)
			script := ""
			ctx := WithCommandRunner(context.Background(), func(ctx context.Context, name string, a ...string) ([]byte, error) {
				if len(a) > 1 && a[0] == "query" && strings.Contains(a[1], "/profiles/") {
					return []byte(`{"devices":{"root":{"type":"disk"}}}`), nil
				}
				if len(a) > 0 && a[0] == "exec" {
					script = strings.Join(a, " ")
					if fail {
						return nil, context.DeadlineExceeded
					}
				}
				return baseRunner(ctx, name, a...)
			})
			inst := &protocol.Instance{ID: 8, Name: "routed", Network: protocol.NetworkConfig{Mode: "dedicated", Bridge: "eth0", IPv4: "192.0.2.20/24", Gateway: "192.0.2.1"}}
			err := NewIncusWithDataDir(t.TempDir()).Start(ctx, inst)
			if fail {
				if err == nil {
					t.Fatal("guest provisioning error swallowed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"192.0.2.20/32", "169.254.0.1", "network: {config: disabled}", "systemctl enable", "virtualis-net.service"} {
				if !strings.Contains(script, want) {
					t.Errorf("missing persistent static guest %s", want)
				}
			}
			if inst.Network.IPv4 != "192.0.2.20/24" || inst.Network.Gateway != "192.0.2.1" {
				t.Fatal("runtime /32 overwrote desired config", inst.Network)
			}
		})
	}
}
