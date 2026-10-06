package driver

import (
	"context"
	"fmt"
	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
	"net"
	"sort"
	"strings"
)

func commandAvailable(ctx context.Context, name string) bool {
	if _, ok := ctx.Value(commandRunnerKey{}).(CommandRunner); ok {
		return true
	}
	return hasCommand(name)
}

// ObservedIPv4 ignores guest loopback, link-local and multicast addresses. The
// configured MAC wins over interface ordering (QEMU and Incus use different names).
func ObservedIPv4(network protocol.NetworkStatus, mac string) string {
	interfaces := append([]protocol.NetworkInterface{}, network.Interfaces...)
	sort.SliceStable(interfaces, func(i, j int) bool {
		a, b := interfaces[i], interfaces[j]
		am, bm := mac != "" && strings.EqualFold(a.MAC, mac), mac != "" && strings.EqualFold(b.MAC, mac)
		if am != bm {
			return am
		}
		if (a.Name == "eth0") != (b.Name == "eth0") {
			return a.Name == "eth0"
		}
		return a.Name < b.Name
	})
	for _, iface := range interfaces {
		if iface.Name == "lo" || strings.EqualFold(iface.State, "down") {
			continue
		}
		if mac != "" && !strings.EqualFold(iface.MAC, mac) {
			continue
		}
		for _, address := range iface.IPv4 {
			ip := net.ParseIP(strings.Split(address, "/")[0])
			if ip != nil && ip.To4() != nil && ip.IsGlobalUnicast() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
				return ip.String()
			}
		}
	}
	return ""
}
func ReconcileFirewall(ctx context.Context, d Driver, inst *protocol.Instance, observed *protocol.NetworkStatus) error {
	if inst.Status != StatusRunning {
		return nil
	}
	if observed == nil {
		n, e := d.Network(ctx, inst)
		if e != nil {
			inst.ObservedIP = ""
			return fmt.Errorf("observe firewall network: %w", e)
		}
		observed = &n
	}
	inst.ObservedIP = ObservedIPv4(*observed, inst.Network.MAC)
	return ApplyFirewall(ctx, inst)
}
