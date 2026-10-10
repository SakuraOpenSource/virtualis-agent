package driver

import (
	"context"
	"fmt"
)

const diskOnlyRestoreExtension = "instance_snapshot_disk_only_restore"

func (d *Incus) snapshotDiskOnlyCapable(ctx context.Context) (bool, error) {
	raw, err := output(ctx, d.cli(), "query", "/1.0")
	if err != nil {
		return false, fmt.Errorf("cannot probe Incus snapshot restore capabilities: %w", err)
	}
	var server struct {
		Extensions []string `json:"api_extensions"`
	}
	if err = incusJSON(raw, &server); err != nil {
		return false, err
	}
	for _, extension := range server.Extensions {
		if extension == diskOnlyRestoreExtension {
			return true, nil
		}
	}
	return false, nil
}
