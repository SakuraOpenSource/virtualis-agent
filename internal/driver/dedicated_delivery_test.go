package driver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

const dedicatedHostFixture = `[{"ifname":"eth0","flags":["UP"],"operstate":"UP","addr_info":[{"family":"inet","local":"192.0.2.10","prefixlen":24}]},{"ifname":"br-public","flags":["UP"],"operstate":"UP","linkinfo":{"info_kind":"bridge"},"addr_info":[]}]`

func dedicatedRunner(t *testing.T, calls *[]string) CommandRunner {
	return func(_ context.Context, name string, a ...string) ([]byte, error) {
		*calls = append(*calls, name+" "+strings.Join(a, " "))
		if name == "ip" && strings.Join(a, " ") == "-j -d address show" {
			return []byte(dedicatedHostFixture), nil
		}
		if name == "ip" && len(a) > 2 && a[0] == "-j" && a[1] == "link" {
			return []byte("[]"), nil
		}
		if name == "ip" && len(a) > 2 && a[0] == "-j" && a[1] == "route" {
			return []byte("[]"), nil
		}
		if name == "sysctl" {
			return []byte("1\n"), nil
		}
		if name == "iptables-restore" {
			b, e := os.ReadFile(a[len(a)-1])
			*calls = append(*calls, string(b))
			return nil, e
		}
		return nil, nil
	}
}

func TestQEMUPhysicalUplinkBuildsOwnedRoutedNetwork(t *testing.T) {
	calls := []string{}
	ctx := WithCommandRunner(context.Background(), dedicatedRunner(t, &calls))
	inst := &protocol.Instance{ID: 8, Name: "routed", Network: protocol.NetworkConfig{Mode: "dedicated", Bridge: "eth0", IPv4: "192.0.2.20/24", Gateway: "192.0.2.1"}}
	before, _ := json.Marshal(inst.Network)
	if e := NewQEMUWithDataDir(t.TempDir()).ensureNetwork(ctx, inst); e != nil {
		t.Fatal(e)
	}
	after, _ := json.Marshal(inst.Network)
	if string(before) != string(after) {
		t.Fatal("rewrote desired pool CIDR", inst.Network)
	}
	joined := strings.Join(calls, "\n")
	for _, want := range []string{"ip link add", "type bridge", "169.254.0.1/32", "ip route replace 192.0.2.20/32", "ip neigh replace proxy 192.0.2.20 dev eth0"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %s in %s", want, joined)
		}
	}
	if strings.Contains(joined, "master eth0") || strings.Contains(joined, "addr flush") || strings.Contains(joined, "sysctl -w net.ipv4.conf.eth0") {
		t.Fatal("modified primary host interface", joined)
	}
}

func TestQEMUDedicatedRejectsHostIPBeforeWriting(t *testing.T) {
	calls := []string{}
	ctx := WithCommandRunner(context.Background(), dedicatedRunner(t, &calls))
	inst := &protocol.Instance{ID: 8, Network: protocol.NetworkConfig{Mode: "dedicated", Bridge: "eth0", IPv4: "192.0.2.10/24"}}
	if e := NewQEMUWithDataDir(t.TempDir()).ensureNetwork(ctx, inst); e == nil || !strings.Contains(e.Error(), "宿主机") {
		t.Fatalf("host IP collision not rejected: %v", e)
	}
	for _, c := range calls {
		if strings.Contains(c, "replace") || strings.Contains(c, "link add") {
			t.Fatal("collision mutated network", calls)
		}
	}
}

func TestQEMUCreateDedicatedAttachesOwnedBridgeAndPersistentStaticGuest(t *testing.T) {
	calls := []string{}
	baseRunner := dedicatedRunner(t, &calls)
	xml, script := "", ""
	ctx := WithCommandRunner(context.Background(), func(ctx context.Context, name string, a ...string) ([]byte, error) {
		if name == "virsh" && a[0] == "dominfo" {
			return nil, errors.New("Domain not found")
		}
		if name == "virsh" && a[0] == "define" {
			b, e := os.ReadFile(a[1])
			xml = string(b)
			return nil, e
		}
		if name == "virt-customize" {
			for n, arg := range a {
				if arg == "--upload" {
					p := strings.SplitN(a[n+1], ":", 2)
					b, e := os.ReadFile(p[0])
					if e != nil {
						return nil, e
					}
					script += string(b)
				}
			}
		}
		return baseRunner(ctx, name, a...)
	})
	// AGT-F4: Create must only attach disks under the agent-managed images
	// directory; the fixture therefore stages the disk inside dataDir/images.
	dataDir := t.TempDir()
	disk := dataDir + "/images/disk.qcow2"
	if e := os.MkdirAll(dataDir+"/images", 0755); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(disk, []byte("offline-image-fixture"), 0600); e != nil {
		t.Fatal(e)
	}
	inst := &protocol.Instance{ID: 8, Name: "routed", Network: protocol.NetworkConfig{Mode: "dedicated", Bridge: "eth0", IPv4: "192.0.2.20/24", Gateway: "192.0.2.1"}, Image: &protocol.Image{Path: disk, Type: "disk"}}
	if e := NewQEMUWithDataDir(dataDir).Create(ctx, inst); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(xml, "source bridge='vb8'") || strings.Contains(xml, "type='direct'") || strings.Contains(xml, "network='default'") {
		t.Fatal("physical uplink is not routed via owned bridge", xml)
	}
	for _, want := range []string{"192.0.2.20/32", "169.254.0.1", "onlink", "WantedBy=multi-user.target", "network: {config: disabled}"} {
		if !strings.Contains(script, want) {
			t.Errorf("missing persistent guest %s", want)
		}
	}
	if strings.Contains(script, "dhclient -1") || strings.Contains(script, "udhcpc -i") {
		t.Fatal("dedicated guest still DHCP", script)
	}
}
