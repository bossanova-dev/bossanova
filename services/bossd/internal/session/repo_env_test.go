package session

import (
	"context"
	"errors"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
)

// repoEnvStore is a minimal db.RepoStore stub exercising only Get; every other
// method panics so a test that accidentally calls one fails loudly.
type repoEnvStore struct {
	db.RepoStore
	repo *models.Repo
	err  error
	gets int
}

func (s *repoEnvStore) Get(context.Context, string) (*models.Repo, error) {
	s.gets++
	return s.repo, s.err
}

func TestRepoForSessionEnv(t *testing.T) {
	ctx := context.Background()
	log := zerolog.Nop()
	want := &models.Repo{LinearAPIKey: "lin"}

	t.Run("returns repo on success", func(t *testing.T) {
		store := &repoEnvStore{repo: want}
		got := RepoForSessionEnv(ctx, store, "repo-1", "sess-1", "test", log)
		if got != want {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("nil store is a no-op returning nil without a lookup", func(t *testing.T) {
		// Passing a typed-nil *repoEnvStore would still be non-nil at the
		// interface; pass an untyped nil db.RepoStore to exercise the guard.
		var store db.RepoStore
		if got := RepoForSessionEnv(ctx, store, "repo-1", "sess-1", "test", log); got != nil {
			t.Fatalf("got %v, want nil for nil store", got)
		}
	})

	t.Run("failed lookup returns nil (non-fatal) and does not surface the error", func(t *testing.T) {
		store := &repoEnvStore{err: errors.New("boom")}
		got := RepoForSessionEnv(ctx, store, "repo-1", "sess-1", "test", log)
		if got != nil {
			t.Fatalf("got %v, want nil on lookup error", got)
		}
		if store.gets != 1 {
			t.Fatalf("Get called %d times, want 1", store.gets)
		}
	})
}
