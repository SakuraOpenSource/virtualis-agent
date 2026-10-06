package driver

import (
	"archive/tar"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func standaloneQCOW() []byte {
	b := make([]byte, 104)
	copy(b, []byte{'Q', 'F', 'I', 0xfb})
	binary.BigEndian.PutUint32(b[4:], 3)
	binary.BigEndian.PutUint64(b[24:], 20<<30)
	binary.BigEndian.PutUint32(b[100:], 104)
	return b
}
func qemuBackup(t *testing.T, withSnapshot bool) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "backup.tar")
	f, _ := os.Create(p)
	tw := tar.NewWriter(f)
	for _, name := range []string{"disk.qcow2", "snapshots/saved.qcow2"} {
		if name != "disk.qcow2" && !withSnapshot {
			continue
		}
		b := standaloneQCOW()
		tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Size: int64(len(b))})
		tw.Write(b)
	}
	tw.Close()
	f.Close()
	return p
}
func qemuTestContext(t *testing.T, hook func(context.Context, string, []string) ([]byte, error)) context.Context {
	t.Helper()
	return WithCommandRunner(context.Background(), func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if hook != nil {
			if b, e := hook(ctx, name, args); b != nil || e != nil {
				return b, e
			}
		}
		switch {
		case name == "virsh" && args[0] == "list":
			return []byte(""), nil
		case name == "virsh" && args[0] == "domstate":
			return []byte("shut off"), nil
		case name == "qemu-img" && args[0] == "info":
			b, _ := json.Marshal(map[string]any{"format": "qcow2", "virtual-size": int64(20 << 30)})
			return b, nil
		}
		return []byte(""), nil
	})
}
func TestQEMUImportCompensatesDefineFailure(t *testing.T) {
	d := NewQEMUWithDataDir(t.TempDir())
	ctx := qemuTestContext(t, func(_ context.Context, n string, a []string) ([]byte, error) {
		if n == "virsh" && a[0] == "define" {
			return nil, errors.New("define failed")
		}
		return nil, nil
	})
	inst := &protocol.Instance{ID: 1, Name: "test", Driver: "qemu", Type: "vm", Spec: protocol.InstanceSpec{CPU: 1, MemoryMB: 512, DiskGB: 20}, Network: protocol.NetworkConfig{Mode: "none"}}
	if err := d.Import(ctx, inst, qemuBackup(t, true)); err == nil {
		t.Fatal("expected define failure")
	}
	if paths, _ := filepath.Glob(filepath.Join(d.imagesDir(), "*.qcow2")); len(paths) != 0 {
		t.Fatalf("failed import leaked disks: %v", paths)
	}
	if _, err := os.Stat(d.snapshotDir(1)); !os.IsNotExist(err) {
		t.Fatal("failed import leaked snapshots")
	}
	if inst.Image != nil {
		t.Fatal("failed import mutated caller image metadata")
	}
}
func TestQEMUImportDoesNotTouchExistingTarget(t *testing.T) {
	calls := []string{}
	d := NewQEMUWithDataDir(t.TempDir())
	ctx := qemuTestContext(t, func(_ context.Context, n string, a []string) ([]byte, error) {
		calls = append(calls, n+" "+strings.Join(a, " "))
		if n == "virsh" && a[0] == "list" {
			return []byte("virtualis-1-test\n"), nil
		}
		return nil, nil
	})
	i := &protocol.Instance{ID: 1, Name: "test", Network: protocol.NetworkConfig{Mode: "none"}}
	if err := d.Import(ctx, i, qemuBackup(t, false)); err == nil {
		t.Fatal("existing target accepted")
	}
	for _, c := range calls {
		if strings.Contains(c, "define") || strings.Contains(c, "net-start") || strings.Contains(c, "convert") {
			t.Fatalf("existing target touched: %v", calls)
		}
	}
}
func TestQEMUValidateImportChecksEverySnapshot(t *testing.T) {
	d := NewQEMUWithDataDir(t.TempDir())
	ctx := qemuTestContext(t, func(_ context.Context, n string, a []string) ([]byte, error) {
		if n == "qemu-img" && a[0] == "check" && strings.Contains(strings.Join(a, " "), "saved.qcow2") {
			return nil, errors.New("snapshot corrupt")
		}
		return nil, nil
	})
	if err := d.ValidateImport(ctx, &protocol.Instance{Type: "vm", Spec: protocol.InstanceSpec{DiskGB: 20}}, qemuBackup(t, true)); err == nil {
		t.Fatal("corrupt snapshot accepted")
	}
}
