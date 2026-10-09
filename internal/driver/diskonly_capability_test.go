package driver

import (
	"context"
	"strings"
	"testing"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

func TestIncusSnapshotRestoreRejectsMissingDiskOnlyExtensionBeforeMutation(t *testing.T) {
	mutated:=false
	ctx:=WithCommandRunner(context.Background(),func(_ context.Context,_ string,a ...string)([]byte,error){
		if a[0]=="query" && a[1]=="/1.0" {return []byte(`{"api_extensions":[]}`),nil}
		if a[0]=="list" {return []byte("virtualis-1-test,STOPPED\n"),nil}
		if a[0]=="snapshot" {mutated=true}
		return []byte(""),nil
	})
	err:=NewIncusWithDataDir(t.TempDir()).RestoreSnapshot(ctx,&protocol.Instance{ID:1,Name:"test"},"saved")
	if err==nil || !strings.Contains(err.Error(),"instance_snapshot_disk_only_restore") || mutated {t.Fatalf("old node did not fail closed: %v mutation=%v",err,mutated)}
}
