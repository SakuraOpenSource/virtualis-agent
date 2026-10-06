package driver

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestQEMUResizeUsesOwnedDiskAndPreservesDefinition(t *testing.T) {
	d := NewQEMUWithDataDir(t.TempDir())
	disk := filepath.Join(t.TempDir(), "owned.qcow2")
	var defined string
	calls := []string{}
	ctx := WithCommandRunner(context.Background(), func(_ context.Context, n string, a ...string) ([]byte, error) {
		calls = append(calls, n+" "+strings.Join(a, " "))
		switch {
		case a[0] == "domstate":
			return []byte("shut off"), nil
		case a[0] == "dumpxml":
			return []byte(`<domain type="kvm"><name>virtualis-1-test</name><uuid>retained</uuid><memory unit="MiB">512</memory><currentMemory unit="MiB">512</currentMemory><vcpu>1</vcpu><os><type arch="x86_64">hvm</type><loader readonly="yes">custom-fw</loader></os><devices><disk device="disk"><source file="` + disk + `"/><target dev="vda"/></disk><interface type="network"><source network="vpc"/><mac address="52:54:00:11:22:33"/></interface><disk device="cdrom"><source file="bootstrap.iso"/></disk></devices></domain>`), nil
		case a[0] == "info":
			return []byte(`{"format":"qcow2","virtual-size":21474836480}`), nil
		case a[0] == "define":
			b, e := os.ReadFile(a[1])
			defined = string(b)
			return []byte(""), e
		}
		return []byte(""), nil
	})
	i := &protocol.Instance{ID: 1, Name: "test", Type: "vm", Spec: protocol.InstanceSpec{CPU: 1, MemoryMB: 512, DiskGB: 20, Arch: "x86_64"}, Network: protocol.NetworkConfig{Mode: "vpc", Bridge: "vpc"}}
	spec := protocol.InstanceSpec{CPU: 2, CPUMilli: 1500, MemoryMB: 1024, DiskGB: 30}
	network := i.Network
	network.BandwidthMbps = 100
	if e := d.Resize(ctx, i, spec, &network); e != nil {
		t.Fatal(e)
	}
	for _, want := range []string{"retained", "custom-fw", "bootstrap.iso", "52:54:00:11:22:33", "100000", "150000"} {
		if !strings.Contains(defined, want) {
			t.Fatalf("definition lost %s: %s", want, defined)
		}
	}
	if !strings.Contains(strings.Join(calls, "\n"), "qemu-img resize "+disk+" 30G") {
		t.Fatal("did not resize owned disk", calls)
	}
	if i.Spec.Arch != "x86_64" || i.Spec.DiskGB != 30 {
		t.Fatal("spec not updated", i.Spec)
	}
	for _, c := range calls {
		if strings.Contains(c, "chpasswd") || strings.Contains(c, "convert") || strings.Contains(c, "start") {
			t.Fatal("resize recreated/started guest", calls)
		}
	}
}
func TestResizeRejectsRunningShrinkingAndChangedArchitecture(t *testing.T) {
	for _, backend := range []string{"incus", "qemu"} {
		for _, reason := range []string{"running", "shrink", "arch", "status error"} {
			t.Run(backend+"/"+reason, func(t *testing.T) {
				mutations := []string{}
				ctx := WithCommandRunner(context.Background(), func(_ context.Context, n string, a ...string) ([]byte, error) {
					switch a[0] {
					case "domstate":
						if reason == "status error" {
							return nil, errors.New("offline")
						}
						if reason == "running" {
							return []byte("running"), nil
						}
						return []byte("shut off"), nil
					case "list":
						if reason == "status error" {
							return nil, errors.New("offline")
						}
						if reason == "running" {
							return []byte("virtualis-1-test,RUNNING"), nil
						}
						return []byte("virtualis-1-test,STOPPED"), nil
					case "query":
						return []byte(`{"architecture":"x86_64","config":{},"expanded_devices":{"root":{"type":"disk","path":"/","pool":"pool","size":"40GiB"}}}`), nil
					case "dumpxml":
						return []byte(`<domain><os><type arch="x86_64">hvm</type></os><devices><disk device="disk"><source file="actual"/><target dev="vda"/></disk></devices></domain>`), nil
					case "info":
						return []byte(`{"format":"qcow2","virtual-size":42949672960}`), nil
					}
					mutations = append(mutations, n+" "+strings.Join(a, " "))
					return []byte(""), nil
				})
				i := &protocol.Instance{ID: 1, Name: "test", Type: "vm", Spec: protocol.InstanceSpec{CPU: 1, MemoryMB: 512, DiskGB: 20, Arch: "x86_64"}}
				s := protocol.InstanceSpec{CPU: 2, MemoryMB: 1024, DiskGB: 40, Arch: "x86_64"}
				if reason == "shrink" {
					s.DiskGB = 30
				}
				if reason == "arch" {
					s.Arch = "aarch64"
				}
				var d ResizeDriver
				if backend == "qemu" {
					d = NewQEMUWithDataDir(t.TempDir())
				} else {
					d = NewIncusWithDataDir(t.TempDir())
				}
				if e := d.Resize(ctx, i, s, nil); e == nil {
					t.Fatal("unsafe resize accepted")
				}
				if len(mutations) > 0 {
					t.Fatal("unsafe resize mutated runtime", mutations)
				}
			})
		}
	}
}
func TestIncusResizeAppliesQuotaCPUAndMemoryWithoutRecreating(t *testing.T) {
	d := NewIncusWithDataDir(t.TempDir())
	var put map[string]any
	calls := []string{}
	ctx := WithCommandRunner(context.Background(), func(_ context.Context, _ string, a ...string) ([]byte, error) {
		calls = append(calls, strings.Join(a, " "))
		switch {
		case a[0] == "list":
			return []byte("virtualis-1-test,STOPPED"), nil
		case a[0] == "query" && len(a) == 2:
			return []byte(`{"architecture":"x86_64","config":{"user.marker":"keep"},"devices":{},"profiles":["virtualis-p-1"],"expanded_devices":{"root":{"type":"disk","path":"/","pool":"pool","size":"20GiB"},"eth0":{"type":"nic","network":"vpc"}}}`), nil
		case a[0] == "query":
			for j, v := range a {
				if v == "--data" {
					json.Unmarshal([]byte(a[j+1]), &put)
				}
			}
			return []byte(`{}`), nil
		case a[0] == "storage":
			return []byte("driver: btrfs"), nil
		}
		return []byte(""), nil
	})
	i := &protocol.Instance{ID: 1, Name: "test", Type: "container", Spec: protocol.InstanceSpec{CPU: 1, MemoryMB: 512, DiskGB: 20, Arch: "x86_64"}}
	if e := d.Resize(ctx, i, protocol.InstanceSpec{CPU: 2, CPUMilli: 1500, MemoryMB: 1024, DiskGB: 30}, nil); e != nil {
		t.Fatal(e)
	}
	if put == nil {
		t.Fatal("no update", calls)
	}
	config := put["config"].(map[string]any)
	if config["limits.cpu"] != "2" || config["limits.cpu.allowance"] != "150ms/100ms" || config["limits.memory"] != "1024MiB" || config["user.marker"] != "keep" {
		t.Fatal(config)
	}
	root := put["devices"].(map[string]any)["root"].(map[string]any)
	if root["size"] != "30GiB" || root["pool"] != "pool" {
		t.Fatal(root)
	}
	for _, c := range calls {
		if strings.Contains(c, "launch") || strings.Contains(c, "delete") || strings.Contains(c, "chpasswd") {
			t.Fatal("destructive resize", calls)
		}
	}
}
