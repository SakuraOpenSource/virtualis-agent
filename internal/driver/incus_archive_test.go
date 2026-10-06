package driver

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
	"os"
	"path/filepath"
	"testing"
)

func incusArchiveFixture(t *testing.T, index string, entries ...*tar.Header) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "backup.gz")
	f, e := os.Create(p)
	if e != nil {
		t.Fatal(e)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	if e = tw.WriteHeader(&tar.Header{Name: "backup/index.yaml", Typeflag: tar.TypeReg, Size: int64(len(index))}); e != nil {
		t.Fatal(e)
	}
	tw.Write([]byte(index))
	for _, h := range entries {
		if e = tw.WriteHeader(h); e != nil {
			t.Fatal(e)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write(make([]byte, h.Size))
		}
	}
	tw.Close()
	gz.Close()
	f.Close()
	return p
}
func TestIncusArchiveRewritePreservesGuestBackupYAML(t *testing.T) {
	base := "name: source\ntype: container\noptimized: false\nconfig:\n  container:\n    architecture: x86_64\n"
	src := incusArchiveFixture(t, base, &tar.Header{Name: "backup/container/rootfs/home/backup.yaml", Typeflag: tar.TypeReg, Size: 1})
	inst := &protocol.Instance{Type: "container", Spec: protocol.InstanceSpec{CPU: 1, MemoryMB: 512, DiskGB: 20, Arch: "x86_64"}}
	if e := scanIncusArchive(context.Background(), inst, src, rewriteIncusMetadata(inst, "target-profile", "target-pool", map[string]any{"name": "target-profile"}), filepath.Join(t.TempDir(), "rewritten.gz")); e != nil {
		t.Fatal("guest file mistaken for control metadata", e)
	}
}

func TestIncusArchivePrevalidationRejectsUnsafeOrIncompatibleData(t *testing.T) {
	base := "name: source\ntype: container\noptimized: false\nconfig:\n  container:\n    architecture: x86_64\n"
	good := &tar.Header{Name: "backup/container/rootfs/etc/file", Typeflag: tar.TypeReg, Size: 1}
	d := NewIncusWithDataDir(t.TempDir())
	i := &protocol.Instance{Type: "container", Spec: protocol.InstanceSpec{Arch: "x86_64", DiskGB: 20}}
	if e := d.ValidateImport(context.Background(), i, incusArchiveFixture(t, base, good)); e != nil {
		t.Fatal(e)
	}
	for name, p := range map[string]string{
		"missing snapshot":  incusArchiveFixture(t, base+"snapshots: [saved]\n", good),
		"smaller disk":      incusArchiveFixture(t, base+"    expanded_devices:\n      root: {type: disk, path: /, pool: source, size: 40GiB}\n", good),
		"external hardlink": incusArchiveFixture(t, base, good, &tar.Header{Name: "backup/container/rootfs/etc/link", Typeflag: tar.TypeLink, Linkname: "backup/index.yaml"}),

		"traversal":         incusArchiveFixture(t, base, &tar.Header{Name: "../host", Typeflag: tar.TypeReg, Size: 1}),
		"missing payload":   incusArchiveFixture(t, base),
		"duplicate":         incusArchiveFixture(t, base, good, good),
		"optimized":         incusArchiveFixture(t, "name: source\ntype: container\noptimized: true\n", good),
		"wrong type":        incusArchiveFixture(t, "name: source\ntype: virtual-machine\noptimized: false\n", good),
		"wrong arch":        incusArchiveFixture(t, "name: source\ntype: container\noptimized: false\nconfig:\n  container:\n    architecture: aarch64\n", good),
		"dependent volumes": incusArchiveFixture(t, base+"  dependent_volumes:\n    - volume: {name: other}\n", good),
	} {
		t.Run(name, func(t *testing.T) {
			if e := d.ValidateImport(context.Background(), i, p); e == nil {
				t.Fatal("unsafe backup accepted")
			}
		})
	}
	p := incusArchiveFixture(t, base, good)
	b, _ := os.ReadFile(p)
	os.WriteFile(p, b[:len(b)-4], 0600)
	if e := d.ValidateImport(context.Background(), i, p); e == nil {
		t.Fatal("truncated gzip accepted")
	}
}
