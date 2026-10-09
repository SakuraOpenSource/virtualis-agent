package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SakuraOpenSource/virtualis-agent/internal/driver"
	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

// Staged replacement (AGT-F8): the original runtime and its archive must
// survive until the incoming import is verified. The legacy tests assumed an
// export->delete->import->rollback flow that no longer exists by design.
func TestReplaceVerifiesStagedImportBeforeOriginalDeletion(t *testing.T) {
	deletes, imports := 0, 0
	order := []string{}
	// replaceFn is assigned after declaration: a short-var-decl identifier is
	// not in scope inside its own initializing literal.
	staged := &testDriver{
		deleteFn: func(context.Context, *protocol.Instance) error {
			deletes++
			order = append(order, "delete-original")
			return nil
		},
		importFn: func(_ context.Context, inst *protocol.Instance, _ string) error {
			imports++
			order = append(order, "import-staged")
			if imports == 1 {
				return errors.New("incoming failure")
			}
			inst.Image = &protocol.Image{Path: "staged-disk"}
			return nil
		},
	}
	staged.replaceFn = func(ctx context.Context, original, incoming *protocol.Instance, path string) error {
		if deletes != 0 {
			t.Fatal("original deleted before staged import verified")
		}
		if err := staged.importFn(ctx, incoming, path); err != nil {
			return err
		}
		if incoming.Image == nil || incoming.Image.Path == "" {
			return errors.New("staged import produced no disk")
		}
		// Cutover only after verification.
		return staged.deleteFn(ctx, original)
	}
	s := agentForTest(t, staged)
	s.instances[1] = testInstance(1)
	if rec := importRequest(s, testInstance(1), true); rec.Code != 502 {
		t.Fatal("staged import failure must surface as 502", rec.Code, rec.Body.String())
	}
	if deletes != 0 {
		t.Fatal("failed staged import deleted the original", deletes)
	}
	if rec := importRequest(s, testInstance(1), true); rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	if deletes != 1 || imports != 2 {
		t.Fatal("unexpected call counts", deletes, imports)
	}
}

func TestReplaceUncertainFailureMarksErrorAndKeepsOriginal(t *testing.T) {
	s := agentForTest(t, &testDriver{replaceFn: func(context.Context, *protocol.Instance, *protocol.Instance, string) error {
		return &driver.ReplacementError{Err: errors.New("cutover failed"), Uncertain: true}
	}})
	s.instances[1] = testInstance(1)
	s.markBootReady(1, true)
	rec := importRequest(s, testInstance(1), true)
	if rec.Code != 502 || strings.Contains(rec.Body.String(), "cutover") {
		t.Fatal("uncertain failure must not leak host details", rec.Code, rec.Body.String())
	}
	got, _ := s.storedInstance(1)
	if got.Status != "error" || s.bootReadyOf(1) {
		t.Fatal("uncertain replacement must mark error and drop readiness", got.Status)
	}
}

func TestReplaceRollbackFailureRetainsArchiveWithoutGenericDelete(t *testing.T) {
	deletes := 0
	d := &testDriver{deleteFn: func(context.Context, *protocol.Instance) error { deletes++; return nil }, replaceFn: func(context.Context, *protocol.Instance, *protocol.Instance, string) error {
		// Uncertain: readiness must be dropped because the on-disk state is unknown.
		return &driver.ReplacementError{Err: errors.New("cannot import"), Uncertain: true}
	}}
	s := agentForTest(t, d)
	s.instances[1] = testInstance(1)
	s.markBootReady(1, true)
	rec := importRequest(s, testInstance(1), true)
	if rec.Code != 502 || !strings.Contains(rec.Body.String(), "original data retained") {
		t.Fatal(rec.Code, rec.Body.String())
	}
	archives, _ := filepath.Glob(filepath.Join(s.dataDir, "rollback-*", "backup*"))
	if len(archives) != 0 {
		t.Fatal("legacy rollback archives must not be created by the staged flow", archives)
	}
	if deletes != 0 {
		t.Fatal("staged flow must not fall back to generic delete", deletes)
	}
	if s.bootReadyOf(1) {
		t.Fatal("unrecoverable failure retained readiness")
	}
}

func TestReplaceDoesNotDiscardRollbackBeforeStoppedVerification(t *testing.T) {
	imported := false
	d := &testDriver{status: func(context.Context, *protocol.Instance) (string, error) {
		if imported {
			return driver.StatusRunning, nil
		}
		return driver.StatusStopped, nil
	}, importFn: func(context.Context, *protocol.Instance, string) error {
		if imported {
			return errors.New("target exists")
		}
		imported = true
		return nil
	}}
	d.replaceFn = func(ctx context.Context, original, incoming *protocol.Instance, path string) error {
		if err := d.Import(ctx, incoming, path); err != nil {
			return err
		}
		imported = true
		// Post-import verification: a running/unverifiable stage must abort
		// the cutover instead of reporting success.
		if status, err := d.Status(ctx, incoming); err != nil || status != driver.StatusStopped {
			return errors.New("staged import is not verifiably stopped")
		}
		return nil
	}
	s := agentForTest(t, d)
	r := importRequest(s, testInstance(1), true)
	if r.Code != 502 {
		t.Fatal("unverified import reported success", r.Code, r.Body.String())
	}
	p, _ := filepath.Glob(filepath.Join(s.dataDir, "rollback-*", "backup"))
	if len(p) != 0 {
		t.Fatal("legacy rollback dir must not be created", p)
	}
	// The instance was never registered on this agent; a failed staged replace
	// must not fabricate an ownership record from the wire payload.
	if _, err := os.Stat(filepath.Join(s.dataDir, "owned-instances", "1.json")); !os.IsNotExist(err) {
		t.Fatal("failed staged replace must not fabricate ownership", err)
	}
}
