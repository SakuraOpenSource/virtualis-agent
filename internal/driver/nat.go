package driver

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

// NAT 映射的落地实现：iptables DNAT。
//
// 规则统一带注释 "virtualis:<实例ID>"，可随时精确枚举与清理。开机应用
// 全量清单（幂等对账：先删本实例不在清单里的规则，再补缺失的），关机/
// 删除时清空。注意 libvirt 会在 FORWARD 链上对进入网桥的流量挂 REJECT，
// 因此放行规则必须插在链首。
const (
	natTagPrefix = "virtualis:"
	forwardChain = "FORWARD"
)

func natTag(id uint) string { return natTagPrefix + strconv.FormatUint(uint64(id), 10) }

func hasIPTables() bool { return hasCommand("iptables") }

// natSlotIP 计算实例在 NAT 网桥子网里的静态保留 IP 与网关（网桥自身地址）。
//
// 槽位按实例 ID 确定性分配（100 + id%140 → x.x.x.100-249），避开网桥
// 自身的 .1，同一被控上不同实例不会撞号。找不到已知 NAT 网桥时返回空。
func natSlotIP(inst *protocol.Instance) (string, string) {
	for _, bridge := range []string{"virbr0", "incusbr0", "lxcbr0"} {
		iface, err := net.InterfaceByName(bridge)
		if err != nil {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			ip, ipNet, err := net.ParseCIDR(addr.String())
			if err != nil || ip.To4() == nil {
				continue
			}
			base := ip.To4()
			mask := ipNet.Mask
			slot := 100 + int(inst.ID%140)
			guest := net.IPv4(base[0]&mask[0], base[1]&mask[1], base[2]&mask[2], byte(slot)).String()
			return guest, ip.String()
		}
	}
	return "", ""
}

// natMAC 为 NAT 实例派生确定性 MAC（未指定 MAC 时用），保证 DHCP 静态
// 保留条目与域 XML 网卡一致。
func natMAC(inst *protocol.Instance) string {
	id := inst.ID
	return fmt.Sprintf("52:54:00:%02x:%02x:%02x", id&0xff, (id>>8)&0xff, (id>>16)&0xff)
}

// ResolveGuestIP 解析实例的转发目标 IP。
//
// 优先静态值（独立 IP 的声明地址 / NAT 的保留地址），拿不到再查询运行
// 中的实例网卡，限时 retries×interval 秒，仍拿不到返回空 —— 调用方据此
// 跳过本次规则应用而不是无限阻塞。
func ResolveGuestIP(ctx context.Context, d Driver, inst *protocol.Instance, retries, intervalSeconds int) string {
	// 只有创建时完成静态保留（或独立 IP 显式声明）的实例才有可信的
	// 静态值；老实例的 DHCP 地址是随机的，必须动态解析。
	if ip := strings.Split(inst.Network.IPv4, "/")[0]; net.ParseIP(ip) != nil {
		return ip
	}
	for i := 0; i < retries; i++ {
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(time.Duration(intervalSeconds) * time.Second):
		}
		status, err := d.Network(ctx, inst)
		if err == nil {
			for _, iface := range status.Interfaces {
				for _, ip := range iface.IPv4 {
					return ip
				}
			}
		}
	}
	return ""
}

// ApplyNATRules 把实例的期望映射清单落地为 iptables 规则（幂等）。
// 返回 (应用的条数, error)。解析不到目标 IP 时返回 (0, nil) 由调用方
// 决定是否重试。
func ApplyNATRules(ctx context.Context, d Driver, inst *protocol.Instance) (int, error) {
	if len(inst.NATMappings) == 0 {
		ClearNATRules(ctx, inst.ID)
		return 0, nil
	}
	if !hasIPTables() {
		return 0, fmt.Errorf("iptables 未安装，无法配置 NAT 端口映射")
	}
	guestIP := ResolveGuestIP(ctx, d, inst, 5, 3)
	if guestIP == "" {
		return 0, fmt.Errorf("无法解析实例 IP，跳过 NAT 映射配置")
	}
	tag := natTag(inst.ID)

	// 先对账：宿主端口是映射的身份，清掉本实例 tag 下不在期望清单里的规则。
	existing := listNATRules(tag)
	desiredHostPorts := make(map[int]bool)
	for _, m := range inst.NATMappings {
		desiredHostPorts[m.HostPort] = true
	}
	for _, rule := range existing {
		if hostPort, ok := ruleHostPort(rule); ok && !desiredHostPorts[hostPort] {
			deleteNATRule(ctx, rule)
		}
	}

	applied := 0
	for _, m := range inst.NATMappings {
		proto := normalizeNATProtocol(m.Protocol)
		hostPort, guestPort := m.HostPort, m.GuestPort

		dnat := []string{
			"-p", proto, "-m", proto, "--dport", itoa(hostPort),
			"-m", "comment", "--comment", tag,
			"-j", "DNAT", "--to-destination", guestIP + ":" + itoa(guestPort),
		}
		ensureRule(ctx, "nat", "PREROUTING", dnat)
		ensureRule(ctx, "nat", "OUTPUT", dnat)

		allow := []string{
			"-d", guestIP, "-p", proto, "-m", proto, "--dport", itoa(guestPort),
			"-m", "comment", "--comment", tag,
			"-j", "ACCEPT",
		}
		// libvirt 的 REJECT 挂在 FORWARD 上，放行必须插在链首。
		ensureForwardRule(ctx, allow)
		applied++
	}
	return applied, nil
}

// ClearNATRules 清除实例的全部 NAT 规则（关机/删除时调用，幂等）。
func ClearNATRules(ctx context.Context, instanceID uint) {
	if !hasIPTables() {
		return
	}
	for _, rule := range listNATRules(natTag(instanceID)) {
		deleteNATRule(ctx, rule)
	}
}

type natRule struct {
	table string
	chain string
	spec  []string // iptables-save 形态的完整规则段（含 -A 之后的部分）
}

// ruleHostPort 从规则 spec 里取 --dport 的值，作为映射身份。
func ruleHostPort(rule natRule) (int, bool) {
	for i, field := range rule.spec {
		if field == "--dport" && i+1 < len(rule.spec) {
			if v, err := strconv.Atoi(rule.spec[i+1]); err == nil {
				return v, true
			}
		}
	}
	return 0, false
}

func normalizeNATProtocol(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	if p == "udp" {
		return "udp"
	}
	return "tcp"
}

func itoa(n int) string { return strconv.Itoa(n) }

// listNATRules 枚举 iptables-save 里带指定 tag 的规则。
func listNATRules(tag string) []natRule {
	var out []natRule
	for _, table := range []string{"nat", "filter"} {
		outBytes, err := output(context.Background(), "iptables-save", "-t", table)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(outBytes), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "-A ") || !strings.Contains(line, tag) {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 3 {
				continue
			}
			out = append(out, natRule{table: table, chain: fields[1], spec: fields[2:]})
		}
	}
	return out
}

func deleteNATRule(ctx context.Context, rule natRule) {
	args := append([]string{"-t", rule.table, "-D", rule.chain}, rule.spec...)
	_ = run(ctx, "iptables", args...)
}

// ensureRule 存在即跳过（-C），缺失才追加（-A）。
func ensureRule(ctx context.Context, table, chain string, spec []string) {
	check := append([]string{"-t", table, "-C", chain}, spec...)
	if err := run(ctx, "iptables", check...); err == nil {
		return
	}
	add := append([]string{"-t", table, "-A", chain}, spec...)
	_ = run(ctx, "iptables", add...)
}

// ensureForwardRule 把放行规则插到 FORWARD 链首，压过 libvirt 的 REJECT。
func ensureForwardRule(ctx context.Context, spec []string) {
	check := append([]string{"-C", forwardChain}, spec...)
	if err := run(ctx, "iptables", check...); err == nil {
		return
	}
	insert := append([]string{"-I", forwardChain, "1"}, spec...)
	_ = run(ctx, "iptables", insert...)
}
