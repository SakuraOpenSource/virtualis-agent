package driver

import (
	"slices"
	"testing"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

func TestFirewallChainName(t *testing.T) {
	if got := FirewallChainName(42); got != "VIRTIS-FW-42" {
		t.Fatalf("FirewallChainName(42) = %q", got)
	}
	if len(FirewallChainName(4294967295)) > 28 {
		t.Fatalf("链名超出 iptables 上限: %q", FirewallChainName(4294967295))
	}
}

func TestFirewallRuleArgs(t *testing.T) {
	chain := "VIRTIS-FW-7"
	cases := []struct {
		name string
		rule protocol.FirewallRule
		want []string
		ok   bool
	}{
		{
			name: "tcp 单端口入向",
			rule: protocol.FirewallRule{Direction: "in", Action: "accept", Protocol: "tcp", PortStart: 22, Enabled: true},
			want: []string{"-A", chain, "-p", "tcp", "--dport", "22", "-j", "ACCEPT"},
			ok:   true,
		},
		{
			name: "tcp 端口区间 + 来源 CIDR",
			rule: protocol.FirewallRule{Direction: "in", Action: "drop", Protocol: "tcp", PortStart: 8000, PortEnd: 8100, CIDR: "203.0.113.0/24", Enabled: true},
			want: []string{"-A", chain, "-s", "203.0.113.0/24", "-p", "tcp", "--dport", "8000:8100", "-j", "DROP"},
			ok:   true,
		},
		{
			name: "出向以 -d 匹配目标",
			rule: protocol.FirewallRule{Direction: "out", Action: "accept", Protocol: "udp", PortStart: 53, CIDR: "8.8.8.8", Enabled: true},
			want: []string{"-A", chain, "-d", "8.8.8.8", "-p", "udp", "--dport", "53", "-j", "ACCEPT"},
			ok:   true,
		},
		{
			name: "icmp 不带端口",
			rule: protocol.FirewallRule{Direction: "in", Action: "accept", Protocol: "icmp", PortStart: 8, Enabled: true},
			want: []string{"-A", chain, "-p", "icmp", "-j", "ACCEPT"},
			ok:   true,
		},
		{
			name: "any 协议不限",
			rule: protocol.FirewallRule{Direction: "in", Action: "drop", Protocol: "any", Enabled: true},
			want: []string{"-A", chain, "-j", "DROP"},
			ok:   true,
		},
		{
			name: "非法动作跳过",
			rule: protocol.FirewallRule{Direction: "in", Action: "reject", Protocol: "tcp", Enabled: true},
			ok:   false,
		},
		{
			name: "非法协议跳过",
			rule: protocol.FirewallRule{Direction: "in", Action: "accept", Protocol: "sctp", Enabled: true},
			ok:   false,
		},
		{
			name: "非法 CIDR 跳过",
			rule: protocol.FirewallRule{Direction: "in", Action: "accept", Protocol: "tcp", CIDR: "not-an-ip", Enabled: true},
			ok:   false,
		},
		{
			name: "端口越界退化为单端口",
			rule: protocol.FirewallRule{Direction: "in", Action: "accept", Protocol: "tcp", PortStart: 443, PortEnd: 70000, Enabled: true},
			want: []string{"-A", chain, "-p", "tcp", "--dport", "443", "-j", "ACCEPT"},
			ok:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := firewallRuleArgs(chain, tc.rule)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("args = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBuildFirewallPlanOrdering(t *testing.T) {
	chain := "VIRTIS-FW-9"
	rules := []protocol.FirewallRule{
		{ID: 3, Direction: "in", Action: "drop", Protocol: "tcp", PortStart: 80, Priority: 10, Enabled: true},
		{ID: 2, Direction: "in", Action: "accept", Protocol: "tcp", PortStart: 22, Priority: 5, Enabled: true},
		{ID: 1, Direction: "in", Action: "accept", Protocol: "icmp", Priority: 5, Enabled: true},
		{ID: 4, Direction: "in", Action: "accept", Protocol: "tcp", PortStart: 8080, Enabled: false},
		{ID: 5, Direction: "out", Action: "accept", Protocol: "udp", PortStart: 53, Priority: 1, Enabled: true},
	}
	ops := buildFirewallPlan(chain, "10.10.10.5", rules)
	// 前两步固定为建链 + 清链。
	if len(ops) < 4 || ops[0].Args[0] != "-N" || ops[1].Args[0] != "-F" {
		t.Fatalf("plan 起始两步异常: %+v", ops[:2])
	}
	var ruleOps [][]string
	var jumpOps [][]string
	for _, op := range ops[2:] {
		if len(op.Args) > 0 && op.Args[0] == "-A" {
			ruleOps = append(ruleOps, op.Args)
		} else {
			jumpOps = append(jumpOps, op.Args)
		}
	}
	// 期望顺序：priority=1 的出向规则 → priority=5 的 icmp、22 → priority=10 的 80；禁用的 8080 不出现。
	wantOrder := []string{"53", "icmp", "22", "80"}
	gotOrder := ruleSignatures(ruleOps)
	if !slices.Equal(gotOrder, wantOrder) {
		t.Fatalf("规则顺序 = %v, want %v", gotOrder, wantOrder)
	}
	// 存在入向与出向规则：应有两条跳转（入向 -d 在前，出向 -s 在后）。
	if len(jumpOps) != 2 {
		t.Fatalf("跳转数 = %d, want 2 (%v)", len(jumpOps), jumpOps)
	}
	if !slices.Equal(jumpOps[0], []string{"-I", "FORWARD", "1", "-d", "10.10.10.5", "-j", chain}) {
		t.Fatalf("入向跳转 = %v", jumpOps[0])
	}
	if !slices.Equal(jumpOps[1], []string{"-I", "FORWARD", "1", "-s", "10.10.10.5", "-j", chain}) {
		t.Fatalf("出向跳转 = %v", jumpOps[1])
	}
}

// ruleSignatures 为每条规则生成一个可比较的签名：icmp 规则取 "icmp"，
// 端口规则取 --dport 值，便于断言顺序。
func ruleSignatures(ruleOps [][]string) []string {
	var sigs []string
	for _, args := range ruleOps {
		sig := ""
		for i, arg := range args {
			if arg == "--dport" && i+1 < len(args) {
				sig = args[i+1]
			}
			if arg == "icmp" {
				sig = "icmp"
			}
		}
		sigs = append(sigs, sig)
	}
	return sigs
}

func TestBuildFirewallPlanEmpty(t *testing.T) {
	ops := buildFirewallPlan("VIRTIS-FW-1", "10.0.0.2", nil)
	if len(ops) != 2 {
		t.Fatalf("空规则应只有建链+清链两步，got %+v", ops)
	}
	// 只有出向规则时不应出现入向跳转。
	outOnly := buildFirewallPlan("VIRTIS-FW-1", "10.0.0.2", []protocol.FirewallRule{
		{ID: 1, Direction: "out", Action: "accept", Protocol: "any", Enabled: true},
	})
	if len(outOnly) != 4 {
		t.Fatalf("出向规则应产生 建链+清链+规则+出向跳转，got %d", len(outOnly))
	}
	if got := outOnly[3].Args; !slices.Equal(got, []string{"-I", "FORWARD", "1", "-s", "10.0.0.2", "-j", "VIRTIS-FW-1"}) {
		t.Fatalf("出向跳转 = %v", got)
	}
}

func TestGuestFirewallIP(t *testing.T) {
	inst := &protocol.Instance{ID: 1, Network: protocol.NetworkConfig{Mode: NetworkModeDedicated, IPv4: "198.51.100.7/26"}}
	if got := guestFirewallIP(inst); got != "198.51.100.7" {
		t.Fatalf("dedicated IP = %q", got)
	}
	inst = &protocol.Instance{ID: 1, Network: protocol.NetworkConfig{Mode: NetworkModeVPC}}
	if got := guestFirewallIP(inst); got != "" {
		t.Fatalf("vpc 无显式 IP 时应为空, got %q", got)
	}
}
