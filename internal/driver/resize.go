package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
	"gopkg.in/yaml.v3"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// ResizeDriver is optional: power-only and older drivers remain compatible.
type ResizeDriver interface {
	Resize(context.Context, *protocol.Instance, protocol.InstanceSpec, *protocol.NetworkConfig) error
}

func ValidateResize(inst *protocol.Instance, s protocol.InstanceSpec, n *protocol.NetworkConfig) error {
	if s.CPU < 1 || s.CPU > 4096 || s.CPUMilli < 0 || s.CPUMilli > 4096000 || s.MemoryMB < 128 || s.MemoryMB > 16777216 || s.DiskGB < 1 || s.DiskGB > 65536 {
		return fmt.Errorf("invalid CPU/memory/disk specification")
	}
	if s.DiskGB < inst.Spec.DiskGB {
		return fmt.Errorf("disk shrinking is forbidden")
	}
	if s.Arch != "" && inst.Spec.Arch != "" && canonicalArch(s.Arch) != canonicalArch(inst.Spec.Arch) {
		return fmt.Errorf("architecture changes are forbidden")
	}
	if n != nil {
		if n.BandwidthMbps < 0 || n.TrafficGB < 0 {
			return fmt.Errorf("invalid network quota")
		}
		a, b := *n, inst.Network
		a.BandwidthMbps, b.BandwidthMbps = 0, 0
		a.TrafficGB, b.TrafficGB = 0, 0
		if !reflect.DeepEqual(a, b) {
			return fmt.Errorf("resize cannot change network attachment/address")
		}
	}
	return nil
}
func normalizedResize(s protocol.InstanceSpec, actualArch string) protocol.InstanceSpec {
	if s.Arch == "" {
		s.Arch = actualArch
	}
	s.CPU = cpuCoresEffective(s)
	return s
}
func checkActualArch(actual string, s protocol.InstanceSpec) error {
	if actual == "" {
		return fmt.Errorf("actual architecture unavailable")
	}
	if s.Arch != "" && canonicalArch(s.Arch) != canonicalArch(actual) {
		return fmt.Errorf("actual architecture cannot change")
	}
	return nil
}
func sizeBytes(s string) (int64, error) {
	for _, u := range []struct {
		s string
		n int64
	}{{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"TiB", 1 << 40}, {"GB", 1000000000}, {"MB", 1000000}, {"TB", 1000000000000}, {"G", 1 << 30}, {"M", 1 << 20}, {"B", 1}} {
		if strings.HasSuffix(s, u.s) {
			v, e := strconv.ParseInt(strings.TrimSuffix(s, u.s), 10, 64)
			if e != nil || v < 0 || v > (1<<62)/u.n {
				return 0, fmt.Errorf("invalid disk size")
			}
			return v * u.n, nil
		}
	}
	return strconv.ParseInt(s, 10, 64)
}
func (d *Incus) Resize(ctx context.Context, inst *protocol.Instance, s protocol.InstanceSpec, n *protocol.NetworkConfig) error {
	if e := ValidateResize(inst, s, n); e != nil {
		return e
	}
	if e := requireStopped(ctx, d, inst); e != nil {
		return e
	}
	endpoint := "/1.0/instances/" + resourceName(d.Name(), inst)
	raw, e := output(ctx, d.cli(), "query", endpoint)
	if e != nil {
		return e
	}
	var current struct {
		Architecture    string                       `json:"architecture"`
		Config          map[string]string            `json:"config"`
		Devices         map[string]map[string]string `json:"devices"`
		ExpandedDevices map[string]map[string]string `json:"expanded_devices"`
		Profiles        []string                     `json:"profiles"`
		Description     string                       `json:"description"`
		Ephemeral       bool                         `json:"ephemeral"`
	}
	if e = incusJSON(raw, &current); e != nil {
		return e
	}
	if e = checkActualArch(current.Architecture, s); e != nil {
		return e
	}
	s = normalizedResize(s, current.Architecture)
	rootName := ""
	var root map[string]string
	for k, v := range current.ExpandedDevices {
		if v["type"] == "disk" && v["path"] == "/" {
			if root != nil {
				return fmt.Errorf("ambiguous root disk")
			}
			rootName, root = k, v
		}
	}
	if root == nil || root["pool"] == "" {
		return fmt.Errorf("actual root disk unavailable")
	}
	actual, e := sizeBytes(root["size"])
	if e != nil || actual <= 0 {
		return fmt.Errorf("actual root disk quota unavailable")
	}
	desired := int64(s.DiskGB) << 30
	if desired < actual {
		return fmt.Errorf("disk shrinking is forbidden")
	}
	if current.Config == nil {
		current.Config = map[string]string{}
	}
	if current.Devices == nil {
		current.Devices = map[string]map[string]string{}
	}
	delete(current.Config, "limits.cpu.allowance")
	for a := incusCPUArgs(&protocol.Instance{Type: inst.Type, Spec: s}); len(a) > 1; a = a[2:] {
		parts := strings.SplitN(a[1], "=", 2)
		current.Config[parts[0]] = parts[1]
	}
	current.Config["limits.memory"] = fmt.Sprintf("%dMiB", s.MemoryMB)
	if desired > actual {
		pool := root["pool"]
		if !storageDriverSupportsQuota(d.storagePoolDriver(ctx, pool)) {
			return fmt.Errorf("target pool does not support disk quotas")
		}
		if pool == dedicatedPoolName(inst.ID) {
			raw, e := output(ctx, d.cli(), "storage", "show", pool)
			if e != nil {
				return e
			}
			var p struct {
				Driver string            `yaml:"driver"`
				Config map[string]string `yaml:"config"`
			}
			if e = yaml.Unmarshal(raw, &p); e != nil {
				return e
			}
			capacity, e := sizeBytes(p.Config["size"])
			if e != nil || capacity <= 0 {
				return fmt.Errorf("dedicated pool capacity unavailable")
			}
			if desired > capacity {
				if p.Driver != "btrfs" && p.Driver != "zfs" {
					return fmt.Errorf("dedicated pool growth unsupported")
				}
				if e = run(ctx, d.cli(), "storage", "set", pool, fmt.Sprintf("size=%dGiB", s.DiskGB)); e != nil {
					return e
				}
			}
		}
	}
	current.Devices[rootName] = cloneStringMap(root)
	current.Devices[rootName]["size"] = fmt.Sprintf("%dGiB", s.DiskGB)
	if n != nil {
		for name, v := range current.ExpandedDevices {
			if v["type"] == "nic" {
				nic := cloneStringMap(v)
				delete(nic, "limits.ingress")
				delete(nic, "limits.egress")
				if n.BandwidthMbps > 0 {
					nic["limits.ingress"] = fmt.Sprintf("%dMbit", n.BandwidthMbps)
					nic["limits.egress"] = nic["limits.ingress"]
				}
				current.Devices[name] = nic
			}
		}
	}
	b, e := json.Marshal(map[string]any{"architecture": current.Architecture, "config": current.Config, "devices": current.Devices, "profiles": current.Profiles, "description": current.Description, "ephemeral": current.Ephemeral})
	if e != nil {
		return e
	}
	if e = run(ctx, d.cli(), "query", endpoint, "--request", "PUT", "--data", string(b), "--wait"); e != nil {
		return e
	}
	inst.Spec = s
	if n != nil {
		inst.Network = *n
	}
	return nil
}
func cloneStringMap(m map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Byte-span edits preserve UUID, firmware, guest agent, disks, bootstrap media,
// namespaces and unknown libvirt extensions instead of regenerating a domain.
type xmlSpan struct {
	path              string
	start, end, close int
	attrs             []xml.Attr
}
type xmlEdit struct {
	start, end int
	text       string
}

func xmlSpans(raw []byte) ([]xmlSpan, error) {
	d := xml.NewDecoder(bytes.NewReader(raw))
	stack := []xmlSpan{}
	out := []xmlSpan{}
	for {
		start := int(d.InputOffset())
		tok, e := d.Token()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, e
		}
		switch t := tok.(type) {
		case xml.StartElement:
			p := t.Name.Local
			if len(stack) > 0 {
				p = stack[len(stack)-1].path + "/" + p
			}
			stack = append(stack, xmlSpan{path: p, start: start, attrs: t.Attr})
		case xml.EndElement:
			if len(stack) == 0 {
				return nil, fmt.Errorf("invalid XML")
			}
			s := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			s.close, s.end = start, int(d.InputOffset())
			out = append(out, s)
		}
	}
	return out, nil
}
func applyXMLEdits(raw []byte, edits []xmlEdit) []byte {
	sort.SliceStable(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	for _, e := range edits {
		raw = append(append(append([]byte{}, raw[:e.start]...), []byte(e.text)...), raw[e.end:]...)
	}
	return raw
}
func (d *QEMU) Resize(ctx context.Context, inst *protocol.Instance, s protocol.InstanceSpec, n *protocol.NetworkConfig) error {
	if e := ValidateResize(inst, s, n); e != nil {
		return e
	}
	if e := requireStopped(ctx, d, inst); e != nil {
		return e
	}
	name := resourceName(d.Name(), inst)
	raw, e := output(ctx, "virsh", "dumpxml", name, "--inactive")
	if e != nil {
		return e
	}
	spans, e := xmlSpans(raw)
	if e != nil {
		return e
	}
	actualArch := ""
	disk, e := d.systemDisk(ctx, inst)
	if e != nil {
		return e
	}
	for _, x := range spans {
		if x.path == "domain/os/type" {
			for _, a := range x.attrs {
				if a.Name.Local == "arch" {
					actualArch = a.Value
				}
			}
		}
	}
	if e = checkActualArch(actualArch, s); e != nil {
		return e
	}
	s = normalizedResize(s, actualArch)
	actual, e := qcowVirtualSize(ctx, disk)
	if e != nil {
		return e
	}
	if int64(s.DiskGB)<<30 < actual {
		return fmt.Errorf("disk shrinking is forbidden")
	}
	edits := []xmlEdit{}
	found := map[string]bool{}
	rootClose := 0
	interfaces := 0
	tune := ""
	if s.CPUMilli > 0 {
		tune = fmt.Sprintf("<period>100000</period><quota>%d</quota>", s.CPUMilli*100)
	}
	for _, x := range spans {
		switch x.path {
		case "domain":
			rootClose = x.close
		case "domain/memory", "domain/currentMemory":
			tag := strings.TrimPrefix(x.path, "domain/")
			edits = append(edits, xmlEdit{x.start, x.end, fmt.Sprintf("<%s unit='MiB'>%d</%s>", tag, s.MemoryMB, tag)})
			found[tag] = true
		case "domain/vcpu":
			edits = append(edits, xmlEdit{x.start, x.end, fmt.Sprintf("<vcpu placement='static'>%d</vcpu>", s.CPU)})
			found["vcpu"] = true
		case "domain/cputune/period", "domain/cputune/quota":
			edits = append(edits, xmlEdit{x.start, x.end, ""})
		case "domain/cputune":
			edits = append(edits, xmlEdit{x.close, x.close, tune})
			found["cputune"] = true
		case "domain/devices/interface/bandwidth":
			if n != nil {
				edits = append(edits, xmlEdit{x.start, x.end, ""})
			}
		case "domain/devices/interface":
			interfaces++
			if n != nil {
				if x.close == x.start || strings.HasSuffix(strings.TrimSpace(string(raw[x.start:x.end])), "/>") {
					return fmt.Errorf("unsupported empty interface XML")
				}
				edits = append(edits, xmlEdit{x.close, x.close, bandwidthXML(n.BandwidthMbps)})
			}
		}
	}
	if rootClose == 0 {
		return fmt.Errorf("missing domain XML")
	}
	if n != nil && n.BandwidthMbps > 0 && interfaces == 0 {
		return fmt.Errorf("no NIC for bandwidth limit")
	}
	insert := ""
	for _, tag := range []string{"memory", "currentMemory"} {
		if !found[tag] {
			insert += fmt.Sprintf("<%s unit='MiB'>%d</%s>", tag, s.MemoryMB, tag)
		}
	}
	if !found["vcpu"] {
		insert += fmt.Sprintf("<vcpu placement='static'>%d</vcpu>", s.CPU)
	}
	if !found["cputune"] && tune != "" {
		insert += "<cputune>" + tune + "</cputune>"
	}
	edits = append(edits, xmlEdit{rootClose, rootClose, insert})
	updated := applyXMLEdits(raw, edits)
	if _, e = xmlSpans(updated); e != nil {
		return e
	}
	if e = os.MkdirAll(d.dataDir, 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(d.dataDir, "resize-*.xml")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(updated); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if int64(s.DiskGB)<<30 > actual {
		if e = run(ctx, "qemu-img", "resize", disk, fmt.Sprintf("%dG", s.DiskGB)); e != nil {
			return e
		}
	}
	if e = run(ctx, "virsh", "define", filepath.Clean(f.Name())); e != nil {
		return fmt.Errorf("domain resize failed (disk growth may already have completed; retry without shrinking): %w", e)
	}
	inst.Spec = s
	if n != nil {
		inst.Network = *n
	}
	return nil
}
