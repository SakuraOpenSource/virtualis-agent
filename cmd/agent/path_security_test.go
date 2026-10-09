package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

func deleteRequest(s *agentServer, instance protocol.Instance) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(map[string]any{"instance": instance})
	req := httptest.NewRequest(http.MethodDelete, "/api/instances/1", strings.NewReader(string(raw)))
	req.Header.Set("X-Agent-Token", "token")
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)
	return rec
}

func TestDeleteRefusesUnregisteredIdentityAndNeverUnlinksClientPath(t *testing.T) {
	deletes := 0
	s := agentForTest(t, &testDriver{deleteFn: func(context.Context, *protocol.Instance) error { deletes++; return nil }})
	victim := filepath.Join(t.TempDir(), "host-secret")
	if err := os.WriteFile(victim, []byte("keep"), 0600); err != nil { t.Fatal(err) }
	i := testInstance(1)
	i.Image = &protocol.Image{Path: victim}
	rec := deleteRequest(s, i)
	if rec.Code != http.StatusConflict || deletes != 0 {
		t.Errorf("unregistered deletion = %d, driver deletions = %d, want conflict without mutation", rec.Code, deletes)
	}
	if b, err := os.ReadFile(victim); err != nil || string(b) != "keep" {
		t.Fatalf("client path was deleted or modified: %v", err)
	}
}

func TestReplacementStagesAndVerifiesBeforeOriginalDeletion(t *testing.T) {
    deletes, staged := 0, false
    d := &testDriver{deleteFn:func(context.Context,*protocol.Instance)error{deletes++;return nil}, replaceFn:func(_ context.Context,old,incoming *protocol.Instance,_ string)error{
        staged=true
        if deletes!=0 { t.Error("original deleted before staging") }
        return nil
    }}
    s:=agentForTest(t,d)
    s.instances[1]=testInstance(1)
    r:=importRequest(s,testInstance(1),true)
    if r.Code!=200 || !staged || deletes!=0 { t.Fatalf("replace must delegate staged cutover, code=%d staged=%v deletes=%d",r.Code,staged,deletes) }
}

func TestDeleteOwnershipSurvivesAgentRestart(t *testing.T) {
	s:=agentForTest(t,&testDriver{})
	i:=testInstance(1)
	r:=requestJSON(s,"/api/instances",map[string]any{"instance":i})
	if r.Code!=200 {t.Fatal(r.Code,r.Body.String())}
	restarted:=newAgentServer("token","test","dev",s.dataDir)
	restarted.registry.Register(&testDriver{})
	r=deleteRequest(restarted,i)
	if r.Code!=http.StatusNoContent {t.Fatalf("registered identity lost across restart: %d %s",r.Code,r.Body.String())}
}

func TestStatusDoesNotAuthorizeUnregisteredDeletion(t *testing.T) {
	deletes:=0
	s:=agentForTest(t,&testDriver{deleteFn:func(context.Context,*protocol.Instance)error{deletes++;return nil}})
	i:=testInstance(1)
	requestJSON(s,"/api/instances/1/status",map[string]any{"instance":i})
	r:=deleteRequest(s,i)
	if r.Code!=http.StatusConflict || deletes!=0 {t.Fatal("status request granted destructive ownership",r.Code,deletes)}
}

func TestCreateRejectsWireImagePaths(t *testing.T) {
	s := agentForTest(t, &testDriver{})
	i := testInstance(1)
	i.Image = &protocol.Image{Path: filepath.Join(s.dataDir, "images", "other-instance.qcow2"), ExtraPath: "/etc/secret.iso"}
	r := requestJSON(s, "/api/instances", map[string]any{"instance": i})
	if r.Code != http.StatusBadRequest { t.Fatalf("wire host path accepted: %d %s", r.Code, r.Body.String()) }
}

func TestPowerRejectsWireImagePaths(t *testing.T) {
	s := agentForTest(t, &testDriver{})
	i := testInstance(1)
	i.Image = &protocol.Image{Path: "/etc/secret.qcow2"}
	r := requestJSON(s, "/api/instances/1/power", map[string]any{"instance": i, "action":"start"})
	if r.Code != http.StatusBadRequest { t.Fatalf("wire host path accepted: %d %s", r.Code, r.Body.String()) }
}

func TestDeleteRefusesStoredIdentityMismatch(t *testing.T) {
	deletes := 0
	s := agentForTest(t, &testDriver{deleteFn: func(context.Context, *protocol.Instance) error { deletes++; return nil }})
	s.instances[1] = testInstance(1)
	i := testInstance(1)
	i.Name = "different"
	r := deleteRequest(s, i)
	if r.Code != http.StatusConflict || deletes != 0 { t.Fatalf("identity mismatch deleted runtime: %d, %d", r.Code, deletes) }
}
