package driver

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareDiskDirOpensTraversal(t *testing.T) {
	root := t.TempDir()
	// 模拟旧安装的 0700 目录。
	images := filepath.Join(root, "images")
	if err := os.MkdirAll(images, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(root, 0o700)

	PrepareDiskDir(root)

	for _, dir := range []string{root, images} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o755 {
			t.Errorf("目录 %s 权限应为 0755，实际 %o", dir, perm)
		}
	}
}

func TestPrepareDiskFileReadableWithoutQEMUUser(t *testing.T) {
	// 开发机/无 libvirt-qemu 的宿主机上退化路径：0666 可读写。
	if _, _, ok := qemuOwnership(); ok {
		t.Skip("宿主机存在 qemu 运行用户，跳过退化路径断言")
	}
	path := filepath.Join(t.TempDir(), "disk.qcow2")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	PrepareDiskFile(path)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o666 {
		t.Errorf("无 QEMU 用户时文件应退化为 0666，实际 %o", perm)
	}
}

func TestPrepareAllDiskFilesRelaxesExistingFiles(t *testing.T) {
	if _, _, ok := qemuOwnership(); ok {
		t.Skip("宿主机存在 qemu 运行用户，跳过退化路径断言")
	}
	root := t.TempDir()
	images := filepath.Join(root, "images")
	if err := os.MkdirAll(images, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.qcow2", "b.iso"} {
		if err := os.WriteFile(filepath.Join(images, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	PrepareAllDiskFiles(root)
	entries, err := os.ReadDir(images)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o666 {
			t.Errorf("存量文件 %s 应放宽为 0666，实际 %o", entry.Name(), perm)
		}
	}
}
