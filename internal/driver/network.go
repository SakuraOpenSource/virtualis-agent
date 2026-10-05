package driver

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

// VPC 虚拟网络的创建与删除。纯逻辑（CIDR 解析、DHCP 范围、掩码换算）
// 与命令执行分离，便于单测；Incus 落地为托管网络，QEMU 落地为 libvirt
// 命名网络，实例都以网络名（protocol.NetworkConfig.Bridge）挂载。

var networkNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,14}$`)

// ValidNetworkName 校验 VPC 网络名：小写字母开头，字母数字与连字符，
// 2-15 位。长度上限对齐 Linux 网卡名（IFNAMSIZ，Incus 托管网络的桥名
// 与网络名同名，过长会创建失败）。
func ValidNetworkName(name string) bool {
	return networkNamePattern.MatchString(strings.TrimSpace(name))
}

// ParseSubnet 解析 VPC 子网 CIDR，返回前缀长度与网络地址。
func ParseSubnet(cidr string) (int, net.IP, error) {
	ip, ipNet, err := net.ParseCIDR(strings.TrimSpace(cidr))
	if err != nil || ip.To4() == nil || ipNet.IP.To4() == nil {
		return 0, nil, fmt.Errorf("子网 CIDR 无效: %s", cidr)
	}
	ones, bits := ipNet.Mask.Size()
	if bits != 32 || ones < 16 || ones > 29 {
		return 0, nil, fmt.Errorf("VPC 子网前缀需在 /16-/29 之间")
	}
	return ones, ipNet.IP.To4(), nil
}

// MaskString 把前缀长度换算成点分十进制掩码（255.255.255.0）。
func MaskString(ones int) string {
	return net.IP(net.CIDRMask(ones, 32)).String()
}

// DefaultDHCPRange 计算子网的默认 DHCP 范围：区间固定在 .10-.250，
// 对更小的子网钳制到可用地址内。返回闭区间起止地址。
func DefaultDHCPRange(subnet net.IP, ones int) (string, string, error) {
	mask := binary.BigEndian.Uint32(net.CIDRMask(ones, 32))
	base := binary.BigEndian.Uint32(subnet.To4()) & mask
	broadcast := base | ^mask
	start := base + 10
	end := base + 250
	if broadcast > base+1 && end > broadcast-1 {
		end = broadcast - 1
	}
	if start >= end {
		start = base + 2
	}
	if start >= end || broadcast <= base+2 {
		return "", "", fmt.Errorf("子网过小，无法划分 DHCP 范围")
	}
	return uint32ToIPv4(start).String(), uint32ToIPv4(end).String(), nil
}

func uint32ToIPv4(v uint32) net.IP {
	ip := make(net.IP, 4)
	binary.BigEndian.PutUint32(ip, v)
	return ip
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// normalizeNetworkSpec 校验下发的网络参数，并补全 DHCP 范围。
func normalizeNetworkSpec(spec protocol.NetworkSpec) (protocol.NetworkSpec, int, net.IP, error) {
	spec.Name = strings.TrimSpace(spec.Name)
	if !ValidNetworkName(spec.Name) {
		return spec, 0, nil, fmt.Errorf("网络名称需为 2-15 位小写字母、数字或连字符")
	}
	ones, subnet, err := ParseSubnet(spec.Subnet)
	if err != nil {
		return spec, 0, nil, err
	}
	spec.Gateway = strings.TrimSpace(spec.Gateway)
	gateway := net.ParseIP(spec.Gateway)
	if gateway == nil || gateway.To4() == nil {
		return spec, 0, nil, fmt.Errorf("网关地址无效: %s", spec.Gateway)
	}
	spec.DHCPStart = strings.TrimSpace(spec.DHCPStart)
	spec.DHCPEnd = strings.TrimSpace(spec.DHCPEnd)
	if spec.DHCPStart == "" || spec.DHCPEnd == "" {
		start, end, err := DefaultDHCPRange(subnet, ones)
		if err != nil {
			return spec, 0, nil, err
		}
		spec.DHCPStart, spec.DHCPEnd = start, end
	}
	return spec, ones, subnet, nil
}

// CreateNetwork 创建 Incus 托管网络（VPC）。
func (d *Incus) CreateNetwork(ctx context.Context, spec protocol.NetworkSpec) error {
	spec, ones, _, err := normalizeNetworkSpec(spec)
	if err != nil {
		return err
	}
	args := []string{"network", "create", spec.Name,
		"ipv4.address=" + spec.Gateway + "/" + strconv.Itoa(ones),
		"ipv4.nat=" + boolText(spec.NAT),
		"ipv4.dhcp=true",
		"ipv4.dhcp.ranges=" + spec.DHCPStart + "-" + spec.DHCPEnd,
		// 默认出站走 IPv4；IPv6 交给 DNAT/防火墙层面处理。
		"ipv6.address=none",
	}
	if err := run(ctx, d.cli(), args...); err != nil {
		return fmt.Errorf("创建 VPC 网络失败: %w", err)
	}
	return nil
}

// DeleteNetwork 删除 Incus 托管网络；仍被实例占用的网络由 Incus 拒绝，
// 错误原样上抛给主控展示。
func (d *Incus) DeleteNetwork(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if !ValidNetworkName(name) {
		return fmt.Errorf("网络名称无效")
	}
	if err := run(ctx, d.cli(), "network", "delete", name); err != nil {
		return fmt.Errorf("删除 VPC 网络失败: %w", err)
	}
	return nil
}

// CreateNetwork 定义并启动一个 libvirt 命名网络（VPC），供 QEMU 实例以
// <source network=.../> 挂载。NAT 开关对应 forward 模式；隔离网络不带
// forward 元素，仅子网内互通。
func (d *QEMU) CreateNetwork(ctx context.Context, spec protocol.NetworkSpec) error {
	spec, ones, _, err := normalizeNetworkSpec(spec)
	if err != nil {
		return err
	}
	if _, err := output(ctx, "virsh", "net-info", spec.Name); err == nil {
		return fmt.Errorf("网络 %s 已存在", spec.Name)
	}
	var b strings.Builder
	b.WriteString("<network>\n")
	fmt.Fprintf(&b, "  <name>%s</name>\n", spec.Name)
	if spec.NAT {
		b.WriteString("  <forward mode='nat'/>\n")
	}
	fmt.Fprintf(&b, "  <ip address='%s' netmask='%s'>\n", spec.Gateway, MaskString(ones))
	fmt.Fprintf(&b, "    <dhcp>\n      <range start='%s' end='%s'/>\n    </dhcp>\n", spec.DHCPStart, spec.DHCPEnd)
	if len(spec.DNS) > 0 {
		b.WriteString("    <dns>\n")
		for _, server := range spec.DNS {
			server = strings.TrimSpace(server)
			if net.ParseIP(server) == nil {
				continue
			}
			fmt.Fprintf(&b, "      <server address='%s'/>\n", server)
		}
		b.WriteString("    </dns>\n")
	}
	b.WriteString("  </ip>\n</network>\n")

	tmp, createErr := os.CreateTemp("", "virtualis-vpc-*.xml")
	if createErr != nil {
		return fmt.Errorf("创建 VPC 网络配置失败: %w", createErr)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, writeErr := tmp.WriteString(b.String()); writeErr != nil {
		tmp.Close()
		return fmt.Errorf("写入 VPC 网络配置失败: %w", writeErr)
	}
	if closeErr := tmp.Close(); closeErr != nil {
		return closeErr
	}
	if defineErr := run(ctx, "virsh", "net-define", tmpName); defineErr != nil {
		return fmt.Errorf("定义 VPC 网络失败: %w", defineErr)
	}
	if startErr := run(ctx, "virsh", "net-start", spec.Name); startErr != nil && !contains(startErr.Error(), "already active") {
		return fmt.Errorf("启动 VPC 网络失败: %w", startErr)
	}
	// autostart 尽力而为：失败只影响宿主机重启后的网络自启。
	_ = run(ctx, "virsh", "net-autostart", spec.Name)
	return nil
}

// DeleteNetwork 停止并移除 libvirt 命名网络（幂等容忍未激活/不存在）。
func (d *QEMU) DeleteNetwork(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if !ValidNetworkName(name) {
		return fmt.Errorf("网络名称无效")
	}
	if err := run(ctx, "virsh", "net-destroy", name); err != nil &&
		!contains(err.Error(), "not active") && !contains(err.Error(), "no network with matching name") {
		return fmt.Errorf("停止 VPC 网络失败: %w", err)
	}
	if err := run(ctx, "virsh", "net-undefine", name); err != nil &&
		!contains(err.Error(), "no network with matching name") {
		return fmt.Errorf("删除 VPC 网络失败: %w", err)
	}
	return nil
}
