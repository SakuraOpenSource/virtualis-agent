package main

import (
	"context"
	"github.com/SakuraOpenSource/virtualis-agent/internal/driver"
	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
	"testing"
)

func TestResizeEndpointPreservesReadinessAndNeverResetsPassword(t *testing.T) {
	called := false
	d := &testDriver{resize: func(_ context.Context, i *protocol.Instance, s protocol.InstanceSpec, n *protocol.NetworkConfig) error {
		called = true
		if i.RootPassword != "" {
			t.Fatal("resize leaked bootstrap password")
		}
		i.Spec = s
		if n != nil {
			i.Network = *n
		}
		return nil
	}}
	server := agentForTest(t, d)
	server.markBootReady(1, true)
	i := testInstance(1)
	i.RootPassword = "not-for-resize"
	s := i.Spec
	s.CPU = 2
	rec := requestJSON(server, "/api/instances/1/resize", map[string]any{"instance": i, "spec": s})
	if rec.Code != 200 || !called {
		t.Fatal(rec.Code, rec.Body.String())
	}
	got, _ := server.storedInstance(1)
	if got.Spec.CPU != 2 || got.RootPassword != "" || !got.SSHReady {
		t.Fatalf("invalid resized cache: %+v", got)
	}
}
func TestResizeEndpointRejectsActualRunningBeforeCallingCapability(t *testing.T) {
	called := false
	d := &testDriver{status: func(context.Context, *protocol.Instance) (string, error) { return driver.StatusRunning, nil }, resize: func(context.Context, *protocol.Instance, protocol.InstanceSpec, *protocol.NetworkConfig) error {
		called = true
		return nil
	}}
	server := agentForTest(t, d)
	i := testInstance(1)
	rec := requestJSON(server, "/api/instances/1/resize", map[string]any{"instance": i, "spec": i.Spec})
	if rec.Code != 409 || called {
		t.Fatal(rec.Code, called, rec.Body.String())
	}
}
