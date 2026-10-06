package driver

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Check the header before invoking qemu-img: never let an uploaded qcow2
// cause qemu-img to open a host backing/data file or an encrypted image.
func standaloneQCOWHeader(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := make([]byte, 104)
	if _, err := io.ReadFull(f, h); err != nil {
		return fmt.Errorf("invalid qcow2 header: %w", err)
	}
	version := binary.BigEndian.Uint32(h[4:8])
	if string(h[:4]) != "QFI\xfb" || (version != 2 && version != 3) {
		return fmt.Errorf("backup disk must be qcow2")
	}
	if binary.BigEndian.Uint64(h[8:16]) != 0 || binary.BigEndian.Uint32(h[16:20]) != 0 {
		return fmt.Errorf("external backing files are forbidden in backups")
	}
	if binary.BigEndian.Uint32(h[32:36]) != 0 {
		return fmt.Errorf("encrypted qcow2 backups are unsupported")
	}
	if version == 3 && binary.BigEndian.Uint64(h[72:80])&^uint64(3) != 0 {
		return fmt.Errorf("external data files or unsupported qcow2 features")
	}
	return nil
}
func qcowVirtualSize(ctx context.Context, path string) (int64, error) {
	raw, err := output(ctx, "qemu-img", "info", "--output=json", path)
	if err != nil {
		return 0, fmt.Errorf("reading disk size: %w: %s", err, raw)
	}
	var info struct {
		Format      string `json:"format"`
		Size        int64  `json:"virtual-size"`
		Backing     string `json:"backing-filename"`
		FullBacking string `json:"full-backing-filename"`
		DataFile    string `json:"data-file"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return 0, err
	}
	if info.Format != "qcow2" || info.Size <= 0 || info.Backing != "" || info.FullBacking != "" || info.DataFile != "" {
		return 0, fmt.Errorf("disk is not an independent qcow2 image")
	}
	return info.Size, nil
}
func validateQCOW(ctx context.Context, path string) (int64, error) {
	if err := standaloneQCOWHeader(path); err != nil {
		return 0, err
	}
	size, err := qcowVirtualSize(ctx, path)
	if err != nil {
		return 0, err
	}
	if err := run(ctx, "qemu-img", "check", "-f", "qcow2", path); err != nil {
		return 0, fmt.Errorf("corrupt backup disk: %w", err)
	}
	return size, nil
}
func (d *QEMU) unpackValidated(ctx context.Context, path, dir string) error {
	if err := extractQEMUArchive(path, dir); err != nil {
		return err
	}
	return filepath.WalkDir(dir, func(path string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			return nil
		}
		_, err = validateQCOW(ctx, path)
		return err
	})
}
