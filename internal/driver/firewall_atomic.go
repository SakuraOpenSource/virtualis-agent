package driver

import (
	"context"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

// Serialize read/replace so concurrent instance changes cannot race the jump
// snapshot. --noflush preserves every foreign chain and rule; COMMIT changes
// the owned chains and their jumps together, or leaves the previous rules intact.
var firewallReplaceMu sync.Mutex

func ValidateFirewall(inst *protocol.Instance) error {
	if inst == nil || inst.ID == 0 {
		return fmt.Errorf("实例无效")
	}
	if p := inst.FirewallPolicy; p != nil {
		for _, v := range []string{p.Ingress, p.Egress} {
			if v != "accept" && v != "drop" {
				return fmt.Errorf("防火墙默认策略只允许 accept/drop")
			}
		}
	}
	if len(inst.Firewall) > 4352 {
		return fmt.Errorf("防火墙规则过多")
	}
	for _, r := range inst.Firewall {
		if !r.Enabled {
			continue
		}
		if _, ok := firewallRuleArgs("validate", r); !ok {
			return fmt.Errorf("防火墙规则 %d 无效", r.ID)
		}
		if r.PortStart < 0 || r.PortStart > 65535 || r.PortEnd < 0 || r.PortEnd > 65535 || (r.PortEnd != 0 && (r.PortStart == 0 || r.PortEnd < r.PortStart)) {
			return fmt.Errorf("防火墙规则 %d 端口范围无效", r.ID)
		}
		p := strings.ToLower(strings.TrimSpace(r.Protocol))
		if (r.PortStart != 0 || r.PortEnd != 0) && p != "tcp" && p != "udp" {
			return fmt.Errorf("非 TCP/UDP 规则不能指定端口")
		}
		if r.CIDR != "" {
			ip := net.ParseIP(r.CIDR)
			if strings.Contains(r.CIDR, "/") {
				ip, _, _ = net.ParseCIDR(r.CIDR)
			}
			if ip == nil || ip.To4() == nil {
				return fmt.Errorf("仅支持 IPv4 防火墙 CIDR")
			}
		}
	}
	return nil
}

func restoreOwnedFilter(ctx context.Context, chains []string, body string) error {
	return restoreOwnedTable(ctx, "filter", "FORWARD", chains, body)
}

func restoreOwnedTable(ctx context.Context, table, hook string, chains []string, body string) error {
	args := []string{"-S", hook}
	if table != "filter" {
		args = append([]string{"-t", table}, args...)
	}
	out, err := output(ctx, "iptables", args...)
	if err != nil {
		return fmt.Errorf("读取防火墙失败: %w", err)
	}
	owned := map[string]bool{}
	for _, c := range chains {
		owned[c] = true
	}
	var b strings.Builder
	b.WriteString("*" + table + "\n")
	for _, c := range chains {
		fmt.Fprintf(&b, ":%s - [0:0]\n-F %s\n", c, c)
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 4 && f[0] == "-A" && f[1] == hook && f[len(f)-2] == "-j" && owned[f[len(f)-1]] {
			b.WriteString("-D" + line[2:] + "\n")
		}
	}
	b.WriteString(body)
	b.WriteString("COMMIT\n")
	f, err := os.CreateTemp("", "virtualis-filter-*.rules")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.WriteString(b.String()); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return run(ctx, "iptables-restore", "--wait", "5", "--noflush", f.Name())
}

func replaceFirewall(ctx context.Context, inst *protocol.Instance) error {
	if err := ValidateFirewall(inst); err != nil {
		return err
	}
	active := inst.FirewallPolicy != nil
	for _, r := range inst.Firewall {
		active = active || r.Enabled
	}
	if active {
		ip := net.ParseIP(guestFirewallIP(inst))
		if ip == nil || ip.To4() == nil || !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			return fmt.Errorf("实例缺少有效 IPv4，规则未应用")
		}
	}
	if !commandAvailable(ctx, "iptables") || !commandAvailable(ctx, "iptables-restore") {
		if !active {
			return nil
		}
		return fmt.Errorf("宿主机缺少 iptables/iptables-restore")
	}
	chain, ip := FirewallChainName(inst.ID), guestFirewallIP(inst)
	rules := append([]protocol.FirewallRule{}, inst.Firewall...)
	sort.SliceStable(rules, func(i, j int) bool {
		if rules[i].Priority != rules[j].Priority {
			return rules[i].Priority < rules[j].Priority
		}
		return rules[i].ID < rules[j].ID
	})
	var b strings.Builder
	in, out := false, false
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		a, _ := firewallRuleArgs(chain, r)
		endpoint := "-d"
		if strings.EqualFold(strings.TrimSpace(r.Direction), "out") {
			endpoint = "-s"
			out = true
		} else {
			in = true
		}
		a = append(a[:2], append([]string{endpoint, ip}, a[2:]...)...)
		if a[len(a)-1] == "ACCEPT" {
			a[len(a)-1] = "RETURN"
		}
		b.WriteString(strings.Join(a, " ") + "\n")
	}
	if p := inst.FirewallPolicy; p != nil {
		in, out = true, true
		for _, v := range []struct{ endpoint, policy string }{{"-d", p.Ingress}, {"-s", p.Egress}} {
			target := "RETURN"
			if v.policy == "drop" {
				target = "DROP"
			}
			fmt.Fprintf(&b, "-A %s %s %s -j %s\n", chain, v.endpoint, ip, target)
		}
	}
	if in {
		fmt.Fprintf(&b, "-I FORWARD 1 -d %s -j %s\n", ip, chain)
	}
	if out {
		fmt.Fprintf(&b, "-I FORWARD 1 -s %s -j %s\n", ip, chain)
	}
	firewallReplaceMu.Lock()
	defer firewallReplaceMu.Unlock()
	return restoreOwnedFilter(ctx, []string{chain}, b.String())
}
