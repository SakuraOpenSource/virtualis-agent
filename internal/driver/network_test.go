package driver

import (
	"testing"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

func TestValidNetworkName(t *testing.T) {
	valid := []string{"vpc1", "vpc-prod", "net01", "a1"}
	invalid := []string{"", "A", "VPC1", "-vpc", "vpc_1", "vpc1!", "abcdefghijklmnop", "1vpc"}
	for _, name := range valid {
		if !ValidNetworkName(name) {
			t.Errorf("ValidNetworkName(%q) = false, want true", name)
		}
	}
	for _, name := range invalid {
		if ValidNetworkName(name) {
			t.Errorf("ValidNetworkName(%q) = true, want false", name)
		}
	}
}

func TestParseSubnet(t *testing.T) {
	ones, subnet, err := ParseSubnet("10.100.0.0/24")
	if err != nil || ones != 24 || subnet.String() != "10.100.0.0" {
		t.Fatalf("ParseSubnet(/24) = %d %v %v", ones, subnet, err)
	}
	// 主机位会被归一化到网络地址。
	ones, subnet, err = ParseSubnet("10.100.0.77/24")
	if err != nil || ones != 24 || subnet.String() != "10.100.0.0" {
		t.Fatalf("ParseSubnet 网络地址归一化失败: %d %v %v", ones, subnet, err)
	}
	for _, bad := range []string{"", "10.100.0.0/30", "10.100.0.0/8", "300.1.1.1/24", "fd00::/64"} {
		if _, _, err := ParseSubnet(bad); err == nil {
			t.Errorf("ParseSubnet(%q) 应报错", bad)
		}
	}
}

func TestMaskString(t *testing.T) {
	if got := MaskString(24); got != "255.255.255.0" {
		t.Fatalf("MaskString(24) = %q", got)
	}
	if got := MaskString(26); got != "255.255.255.192" {
		t.Fatalf("MaskString(26) = %q", got)
	}
}

func TestDefaultDHCPRange(t *testing.T) {
	_, subnet, err := ParseSubnet("10.100.0.0/24")
	if err != nil {
		t.Fatal(err)
	}
	start, end, err := DefaultDHCPRange(subnet, 24)
	if err != nil || start != "10.100.0.10" || end != "10.100.0.250" {
		t.Fatalf("DefaultDHCPRange(/24) = %q-%q, %v", start, end, err)
	}
	// /26 子网：范围钳制到广播地址前一格。
	_, subnet, err = ParseSubnet("172.16.5.0/26")
	if err != nil {
		t.Fatal(err)
	}
	start, end, err = DefaultDHCPRange(subnet, 26)
	if err != nil || start != "172.16.5.10" || end != "172.16.5.62" {
		t.Fatalf("DefaultDHCPRange(/26) = %q-%q, %v", start, end, err)
	}
	// /29 子网：地址少，范围收缩到实际可用区间（网关通常占 .1）。
	_, subnet, err = ParseSubnet("192.168.9.0/29")
	if err != nil {
		t.Fatal(err)
	}
	start, end, err = DefaultDHCPRange(subnet, 29)
	if err != nil || start != "192.168.9.2" || end != "192.168.9.6" {
		t.Fatalf("DefaultDHCPRange(/29) = %q-%q, %v", start, end, err)
	}
}

// testSpec 组装一个测试用 NetworkSpec。
func testSpec(name, subnet, gateway string) protocol.NetworkSpec {
	return protocol.NetworkSpec{Name: name, Subnet: subnet, Gateway: gateway, NAT: true}
}

func TestNormalizeNetworkSpec(t *testing.T) {
	spec, ones, subnet, err := normalizeNetworkSpec(testSpec("vpc-prod", "10.7.0.0/24", "10.7.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	if ones != 24 || subnet.String() != "10.7.0.0" || spec.DHCPStart != "10.7.0.10" || spec.DHCPEnd != "10.7.0.250" {
		t.Fatalf("normalizeNetworkSpec = %+v", spec)
	}
	// 显式 DHCP 范围不被默认值覆盖。
	explicit := testSpec("vpc-prod", "10.7.0.0/24", "10.7.0.1")
	explicit.DHCPStart, explicit.DHCPEnd = "10.7.0.50", "10.7.0.60"
	spec, _, _, err = normalizeNetworkSpec(explicit)
	if err != nil || spec.DHCPStart != "10.7.0.50" || spec.DHCPEnd != "10.7.0.60" {
		t.Fatalf("显式 DHCP 范围被覆盖: %+v, %v", spec, err)
	}
	if _, _, _, err := normalizeNetworkSpec(testSpec("Bad_Name", "10.7.0.0/24", "10.7.0.1")); err == nil {
		t.Fatal("非法网络名应报错")
	}
	if _, _, _, err := normalizeNetworkSpec(testSpec("vpc-prod", "10.7.0.0/24", "not-ip")); err == nil {
		t.Fatal("非法网关应报错")
	}
}
