package db

import (
	"context"
	"testing"

	"github.com/recurser/bossd/internal/domain"
)

func TestCredentialVersionRejectsRollback(t *testing.T) {
	ctx := context.Background()
	store := NewAgentAccountStore(setupTestDB(t))
	a, err := store.Create(ctx, CreateAgentAccountParams{Provider: domain.AgentAccountProviderCodex, Label: "version"})
	if err != nil {
		t.Fatal(err)
	}
	if a.SyncedCredentialVersion != 0 {
		t.Fatal("independent credential has version")
	}
	if err := store.SetCredentialVersion(ctx, a.ID, 5); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCredentialVersion(ctx, a.ID, 4); err == nil {
		t.Fatal("rollback accepted")
	}
	got, err := store.Get(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SyncedCredentialVersion != 5 {
		t.Fatalf("version=%d", got.SyncedCredentialVersion)
	}
	rows, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].SyncedCredentialVersion != 5 {
		t.Fatal("list lost version")
	}
}
