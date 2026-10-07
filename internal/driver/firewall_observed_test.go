package driver

import (
	"context"
	"errors"
	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
	"os"
	"strings"
	"testing"
)

type observedNetworkDriver struct {
	Driver
	network protocol.NetworkStatus
	err     error
}

func (d *observedNetworkDriver) Network(context.Context, *protocol.Instance) (protocol.NetworkStatus, error) {
	return d.network, d.err
}
func TestDHCPFirewallReconciliationUsesObservedAddressAndRemovesOldJumps(t *testing.T) {
	calls := []string{}
	ctx := WithCommandRunner(context.Background(), func(_ context.Context, name string, a ...string) ([]byte, error) {
		if name == "iptables-restore" {
			b, err := os.ReadFile(a[len(a)-1])
			calls = append(calls, string(b))
			return nil, err
		}
		calls = append(calls, strings.Join(a, " "))
		if a[0] == "-S" {
			return []byte("-A FORWARD -d 10.1.0.8 -j VIRTIS-FW-1\n"), nil
		}
		if a[0] == "-C" {
			return nil, errors.New("not present")
		}
		return []byte(""), nil
	})
	d := &observedNetworkDriver{network: protocol.NetworkStatus{Interfaces: []protocol.NetworkInterface{{Name: "lo", IPv4: []string{"127.0.0.1"}}, {Name: "eth0", IPv4: []string{"169.254.1.1", "10.1.0.9/24"}}}}}
	i := &protocol.Instance{ID: 1, Status: StatusRunning, Network: protocol.NetworkConfig{Mode: "vpc", Bridge: "vpc"}, Firewall: []protocol.FirewallRule{{Direction: "in", Action: "drop", Protocol: "tcp", PortStart: 22, Enabled: true}}}
	if e := ReconcileFirewall(ctx, d, i, nil); e != nil {
		t.Fatal(e)
	}
	if i.ObservedIP != "10.1.0.9" || i.Network.IPv4 != "" {
		t.Fatalf("DHCP must not become desired static config: %+v", i)
	}
	joined := strings.Join(calls, "\n")
	for _, want := range []string{"-D FORWARD -d 10.1.0.8 -j VIRTIS-FW-1", "-A VIRTIS-FW-1 -d 10.1.0.9", "-I FORWARD 1 -d 10.1.0.9"} {
		if !strings.Contains(joined, want) {
			t.Fatal("missing "+want, joined)
		}
	}
}
func TestDHCPFirewallRefusesStaleOrInvalidObservedAddress(t *testing.T) {
	d := &observedNetworkDriver{network: protocol.NetworkStatus{Interfaces: []protocol.NetworkInterface{{Name: "eth0", IPv4: []string{"127.0.0.1", "::1", "224.0.0.1"}}}}}
	mutations := 0
	ctx := WithCommandRunner(context.Background(), func(context.Context, string, ...string) ([]byte, error) { mutations++; return nil, nil })
	i := &protocol.Instance{ID: 1, Status: StatusRunning, ObservedIP: "10.1.0.8", Network: protocol.NetworkConfig{Mode: "vpc"}, Firewall: []protocol.FirewallRule{{Direction: "out", Action: "drop", Protocol: "any", Enabled: true}}}
	if e := ReconcileFirewall(ctx, d, i, nil); e == nil {
		t.Fatal("stale observation applied")
	}
	if i.ObservedIP != "" || mutations != 0 {
		t.Fatal("bad DHCP state mutated firewall", i, mutations)
	}
}
