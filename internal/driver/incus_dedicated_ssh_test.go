package driver

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

func TestIncusDedicatedSSHBootstrapRepairsStaticNetworkBeforeInstall(t *testing.T) {
	var calls []string
	base := dedicatedRunner(t, &calls)
	persisted, installed := false, false
	ctx := WithCommandRunner(context.Background(), func(ctx context.Context, command string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if len(args) > 1 && args[0] == "query" && strings.HasSuffix(args[1], "/state") {
			return []byte(`{"network":{"eth0":{"addresses":[{"family":"inet","scope":"global","address":"192.0.2.20"}]}}}`), nil
		}
		if len(args) > 0 && args[0] == "exec" {
			if strings.Contains(joined, "command -v sshd") {
				return nil, errors.New("sshd missing")
			}
			if strings.Contains(joined, "virtualis-net.service") {
				persisted = true
			}
			if strings.Contains(joined, "apt-get update") {
				installed = true
				if !persisted {
					t.Error("安装 SSH 前未修复独立网络和 DNS")
				}
				return nil, context.DeadlineExceeded
			}
		}
		return base(ctx, command, args...)
	})
	inst := &protocol.Instance{ID: 8, Name: "routed", Network: protocol.NetworkConfig{Mode: "dedicated", Bridge: "eth0", IPv4: "192.0.2.20/32", DNS: []string{"1.1.1.1"}}}
	err := NewIncusWithDataDir(t.TempDir()).SetRootPassword(ctx, inst, "test-password")
	if !persisted || !installed || err == nil {
		t.Fatalf("SSH 重试未沿用独立网络引导：persisted=%t installed=%t err=%v", persisted, installed, err)
	}
}
