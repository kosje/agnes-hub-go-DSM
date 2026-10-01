package hub

import (
	"context"
	"testing"
	"time"

	"agneshub/internal/config"
)

func TestMaintenanceShutdownFlushesBindings(t *testing.T) {
	h, store := newTestHub(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := h.StartMaintenance(ctx)
	store.Bind("shutdown-session", "account")
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("maintenance did not stop")
	}
	reloaded, err := config.NewStore(store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if b, ok := reloaded.BindingsGet("shutdown-session"); !ok || b.AccountID != "account" {
		t.Fatal("shutdown returned before persisting bindings")
	}
}
