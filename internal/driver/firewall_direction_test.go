package driver

import (
	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
	"slices"
	"testing"
)

func TestFirewallPlanIsolatesDirectionsWithoutCIDR(t *testing.T) {
	ip := "10.100.0.8"
	rules := []protocol.FirewallRule{
		{Direction: "in", Action: "drop", Protocol: "tcp", PortStart: 22, Enabled: true},
		{Direction: "out", Action: "accept", Protocol: "udp", PortStart: 53, Enabled: true},
	}
	ops := buildFirewallPlan("VIRTIS-FW-8", ip, rules)
	for i, want := range []string{"-d", "-s"} {
		args := ops[i+2].Args
		index := slices.Index(args, want)
		if index < 0 || index+1 >= len(args) || args[index+1] != ip {
			t.Fatalf("direction %s can match opposite traffic: %v", rules[i].Direction, args)
		}
	}
}

func TestFirewallPlanRejectsUnknownDirection(t *testing.T) {
	ops := buildFirewallPlan("VIRTIS-FW-8", "10.100.0.8", []protocol.FirewallRule{{Direction: "sideways", Action: "accept", Protocol: "any", Enabled: true}})
	if len(ops) != 2 {
		t.Fatalf("invalid direction generated allow/jump: %v", ops)
	}
}
