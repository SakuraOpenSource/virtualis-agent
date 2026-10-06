package driver

import (
	"context"
	"fmt"
	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestQEMURestorePreservesActualDiskCapacityAndContent(t *testing.T) {
	d := NewQEMUWithDataDir(t.TempDir())
	os.MkdirAll(d.imagesDir(), 0755)
	disk := filepath.Join(d.imagesDir(), "actual.qcow2")
	os.WriteFile(disk, []byte("old disk"), 0600)
	snap, _ := d.snapshotPath(1, "saved")
	os.MkdirAll(filepath.Dir(snap), 0700)
	os.WriteFile(snap, standaloneQCOW(), 0600)
	resized := ""
	ctx := qemuTestContext(t, func(_ context.Context, n string, a []string) ([]byte, error) {
		if n == "virsh" && a[0] == "dumpxml" {
			return []byte(fmt.Sprintf(`<domain><devices><disk device="disk"><source file="%s"/><target dev="vda"/></disk></devices></domain>`, disk)), nil
		}
		if n == "qemu-img" && a[0] == "info" {
			size := int64(20 << 30)
			if a[len(a)-1] == disk {
				size = 40 << 30
			}
			return []byte(fmt.Sprintf(`{"format":"qcow2","virtual-size":%d}`, size)), nil
		}
		if n == "qemu-img" && a[0] == "convert" {
			os.WriteFile(a[len(a)-1], standaloneQCOW(), 0600)
			return []byte(""), nil
		}
		if n == "qemu-img" && a[0] == "resize" {
			resized = a[len(a)-1]
			return []byte(""), nil
		}
		return nil, nil
	})
	i := &protocol.Instance{ID: 1, Name: "test", Spec: protocol.InstanceSpec{DiskGB: 20}, Image: &protocol.Image{Path: "wrong-control-plane-path"}}
	if err := d.RestoreSnapshot(ctx, i, "saved"); err != nil {
		t.Fatal(err)
	}
	if resized != "40G" {
		t.Fatalf("restore must preserve actual 40G capacity, got resize %q", resized)
	}
	b, _ := os.ReadFile(disk)
	if !slices.Equal(b, standaloneQCOW()) {
		t.Fatal("system disk was not replaced by snapshot")
	}
}
func TestQEMUExportCreatesMissingImagesDirectory(t *testing.T) {
	d := NewQEMUWithDataDir(t.TempDir())
	disk := filepath.Join(d.dataDir, "actual.qcow2")
	os.WriteFile(disk, standaloneQCOW(), 0600)
	ctx := qemuTestContext(t, func(_ context.Context, n string, a []string) ([]byte, error) {
		if n == "virsh" && a[0] == "dumpxml" {
			return []byte(fmt.Sprintf(`<domain><devices><disk device="disk"><source file="%s"/><target dev="vda"/></disk></devices></domain>`, disk)), nil
		}
		if n == "qemu-img" && a[0] == "convert" {
			os.WriteFile(a[len(a)-1], standaloneQCOW(), 0600)
			return []byte(""), nil
		}
		return nil, nil
	})
	archive := filepath.Join(d.dataDir, "export.tar")
	if err := d.Export(ctx, &protocol.Instance{ID: 1}, archive); err != nil {
		t.Fatal(err)
	}
	if err := extractQEMUArchive(archive, filepath.Join(d.dataDir, "verify")); err != nil {
		t.Fatal(err)
	}
}
