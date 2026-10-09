package driver

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

// RecoveryDriver keeps recovery and transfer capabilities independent of power operations.
type RecoveryDriver interface {
	CreateSnapshot(context.Context, *protocol.Instance, string) (int64, error)
	RestoreSnapshot(context.Context, *protocol.Instance, string) error
	DeleteSnapshot(context.Context, *protocol.Instance, string) error
	Export(context.Context, *protocol.Instance, string) error
	Import(context.Context, *protocol.Instance, string) error
}

// ImportValidator preflights the complete archive before replacing a runtime.
// Validation must not alter the target. Import itself owns staged compensation.
type ImportValidator interface {
	ValidateImport(context.Context, *protocol.Instance, string) error
}

var snapshotNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

func ValidSnapshotName(name string) bool { return snapshotNamePattern.MatchString(name) }

func requireStopped(ctx context.Context, d Driver, inst *protocol.Instance) error {
	status, err := d.Status(ctx, inst)
	if err != nil {
		return err
	}
	if status != StatusStopped {
		return fmt.Errorf("请先关闭实例并确认状态后重试")
	}
	return nil
}

func (d *Incus) CreateSnapshot(ctx context.Context, inst *protocol.Instance, name string) (int64, error) {
	if !ValidSnapshotName(name) {
		return 0, fmt.Errorf("快照名称无效")
	}
	return 0, run(ctx, d.cli(), "snapshot", "create", resourceName(d.Name(), inst), name, "--no-expiry")
}
func (d *Incus) RestoreSnapshot(ctx context.Context, inst *protocol.Instance, name string) error {
	// Older nodes must not fall back to restoring guest config along with the disk.
	supported, err := d.snapshotDiskOnlyCapable(ctx)
	if err != nil { return err }
	if !supported { return fmt.Errorf("Incus version is too old: requires %s",diskOnlyRestoreExtension) }
	if !ValidSnapshotName(name) {
		return fmt.Errorf("快照名称无效")
	}
	if err := requireStopped(ctx, d, inst); err != nil {
		return err
	}
	return run(ctx, d.cli(), "snapshot", "restore", resourceName(d.Name(), inst), name, "--diskonly")
}
func (d *Incus) DeleteSnapshot(ctx context.Context, inst *protocol.Instance, name string) error {
	if !ValidSnapshotName(name) {
		return fmt.Errorf("快照名称无效")
	}
	err := run(ctx, d.cli(), "snapshot", "delete", resourceName(d.Name(), inst), name)
	if err != nil && !contains(err.Error(), "not found") {
		return err
	}
	return nil
}
func (d *Incus) Export(ctx context.Context, inst *protocol.Instance, path string) error {
	if err := requireStopped(ctx, d, inst); err != nil {
		return err
	}
	return run(ctx, d.cli(), "export", resourceName(d.Name(), inst), path, "--compression", "gzip")
}
func (d *QEMU) snapshotDir(id uint) string {
	return filepath.Join(d.dataDir, "snapshots", strconv.FormatUint(uint64(id), 10))
}
func (d *QEMU) snapshotPath(id uint, name string) (string, error) {
	if !ValidSnapshotName(name) {
		return "", fmt.Errorf("快照名称无效")
	}
	return filepath.Join(d.snapshotDir(id), name+".qcow2"), nil
}

// Read the actual system disk from libvirt instead of trusting image metadata from the master.
func (d *QEMU) systemDisk(ctx context.Context, inst *protocol.Instance) (string, error) {
	raw, err := output(ctx, "virsh", "dumpxml", resourceName(d.Name(), inst), "--inactive")
	if err != nil {
		return "", fmt.Errorf("读取系统盘失败: %w: %s", err, raw)
	}
	var domain struct {
		Disks []struct {
			Device string `xml:"device,attr"`
			Source struct {
				File string `xml:"file,attr"`
			} `xml:"source"`
			Target struct {
				Dev string `xml:"dev,attr"`
			} `xml:"target"`
		} `xml:"devices>disk"`
	}
	if err := xml.Unmarshal(raw, &domain); err != nil {
		return "", err
	}
	for _, disk := range domain.Disks {
		if disk.Device == "disk" && disk.Target.Dev == "vda" && disk.Source.File != "" {
			return disk.Source.File, nil
		}
	}
	return "", fmt.Errorf("找不到文件形式的系统盘 vda")
}

func (d *QEMU) CreateSnapshot(ctx context.Context, inst *protocol.Instance, name string) (int64, error) {
	path, err := d.snapshotPath(inst.ID, name)
	if err != nil {
		return 0, err
	}
	if err := requireStopped(ctx, d, inst); err != nil {
		return 0, err
	}
	if _, err := os.Stat(path); err == nil {
		return 0, fmt.Errorf("快照已存在")
	}
	disk, err := d.systemDisk(ctx, inst)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return 0, err
	}
	if err := run(ctx, "qemu-img", "convert", "-O", "qcow2", disk, path); err != nil {
		_ = os.Remove(path)
		return 0, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}
func (d *QEMU) RestoreSnapshot(ctx context.Context, inst *protocol.Instance, name string) error {
	path, err := d.snapshotPath(inst.ID, name)
	if err != nil {
		return err
	}
	if err := requireStopped(ctx, d, inst); err != nil {
		return err
	}
	disk, err := d.systemDisk(ctx, inst)
	if err != nil {
		return err
	}
	// Convert beside the disk and rename only after a successful complete conversion.
	staging, err := os.CreateTemp(filepath.Dir(disk), ".restore-*.qcow2")
	if err != nil {
		return err
	}
	stage := staging.Name()
	staging.Close()
	defer os.Remove(stage)
	if err := run(ctx, "qemu-img", "convert", "-O", "qcow2", path, stage); err != nil {
		return err
	}
	actual, err := qcowVirtualSize(ctx, disk)
	if err != nil {
		return err
	}
	stageSize, err := validateQCOW(ctx, stage)
	if err != nil {
		return err
	}
	// A snapshot can predate a disk extension. Neither stale master metadata
	// nor a larger snapshot may shrink the capacity of the currently owned disk.
	target := actual
	if desired := int64(inst.Spec.DiskGB) << 30; desired > target {
		target = desired
	}
	if stageSize > target {
		target = stageSize
	}
	if target > stageSize {
		if err := run(ctx, "qemu-img", "resize", stage, fmt.Sprintf("%dG", (target+(1<<30)-1)>>30)); err != nil {
			return err
		}
	}
	if err := prepareRecoveryDisk(stage); err != nil {
		return err
	}
	return os.Rename(stage, disk)
}
func (d *QEMU) DeleteSnapshot(_ context.Context, inst *protocol.Instance, name string) error {
	path, err := d.snapshotPath(inst.ID, name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func archiveFile(tw *tar.Writer, path, name string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("备份仅支持普通文件")
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: info.Size()}); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}
func (d *QEMU) Export(ctx context.Context, inst *protocol.Instance, path string) error {
	if err := requireStopped(ctx, d, inst); err != nil {
		return err
	}
	disk, err := d.systemDisk(ctx, inst)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d.imagesDir(), 0o755); err != nil {
		return err
	}
	flat, err := os.CreateTemp(d.imagesDir(), ".export-*.qcow2")
	if err != nil {
		return err
	}
	flat.Close()
	defer os.Remove(flat.Name())
	if err := run(ctx, "qemu-img", "convert", "-O", "qcow2", disk, flat.Name()); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(f)
	err = archiveFile(tw, flat.Name(), "disk.qcow2")
	if err == nil {
		entries, readErr := os.ReadDir(d.snapshotDir(inst.ID))
		if readErr != nil && !os.IsNotExist(readErr) {
			err = readErr
		}
		for _, entry := range entries {
			if err != nil {
				break
			}
			name := strings.TrimSuffix(entry.Name(), ".qcow2")
			if !entry.IsDir() && ValidSnapshotName(name) && strings.HasSuffix(entry.Name(), ".qcow2") {
				err = archiveFile(tw, filepath.Join(d.snapshotDir(inst.ID), entry.Name()), "snapshots/"+entry.Name())
			}
		}
	}
	closeErr := tw.Close()
	fileErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return fileErr
}

func extractQEMUArchive(path, dir string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	// archive/tar deliberately accepts a missing end marker. Backups must be
	// complete: require both 512-byte zero blocks and reject trailing payloads.
	if info.Size() < 1024 || info.Size()%512 != 0 {
		return fmt.Errorf("备份 TAR 长度或结束标记无效")
	}
	footer := make([]byte, 1024)
	if _, err := f.ReadAt(footer, info.Size()-1024); err != nil || !bytes.Equal(footer, make([]byte, 1024)) {
		return fmt.Errorf("备份 TAR 缺少完整结束标记")
	}
	tr := tar.NewReader(f)
	seen := map[string]bool{}
	var total int64
	for {
		header, err := tr.Next()
		if err == io.EOF {
			buffer := make([]byte, 32<<10)
			for {
				n, readErr := f.Read(buffer)
				if !bytes.Equal(buffer[:n], make([]byte, n)) {
					return fmt.Errorf("备份 TAR 结束后包含额外内容")
				}
				if readErr == io.EOF {
					break
				}
				if readErr != nil {
					return readErr
				}
			}
			break
		}
		if err != nil {
			return err
		}
		name := header.Name
		valid := name == "disk.qcow2" || (strings.HasPrefix(name, "snapshots/") && strings.HasSuffix(name, ".qcow2") && ValidSnapshotName(strings.TrimSuffix(strings.TrimPrefix(name, "snapshots/"), ".qcow2")))
		if !valid || header.Typeflag != tar.TypeReg || seen[name] || header.Size <= 0 {
			return fmt.Errorf("备份归档包含无效条目")
		}
		seen[name] = true
		if header.Size > (64<<30)-total || len(seen) > 1001 {
			return fmt.Errorf("备份超过大小或快照数量上限")
		}
		total += header.Size
		out := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
			return err
		}
		file, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(file, tr)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	if !seen["disk.qcow2"] {
		return fmt.Errorf("备份缺少系统盘")
	}
	return nil
}
