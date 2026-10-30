package main

import (
	"context"
	"testing"

	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/dbtest"
	"github.com/recurser/bossd/internal/domain"
)

func TestSyncedRefreshCannotBypassMissingCoordination(t *testing.T) {
	ctx := context.Background()
	store := db.NewAgentAccountStore(dbtest.New(t))
	a, err := store.Create(ctx, domain.CreateAgentAccountParams{Provider: "codex", Label: "shared"})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SetSync(ctx, a.ID, "synced", 1); err != nil {
		t.Fatal(err)
	}
	gate := daemonRefreshGate{w: &daemonWiring{accounts: store}}
	if _, err = gate.Acquire(ctx, a.ID, make([]byte, 32), "attempt"); err == nil {
		t.Fatal("synced credential acquired without coordination")
	}
	if err = gate.Receive(ctx, a.ID); err == nil {
		t.Fatal("synced credential received without coordination")
	}
	local, err := store.Create(ctx, domain.CreateAgentAccountParams{Provider: "codex", Label: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if acquired, err := gate.Acquire(ctx, local.ID, make([]byte, 32), "attempt"); err != nil || !acquired {
		t.Fatal("independent local account required cloud coordination")
	}
}
