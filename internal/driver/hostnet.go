package driver

import (
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

// NetworkModeNat 是 NAT 模式：实例共享主机出口 IP。
const NetworkModeNat = "nat"

// NetworkModeDedicated 是独立 IP 模式：实例网卡直连主机所在网段，
// 使用与主机同网段的独立 IP。只有主机拥有两个及以上 IPv4 地址时才有意义。
const NetworkModeDedicated = "dedicated"

// NetworkModeNone 关闭实例网络。
const NetworkModeNone = "none"

// NormalizeNetworkMode 归一化网络模式；历史数据里的 bridge 视为 dedicated。
func NormalizeNetworkMode(mode string) string {
	mode = strings.ToLower(strings.TrimSpace(mode))
	switch mode {
	case "", NetworkModeNat:
		return NetworkModeNat
	case "bridge", NetworkModeDedicated:
		return NetworkModeDedicated
	case NetworkModeNone:
		return NetworkModeNone
	}
	return NetworkModeNat
}

// HostInterface 是主机上的一个网卡，供独立 IP 模式选择挂载目标。
type HostInterface struct {
	Name  string   `json:"name"`
	Kind  string   `json:"kind"` // bridge=软件网桥, physical=物理网卡, vlan=VLAN 子接口, other=其它
	State string   `json:"state"`
	MAC   string   `json:"mac,omitempty"`
	IPv4  []string `json:"ipv4,omitempty"`
	IPv6  []string `json:"ipv6,omitempty"`
}

// HostNetworkSummary 汇总主机网络，用于独立 IP 模式的可用性判断。
type HostNetworkSummary struct {
	Interfaces []HostInterface `json:"interfaces"`
	// IPv4Count 是所有非 lo 网卡的 IPv4 地址总数（含网桥）。
	// 独立 IP 模式要求至少 2 个：一个归主机用，剩下的才是可以分给实例的。
	IPv4Count int `json:"ipv4_count"`
}

// isGlobalIPv4 过滤掉链路本地 169.254/16 与 127/8，其余 IPv4 都算有效地址。
func isGlobalIPv4(ip net.IP) bool {
	return ip != nil && ip.To4() != nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast()
}

// CollectHostNetwork 枚举主机网卡与地址。独立 IP 模式选择挂载接口、
// 判断「主机是否有富余 IP」都以此为准。
func CollectHostNetwork() HostNetworkSummary {
	summary := HostNetworkSummary{Interfaces: []HostInterface{}}
	ifaces, err := net.Interfaces()
	if err != nil {
		return summary
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		item := HostInterface{
			Name:  iface.Name,
			Kind:  classifyInterface(iface.Name),
			State: "down",
			MAC:   iface.HardwareAddr.String(),
		}
		if iface.Flags&net.FlagUp != 0 {
			item.State = "up"
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			value := strings.Split(addr.String(), "/")[0]
			ip := net.ParseIP(value)
			if ip == nil {
				continue
			}
			if ip.To4() != nil {
				item.IPv4 = append(item.IPv4, value)
				if isGlobalIPv4(ip) {
					summary.IPv4Count++
				}
			} else if !ip.IsLinkLocalUnicast() {
				item.IPv6 = append(item.IPv6, value)
			}
		}
		summary.Interfaces = append(summary.Interfaces, item)
	}
	return summary
}

// classifyInterface 判断网卡类型：软件网桥、物理口还是 VLAN 子接口。
func classifyInterface(name string) string {
	if _, err := os.Stat("/sys/class/net/" + name + "/bridge"); err == nil {
		return "bridge"
	}
	if strings.Contains(name, ".") {
		return "vlan"
	}
	if _, err := os.Stat("/sys/class/net/" + name + "/device"); err == nil {
		return "physical"
	}
	return "other"
}

// isLinuxBridge 报告 name 是否是主机上已存在的软件网桥。
func isLinuxBridge(name string) bool {
	if name == "" {
		return false
	}
	fi, err := os.Stat("/sys/class/net/" + name + "/bridge")
	return err == nil && fi.IsDir()
}

// isHostInterface 报告 name 是否是主机上存在的网卡（含网桥与 VLAN）。
func isHostInterface(name string) bool {
	if name == "" {
		return false
	}
	_, err := os.Stat("/sys/class/net/" + name)
	return err == nil
}

// DedicatedReady 报告主机是否具备独立 IP 模式条件：至少拥有 2 个
// IPv4 地址（主机自身用一个，剩下的才能分给实例所在网段）。
func DedicatedReady() bool {
	return CollectHostNetwork().IPv4Count >= 2
}

// dedicatedTarget 解析独立 IP 模式的挂载目标。bridge/interface 字段留空时
// 自动选第一个 up 且有 IPv4 的物理网卡。返回 (挂载名, 是否网桥, 错误)。
func dedicatedTarget(network protocol.NetworkConfig) (string, bool, error) {
	name := strings.TrimSpace(network.Bridge)
	if name != "" {
		if !isHostInterface(name) {
			return "", false, fmt.Errorf("主机网卡 %s 不存在", name)
		}
		return name, isLinuxBridge(name), nil
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", false, err
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback == 0 && iface.Flags&net.FlagUp != 0 && classifyInterface(iface.Name) == "physical" {
			addrs, _ := iface.Addrs()
			for _, addr := range addrs {
				if ip := net.ParseIP(strings.Split(addr.String(), "/")[0]); isGlobalIPv4(ip) {
					return iface.Name, false, nil
				}
			}
		}
	}
	return "", false, fmt.Errorf("主机没有可用的物理网卡，请在网络设置里指定挂载接口")
}
