package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
