package driver

import (
	"context"
	"fmt"
	"strconv"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

func antiSpoofChain(id uint) string { return "VIRTIS-AS-" + strconv.FormatUint(uint64(id), 10) }

func applyDedicatedProtection(ctx context.Context, inst *protocol.Instance, a dedicatedAttachment, qemu bool) error {
	if !commandAvailable(ctx, "iptables") || !commandAvailable(ctx, "iptables-restore") {
		return fmt.Errorf("独立网络防伪造需要 iptables/iptables-restore")
	}
	chain := antiSpoofChain(inst.ID)
	match := "-i " + ownedNIC(inst.ID)
	if qemu && a.Mode == "routed" {
		match = "-i " + ownedBridge(inst.ID)
	}
	if a.Mode == "bridge" {
		match = "-m physdev --physdev-in " + ownedNIC(inst.ID)
	}
	body := fmt.Sprintf("-A %s -s %s -j RETURN\n-A %s -j DROP\n-I PREROUTING 1 %s -j %s\n", chain, a.IP, chain, match, chain)
	firewallReplaceMu.Lock()
	err := restoreOwnedTable(ctx, "raw", "PREROUTING", []string{chain}, body)
	firewallReplaceMu.Unlock()
	if err != nil {
		return fmt.Errorf("独立网络防伪造规则失败: %w", err)
	}
	return ApplyFirewall(ctx, inst)
}
