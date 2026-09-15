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

func TestParseStoragePoolsCSV(t *testing.T) {
	out := "\"NAME\",\"DRIVER\"\n\"default\",\"dir\"\n\"fast\",\"btrfs\"\n"
	pools := parseStoragePoolsCSV(out)
	if len(pools) != 2 {
		t.Fatalf("parseStoragePoolsCSV() 数量 = %d, want 2", len(pools))
	}
	if pools[0].name != "default" || pools[0].driver != "dir" {
		t.Fatalf("parseStoragePoolsCSV()[0] = %+v, want {default dir}", pools[0])
	}
	if pools[1].name != "fast" || pools[1].driver != "btrfs" {
		t.Fatalf("parseStoragePoolsCSV()[1] = %+v, want {fast btrfs}", pools[1])
	}
	// 无引号、大小写混杂也应归一化。
	plain := parseStoragePoolsCSV("NAME,DRIVER\ns1,ZFS\n")
	if len(plain) != 1 || plain[0].name != "s1" || plain[0].driver != "zfs" {
		t.Fatalf("parseStoragePoolsCSV() 大小写归一化失败: %+v", plain)
	}
}

func TestParseStoragePoolTable(t *testing.T) {
	// 表格携带 DRIVER 列：名称与驱动都要提取出来。
	table := "+-----------+--------+\n| NAME | DRIVER |\n+-----------+--------+\n| s1 | dir |\n| fast | btrfs |\n+-----------+--------+\n"
	pools := parseStoragePoolTable(table)
	if len(pools) != 2 {
		t.Fatalf("parseStoragePoolTable() 数量 = %d, want 2", len(pools))
	}
	if pools[0].name != "s1" || pools[0].driver != "dir" {
		t.Fatalf("parseStoragePoolTable()[0] = %+v, want {s1 dir}", pools[0])
	}
	if pools[1].name != "fast" || pools[1].driver != "btrfs" {
		t.Fatalf("parseStoragePoolTable()[1] = %+v, want {fast btrfs}", pools[1])
	}
	// 无 DRIVER 列的老表格：驱动留空，由上层 storage show 补查。
	legacy := "+---------+\n| NAME |\n+---------+\n| s1 |\n+---------+"
	legacyPools := parseStoragePoolTable(legacy)
	if len(legacyPools) != 1 || legacyPools[0].name != "s1" || legacyPools[0].driver != "" {
		t.Fatalf("parseStoragePoolTable() 老表格解析失败: %+v", legacyPools)
	}
	// 旧行为由 selectStoragePool 保持：无配额需求时优先 default，否则第一个。
	withDefault := []storagePool{{name: "default", driver: "dir"}, {name: "s1", driver: "dir"}}
	if sel, err := selectStoragePool(withDefault, 0); err != nil || sel != "default" {
		t.Fatalf("selectStoragePool(无配额) = %q, %v; want default, nil", sel, err)
	}
}

func TestStorageDriverSupportsQuota(t *testing.T) {
	for _, d := range []string{"btrfs", "zfs", "lvm", "lvmcluster", "ceph", "ZFS", " Btrfs "} {
		if !storageDriverSupportsQuota(d) {
			t.Errorf("storageDriverSupportsQuota(%q) = false, want true", d)
		}
	}
	for _, d := range []string{"dir", "", "cephfs", "unknown"} {
		if storageDriverSupportsQuota(d) {
			t.Errorf("storageDriverSupportsQuota(%q) = true, want false", d)
		}
	}
}

func TestParseStorageDriver(t *testing.T) {
	yaml := "name: fast\ndriver: btrfs\ndescription: \"\"\nconfig:\n  size: 10GiB\n"
	if got := parseStorageDriver(yaml); got != "btrfs" {
		t.Fatalf("parseStorageDriver() = %q, want btrfs", got)
	}
	quoted := "name: s1\ndriver: \"dir\" # 行尾注释\n"
	if got := parseStorageDriver(quoted); got != "dir" {
		t.Fatalf("parseStorageDriver() = %q, want dir", got)
	}
	if got := parseStorageDriver("name: s1\nconfig: {}\n"); got != "" {
		t.Fatalf("parseStorageDriver() 无 driver 字段应返回空，得到 %q", got)
	}
}

func TestSelectStoragePool(t *testing.T) {
	quota := []storagePool{{name: "default", driver: "dir"}, {name: "fast", driver: "btrfs"}}
	// 需要配额：配额型池优先于 dir 的 default。
	if sel, err := selectStoragePool(quota, 20); err != nil || sel != "fast" {
		t.Fatalf("selectStoragePool(配额) = %q, %v; want fast, nil", sel, err)
	}
	// 需要配额且 default 本身就是配额型：仍优先 default。
	quotaDefault := []storagePool{{name: "default", driver: "zfs"}, {name: "fast", driver: "btrfs"}}
	if sel, err := selectStoragePool(quotaDefault, 20); err != nil || sel != "default" {
		t.Fatalf("selectStoragePool(配额+default) = %q, %v; want default, nil", sel, err)
	}
	// 只有 dir 池却要配额：指名报错，绝不静默成功。
	dirOnly := []storagePool{{name: "default", driver: "dir"}, {name: "s1", driver: "dir"}}
	if sel, err := selectStoragePool(dirOnly, 20); err == nil {
		t.Fatalf("selectStoragePool(dir+配额) = %q, want error", sel)
	} else if msg := err.Error(); !strings.Contains(msg, "default") || !strings.Contains(msg, "dir") || !strings.Contains(msg, "不支持磁盘配额") {
		t.Fatalf("selectStoragePool(dir+配额) 报错缺少指名池/原因: %q", msg)
	}
	// 同上：无 default 时指名首个池。
	dirNoDefault := []storagePool{{name: "s1", driver: "dir"}}
	if _, err := selectStoragePool(dirNoDefault, 1); err == nil || !strings.Contains(err.Error(), "s1") {
		t.Fatalf("selectStoragePool(s1 dir+配额) 应指名 s1 报错，得到 %v", err)
	}
	// 驱动未知 + 配额：同样报错（fail-closed）。
	if _, err := selectStoragePool([]storagePool{{name: "s1"}}, 10); err == nil || !strings.Contains(err.Error(), "驱动未知") {
		t.Fatalf("selectStoragePool(未知驱动+配额) 应报错，得到 %v", err)
	}
	// 无配额需求：保持旧行为（优先 default，否则第一个），dir 也可用。
	if sel, err := selectStoragePool(dirOnly, 0); err != nil || sel != "default" {
		t.Fatalf("selectStoragePool(无配额) = %q, %v; want default, nil", sel, err)
	}
	if sel, err := selectStoragePool(dirNoDefault, 0); err != nil || sel != "s1" {
		t.Fatalf("selectStoragePool(无配额s1) = %q, %v; want s1, nil", sel, err)
	}
	if _, err := selectStoragePool(nil, 20); err == nil {
		t.Fatal("selectStoragePool(空列表) 应报错")
	}
}

// writeFakeIncus 在 PATH 首位放置 fake incus，沿用本包已有的 fake CLI 模式。
func writeFakeIncus(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	fake := filepath.Join(dir, "incus")
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	oldPath := os.Getenv("PATH")
	if err := os.Setenv("PATH", dir+string(os.PathListSeparator)+oldPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Setenv("PATH", oldPath) })
}

func TestListStoragePoolsPrefersQuotaCapable(t *testing.T) {
	writeFakeIncus(t, "#!/bin/sh\n"+
		"if [ \"$1\" = \"storage\" ] && [ \"$2\" = \"list\" ]; then\n"+
		"  printf '\"NAME\",\"DRIVER\"\\n\"default\",\"dir\"\\n\"fast\",\"btrfs\"\\n'\n"+
		"  exit 0\n"+
		"fi\n"+
		"exit 1\n")
	d := NewIncus()
	pools := d.listStoragePools(context.Background())
	if len(pools) != 2 || pools[0].driver != "dir" || pools[1].driver != "btrfs" {
		t.Fatalf("listStoragePools() = %+v, want [{default dir} {fast btrfs}]", pools)
	}
	if sel, err := selectStoragePool(pools, 20); err != nil || sel != "fast" {
		t.Fatalf("配额应选中 fast，得到 %q, %v", sel, err)
	}
}

func TestListStoragePoolsDirOnlyQuotaFails(t *testing.T) {
	writeFakeIncus(t, "#!/bin/sh\n"+
		"if [ \"$1\" = \"storage\" ] && [ \"$2\" = \"list\" ]; then\n"+
		"  printf '\"NAME\",\"DRIVER\"\\n\"s1\",\"dir\"\\n'\n"+
		"  exit 0\n"+
		"fi\n"+
		"if [ \"$1\" = \"storage\" ] && [ \"$2\" = \"create\" ]; then\n"+
		"  echo 'unexpected storage create' >&2\n"+
		"  exit 1\n"+
		"fi\n"+
		"exit 1\n")
	d := NewIncus()
	ctx := context.Background()
	if _, err := d.ensureStoragePool(ctx, 20); err == nil || !strings.Contains(err.Error(), "不支持磁盘配额") {
		t.Fatalf("ensureStoragePool(dir+配额) 应明确报错，得到 %v", err)
	}
	// 无配额需求时 dir 池照常用，且不需要走到自动创建。
	if sel, err := d.ensureStoragePool(ctx, 0); err != nil || sel != "s1" {
		t.Fatalf("ensureStoragePool(dir+无配额) = %q, %v; want s1, nil", sel, err)
	}
}

func TestListStoragePoolsTableFallbackWithShow(t *testing.T) {
	// 老版本不支持 -c n,driver：csv 失败 → 表格（无 DRIVER 列）→ storage show 补驱动。
	writeFakeIncus(t, "#!/bin/sh\n"+
		"if [ \"$1\" = \"storage\" ] && [ \"$2\" = \"list\" ]; then\n"+
		"  for a in \"$@\"; do\n"+
		"    if [ \"$a\" = \"--format\" ]; then echo 'Error: unknown column' >&2; exit 1; fi\n"+
		"  done\n"+
		"  printf '+------+\\n| NAME |\\n+------+\\n| s1 |\\n+------+\\n'\n"+
		"  exit 0\n"+
		"fi\n"+
		"if [ \"$1\" = \"storage\" ] && [ \"$2\" = \"show\" ]; then\n"+
		"  printf 'name: s1\\ndriver: zfs\\nconfig: {}\\n'\n"+
		"  exit 0\n"+
		"fi\n"+
		"exit 1\n")
	pools := NewIncus().listStoragePools(context.Background())
	if len(pools) != 1 || pools[0].name != "s1" || pools[0].driver != "zfs" {
		t.Fatalf("表格回退+show 补驱动失败: %+v", pools)
	}
	if sel, err := selectStoragePool(pools, 10); err != nil || sel != "s1" {
		t.Fatalf("zfs 池应承载配额，得到 %q, %v", sel, err)
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
	// NAT 托管网络：用 network= 挂载并保留静态地址，限速与之一并下发。
	got := strings.Join(incusEth0DeviceArgs(protocol.NetworkConfig{
		Mode: NetworkModeNat, IPv4: "10.10.10.130", BandwidthMbps: 50,
	}, "incusbr0"), ",")
	for _, want := range []string{"network=incusbr0", "ipv4.address=10.10.10.130", "limits.ingress=50Mbit", "limits.egress=50Mbit"} {
		if !strings.Contains(got, want) {
			t.Errorf("NAT profile device 缺少 %q: %s", want, got)
		}
	}
	if strings.Contains(got, "nictype=bridged") || strings.Contains(got, "parent=") {
		t.Errorf("托管网络不应再带 nictype/parent: %s", got)
	}
	// 未限速时不写 limits，与 omitempty 行为一致。
	plain := strings.Join(incusEth0DeviceArgs(protocol.NetworkConfig{
		Mode: NetworkModeNat, IPv4: "10.10.10.131",
	}, "incusbr0"), ",")
	if strings.Contains(plain, "limits.") {
		t.Errorf("未限速时不应写 limits: %s", plain)
	}
	// CIDR 后缀要剥离，MAC 要透传。
	withMAC := strings.Join(incusEth0DeviceArgs(protocol.NetworkConfig{
		Mode: NetworkModeDedicated, IPv4: "192.0.2.10/24", MAC: "52:54:00:00:00:09", BandwidthMbps: 100,
	}, "br0"), ",")
	for _, want := range []string{"network=br0", "ipv4.address=192.0.2.10", "hwaddr=52:54:00:00:00:09", "limits.ingress=100Mbit"} {
		if !strings.Contains(withMAC, want) {
			t.Errorf("托管网络设备缺少 %q: %s", want, withMAC)
		}
	}
	// 非托管父桥走 incusEth0UnmanagedArgs：保留 nictype/parent，不带 ipv4.address。
	unmanaged := strings.Join(incusEth0UnmanagedArgs(protocol.NetworkConfig{
		Mode: NetworkModeDedicated, IPv4: "192.0.2.10/24", MAC: "52:54:00:00:00:09", BandwidthMbps: 100,
	}, "br0"), ",")
	for _, want := range []string{"nictype=bridged", "parent=br0", "hwaddr=52:54:00:00:00:09", "limits.ingress=100Mbit"} {
		if !strings.Contains(unmanaged, want) {
			t.Errorf("非托管网络设备缺少 %q: %s", want, unmanaged)
		}
	}
	if strings.Contains(unmanaged, "ipv4.address") {
		t.Errorf("非托管桥不能带 ipv4.address: %s", unmanaged)
	}
}

func TestIncusCPUArgs(t *testing.T) {
	whole := incusCPUArgs(&protocol.Instance{Spec: protocol.InstanceSpec{CPU: 2}})
	if len(whole) != 2 || whole[1] != "limits.cpu=2" {
		t.Fatalf("whole cores: %#v", whole)
	}
	frac := incusCPUArgs(&protocol.Instance{Spec: protocol.InstanceSpec{CPU: 1, CPUMilli: 400}})
	joined := strings.Join(frac, " ")
	if !strings.Contains(joined, "limits.cpu=1") || !strings.Contains(joined, "limits.cpu.allowance=40ms/100ms") {
		t.Fatalf("sub-core should pin 1 cpu + 40ms/100ms allowance: %#v", frac)
	}
	multi := incusCPUArgs(&protocol.Instance{Spec: protocol.InstanceSpec{CPUMilli: 2500}})
	joined = strings.Join(multi, " ")
	if !strings.Contains(joined, "limits.cpu=3") || !strings.Contains(joined, "limits.cpu.allowance=250ms/100ms") {
		t.Fatalf("2.5 cores should pin 3 cpus + 250ms/100ms: %#v", multi)
	}
	vm := incusCPUArgs(&protocol.Instance{Type: "vm", Spec: protocol.InstanceSpec{CPUMilli: 400}})
	joined = strings.Join(vm, " ")
	if joined != "-c limits.cpu=1" || strings.Contains(joined, "allowance") {
		t.Fatalf("vm should use ceil cores without allowance: %#v", vm)
	}
	wholeMilli := incusCPUArgs(&protocol.Instance{Spec: protocol.InstanceSpec{CPU: 2, CPUMilli: 2000}})
	if strings.Join(wholeMilli, " ") != "-c limits.cpu=2" {
		t.Fatalf("whole milli cores should not add allowance: %#v", wholeMilli)
	}
}
