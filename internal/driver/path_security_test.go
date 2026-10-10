package driver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

func TestQEMUDeleteRemovesOnlyOwnedVolumesAfterUndefine(t *testing.T) {
	d := NewQEMUWithDataDir(t.TempDir())
	if err := os.MkdirAll(d.imagesDir(), 0755); err != nil {
		t.Fatal(err)
	}
	i := &protocol.Instance{ID: 1, Name: "safe"}
	owned := filepath.Join(d.imagesDir(), resourceName("qemu", i)+".qcow2")
	other := filepath.Join(d.imagesDir(), "virtualis-2-other.qcow2")
	foreign := filepath.Join(t.TempDir(), "host.qcow2")
	for _, p := range []string{owned, other, foreign} {
		if err := os.WriteFile(p, standaloneQCOW(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx := qemuTestContext(t, func(_ context.Context, n string, a []string) ([]byte, error) {
		if n == "virsh" && a[0] == "list" {
			return []byte(resourceName("qemu", i) + "\n"), nil
		}
		if n == "virsh" && a[0] == "dumpxml" {
			return []byte(`<domain><devices><disk device='disk'><source file='` + owned + `'/></disk><disk device='disk'><source file='` + other + `'/></disk><disk device='disk'><source file='` + foreign + `'/></disk></devices></domain>`), nil
		}
		if n == "virsh" && a[0] == "undefine" {
			if strings.Contains(strings.Join(a, " "), "--remove-all-storage") {
				t.Error("undefine must not bulk-delete storage")
			}
			if _, err := os.Stat(owned); err != nil {
				t.Error("owned volume unlinked before domain undefine")
			}
		}
		return nil, nil
	})
	if err := d.Delete(ctx, i); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(owned); !os.IsNotExist(err) {
		t.Error("owned volume was not removed")
	}
	for _, p := range []string{other, foreign} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("unrelated volume removed: %v", err)
		}
	}
}

func TestQEMUCreateRejectsForeignImageBeforeAnyMutation(t *testing.T) {
	for _, imageType := range []string{"disk", "iso"} {
		t.Run(imageType, func(t *testing.T) {
			d := NewQEMUWithDataDir(t.TempDir())
			victim := filepath.Join(t.TempDir(), "foreign.qcow2")
			if err := os.WriteFile(victim, standaloneQCOW(), 0600); err != nil {
				t.Fatal(err)
			}
			calls := []string{}
			ctx := qemuTestContext(t, func(_ context.Context, n string, a []string) ([]byte, error) {
				calls = append(calls, n+" "+strings.Join(a, " "))
				return nil, nil
			})
			i := &protocol.Instance{ID: 1, Name: "safe", Network: protocol.NetworkConfig{Mode: "none"}, Image: &protocol.Image{Type: imageType, Path: victim}}
			if err := d.Create(ctx, i); err == nil {
				t.Error("foreign host path accepted as guest image")
			}
			for _, c := range calls {
				if strings.Contains(c, "define") || strings.Contains(c, "create") {
					t.Fatalf("foreign image reached mutation: %s", c)
				}
			}
		})
	}
}
