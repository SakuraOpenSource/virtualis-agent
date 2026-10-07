package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SakuraOpenSource/virtualis-agent/internal/driver"
	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

type reconcileTestDriver struct {
	testDriver
	reconciles int
	cleaned    int
	stopped    atomic.Bool
}

func (d *reconcileTestDriver) ReconcileNetworkResources(_ context.Context, i *protocol.Instance) error {
	if i.Status == driver.StatusRunning {
		d.reconciles++
	} else {
		d.cleaned++
	}
	return nil
}

func TestFirewallUpdateAcceptsPolicyAndStatusReconcilesOwnedNetwork(t *testing.T) {
	d := &reconcileTestDriver{}
	d.status = func(context.Context, *protocol.Instance) (string, error) {
		if d.stopped.Load() {
			return driver.StatusStopped, nil
		}
		return driver.StatusRunning, nil
	}
	s := agentForTest(t, &d.testDriver)
	s.registry.Register(d)
	i := testInstance(1)
	i.Network = protocol.NetworkConfig{Mode: "dedicated", Bridge: "eth0", IPv4: "192.0.2.20/24"}
	i.Firewall = []protocol.FirewallRule{{Direction: "in", Action: "accept", Protocol: "tcp", PortStart: 22, Enabled: true}}
	s.instances[1] = i

	i.FirewallPolicy = &protocol.FirewallPolicy{Ingress: "drop", Egress: "accept"}
	push := map[string]any{"instance": i, "rules": i.Firewall}
	raw, _ := json.Marshal(push)
	r := httptest.NewRequest(http.MethodPost, "/api/instances/1/firewall", strings.NewReader(string(raw)))
	r.Header.Set("X-Agent-Token", "token")
	r = r.WithContext(driver.WithCommandRunner(r.Context(), func(context.Context, string, ...string) ([]byte, error) { return []byte(""), nil }))
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	stored, _ := s.storedInstance(1)
	if stored.FirewallPolicy == nil || stored.FirewallPolicy.Ingress != "drop" {
		t.Fatal("firewall policy dropped by agent merge", stored.FirewallPolicy)
	}

	for n := 0; n < 2; n++ {
		r := httptest.NewRequest(http.MethodPost, "/api/instances/1/status", strings.NewReader(`{"instance":{"id":1}}`))
		r.Header.Set("X-Agent-Token", "token")
		r = r.WithContext(driver.WithCommandRunner(r.Context(), func(context.Context, string, ...string) ([]byte, error) { return []byte(""), nil }))
		rec := httptest.NewRecorder()
		s.handler().ServeHTTP(rec, r)
		if rec.Code != 200 {
			t.Fatal(rec.Code, rec.Body.String())
		}
	}
	if d.reconciles != 2 {
		t.Fatal("running instance owned resources not re-applied on status", d.reconciles)
	}

	s.mu.Lock()
	s.mu.Unlock()
	d.stopped.Store(true)
	r2 := httptest.NewRequest(http.MethodPost, "/api/instances/1/status", strings.NewReader(`{"instance":{"id":1}}`))
	r2.Header.Set("X-Agent-Token", "token")
	r2 = r2.WithContext(driver.WithCommandRunner(r2.Context(), func(context.Context, string, ...string) ([]byte, error) { return []byte(""), nil }))
	rec2 := httptest.NewRecorder()
	s.handler().ServeHTTP(rec2, r2)
	if rec2.Code != 200 {
		t.Fatal(rec2.Code, rec2.Body.String())
	}
	if d.cleaned != 1 {
		t.Fatal("stopped instance owned resources not cleaned", d.cleaned)
	}
}
