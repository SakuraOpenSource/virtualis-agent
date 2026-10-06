package driver

import (
	"context"
	"errors"
	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
	"os"
	"path/filepath"
	"testing"
)

func TestQEMUDeleteCleansOrphanSnapshotsButNotOnExistenceFailure(t *testing.T) {
	for _, offline := range []bool{false, true} {
		t.Run(map[bool]string{true: "offline", false: "missing"}[offline], func(t *testing.T) {
			d := NewQEMUWithDataDir(t.TempDir())
			p, _ := d.snapshotPath(1, "saved")
			os.MkdirAll(filepath.Dir(p), 0700)
			os.WriteFile(p, []byte("keep"), 0600)
			ctx := WithCommandRunner(context.Background(), func(context.Context, string, ...string) ([]byte, error) {
				if offline {
					return nil, errors.New("offline")
				}
				return []byte(""), nil
			})
			e := d.Delete(ctx, &protocol.Instance{ID: 1, Name: "test"})
			_, stat := os.Stat(p)
			if offline && (e == nil || stat != nil) {
				t.Fatal("query failure treated as missing and deleted data", e, stat)
			}
			if !offline && (e != nil || !os.IsNotExist(stat)) {
				t.Fatal("orphan snapshots retained", e, stat)
			}
		})
	}
}
func TestIncusStoppedSafetyRejectsEmptyFrozenAndUnknownState(t *testing.T) {
	for _, state := range []string{"", "virtualis-1-test,FROZEN", "virtualis-1-test,STARTING", "virtualis-1-test,ERROR"} {
		t.Run(state, func(t *testing.T) {
			ctx := WithCommandRunner(context.Background(), func(context.Context, string, ...string) ([]byte, error) { return []byte(state), nil })
			if e := requireStopped(ctx, NewIncus(), &protocol.Instance{ID: 1, Name: "test"}); e == nil {
				t.Fatal("unconfirmed state accepted as stopped")
			}
		})
	}
}
