package driver

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

func TestProfileRootDeviceArgs(t *testing.T) {
	cases := []struct {
		name   string
		exists bool
		diskGB int
		pool   string
		want   []string
	}{
		{
			name:   "missing root is added with size",
			diskGB: 20,
			pool:   "default",
			want:   []string{"profile", "device", "add", "p-1", "root", "disk", "path=/", "pool=default", "size=20GiB"},
		},
		{
			name:   "missing root uses custom pool",
			diskGB: 20,
			pool:   "s1",
			want:   []string{"profile", "device", "add", "p-1", "root", "disk", "path=/", "pool=s1", "size=20GiB"},
		},
		{
			name:   "existing root is updated",
			exists: true,
			diskGB: 20,
			pool:   "s1",
			want:   []string{"profile", "device", "set", "p-1", "root", "size=20GiB"},
		},
		{
			name:   "existing root without quota is unchanged",
			exists: true,
			pool:   "s1",
			want:   nil,
		},
		{
			name: "missing root without quota is still added",
			pool: "default",
			want: []string{"profile", "device", "add", "p-1", "root", "disk", "path=/", "pool=default"},
		},
		{
			name: "empty pool falls back to default",
			want: []string{"profile", "device", "add", "p-1", "root", "disk", "path=/", "pool=default"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := profileRootDeviceArgs("p-1", tc.exists, tc.diskGB, tc.pool); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("profileRootDeviceArgs() = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestIncusEth0DeviceArgsUsesManagedNetwork(t *testing.T) {
	got := incusEth0DeviceArgs(protocol.NetworkConfig{IPv4: "10.10.10.147", MAC: "52:54:00:30:00:00", BandwidthMbps: 100}, "incusbr0")
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "network=incusbr0") {
		t.Fatalf("托管网络应使用 network=incusbr0，得到 %#v", got)
	}
	if strings.Contains(joined, "nictype=bridged") || strings.Contains(joined, "parent=") {
		t.Fatalf("托管网络不应再带 nictype/parent，得到 %#v", got)
	}
	if !strings.Contains(joined, "ipv4.address=10.10.10.147") {
		t.Fatalf("托管网络应保留静态地址，得到 %#v", got)
	}
}

func TestIncusEth0UnmanagedArgsOmitsIPv4(t *testing.T) {
	got := incusEth0UnmanagedArgs(protocol.NetworkConfig{IPv4: "192.168.1.10", MAC: "52:54:00:00:00:01", BandwidthMbps: 50}, "net0")
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "nictype=bridged") || !strings.Contains(joined, "parent=net0") {
		t.Fatalf("非托管桥应使用 nictype/parent，得到 %#v", got)
	}
	if strings.Contains(joined, "ipv4.address") {
		t.Fatalf("非托管桥不能带 ipv4.address，得到 %#v", got)
	}
}

func TestParseStoragePoolTable(t *testing.T) {
	table := "+-----------+--------+--+\n| NAME | DRIVER |\n+-----------+--------+--+\n| s1 | dir |\n| incusbr01 | dir |\n+-----------+--------+--+"
	if got := parseStoragePoolTable(table); got != "s1" {
		t.Fatalf("parseStoragePoolTable() = %q, want %q", got, "s1")
	}
	table2 := "+------+--------+\n| NAME | DRIVER |\n+------+--------+\n| default | dir |\n| s1 | dir |\n+------+--------+"
	if got := parseStoragePoolTable(table2); got != "default" {
		t.Fatalf("parseStoragePoolTable() = %q, want default", got)
	}
}

func TestProfileHasDevice(t *testing.T) {
	cases := []struct {
		name string
		data string
		want bool
		err  bool
	}{
		{name: "device exists", data: `{"devices":{"root":{"type":"disk"},"eth0":{"type":"nic"}}}`, want: true},
		{name: "device missing", data: `{"devices":{"root":{"type":"disk"}}}`, want: false},
		{name: "empty profile", data: `{"name":"p-1","devices":{}}`, want: false},
		{name: "missing devices field", data: `{"name":"p-1"}`, want: false},
		{name: "metadata envelope", data: `{"metadata":{"devices":{"eth0":{"type":"nic"}}}}`, want: true},
		{name: "metadata envelope missing device", data: `{"metadata":{"devices":{"root":{"type":"disk"}}}}`, want: false},
		{name: "malformed profile", data: `{`, err: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := profileHasDevice([]byte(tc.data), "eth0")
			if (err != nil) != tc.err {
				t.Fatalf("profileHasDevice() error = %v, want error %v", err, tc.err)
			}
			if err == nil && got != tc.want {
				t.Fatalf("profileHasDevice() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestProfileDeviceExistsPropagatesCommandErrors(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "incus")
	script := "#!/bin/sh\nprintf 'profile unavailable' >&2\nexit 1\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	oldPath := os.Getenv("PATH")
	if err := os.Setenv("PATH", dir+string(os.PathListSeparator)+oldPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Setenv("PATH", oldPath) })

	_, err := profileDeviceExists(context.Background(), "incus", "p-1", "root")
	if err == nil || !strings.Contains(err.Error(), "profile unavailable") {
		t.Fatalf("profileDeviceExists() error = %v, want command error", err)
	}
}

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

func TestDomainXMLDefaultsToNATWhenModeEmpty(t *testing.T) {
	// 创建时未配置网络模式：按 NAT 处理，网卡挂 libvirt default 网络，
	// 由被控自动定义并拉起该网络（共享主机出口 IP）。
	inst := &protocol.Instance{
		ID:   9,
		Name: "bare",
		Type: "vm",
		Spec: protocol.InstanceSpec{CPU: 1, MemoryMB: 512, DiskGB: 10},
	}
	xml := domainXML(resourceName("qemu", inst), inst, "/data/images/disk.qcow2", "")
	if !strings.Contains(xml, "<interface type='network'>") || !strings.Contains(xml, "network='default'") {
		t.Errorf("空网络模式应默认 NAT 并挂 default 网络:\n%s", xml)
	}
}

func TestUnquoteSpecStripsSaveQuotes(t *testing.T) {
	spec := []string{"-m", "comment", "--comment", `"virtualis:36"`, "--dport", "20017"}
	got := unquoteSpec(spec)
	want := []string{"-m", "comment", "--comment", "virtualis:36", "--dport", "20017"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("token %d: got %q, want %q", i, got[i], want[i])
		}
	}
	// 没有引号的 token 原样保留。
	plain := unquoteSpec([]string{"-p", "tcp"})
	if plain[0] != "-p" || plain[1] != "tcp" {
		t.Fatalf("plain spec 被误改: %v", plain)
	}
}

func TestIncusEth0DeviceArgsIncludesLimits(t *testing.T) {
	// NAT + 限速：profile 设备必须带 limits.ingress/egress，否则 Incus 限速不生效。
	got := strings.Join(incusEth0DeviceArgs(protocol.NetworkConfig{
		Mode: NetworkModeNat, IPv4: "10.10.10.130", BandwidthMbps: 50,
	}, "incusbr0"), ",")
	for _, want := range []string{"nictype=bridged", "parent=incusbr0", "ipv4.address=10.10.10.130", "limits.ingress=50Mbit", "limits.egress=50Mbit"} {
		if !strings.Contains(got, want) {
			t.Errorf("NAT 网卡参数缺少 %q: %s", want, got)
		}
	}
	// 不限速时不写 limits（0 即不限，与 omitempty 语义一致）。
	plain := strings.Join(incusEth0DeviceArgs(protocol.NetworkConfig{
		Mode: NetworkModeNat, IPv4: "10.10.10.131",
	}, "incusbr0"), ",")
	if strings.Contains(plain, "limits.") {
		t.Errorf("不限速时不应写 limits: %s", plain)
	}
	// CIDR 后缀要剥掉，MAC 要透传。
	withMAC := strings.Join(incusEth0DeviceArgs(protocol.NetworkConfig{
		Mode: NetworkModeDedicated, IPv4: "192.0.2.10/24", MAC: "52:54:00:00:00:09", BandwidthMbps: 100,
	}, "br0"), ",")
	for _, want := range []string{"parent=br0", "ipv4.address=192.0.2.10", "hwaddr=52:54:00:00:00:09", "limits.ingress=100Mbit"} {
		if !strings.Contains(withMAC, want) {
			t.Errorf("独立 IP 网卡参数缺少 %q: %s", want, withMAC)
		}
	}
}

func TestIncusDeviceArgsNatIncludesLimits(t *testing.T) {
	// 无调用者的旧分支同步修复：NAT 有保留地址 + 限速时 -d 参数也要带 limits。
	args := incusDeviceArgs(protocol.NetworkConfig{
		Mode: NetworkModeNat, IPv4: "10.10.10.132", BandwidthMbps: 20,
	}, &protocol.Instance{})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "limits.ingress=20Mbit") || !strings.Contains(joined, "limits.egress=20Mbit") {
		t.Errorf("NAT -d 参数缺少限速: %v", args)
	}
}
