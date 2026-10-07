package driver

import (
	"context"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

// ReconcileNetworkResources restores owned host protection/networking without
// restarting running guests. Missing support is harmless for legacy test drivers.
func ReconcileNetworkResources(ctx context.Context, d Driver, inst *protocol.Instance) error {
	if reconciler, ok := d.(interface {
		ReconcileNetworkResources(context.Context, *protocol.Instance) error
	}); ok {
		return reconciler.ReconcileNetworkResources(ctx, inst)
	}
	return nil
}

func (d *QEMU) ReconcileNetworkResources(ctx context.Context, inst *protocol.Instance) error {
	if inst.Status != StatusRunning {
		return cleanupDedicated(ctx, d.dataDir, inst.ID)
	}
	if NormalizeNetworkMode(inst.Network.Mode) == NetworkModeDedicated {
		return d.ensureNetwork(ctx, inst)
	}
	return nil
}

func (d *Incus) ensureDedicatedProtection(ctx context.Context, inst *protocol.Instance) error {
	a, err := resolveDedicated(ctx, inst.Network)
	if err != nil {
		return err
	}
	if err = ValidateFirewall(inst); err != nil {
		return err
	}
	if a.Mode == "bridge" {
		if err = ensureBridgeFiltering(ctx); err != nil {
			return err
		}
	}
	dedicatedResourcesMu.Lock()
	err = saveDedicatedRecord(d.dataDir, dedicatedRecord{ID: inst.ID, Driver: "incus", Attachment: a, Network: inst.Network})
	dedicatedResourcesMu.Unlock()
	if err != nil {
		return err
	}
	return applyDedicatedProtection(ctx, inst, a, false)
}

func (d *Incus) ReconcileNetworkResources(ctx context.Context, inst *protocol.Instance) error {
	if inst.Status != StatusRunning {
		return cleanupDedicated(ctx, d.dataDir, inst.ID)
	}
	if NormalizeNetworkMode(inst.Network.Mode) == NetworkModeDedicated {
		return d.ensureDedicatedProtection(ctx, inst)
	}
	return nil
}
