package driver

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func ManagedImagePath(dataDir, path string) error {
	root, err := filepath.Abs(filepath.Join(dataDir, "images"))
	if err != nil {
		return err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(root, absolute)
	if err != nil || relative == "." || filepath.Dir(relative) != "." || strings.ContainsAny(relative, `/\:`) || relative == ".." {
		return fmt.Errorf("image is not a direct child of managed storage")
	}
	// Reject redirected directories before any hypervisor or chmod operation.
	// Comparing resolved parent-vs-root tolerates legitimate host-level
	// aliasing (Windows 8.3 short names, case differences) while still
	// rejecting an actually symlinked images root or file.
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("managed image directory unavailable: %w", err)
	}
	resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return fmt.Errorf("managed image directory unavailable: %w", err)
	}
	if !samePath(resolvedParent, resolvedRoot) {
		return fmt.Errorf("image is not inside the resolved managed image directory")
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("managed image must be a regular file")
	}
	return nil
}

func samePath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}
