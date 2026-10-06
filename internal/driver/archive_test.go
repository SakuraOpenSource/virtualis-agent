package driver

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func tarFixture(t *testing.T, headers []*tar.Header) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, h := range headers {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write(bytes.Repeat([]byte("x"), int(h.Size))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func TestQEMUArchiveRejectsAttacksAndIncompleteTrailer(t *testing.T) {
	good := tarFixture(t, []*tar.Header{{Name: "disk.qcow2", Typeflag: tar.TypeReg, Size: 4}})
	cases := map[string][]byte{
		"truncated footer":  good[:len(good)-512],
		"trailing payload":  append(append([]byte{}, good...), []byte("malicious")...),
		"traversal":         tarFixture(t, []*tar.Header{{Name: "../disk.qcow2", Typeflag: tar.TypeReg, Size: 4}}),
		"absolute":          tarFixture(t, []*tar.Header{{Name: "/disk.qcow2", Typeflag: tar.TypeReg, Size: 4}}),
		"windows separator": tarFixture(t, []*tar.Header{{Name: `snapshots\evil.qcow2`, Typeflag: tar.TypeReg, Size: 4}}),
		"symlink":           tarFixture(t, []*tar.Header{{Name: "disk.qcow2", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}}),
		"hardlink":          tarFixture(t, []*tar.Header{{Name: "disk.qcow2", Typeflag: tar.TypeLink, Linkname: "/etc/passwd"}}),
		"duplicate":         tarFixture(t, []*tar.Header{{Name: "disk.qcow2", Typeflag: tar.TypeReg, Size: 4}, {Name: "disk.qcow2", Typeflag: tar.TypeReg, Size: 4}}),
		"missing disk":      tarFixture(t, []*tar.Header{{Name: "snapshots/a.qcow2", Typeflag: tar.TypeReg, Size: 4}}),
		"empty disk":        tarFixture(t, []*tar.Header{{Name: "disk.qcow2", Typeflag: tar.TypeReg}}),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "archive.tar")
			os.WriteFile(p, data, 0600)
			if err := extractQEMUArchive(p, filepath.Join(dir, "out")); err == nil {
				t.Fatal("unsafe/corrupt archive accepted")
			}
		})
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "good.tar")
	os.WriteFile(p, good, 0600)
	if err := extractQEMUArchive(p, filepath.Join(dir, "out")); err != nil {
		t.Fatal(err)
	}
}
func TestQEMUArchiveRejectsOversizeBeforeWriting(t *testing.T) {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	if err := tw.WriteHeader(&tar.Header{Name: "disk.qcow2", Typeflag: tar.TypeReg, Size: (64 << 30) + 1}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "huge.tar")
	os.WriteFile(p, b.Bytes(), 0600)
	if err := extractQEMUArchive(p, filepath.Join(dir, "out")); err == nil {
		t.Fatal("oversize archive accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, "out", "disk.qcow2")); !os.IsNotExist(err) {
		t.Fatal("oversize entry wrote file")
	}
}
