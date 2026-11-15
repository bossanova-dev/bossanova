package db

import (
	"context"
	"testing"

	"github.com/recurser/bossd/internal/domain"
)

func TestRepoStore_ServeScriptRoundTrip(t *testing.T) {
	store := NewRepoStore(setupTestDB(t))
	ctx := context.Background()
	setup, serve := "make setup", "make serve"
	repo, err := store.Create(ctx, domain.CreateRepoParams{
		DisplayName: "serve", LocalPath: t.TempDir(), SetupScript: &setup, ServeScript: &serve,
	})
	if err != nil {
		t.Fatal(err)
	}
	check := func(want *string) {
		t.Helper()
		got, err := store.Get(ctx, repo.ID)
		if err != nil {
			t.Fatal(err)
		}
		if (got.ServeScript == nil) != (want == nil) || (want != nil && *got.ServeScript != *want) {
			t.Fatalf("ServeScript = %v, want %v", got.ServeScript, want)
		}
		if got.SetupScript == nil || *got.SetupScript != setup {
			t.Fatalf("SetupScript = %v, want %q", got.SetupScript, setup)
		}
	}
	check(&serve)
	updated := "make dev"
	value := &updated
	if _, err := store.Update(ctx, repo.ID, UpdateRepoParams{ServeScript: &value}); err != nil {
		t.Fatal(err)
	}
	check(&updated)
	if _, err := store.Update(ctx, repo.ID, UpdateRepoParams{}); err != nil {
		t.Fatal(err)
	}
	check(&updated)
	if _, err := store.Update(ctx, repo.ID, UpdateRepoParams{ServeScript: new(*string)}); err != nil {
		t.Fatal(err)
	}
	check(nil)
}
