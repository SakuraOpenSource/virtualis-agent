package driver

import (
	"context"
	"strings"
	"testing"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

func TestIncusDedicatedCreatePersistsNetworkBeforeReturning(t *testing.T) {
	for _, failGuest := range []bool{false, true} {
		name := "configured"
		if failGuest {
			name = "guest-rejected"
		}
		t.Run(name, func(t *testing.T) {
			var calls []string
			base := dedicatedRunner(t, &calls)
			var script string
			ctx := WithCommandRunner(context.Background(), func(ctx context.Context, command string, args ...string) ([]byte, error) {
				if len(args) > 1 && args[0] == "query" && strings.Contains(args[1], "/profiles/") {
					return []byte(`{"devices":{"root":{"type":"disk"}}}`), nil
				}
				if len(args) > 0 && args[0] == "exec" {
					script = strings.Join(args, " ")
					if failGuest {
						return nil, context.DeadlineExceeded
					}
				}
				return base(ctx, command, args...)
			})
			inst := &protocol.Instance{
				ID: 8, Name: "routed", Type: "container",
				Image:   &protocol.Image{Path: "/fixture/rootfs.tar"},
				Network: protocol.NetworkConfig{Mode: "dedicated", DedicatedMode: "auto", Bridge: "eth0", IPv4: "192.0.2.20/32", Gateway: "192.0.2.1", DNS: []string{"1.1.1.1"}},
			}
			err := NewIncusWithDataDir(t.TempDir()).Create(ctx, inst)
			if failGuest {
				if err == nil {
					t.Fatal("guest 网络配置失败时仍报告创建成功")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"DHCP=no", "192.0.2.20/32", "169.254.0.1", "DNS=1.1.1.1", "virtualis-net.service"} {
				if !strings.Contains(script, want) {
					t.Errorf("创建返回前未持久化 guest 网络：缺少 %s", want)
				}
			}
		})
	}
}
