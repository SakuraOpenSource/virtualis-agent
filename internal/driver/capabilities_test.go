package driver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestCapabilitiesAdvertiseFirewallPolicy guards the master<->agent contract:
// virtualis master's RequireFirewallPolicy rejects instance creation unless a
// driver reports {"firewall_policy": true}. The JSON field must therefore be
// present; drivers implementing transactional policy delivery set it to true.
func TestCapabilitiesAdvertiseFirewallPolicy(t *testing.T) {
	reg := NewRegistryWithDataDir(t.TempDir())
	// incus/qemu binary may be missing on the test host; capability flags must
	// round-trip regardless of probe outcome.
	payload, err := json.Marshal(map[string]any{"items": reg.Capabilities(context.Background())})
	if err != nil {
		t.Fatalf("marshal capabilities: %v", err)
	}
	var wrapped struct {
		Items []Capability `json:"items"`
	}
	if err := json.Unmarshal(payload, &wrapped); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(wrapped.Items) == 0 {
		t.Fatalf("no drivers registered")
	}
	raw := string(payload)
	if !strings.Contains(raw, "firewall_policy") {
		t.Fatalf("capabilities payload missing firewall_policy field: %s", raw)
	}
	for _, item := range wrapped.Items {
		if item.Name != "incus" && item.Name != "qemu" {
			continue
		}
		// Both shipped drivers implement policy delivery; if the probe says the
		// driver is usable the flag must be advertised as true.
		if item.Available && !item.FirewallPolicy {
			t.Fatalf("driver %s available but firewall_policy not advertised", item.Name)
		}
	}
}
