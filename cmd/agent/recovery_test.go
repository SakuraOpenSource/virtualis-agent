package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SakuraOpenSource/virtualis-agent/internal/driver"
	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

func importRequest(s *agentServer, inst protocol.Instance, replace bool) *httptest.ResponseRecorder {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	raw, _ := json.Marshal(inst)
	mw.WriteField("instance", string(raw))
	part, _ := mw.CreateFormFile("image", "backup.tar")
	part.Write([]byte("incoming"))
	mw.Close()
	path := "/api/instances/1/import"
	if replace {
		path += "?replace=true"
	}
	req := httptest.NewRequest(http.MethodPost, path, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Agent-Token", "token")
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)
	return rec
}
func TestReplaceValidatesBeforeDeletingOriginal(t *testing.T) {
	deletes, exports := 0, 0
	d := &testDriver{validate: func(context.Context, *protocol.Instance, string) error { return errors.New("corrupt") }, deleteFn: func(context.Context, *protocol.Instance) error { deletes++; return nil }, export: func(context.Context, *protocol.Instance, string) error { exports++; return nil }}
	s := agentForTest(t, d)
	rec := importRequest(s, testInstance(1), true)
	if rec.Code != 400 || deletes != 0 || exports != 0 {
		t.Fatalf("validation must precede destructive work: code=%d deletes=%d exports=%d %s", rec.Code, deletes, exports, rec.Body.String())
	}
}
func TestReplaceRollbackRestoresOriginalMetadataAndReadiness(t *testing.T) {
	calls := 0
	d := &testDriver{importFn: func(ctx context.Context, inst *protocol.Instance, path string) error {
		calls++
		if calls == 1 {
			inst.Spec.CPU = 9
			return errors.New("incoming failure")
		}
		if ctx.Err() != nil {
			t.Fatal("rollback inherited cancelled context")
		}
		inst.Image = &protocol.Image{Path: "actual-restored-disk"}
		return nil
	}}
	s := agentForTest(t, d)
	old := testInstance(1)
	old.Spec.CPU = 2
	old.SSHReady = true
	s.instances[1] = old
	s.markBootReady(1, true)
	incoming := testInstance(1)
	incoming.Spec.CPU = 4
	rec := importRequest(s, incoming, true)
	if rec.Code != 502 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	got, _ := s.storedInstance(1)
	if got.Spec.CPU != 2 || got.Status != driver.StatusStopped || !s.bootReadyOf(1) || got.Image == nil || got.Image.Path != "actual-restored-disk" {
		t.Fatalf("rollback cache not restored: %+v ready=%v", got, s.bootReadyOf(1))
	}
	entries, _ := filepath.Glob(filepath.Join(s.dataDir, "rollback-*"))
	if len(entries) != 0 {
		t.Fatalf("successful rollback leaked archive %v", entries)
	}
}
func TestReplaceRollbackFailureRetainsArchiveWithoutGenericDelete(t *testing.T) {
	deletes := 0
	d := &testDriver{deleteFn: func(context.Context, *protocol.Instance) error { deletes++; return nil }, importFn: func(context.Context, *protocol.Instance, string) error { return errors.New("cannot import") }}
	s := agentForTest(t, d)
	s.instances[1] = testInstance(1)
	s.markBootReady(1, true)
	rec := importRequest(s, testInstance(1), true)
	if rec.Code != 502 || !strings.Contains(rec.Body.String(), "rollback failed") {
		t.Fatal(rec.Code, rec.Body.String())
	}
	archives, _ := filepath.Glob(filepath.Join(s.dataDir, "rollback-*", "backup*"))
	if len(archives) != 1 {
		t.Fatalf("recovery archive lost: %v", archives)
	}
	if b, err := os.ReadFile(archives[0]); err != nil || string(b) != "original" {
		t.Fatal("invalid retained recovery archive", err)
	}
	if deletes != 1 {
		t.Fatalf("failed import triggered generic delete (could destroy unrelated target): %d", deletes)
	}
	if s.bootReadyOf(1) {
		t.Fatal("unrecoverable failure retained readiness")
	}
}
func TestImportClearsReadinessWithoutResettingPassword(t *testing.T) {
	d := &testDriver{importFn: func(_ context.Context, inst *protocol.Instance, _ string) error {
		if inst.RootPassword != "" {
			t.Fatal("recovery must not send a bootstrap password")
		}
		return nil
	}}
	s := agentForTest(t, d)
	s.markBootReady(1, true)
	i := testInstance(1)
	i.RootPassword = "unused"
	i.SSHReady = true
	rec := importRequest(s, i, false)
	if rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	if s.bootReadyOf(1) || strings.Contains(rec.Body.String(), "ssh_ready\":true") {
		t.Fatal("backup import falsely reported SSH ready")
	}
}
