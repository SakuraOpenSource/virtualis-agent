package driver

import (
	"context"
	"errors"
	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
	"testing"
)

func TestQEMUStoppedCheckRejectsCommandFailure(t *testing.T) {
	ctx := WithCommandRunner(context.Background(), func(context.Context, string, ...string) ([]byte, error) {
		return []byte("permission denied"), errors.New("denied")
	})
	d := NewQEMUWithDataDir(t.TempDir())
	if err := requireStopped(ctx, d, &protocol.Instance{ID: 1, Name: "test"}); err == nil {
		t.Fatal("command failure mistaken for stopped; destructive operation allowed")
	}
}
func TestQEMUStoppedCheckRejectsIntermediateState(t *testing.T) {
	ctx := WithCommandRunner(context.Background(), func(context.Context, string, ...string) ([]byte, error) { return []byte("in shutdown"), nil })
	if err := requireStopped(ctx, NewQEMUWithDataDir(t.TempDir()), &protocol.Instance{ID: 1}); err == nil {
		t.Fatal("shutdown in progress mistaken for stopped")
	}
}
