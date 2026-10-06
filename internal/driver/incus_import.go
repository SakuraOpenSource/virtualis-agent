package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func incusJSON(raw []byte, target any) error {
	var envelope map[string]json.RawMessage
	if e := json.Unmarshal(raw, &envelope); e == nil {
		if data, ok := envelope["metadata"]; ok {
			return json.Unmarshal(data, target)
		}
	}
	return json.Unmarshal(raw, target)
}
func (d *Incus) resourceExists(ctx context.Context, kind, name string) (bool, error) {
	raw, e := output(ctx, d.cli(), "query", "/1.0/"+kind)
	if e != nil {
		return false, fmt.Errorf("cannot check %s: %w: %s", kind, e, raw)
	}
	var names []string
	if e = incusJSON(raw, &names); e != nil {
		return false, e
	}
	for _, p := range names {
		u, e := url.Parse(p)
		if e == nil && strings.TrimPrefix(u.Path, "/1.0/"+kind+"/") == name {
			return true, nil
		}
	}
	return false, nil
}
func (d *Incus) Import(ctx context.Context, inst *protocol.Instance, archive string) (err error) {
	target := resourceName(d.Name(), inst)
	exists, err := d.resourceExists(ctx, "instances", target)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("目标实例已存在")
	}
	if err = d.ValidateImport(ctx, inst, archive); err != nil {
		return err
	}
	profile := fmt.Sprintf("virtualis-p-%d", inst.ID)
	exists, err = d.resourceExists(ctx, "profiles", profile)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("target profile already exists; refusing foreign ownership")
	}
	if err = os.MkdirAll(d.dataDir, 0700); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(d.dataDir, "incus-import-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	stage := "virtualis-" + filepath.Base(dir)
	cleanupName := stage
	ownedProfile, imported := false, false
	defer func() {
		if err == nil {
			return
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		if imported {
			if e := run(cleanup, d.cli(), "delete", cleanupName, "--force"); e != nil {
				err = fmt.Errorf("%w; stage cleanup failed: %v; retained stage: %s", err, e, cleanupName)
				return
			}
		}
		if ownedProfile {
			if e := run(cleanup, d.cli(), "profile", "delete", profile); e != nil {
				err = fmt.Errorf("%w; profile cleanup: %v", err, e)
			}
		}
	}()
	// Create explicitly so compensation owns precisely this profile.
	if err = run(ctx, d.cli(), "profile", "create", profile); err != nil {
		return err
	}
	ownedProfile = true
	if err = d.ensureProfile(ctx, profile, inst.Network, inst, false); err != nil {
		return err
	}
	raw, err := output(ctx, d.cli(), "query", "/1.0/profiles/"+profile)
	if err != nil {
		return err
	}
	var p map[string]any
	if err = incusJSON(raw, &p); err != nil {
		return err
	}
	p["name"] = profile
	var device struct {
		Devices map[string]map[string]string `json:"devices"`
	}
	if err = incusJSON(raw, &device); err != nil {
		return err
	}
	pool := device.Devices["root"]["pool"]
	if pool == "" {
		return fmt.Errorf("target profile has no root pool")
	}
	sanitized := filepath.Join(dir, "backup.tar.gz")
	if err = scanIncusArchive(ctx, inst, archive, rewriteIncusMetadata(inst, profile, pool, p), sanitized); err != nil {
		return err
	}
	if err = run(ctx, d.cli(), "import", sanitized, stage, "--storage", pool); err != nil {
		found, e := d.resourceExists(context.WithoutCancel(ctx), "instances", stage)
		imported = found || e != nil
		return err
	}
	imported = true
	raw, err = output(ctx, d.cli(), "query", "/1.0/instances/"+stage)
	if err != nil {
		return err
	}
	var current struct {
		Architecture string            `json:"architecture"`
		Config       map[string]string `json:"config"`
	}
	if err = incusJSON(raw, &current); err != nil {
		return err
	}
	payload := map[string]any{"architecture": current.Architecture, "config": targetIncusConfig(current.Config, inst), "devices": map[string]any{}, "profiles": []string{profile}, "ephemeral": false, "stateful": false}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if err = run(ctx, d.cli(), "query", "/1.0/instances/"+stage, "--request", "PUT", "--data", string(b), "--wait"); err != nil {
		return err
	}
	if err = run(ctx, d.cli(), "move", stage, target); err != nil {
		return err
	}
	cleanupName = target
	if err = requireStopped(ctx, d, inst); err != nil {
		return err
	}
	inst.RootPassword, inst.SSHReady = "", false
	return nil
}
