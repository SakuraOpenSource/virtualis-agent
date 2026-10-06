package main

import (
	"context"
	"encoding/json"
	"github.com/SakuraOpenSource/virtualis-agent/internal/driver"
	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBootstrapRetainsInstanceBusyLease(t *testing.T) {
	entered, leave, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	d := &testDriver{status: func(context.Context, *protocol.Instance) (string, error) { return driver.StatusRunning, nil }, passwordFn: func(context.Context, *protocol.Instance, string) error {
		close(entered)
		<-leave
		close(finished)
		return nil
	}}
	s := agentForTest(t, d)
	i := testInstance(1)
	i.RootPassword = "bootstrap"
	rec := requestJSON(s, "/api/instances", map[string]any{"instance": i})
	if rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	defer func() { close(leave); <-finished }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("bootstrap never started")
	}
	if r := requestJSON(s, "/api/instances/1/status", map[string]any{"instance": i}); r.Code != 409 {
		t.Fatal("bootstrap lease released before background work", r.Code)
	}
}
func TestTrafficEnforcerSkipsBusyInstances(t *testing.T) {
	var samples atomic.Int32
	d := &testDriver{metricsFn: func(context.Context, *protocol.Instance) (protocol.Metrics, error) {
		samples.Add(1)
		return protocol.Metrics{}, nil
	}}
	s := agentForTest(t, d)
	i := testInstance(1)
	i.Network.TrafficGB = 1
	s.instances[1] = i
	l, _ := s.tryOperation(1)
	s.enforceTrafficQuotas(context.Background())
	l.done()
	if samples.Load() != 0 {
		t.Fatal("busy recovery raced traffic enforcement")
	}
	s.enforceTrafficQuotas(context.Background())
	if samples.Load() != 1 {
		t.Fatal("lease leaked after operation")
	}
}
func TestNetworkRoutesReconcileObservedDHCP(t *testing.T) {
	for _, route := range []string{"network", "network/configure", "firewall", "power"} {
		t.Run(route, func(t *testing.T) {
			d := &testDriver{status: func(context.Context, *protocol.Instance) (string, error) { return driver.StatusRunning, nil }, networkFn: func(context.Context, *protocol.Instance) (protocol.NetworkStatus, error) {
				return protocol.NetworkStatus{Interfaces: []protocol.NetworkInterface{{Name: "eth0", IPv4: []string{"10.100.0.10/24"}}}}, nil
			}}
			s := agentForTest(t, d)
			i := testInstance(1)
			i.Network = protocol.NetworkConfig{Mode: "vpc", Bridge: "vpc"}
			i.Firewall = []protocol.FirewallRule{{Direction: "in", Action: "drop", Protocol: "any", Enabled: true}}
			raw, _ := json.Marshal(map[string]any{"instance": i, "rules": i.Firewall, "action": "start"})
			r := httptest.NewRequest(http.MethodPost, "/api/instances/1/"+route, strings.NewReader(string(raw)))
			r.Header.Set("X-Agent-Token", "token")
			r = r.WithContext(driver.WithCommandRunner(r.Context(), func(context.Context, string, ...string) ([]byte, error) { return []byte(""), nil }))
			rec := httptest.NewRecorder()
			s.handler().ServeHTTP(rec, r)
			if rec.Code != 200 {
				t.Fatal(rec.Code, rec.Body.String())
			}
			stored, _ := s.storedInstance(1)
			if stored.ObservedIP != "10.100.0.10" || stored.Network.IPv4 != "" {
				t.Fatal("network route lost runtime DHCP address", stored)
			}
		})
	}
}

func TestStatusObservesDHCPFirewallWithoutPromotingStaticConfig(t *testing.T) {
	d := &testDriver{status: func(context.Context, *protocol.Instance) (string, error) { return driver.StatusRunning, nil }, networkFn: func(context.Context, *protocol.Instance) (protocol.NetworkStatus, error) {
		return protocol.NetworkStatus{Interfaces: []protocol.NetworkInterface{{Name: "eth0", IPv4: []string{"10.100.0.9/24"}}}}, nil
	}}
	s := agentForTest(t, d)
	i := testInstance(1)
	i.Network = protocol.NetworkConfig{Mode: "vpc", Bridge: "vpc"}
	i.Firewall = []protocol.FirewallRule{{Direction: "in", Action: "drop", Protocol: "any", Enabled: true}}
	raw, _ := json.Marshal(map[string]any{"instance": i})
	r := httptest.NewRequest(http.MethodPost, "/api/instances/1/status", strings.NewReader(string(raw)))
	r.Header.Set("X-Agent-Token", "token")
	r = r.WithContext(driver.WithCommandRunner(r.Context(), func(context.Context, string, ...string) ([]byte, error) { return []byte(""), nil }))
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	stored, _ := s.storedInstance(1)
	if stored.ObservedIP != "10.100.0.9" || stored.Network.IPv4 != "" {
		t.Fatal("runtime DHCP observation missing or became static", stored)
	}
}
