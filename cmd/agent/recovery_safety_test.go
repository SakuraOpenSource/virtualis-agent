package main

import (
	"bytes"
	"context"
	"errors"
	"github.com/SakuraOpenSource/virtualis-agent/internal/driver"
	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReplaceRefusesStoredIdentityChange(t *testing.T) {
	deletes := 0
	d := &testDriver{deleteFn: func(context.Context, *protocol.Instance) error { deletes++; return nil }}
	s := agentForTest(t, d)
	s.instances[1] = testInstance(1)
	i := testInstance(1)
	i.Name = "other-target"
	r := importRequest(s, i, true)
	if r.Code != 409 || deletes != 0 {
		t.Fatal("replacement deleted different stored identity", r.Code, deletes, r.Body.String())
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
	s := agentForTest(t, d)
	r := importRequest(s, testInstance(1), true)
	if r.Code != 502 {
		t.Fatal("unverified import reported success", r.Code, r.Body.String())
	}
	p, _ := filepath.Glob(filepath.Join(s.dataDir, "rollback-*", "backup"))
	if len(p) != 1 {
		t.Fatal("original archive removed before verification", p)
	}
}
func TestSnapshotRestoreInvalidatesCachedReadiness(t *testing.T) {
	s := agentForTest(t, &testDriver{})
	i := testInstance(1)
	i.ObservedIP = "10.1.0.9"
	i.SSHReady = true
	s.instances[1] = i
	s.markBootReady(1, true)
	r := requestJSON(s, "/api/instances/1/snapshots", map[string]any{"instance": i, "action": "restore", "name": "saved"})
	if r.Code != 200 {
		t.Fatal(r.Code)
	}
	got, _ := s.storedInstance(1)
	if s.bootReadyOf(1) || got.SSHReady || got.ObservedIP != "" {
		t.Fatal("snapshot restore retained stale guest readiness", got)
	}
}
func TestMultipartCleanupRemovesSpilledRecoveryUpload(t *testing.T) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("instance", `{"id":1,"name":"test","driver":"test"}`)
	f, _ := mw.CreateFormFile("image", "backup.tar")
	io.Copy(f, io.LimitReader(strings.NewReader(strings.Repeat("x", 33<<20)), 33<<20))
	mw.Close()
	r := httptest.NewRequest(http.MethodPost, "/api/instances/1/import", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	_, file, _, _, _, cleanup, e := parseInstance(r)
	if e != nil {
		t.Fatal(e)
	}
	spill, ok := file.(*os.File)
	if !ok {
		t.Fatal("fixture did not spill to disk")
	}
	p := spill.Name()
	cleanup()
	if _, e := os.Stat(p); !os.IsNotExist(e) {
		t.Fatal("multipart spill leaked after cleanup", p, e)
	}
}
