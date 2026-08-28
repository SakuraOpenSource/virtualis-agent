package driver

import (
	"strings"
	"testing"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

func TestNormalizeNetworkMode(t *testing.T) {
	cases := map[string]string{
		"":          NetworkModeNat,
		"nat":       NetworkModeNat,
		"NAT":       NetworkModeNat,
		"bridge":    NetworkModeDedicated,
		"dedicated": NetworkModeDedicated,
		" none ":    NetworkModeNone,
	}
	for input, want := range cases {
		if got := NormalizeNetworkMode(input); got != want {
			t.Errorf("NormalizeNetworkMode(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestDomainXMLBasics(t *testing.T) {
	inst := &protocol.Instance{
		ID:   7,
		Name: "web-01",
		Type: "vm",
		Spec: protocol.InstanceSpec{CPU: 2, MemoryMB: 2048, DiskGB: 20, Arch: "x86_64"},
		Network: protocol.NetworkConfig{
			Mode: NetworkModeNat, MAC: "52:54:00:12:34:56", BandwidthMbps: 100,
		},
	}
	xml := domainXML(resourceName("qemu", inst), inst, "/data/images/disk.qcow2", "/data/images/install.iso")

	checks := []string{
		"<name>virtualis-7-web-01</name>",
		"<memory unit='MiB'>2048</memory>",
		"<vcpu placement='static'>2</vcpu>",
		"machine='pc'",
		"mode='host-passthrough'",
		"<boot dev='cdrom'/>", // ISO 存在时优先光驱引导
		"type='qcow2'",
		"target dev='vda' bus='virtio'",
		"device='cdrom'",
		"org.qemu.guest_agent.0",
		"type='vnc' autoport='yes'",
		"<model type='vga'",
	}
	for _, want := range checks {
		if !strings.Contains(xml, want) {
			t.Errorf("domain XML 缺少 %q\n%s", want, xml)
		}
	}
}

func TestQEMUInterfaceXMLModes(t *testing.T) {
	nat := qemuInterfaceXML(protocol.NetworkConfig{Mode: NetworkModeNat, MAC: "52:54:00:00:00:01"})
	if !strings.Contains(nat, "type='network'") || !strings.Contains(nat, "network='default'") {
		t.Errorf("NAT 网卡应挂 default 网络: %s", nat)
	}
	none := qemuInterfaceXML(protocol.NetworkConfig{Mode: NetworkModeNone})
	if none != "" {
		t.Errorf("关闭模式不应生成网卡: %s", none)
	}
	// bandwidth 限制按 Mbps → Kbps 换算。
	bw := qemuInterfaceXML(protocol.NetworkConfig{Mode: NetworkModeNat, BandwidthMbps: 50})
	if !strings.Contains(bw, "average='50000'") {
		t.Errorf("带宽应换算为 Kbps: %s", bw)
	}
}

func TestDedicatedTargetRejectsUnknownInterface(t *testing.T) {
	_, _, err := dedicatedTarget(protocol.NetworkConfig{Mode: NetworkModeDedicated, Bridge: "no-such-if-01"})
	if err == nil {
		t.Fatal("不存在的网卡应报错")
	}
}

func TestCollectHostNetworkExcludesLoopback(t *testing.T) {
	summary := CollectHostNetwork()
	for _, iface := range summary.Interfaces {
		if iface.Name == "lo" {
			t.Error("lo 不应出现在网卡清单里")
		}
		if iface.Kind == "" {
			t.Errorf("网卡 %s 缺少类型标注", iface.Name)
		}
	}
}
