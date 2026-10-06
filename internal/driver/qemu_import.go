package driver

import (
	"context"
	"fmt"
	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (d *QEMU) ValidateImport(ctx context.Context, inst *protocol.Instance, path string) error {
	if inst.Type != "" && inst.Type != "vm" {
		return fmt.Errorf("QEMU requires a VM backup")
	}
	if err := os.MkdirAll(d.dataDir, 0755); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(d.dataDir, "validate-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := d.unpackValidated(ctx, path, dir); err != nil {
		return err
	}
	return d.checkImportSize(ctx, inst, filepath.Join(dir, "disk.qcow2"))
}
func (d *QEMU) checkImportSize(ctx context.Context, inst *protocol.Instance, disk string) error {
	size, err := qcowVirtualSize(ctx, disk)
	if err != nil {
		return err
	}
	if inst.Spec.DiskGB > 0 && int64(inst.Spec.DiskGB)<<30 < size {
		return fmt.Errorf("requested disk is smaller than backup virtual disk")
	}
	return nil
}

// Import defines a unique stopped staging domain, then publishes its name.
// Compensation only owns this transaction's files/domain, never the target
// that may have pre-existed or been created by another administrator.
func (d *QEMU) Import(ctx context.Context, inst *protocol.Instance, path string) (err error) {
	name := resourceName(d.Name(), inst)
	found, err := d.domainExists(ctx, name)
	if err != nil {
		return err
	}
	if found {
		return fmt.Errorf("目标实例已存在")
	}
	if err := os.MkdirAll(d.imagesDir(), 0755); err != nil {
		return err
	}
	PrepareDiskDir(d.dataDir)
	dir, err := os.MkdirTemp(d.dataDir, "import-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	stageName := "virtualis-" + filepath.Base(dir)
	if err := d.unpackValidated(ctx, path, dir); err != nil {
		return err
	}
	if err := d.checkImportSize(ctx, inst, filepath.Join(dir, "disk.qcow2")); err != nil {
		return err
	}
	if err := d.ensureNetwork(ctx, inst); err != nil {
		return err
	}
	disk := filepath.Join(d.imagesDir(), name+".qcow2")
	for _, target := range []string{disk, d.snapshotDir(inst.ID)} {
		if _, statErr := os.Lstat(target); !os.IsNotExist(statErr) {
			return fmt.Errorf("target path already exists or cannot be checked: %s: %v", target, statErr)
		}
	}
	// Exclusive no-clobber publication on the same filesystem.
	if err := os.Link(filepath.Join(dir, "disk.qcow2"), disk); err != nil {
		return err
	}
	ownedSnapshots, defined := false, false
	cleanupName := stageName
	defer func() {
		if err == nil {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		if defined {
			if cleanupErr := run(cleanupCtx, "virsh", "undefine", cleanupName, "--nvram"); cleanupErr != nil {
				err = fmt.Errorf("%w; staging cleanup failed: %v; retained disk: %s", err, cleanupErr, disk)
				return // Never unlink storage still referenced by a domain.
			}
		}
		if ownedSnapshots {
			if e := os.RemoveAll(d.snapshotDir(inst.ID)); e != nil {
				err = fmt.Errorf("%w; snapshots cleanup: %v", err, e)
			}
		}
		if e := os.Remove(disk); e != nil && !os.IsNotExist(e) {
			err = fmt.Errorf("%w; disk cleanup: %v", err, e)
		}
	}()
	if err := prepareRecoveryDisk(disk); err != nil {
		return err
	}
	if inst.Spec.DiskGB > 0 {
		size, sizeErr := qcowVirtualSize(ctx, disk)
		if sizeErr != nil {
			return sizeErr
		}
		if int64(inst.Spec.DiskGB)<<30 > size {
			if err := run(ctx, "qemu-img", "resize", disk, fmt.Sprintf("%dG", inst.Spec.DiskGB)); err != nil {
				return err
			}
		}
	}
	if _, e := os.Stat(filepath.Join(dir, "snapshots")); e == nil {
		if err := os.MkdirAll(filepath.Dir(d.snapshotDir(inst.ID)), 0700); err != nil {
			return err
		}
		if err := os.Rename(filepath.Join(dir, "snapshots"), d.snapshotDir(inst.ID)); err != nil {
			return err
		}
		ownedSnapshots = true
	}
	xmlPath := filepath.Join(dir, "domain.xml")
	if err := os.WriteFile(xmlPath, []byte(domainXML(stageName, inst, disk, "")), 0600); err != nil {
		return err
	}
	if err := run(ctx, "virsh", "define", xmlPath); err != nil {
		// define may have succeeded remotely before a broken connection.
		if exists, e := d.domainExists(context.WithoutCancel(ctx), stageName); e != nil {
			defined = true
		} else {
			defined = exists
		}
		return err
	}
	defined = true
	if err := run(ctx, "virsh", "domrename", stageName, name); err != nil {
		return err
	}
	cleanupName = name
	if err := requireStopped(ctx, d, inst); err != nil {
		return err
	}
	inst.Image = &protocol.Image{Driver: d.Name(), Type: "disk", Path: disk}
	inst.RootPassword, inst.SSHReady = "", false
	return nil
}
func (d *QEMU) domainExists(ctx context.Context, name string) (bool, error) {
	raw, err := output(ctx, "virsh", "list", "--all", "--name")
	if err != nil {
		return false, fmt.Errorf("cannot check target domain: %w: %s", err, raw)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == name {
			return true, nil
		}
	}
	return false, nil
}
func prepareRecoveryDisk(path string) error {
	if uid, gid, ok := qemuOwnership(); ok {
		if err := os.Chown(path, uid, gid); err != nil {
			return err
		}
		return os.Chmod(path, 0660)
	}
	return os.Chmod(path, 0666)
}
