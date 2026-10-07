package driver

import (
	"context"
	"strings"
	"testing"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

func TestIncusDedicatedNetworkActionsPersistGuest(t *testing.T) {
	for _, action := range []string{"configure", "restart"} {
		t.Run(action, func(t *testing.T) {
			var calls []string
			base := dedicatedRunner(t, &calls)
			var script string
			ctx := WithCommandRunner(context.Background(), func(ctx context.Context, command string, args ...string) ([]byte, error) {
				if len(args) > 1 && args[0] == "query" && strings.Contains(args[1], "/profiles/") {
					return []byte(`{"devices":{"root":{"type":"disk"}}}`), nil
				}
				if len(args) > 0 && args[0] == "list" {
					return []byte("virtualis-8-routed,RUNNING\n"), nil
				}
				if len(args) > 0 && args[0] == "exec" {
					script = strings.Join(args, " ")
				}
				return base(ctx, command, args...)
			})
			inst := &protocol.Instance{ID: 8, Name: "routed", Network: protocol.NetworkConfig{Mode: "dedicated", Bridge: "eth0", IPv4: "192.0.2.20/32", DNS: []string{"1.1.1.1"}}}
			driver := NewIncusWithDataDir(t.TempDir())
			var err error
			if action == "configure" {
				err = driver.ConfigureNetwork(ctx, inst)
			} else {
				err = driver.Restart(ctx, inst)
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(script, "DHCP=no") || !strings.Contains(script, "169.254.0.1") {
				t.Fatalf("%s 未恢复持久化独立网络", action)
			}
		})
	}
}
