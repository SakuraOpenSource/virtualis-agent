package driver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

func TestFirewallInvalidReplacementLeavesProtection(t *testing.T) {
	inst := &protocol.Instance{ID: 8, Network: protocol.NetworkConfig{Mode: "vpc", IPv4: "10.1.0.8"}, Firewall: []protocol.FirewallRule{{Direction: "in", Action: "accept", Protocol: "tcp", PortStart: 70000, Enabled: true}}}
	calls := 0
	ctx := WithCommandRunner(context.Background(), func(context.Context, string, ...string) ([]byte, error) { calls++; return nil, nil })
	if err := ApplyFirewall(ctx, inst); err == nil {
		t.Fatal("invalid replacement silently weakens protection")
	}
	if calls != 0 {
		t.Fatalf("invalid replacement touched host: %d", calls)
	}
}

func TestFirewallPolicyReplacementIsOneTransaction(t *testing.T) {
	var inst protocol.Instance
	if err := json.Unmarshal([]byte(`{"id":8,"network":{"mode":"vpc","ipv4":"10.1.0.8"},"firewall_policy":{"ingress":"drop","egress":"accept"},"firewall":[{"direction":"in","action":"accept","protocol":"tcp","port_start":22,"enabled":true}]}`), &inst); err != nil {
		t.Fatal(err)
	}
	transaction := ""
	writes := 0
	ctx := WithCommandRunner(context.Background(), func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "iptables" && args[0] == "-S" {
			return []byte("-A FORWARD -d 10.1.0.7 -j VIRTIS-FW-8\n-A FORWARD -j DOCKER-USER\n"), nil
		}
		if name != "iptables-restore" {
			t.Fatalf("detached/modified protection outside transaction: %s %v", name, args)
		}
		writes++
		b, err := os.ReadFile(args[len(args)-1])
		if err != nil {
			return nil, err
		}
		transaction = string(b)
		return nil, errors.New("simulated atomic kernel rejection")
	})
	if err := ApplyFirewall(ctx, &inst); err == nil {
		t.Fatal("transaction failure swallowed")
	}
	if writes != 1 {
		t.Fatalf("want one transaction got %d", writes)
	}
	for _, want := range []string{"*filter", "-D FORWARD -d 10.1.0.7 -j VIRTIS-FW-8", "-A VIRTIS-FW-8 -d 10.1.0.8 -p tcp --dport 22 -j RETURN", "-A VIRTIS-FW-8 -d 10.1.0.8 -j DROP", "-A VIRTIS-FW-8 -s 10.1.0.8 -j RETURN", "COMMIT"} {
		if !strings.Contains(transaction, want) {
			t.Errorf("missing %s in %s", want, transaction)
		}
	}
	if strings.Contains(transaction, "ACCEPT") || strings.Contains(transaction, "DOCKER") || strings.Contains(transaction, "ESTABLISHED") {
		t.Fatal("replacement bypasses other instances or changes foreign rules", transaction)
	}
}
