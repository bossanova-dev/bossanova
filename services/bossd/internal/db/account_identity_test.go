package db

import (
	"context"
	"testing"

	"github.com/recurser/bossd/internal/domain"
)

func TestAccountIdentityRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := NewAgentAccountStore(setupTestDB(t))
	a, err := store.Create(ctx, CreateAgentAccountParams{Provider: domain.AgentAccountProviderCodex, Label: "identity"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Identity.Kind != "" || a.Identity.Digest != "" {
		t.Fatalf("migration default: %+v", a.Identity)
	}
	snap := domain.UsageSnapshot{Util5h: 0.5, Status: "ok"}
	if err := store.RecordUsageProbe(ctx, a.ID, snap); err != nil {
		t.Fatal(err)
	}
	check := domain.AuthCheck{Outcome: domain.AuthCheckOutcomeHealthy}
	if err := store.RecordAuthCheck(ctx, a.ID, check); err != nil {
		t.Fatal(err)
	}
	want := domain.AgentAccountIdentity{Kind: domain.AgentAccountIdentityKindCodexUser, Digest: "digest"}
	if err := store.SetIdentity(ctx, a.ID, want); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Identity != want || got.AuthCheck.Outcome != check.Outcome || got.Usage == nil || got.Usage.Util5h != snap.Util5h {
		t.Fatalf("unexpected stored metadata: %+v", got)
	}
	listed, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Identity != want {
		t.Fatalf("list identity: %+v", listed)
	}
	if err := store.SetIdentity(ctx, "missing", want); err != domain.ErrNotFound {
		t.Fatalf("missing: %v", err)
	}
}
