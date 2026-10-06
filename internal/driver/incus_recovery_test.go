package driver

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func incusBackup(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "backup.tar.gz")
	f, _ := os.Create(p)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for n, b := range map[string]string{"backup/index.yaml": "name: source\ntype: container\noptimized: false\nconfig:\n  container:\n    architecture: x86_64\n", "backup/container/rootfs/etc/marker": "os-preserved"} {
		tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeReg, Size: int64(len(b))})
		tw.Write([]byte(b))
	}
	tw.Close()
	gz.Close()
	f.Close()
	return p
}
func TestIncusImportUsesExistingTargetPoolWithoutLeakingDedicatedPool(t *testing.T) {
	bin := t.TempDir()
	exe := "mkfs.btrfs"
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	os.WriteFile(filepath.Join(bin, exe), nil, 0755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	d := NewIncusWithDataDir(t.TempDir())
	pool := ""
	dedicated := false
	ctx := WithCommandRunner(context.Background(), func(_ context.Context, _ string, a ...string) ([]byte, error) {
		switch {
		case a[0] == "query" && (a[1] == "/1.0/instances" || a[1] == "/1.0/profiles"):
			return []byte(`[]`), nil
		case a[0] == "query" && strings.HasPrefix(a[1], "/1.0/profiles/"):
			if pool == "" {
				return []byte(`{"devices":{}}`), nil
			}
			return []byte(`{"devices":{"root":{"type":"disk","path":"/","pool":"` + pool + `"}}}`), nil
		case a[0] == "storage" && a[1] == "list":
			return []byte("pool,btrfs"), nil
		case a[0] == "storage" && a[1] == "info":
			return nil, errors.New("not found")
		case a[0] == "storage" && a[1] == "create":
			dedicated = true
			return []byte(""), nil
		case a[0] == "profile" && len(a) > 5 && a[2] == "add":
			for _, v := range a {
				if strings.HasPrefix(v, "pool=") {
					pool = strings.TrimPrefix(v, "pool=")
				}
			}
			return []byte(""), nil
		case a[0] == "query" && strings.HasPrefix(a[1], "/1.0/instances/") && len(a) > 2:
			return nil, errors.New("configuration failure")
		case a[0] == "query":
			return []byte(`{"architecture":"x86_64","config":{}}`), nil
		}
		return []byte(""), nil
	})
	i := &protocol.Instance{ID: 1, Name: "test", Type: "container", Spec: protocol.InstanceSpec{CPU: 1, MemoryMB: 512, DiskGB: 20}, Network: protocol.NetworkConfig{Mode: "none"}}
	if e := d.Import(ctx, i, incusBackup(t)); e == nil {
		t.Fatal("expected config failure")
	}
	if dedicated || pool != "pool" {
		t.Fatal("import created/leaked dedicated storage rather than using owned target pool", dedicated, pool)
	}
}

func TestIncusImportOwnsRootNICAndProfilesWithValidCommands(t *testing.T) {
	d := NewIncusWithDataDir(t.TempDir())
	var put map[string]any
	target := "virtualis-1-test"
	calls := []string{}
	ctx := WithCommandRunner(context.Background(), func(_ context.Context, n string, a ...string) ([]byte, error) {
		calls = append(calls, strings.Join(a, " "))
		switch {
		case len(a) > 1 && a[0] == "query" && a[1] == "/1.0/instances":
			return []byte(`[]`), nil
		case len(a) > 1 && a[0] == "query" && a[1] == "/1.0/profiles":
			return []byte(`[]`), nil
		case a[0] == "query" && strings.HasPrefix(a[1], "/1.0/profiles/"):
			return []byte(`{"devices":{"root":{"type":"disk","path":"/","pool":"target-pool"}}}`), nil
		case a[0] == "query" && strings.HasPrefix(a[1], "/1.0/instances/"):
			if len(a) > 2 {
				for i, v := range a {
					if v == "--data" || v == "-d" {
						json.Unmarshal([]byte(a[i+1]), &put)
					}
				}
				return []byte(`{}`), nil
			}
			return []byte(`{"architecture":"x86_64","config":{"volatile.eth0.hwaddr":"source-mac","limits.cpu":"99"},"devices":{"root":{"type":"disk","path":"/","pool":"source-pool"},"ens3":{"type":"nic","network":"source-vpc"}},"profiles":["foreign"]}`), nil
		case a[0] == "profile" && a[1] == "device" && a[2] == "get":
			return []byte("target-pool"), nil
		case a[0] == "storage" && a[1] == "show":
			return []byte("driver: btrfs\n"), nil
		case a[0] == "list":
			return []byte(target + ",STOPPED\n"), nil
		}
		return []byte(""), nil
	})
	i := &protocol.Instance{ID: 1, Name: "test", Driver: "incus", Type: "container", Spec: protocol.InstanceSpec{CPU: 2, MemoryMB: 512, DiskGB: 20, Arch: "x86_64"}, Network: protocol.NetworkConfig{Mode: "none"}}
	if err := d.Import(ctx, i, incusBackup(t)); err != nil {
		t.Fatal(err)
	}
	if put == nil {
		t.Fatalf("import never replaced source-owned configuration: %v", calls)
	}
	if devices, ok := put["devices"].(map[string]any); !ok || len(devices) != 0 {
		t.Fatalf("local source root/NIC survived: %v", put)
	}
	if config := put["config"].(map[string]any); config["limits.cpu"] != "2" || config["limits.memory"] != "512MiB" || config["volatile.eth0.hwaddr"] != nil {
		t.Fatalf("source limits/MAC survived: %v", config)
	}
	for _, c := range calls {
		if strings.Contains(c, "config show") || strings.Contains(c, "chpasswd") {
			t.Fatalf("invalid query/password reset: %v", calls)
		}
	}
}
func TestIncusImportRefusesExistingTargetBeforeProfileMutation(t *testing.T) {
	d := NewIncusWithDataDir(t.TempDir())
	calls := []string{}
	ctx := WithCommandRunner(context.Background(), func(_ context.Context, _ string, a ...string) ([]byte, error) {
		calls = append(calls, strings.Join(a, " "))
		if a[0] == "query" && a[1] == "/1.0/instances" {
			return []byte(`["/1.0/instances/virtualis-1-test"]`), nil
		}
		return []byte("{}"), nil
	})
	if err := d.Import(ctx, &protocol.Instance{ID: 1, Name: "test"}, incusBackup(t)); err == nil {
		t.Fatal("existing target accepted")
	}
	for _, c := range calls {
		if !strings.HasPrefix(c, "query") {
			t.Fatalf("existing instance/profile touched: %v", calls)
		}
	}
}
func TestIncusImportCompensatesOnlyItsStageAfterConfigFailure(t *testing.T) {
	d := NewIncusWithDataDir(t.TempDir())
	deleted := []string{}
	ctx := WithCommandRunner(context.Background(), func(_ context.Context, _ string, a ...string) ([]byte, error) {
		if a[0] == "query" && len(a) > 2 && strings.Contains(strings.Join(a, " "), "PUT") {
			return nil, errors.New("config failure")
		}
		if a[0] == "delete" {
			deleted = append(deleted, a[1])
			return []byte(""), nil
		}
		switch {
		case a[0] == "query" && (a[1] == "/1.0/instances" || a[1] == "/1.0/profiles"):
			return []byte(`[]`), nil
		case a[0] == "query":
			return []byte(`{"devices":{"root":{"pool":"pool"}},"config":{}}`), nil
		case a[0] == "profile" && len(a) > 2 && a[2] == "get":
			return []byte("pool"), nil
		case a[0] == "storage" && a[1] == "show":
			return []byte("driver: btrfs"), nil
		}
		return []byte(""), nil
	})
	i := &protocol.Instance{ID: 1, Name: "test", Type: "container", Spec: protocol.InstanceSpec{CPU: 1, MemoryMB: 512, DiskGB: 20}, Network: protocol.NetworkConfig{Mode: "none"}}
	if err := d.Import(ctx, i, incusBackup(t)); err == nil {
		t.Fatal("expected failure")
	}
	if len(deleted) != 1 || deleted[0] == "virtualis-1-test" {
		t.Fatalf("staging cleanup not isolated: %v", deleted)
	}
}
