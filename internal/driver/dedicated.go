package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

const routedGateway = "169.254.0.1"
const ownedRouteProtocol = "186"

var hostInterfaceName = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,15}$`)

type dedicatedAttachment struct{ Uplink, Mode, IP, Bridge string }
type hostAddress struct {
	Name     string   `json:"ifname"`
	Flags    []string `json:"flags"`
	State    string   `json:"operstate"`
	Alias    string   `json:"ifalias"`
	LinkInfo struct {
		Kind string `json:"info_kind"`
	} `json:"linkinfo"`
	Addresses []struct {
		Family string `json:"family"`
		Local  string `json:"local"`
		Prefix int    `json:"prefixlen"`
	} `json:"addr_info"`
}

func ownedBridge(id uint) string    { return "vb" + strconv.FormatUint(uint64(id), 36) }
func ownedNIC(id uint) string       { return "vt" + strconv.FormatUint(uint64(id), 36) }
func dedicatedOwner(id uint) string { return "virtualis:" + strconv.FormatUint(uint64(id), 10) }

func resolveDedicated(ctx context.Context, network protocol.NetworkConfig) (dedicatedAttachment, error) {
	var a dedicatedAttachment
	mode := strings.TrimSpace(network.DedicatedMode)
	if mode != "" && mode != "auto" && mode != "routed" && mode != "bridge" {
		return a, fmt.Errorf("dedicated_mode 仅支持 auto/routed/bridge")
	}
	ip := net.ParseIP(strings.Split(network.IPv4, "/")[0])
	if strings.Contains(network.IPv4, "/") {
		var err error
		ip, _, err = net.ParseCIDR(network.IPv4)
		if err != nil {
			return a, fmt.Errorf("独立 IPv4 CIDR 无效")
		}
	}
	if ip == nil || ip.To4() == nil || !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return a, fmt.Errorf("独立模式必须提供有效单播 IPv4")
	}
	if network.Gateway != "" {
		g := net.ParseIP(network.Gateway)
		if g == nil || g.To4() == nil || !g.IsGlobalUnicast() || g.IsLoopback() || g.IsLinkLocalUnicast() || g.Equal(ip) {
			return a, fmt.Errorf("独立模式网关无效")
		}
	}
	for _, dns := range network.DNS {
		if net.ParseIP(dns) == nil {
			return a, fmt.Errorf("DNS 地址无效")
		}
	}
	if network.MAC != "" {
		m, e := net.ParseMAC(network.MAC)
		if e != nil || len(m) != 6 || m[0]&1 != 0 {
			return a, fmt.Errorf("MAC 地址无效")
		}
	}
	out, err := output(ctx, "ip", "-j", "-d", "address", "show")
	if err != nil {
		return a, fmt.Errorf("读取宿主机接口失败: %w", err)
	}
	var hosts []hostAddress
	if err = json.Unmarshal(out, &hosts); err != nil {
		return a, fmt.Errorf("读取宿主机接口失败: %w", err)
	}
	name := strings.TrimSpace(network.Bridge)
	for _, h := range hosts {
		for _, addr := range h.Addresses {
			if parsed := net.ParseIP(addr.Local); parsed != nil && parsed.Equal(ip) {
				return a, fmt.Errorf("IPv4 %s 已分配给宿主机，拒绝地址冲突", ip)
			}
		}
	}
	var selected *hostAddress
	for i := range hosts {
		h := &hosts[i]
		up := false
		for _, f := range h.Flags {
			up = up || f == "UP"
		}
		if !up || h.Name == "lo" {
			continue
		}
		if name != "" && h.Name == name {
			selected = h
			break
		}
		if name == "" && h.LinkInfo.Kind == "" && len(h.Addresses) > 0 {
			selected = h
			break
		}
	}
	if selected == nil || !hostInterfaceName.MatchString(selected.Name) {
		return a, fmt.Errorf("独立模式上联网卡不存在或未启用")
	}
	bridge := selected.LinkInfo.Kind == "bridge"
	if mode == "bridge" && !bridge {
		return a, fmt.Errorf("bridge 模式必须挂载已存在的 Linux 网桥")
	}
	if mode == "" || mode == "auto" {
		mode = "routed"
		if bridge {
			mode = "bridge"
		}
	}
	if mode == "bridge" && network.Gateway == "" {
		return a, fmt.Errorf("bridge 模式必须提供网关")
	}
	a = dedicatedAttachment{Uplink: selected.Name, Mode: mode, IP: ip.String(), Bridge: selected.Name}
	return a, nil
}

func requireSysctl(ctx context.Context, key string) error {
	out, e := output(ctx, "sysctl", "-n", key)
	if e != nil || strings.TrimSpace(string(out)) != "1" {
		return fmt.Errorf("网络前置条件 %s=1 未满足，请管理员预先启用（Agent 不修改主接口或全局转发设置）", key)
	}
	return nil
}

func ensureBridgeFiltering(ctx context.Context) error {
	if e := run(ctx, "modprobe", "br_netfilter"); e != nil {
		return fmt.Errorf("加载桥接过滤模块失败: %w", e)
	}
	// bridge-netfilter is required for filtering real L2 bridge traffic.
	if e := run(ctx, "sysctl", "-w", "net.bridge.bridge-nf-call-iptables=1"); e != nil {
		return e
	}
	return requireSysctl(ctx, "net.bridge.bridge-nf-call-iptables")
}

func ensureQEMUDedicated(ctx context.Context, inst *protocol.Instance, dataDir string) error {
	dedicatedResourcesMu.Lock()
	defer dedicatedResourcesMu.Unlock()
	a, e := resolveDedicated(ctx, inst.Network)
	if e != nil {
		return e
	}
	if a.Mode == "bridge" {
		if e = ensureBridgeFiltering(ctx); e != nil {
			return e
		}
		if e = saveDedicatedRecord(dataDir, dedicatedRecord{ID: inst.ID, Driver: "qemu", Attachment: a, Network: inst.Network}); e != nil {
			return e
		}
		return applyDedicatedProtection(ctx, inst, a, true)
	}
	if e = requireSysctl(ctx, "net.ipv4.ip_forward"); e != nil {
		return e
	}
	if e = requireSysctl(ctx, "net.ipv4.conf."+a.Uplink+".proxy_arp"); e != nil {
		return e
	}
	bridge := ownedBridge(inst.ID)
	if e = checkOwnedRoute(ctx, a.IP, bridge); e != nil {
		return e
	}
	out, e := output(ctx, "ip", "-j", "link", "show", "dev", bridge)
	var links []hostAddress
	if e == nil {
		if e = json.Unmarshal(out, &links); e != nil {
			return e
		}
	} else if !absentNetworkError(e) {
		return e
	}
	if len(links) > 0 {
		if links[0].Alias != dedicatedOwner(inst.ID) || links[0].LinkInfo.Kind != "bridge" {
			return fmt.Errorf("拒绝修改非本实例拥有的接口 %s", bridge)
		}
	}
	if e = ValidateFirewall(inst); e != nil {
		return e
	}
	if e = saveDedicatedRecord(dataDir, dedicatedRecord{ID: inst.ID, Driver: "qemu", Attachment: a, Network: inst.Network}); e != nil {
		return e
	}
	if e = applyDedicatedProtection(ctx, inst, a, true); e != nil {
		return e
	}
	if len(links) == 0 {
		if e = run(ctx, "ip", "link", "add", "name", bridge, "type", "bridge"); e != nil {
			return e
		}
		if e = run(ctx, "ip", "link", "set", "dev", bridge, "alias", dedicatedOwner(inst.ID)); e != nil {
			return e
		}
	}
	for _, args := range [][]string{{"address", "replace", routedGateway + "/32", "dev", bridge}, {"link", "set", "dev", bridge, "up"}, {"route", "replace", a.IP + "/32", "dev", bridge, "proto", ownedRouteProtocol}, {"neigh", "replace", "proxy", a.IP, "dev", a.Uplink}} {
		if e = run(ctx, "ip", args...); e != nil {
			return fmt.Errorf("配置独立路由失败: %w", e)
		}
	}
	return nil
}
