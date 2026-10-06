package driver

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
	"gopkg.in/yaml.v3"
)

type incusBackupIndex struct {
	Name      string         `yaml:"name"`
	Type      string         `yaml:"type"`
	Optimized bool           `yaml:"optimized"`
	Config    map[string]any `yaml:"config"`
	Snapshots []string       `yaml:"snapshots"`
}

// No guest files are extracted by the agent. Scan the entire stream (including
// gzip checksum and TAR trailer) before Incus sees it. Portable non-optimized
// archives only; dependent custom volumes are deliberately unsupported.
func scanIncusArchive(ctx context.Context, inst *protocol.Instance, source string, rewrite func(string, []byte) ([]byte, error), dest string) (err error) {
	f, err := os.Open(source)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tracker := &archiveTracker{r: io.LimitReader(gz, (64<<30)+(64<<20)+1)}
	tr := tar.NewReader(tracker)
	var tw *tar.Writer
	var out *os.File
	var compressor *gzip.Writer
	if dest != "" {
		out, err = os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer out.Close()
		compressor = gzip.NewWriter(out)
		tw = tar.NewWriter(compressor)
		defer func() {
			if err != nil {
				os.Remove(dest)
			}
		}()
	}
	seen := map[string]byte{}
	links := map[string]bool{}
	hardlinks := map[string]string{}
	var total int64
	var index incusBackupIndex
	hasIndex, payload := false, false
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		n := strings.TrimSuffix(h.Name, "/")
		if n != "backup" && (!strings.HasPrefix(n, "backup/") || path.Clean(n) != n) || strings.Contains(n, "\\") || strings.Contains(n, ":") {
			return fmt.Errorf("unsafe backup path: %s", h.Name)
		}
		if _, ok := seen[n]; ok {
			return fmt.Errorf("duplicate backup path: %s", n)
		}
		seen[n] = h.Typeflag
		if len(seen) > 1000000 || h.Size < 0 || h.Size > (64<<30)-total {
			return fmt.Errorf("backup exceeds limits")
		}
		total += h.Size
		for p := path.Dir(n); p != "."; p = path.Dir(p) {
			if links[p] {
				return fmt.Errorf("entry below symlink: %s", n)
			}
		}
		switch h.Typeflag {
		case tar.TypeReg, tar.TypeDir:
		case tar.TypeSymlink:
			if !strings.Contains(n, "/rootfs/") {
				return fmt.Errorf("symlink outside guest rootfs")
			}
			links[n] = true
			for p := range seen {
				if strings.HasPrefix(p, n+"/") {
					return fmt.Errorf("symlink contains existing children")
				}
			}
		case tar.TypeLink:
			root := strings.SplitN(n, "/rootfs/", 2)[0] + "/rootfs/"
			if !strings.Contains(n, "/rootfs/") || !strings.HasPrefix(h.Linkname, root) || path.Clean(h.Linkname) != h.Linkname || strings.Contains(h.Linkname, "\\") {
				return fmt.Errorf("unsafe hardlink")
			}
			hardlinks[n] = h.Linkname
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			if !strings.Contains(n, "/rootfs/") {
				return fmt.Errorf("special file outside guest rootfs")
			}
		default:
			return fmt.Errorf("unsupported backup entry type")
		}
		var data []byte
		metadata := n == "backup/index.yaml" || (strings.HasSuffix(n, "/backup.yaml") && !strings.Contains(n, "/rootfs/") && (n == "backup/container/backup.yaml" || n == "backup/virtual-machine/backup.yaml" || strings.HasPrefix(n, "backup/container-snapshots/") || strings.HasPrefix(n, "backup/virtual-machine-snapshots/")))
		if metadata {
			if h.Typeflag != tar.TypeReg || h.Size > 1<<20 {
				return fmt.Errorf("invalid backup metadata")
			}
			data, e = io.ReadAll(tr)
			if e != nil {
				return e
			}
			if n == "backup/index.yaml" {
				if e = yaml.Unmarshal(data, &index); e != nil {
					return e
				}
				hasIndex = true
			}
			if rewrite != nil {
				data, e = rewrite(n, data)
				if e != nil {
					return e
				}
				h.Size = int64(len(data))
			}
		}
		if h.Typeflag == tar.TypeReg && h.Size > 0 && (strings.Contains(n, "/rootfs/") || n == "backup/virtual-machine.img") {
			payload = true
		}
		if tw != nil {
			if e = tw.WriteHeader(h); e != nil {
				return e
			}
			if metadata {
				_, e = tw.Write(data)
			} else {
				_, e = io.Copy(tw, tr)
			}
		} else if !metadata {
			_, e = io.Copy(io.Discard, tr)
		}
		if e != nil {
			return e
		}
	}
	// tar.Reader stops at its first footer; ensure the remainder is padding only,
	// and force gzip EOF to verify CRC/truncation.
	b := make([]byte, 32<<10)
	for {
		n, e := tracker.Read(b)
		if !bytes.Equal(b[:n], make([]byte, n)) {
			return fmt.Errorf("trailing backup payload")
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
	}
	if tracker.count < 1024 || tracker.count%512 != 0 || !bytes.Equal(tracker.tail, make([]byte, 1024)) {
		return fmt.Errorf("incomplete TAR trailer")
	}
	if tracker.count > (64<<30)+(64<<20) {
		return fmt.Errorf("backup exceeds limits")
	}
	if !hasIndex || index.Name == "" || index.Config == nil || !payload || index.Optimized {
		return fmt.Errorf("missing metadata/payload or optimized backup unsupported")
	}
	expected := "container"
	if inst.Type == "vm" {
		expected = "virtual-machine"
	}
	if index.Type != expected {
		return fmt.Errorf("backup type mismatch")
	}
	container, ok := index.Config["container"].(map[string]any)
	if !ok {
		return fmt.Errorf("missing instance config")
	}
	arch, _ := container["architecture"].(string)
	if inst.Spec.Arch != "" && canonicalArch(arch) != canonicalArch(inst.Spec.Arch) {
		return fmt.Errorf("backup architecture mismatch")
	}
	if v, ok := index.Config["dependent_volumes"]; ok && v != nil {
		if a, ok := v.([]any); !ok || len(a) > 0 {
			return fmt.Errorf("dependent volumes unsupported")
		}
	}
	for _, s := range index.Snapshots {
		if !ValidSnapshotName(s) {
			return fmt.Errorf("invalid snapshot name")
		}
		present := false
		for n, typ := range seen {
			if typ == tar.TypeReg && (strings.HasPrefix(n, "backup/container-snapshots/"+s+"/rootfs/") || n == "backup/virtual-machine-snapshots/"+s+".img") {
				present = true
				break
			}
		}
		if !present {
			return fmt.Errorf("missing snapshot payload: %s", s)
		}
	}
	for _, target := range hardlinks {
		if seen[target] != tar.TypeReg {
			return fmt.Errorf("hardlink target is not a guest regular file")
		}
	}
	if inst.Spec.DiskGB > 0 {
		var capacity int64
		check := func(c map[string]any) error {
			for _, key := range []string{"devices", "expanded_devices"} {
				if devices, ok := c[key].(map[string]any); ok {
					for _, v := range devices {
						if r, ok := v.(map[string]any); ok && r["type"] == "disk" && r["path"] == "/" {
							if size, ok := r["size"].(string); ok && size != "" {
								b, e := sizeBytes(size)
								if e != nil {
									return e
								}
								if b > capacity {
									capacity = b
								}
							}
						}
					}
				}
			}
			return nil
		}
		if e := check(container); e != nil {
			return e
		}
		if profiles, ok := index.Config["profiles"].([]any); ok {
			for _, p := range profiles {
				if profile, ok := p.(map[string]any); ok {
					if e := check(profile); e != nil {
						return e
					}
				}
			}
		}
		if int64(inst.Spec.DiskGB)<<30 < capacity {
			return fmt.Errorf("requested root disk is smaller than backup")
		}
	}
	if tw != nil {
		if e := tw.Close(); e != nil {
			return e
		}
		if e := compressor.Close(); e != nil {
			return e
		}
		return out.Close()
	}
	return nil
}

type archiveTracker struct {
	r     io.Reader
	count int64
	tail  []byte
}

func (r *archiveTracker) Read(b []byte) (int, error) {
	n, e := r.r.Read(b)
	r.count += int64(n)
	r.tail = append(r.tail, b[:n]...)
	if len(r.tail) > 1024 {
		r.tail = r.tail[len(r.tail)-1024:]
	}
	return n, e
}
func (d *Incus) ValidateImport(ctx context.Context, inst *protocol.Instance, file string) error {
	return scanIncusArchive(ctx, inst, file, nil, "")
}
func canonicalArch(s string) string {
	switch s {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	}
	return s
}

func targetIncusConfig(config map[string]string, inst *protocol.Instance) map[string]string {
	out := map[string]string{}
	for k, v := range config {
		if !strings.HasPrefix(k, "volatile.") && !strings.HasPrefix(k, "limits.") && !strings.HasPrefix(k, "raw.") && !strings.HasPrefix(k, "security.") && !strings.HasPrefix(k, "linux.") {
			out[k] = v
		}
	}
	for a := incusCPUArgs(inst); len(a) >= 2; a = a[2:] {
		parts := strings.SplitN(a[1], "=", 2)
		out[parts[0]] = parts[1]
	}
	out["limits.memory"] = fmt.Sprintf("%dMiB", inst.Spec.MemoryMB)
	return out
}
func rewriteIncusMetadata(inst *protocol.Instance, profile, pool string, profileData map[string]any) func(string, []byte) ([]byte, error) {
	return func(name string, b []byte) ([]byte, error) {
		var doc map[string]any
		if e := yaml.Unmarshal(b, &doc); e != nil {
			return nil, e
		}
		config := doc
		if name == "backup/index.yaml" {
			var ok bool
			config, ok = doc["config"].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("missing config")
			}
			doc["pool"] = pool
		}
		sanitize := func(c map[string]any) {
			raw := map[string]string{}
			if m, ok := c["config"].(map[string]any); ok {
				for k, v := range m {
					if s, ok := v.(string); ok {
						raw[k] = s
					}
				}
			}
			c["config"] = targetIncusConfig(raw, inst)
			c["devices"] = map[string]any{}
			delete(c, "expanded_config")
			delete(c, "expanded_devices")
			c["profiles"] = []string{profile}
		}
		if c, ok := config["container"].(map[string]any); ok {
			sanitize(c)
		} else {
			return nil, fmt.Errorf("missing container backup config")
		}
		if snapshots, ok := config["snapshots"].([]any); ok {
			for _, s := range snapshots {
				if c, ok := s.(map[string]any); ok {
					sanitize(c)
				}
			}
		}
		config["profiles"] = []any{profileData}
		config["pool"] = map[string]any{"name": pool}
		delete(config, "dependent_volumes")
		return yaml.Marshal(doc)
	}
}
