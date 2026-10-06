package driver

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

// 实例防火墙的落地实现：iptables filter 表，每个实例一条独立链
// VIRTIS-FW-<id>，FORWARD 链按实例 IP 挂跳转（入向匹配 -d、出向匹配
// -s）。规则按 priority 升序、同优先级按 ID 升序匹配（先命中先生效）；
// 没有任何启用规则时等同于不过滤（允许全部流量）。
//
// 与 NAT 的 DNAT 规则不同，防火墙链不随实例关机清除：链只挂在
// FORWARD 上，实例关机后没有流量命中，留着无害；删除实例时才彻底清理。

const firewallChainPrefix = "VIRTIS-FW-"

// FirewallChainName 返回实例的防火墙链名。
func FirewallChainName(id uint) string {
	return firewallChainPrefix + strconv.FormatUint(uint64(id), 10)
}

// guestFirewallIP 解析防火墙规则要匹配的实例地址：NAT 模式用静态保留
// IP，其余模式用显式配置的 IPv4。
func guestFirewallIP(inst *protocol.Instance) string {
	if NormalizeNetworkMode(inst.Network.Mode) == NetworkModeNat {
		if ip, _ := natSlotIP(inst); ip != "" {
			return ip
		}
	}
	if v := strings.TrimSpace(inst.Network.IPv4); v != "" {
		return strings.Split(v, "/")[0]
	}
	if ip := net.ParseIP(inst.ObservedIP); ip != nil && ip.To4() != nil && ip.IsGlobalUnicast() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
		return ip.String()
	}
	return ""
}

// firewallRuleArgs 把一条规则翻译成 iptables 参数；返回 false 表示规则
// 无效（乱填的协议/动作在落地端跳过而不是放行）。
func firewallRuleArgs(chain string, rule protocol.FirewallRule) ([]string, bool) {
	direction := strings.ToLower(strings.TrimSpace(rule.Direction))
	if direction != "in" && direction != "out" {
		return nil, false
	}
	action := strings.ToLower(strings.TrimSpace(rule.Action))
	if action != "accept" && action != "drop" {
		return nil, false
	}
	args := []string{"-A", chain}
	if cidr := strings.TrimSpace(rule.CIDR); cidr != "" {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			if net.ParseIP(cidr) == nil {
				return nil, false
			}
		}
		if strings.EqualFold(strings.TrimSpace(rule.Direction), "out") {
			args = append(args, "-d", cidr)
		} else {
			args = append(args, "-s", cidr)
		}
	}
	switch strings.ToLower(strings.TrimSpace(rule.Protocol)) {
	case "tcp", "udp":
		proto := strings.ToLower(strings.TrimSpace(rule.Protocol))
		args = append(args, "-p", proto)
		if rule.PortStart > 0 && rule.PortStart <= 65535 {
			end := rule.PortEnd
			if end < rule.PortStart || end > 65535 {
				end = rule.PortStart
			}
			if end > rule.PortStart {
				args = append(args, "--dport", fmt.Sprintf("%d:%d", rule.PortStart, end))
			} else {
				args = append(args, "--dport", strconv.Itoa(rule.PortStart))
			}
		}
	case "icmp":
		args = append(args, "-p", "icmp")
	case "", "any", "all":
		// 不限定协议。
	default:
		return nil, false
	}
	if action == "drop" {
		args = append(args, "-j", "DROP")
	} else {
		args = append(args, "-j", "ACCEPT")
	}
	return args, true
}

// firewallOp 是一步落地操作。
type firewallOp struct {
	// Check 非空时先执行它：成功（exit 0）代表目标状态已存在，跳过 Args。
	Check []string
	Args  []string
	// IgnoreError 为 true 时失败不中断后续操作（幂等操作：链已存在等）。
	IgnoreError bool
}

// buildFirewallPlan 生成完整应用计划，顺序：建链（容忍已存在）、清链、
// 规则（按优先级）、入向/出向跳转（仅在存在对应方向规则时插入）。
func buildFirewallPlan(chain, ip string, rules []protocol.FirewallRule) []firewallOp {
	sorted := make([]protocol.FirewallRule, 0, len(rules))
	for _, rule := range rules {
		if rule.Enabled {
			sorted = append(sorted, rule)
		}
	}
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Priority != sorted[j].Priority {
			return sorted[i].Priority < sorted[j].Priority
		}
		return sorted[i].ID < sorted[j].ID
	})
	ops := []firewallOp{
		{Args: []string{"-N", chain}, IgnoreError: true},
		{Args: []string{"-F", chain}},
	}
	hasIn, hasOut := false, false
	for _, rule := range sorted {
		args, ok := firewallRuleArgs(chain, rule)
		if !ok {
			continue
		}
		// Both FORWARD directions enter this chain; every rule must also bind
		// its own instance endpoint or an inbound rule can allow/drop egress.
		endpoint := "-d"
		if strings.EqualFold(strings.TrimSpace(rule.Direction), "out") {
			endpoint = "-s"
		}
		args = append(args[:2], append([]string{endpoint, ip}, args[2:]...)...)
		if strings.EqualFold(strings.TrimSpace(rule.Direction), "out") {
			hasOut = true
		} else {
			hasIn = true
		}
		ops = append(ops, firewallOp{Args: args})
	}
	if hasIn {
		ops = append(ops, firewallOp{
			Check: []string{"-C", "FORWARD", "-d", ip, "-j", chain},
			Args:  []string{"-I", "FORWARD", "1", "-d", ip, "-j", chain},
		})
	}
	if hasOut {
		ops = append(ops, firewallOp{
			Check: []string{"-C", "FORWARD", "-s", ip, "-j", chain},
			Args:  []string{"-I", "FORWARD", "1", "-s", ip, "-j", chain},
		})
	}
	return ops
}

// removeChainJumps 删除 FORWARD 链上所有指向指定链的跳转（按 iptables -S
// 的精确语法反向执行，避免误删其它规则）。
func removeChainJumps(ctx context.Context, chain string) {
	out, err := output(ctx, "iptables", "-S", "FORWARD")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 4 || fields[0] != "-A" || fields[1] != "FORWARD" {
			continue
		}
		if fields[len(fields)-1] != chain || fields[len(fields)-2] != "-j" {
			continue
		}
		del := append([]string{"-D", "FORWARD"}, fields[2:]...)
		_ = run(ctx, "iptables", del...)
	}
}

// ApplyFirewall 把实例的规则清单幂等落到宿主机 iptables；清单为空等同于
// 清除（链保留但不再挂跳转）。任何失败都会返回错误交给调用方记录。
func ApplyFirewall(ctx context.Context, inst *protocol.Instance) error {
	if inst == nil || inst.ID == 0 {
		return fmt.Errorf("实例无效")
	}
	chain := FirewallChainName(inst.ID)
	if !commandAvailable(ctx, "iptables") {
		hasRules := false
		for _, rule := range inst.Firewall {
			if rule.Enabled {
				hasRules = true
				break
			}
		}
		if !hasRules {
			return nil
		}
		return fmt.Errorf("宿主机缺少 iptables，无法应用防火墙规则")
	}
	ip := guestFirewallIP(inst)
	ops := buildFirewallPlan(chain, ip, inst.Firewall)
	hasRules := len(ops) > 2
	if hasRules && ip == "" {
		return fmt.Errorf("实例 %d 缺少可用于防火墙匹配的 IP，规则未应用", inst.ID)
	}
	// 先摘掉旧跳转，再全量重建：方向变化（入向 ↔ 出向）时不会残留。
	removeChainJumps(ctx, chain)
	for _, op := range ops {
		if len(op.Check) > 0 && run(ctx, "iptables", op.Check...) == nil {
			continue
		}
		if err := run(ctx, "iptables", op.Args...); err != nil && !op.IgnoreError {
			return fmt.Errorf("应用防火墙规则失败: %w", err)
		}
	}
	return nil
}

// ClearFirewall 彻底清理实例的防火墙链（删除实例时调用）。宿主机没有
// iptables 时是空操作；链或跳转不存在时静默跳过。
func ClearFirewall(ctx context.Context, id uint) {
	if id == 0 || !hasIPTables() {
		return
	}
	chain := FirewallChainName(id)
	removeChainJumps(ctx, chain)
	_ = run(ctx, "iptables", "-F", chain)
	_ = run(ctx, "iptables", "-X", chain)
}
