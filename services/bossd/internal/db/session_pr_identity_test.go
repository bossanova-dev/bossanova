package db

import (
	"context"
	"testing"
)

func newPRIdentitySession(t *testing.T) (*SQLiteSessionStore, string) {
	t.Helper()
	db := setupTestDB(t)
	store := NewSessionStore(db)
	repo := createTestRepo(t, NewRepoStore(db))
	sess, err := store.Create(context.Background(), CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "PR identity",
		WorktreePath: "/tmp/wt/pr-identity",
		BranchName:   "feat/pr-identity",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	setPRNumber(t, store, sess.ID, 12)
	return store, sess.ID
}

func setPRNumber(t *testing.T, store *SQLiteSessionStore, id string, n int) {
	t.Helper()
	ptr := &n
	if _, err := store.Update(context.Background(), id, UpdateSessionParams{PRNumber: &ptr}); err != nil {
		t.Fatalf("set PR number %d: %v", n, err)
	}
}

func assertPRIdentity(t *testing.T, store *SQLiteSessionStore, id, author, target string) {
	t.Helper()
	got, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.PRAuthorLogin != author || got.PRTargetBranch != target {
		t.Fatalf("stored (author, target) = (%q, %q); want (%q, %q)", got.PRAuthorLogin, got.PRTargetBranch, author, target)
	}
	if got.BaseBranch != "main" {
		t.Fatalf("base_branch = %q; want it left at the creation-time main", got.BaseBranch)
	}
}

func TestSetPRAuthorAndTarget(t *testing.T) {
	ctx := context.Background()

	t.Run("writes both values for the current PR", func(t *testing.T) {
		store, id := newPRIdentitySession(t)
		if err := store.SetPRAuthorAndTarget(ctx, id, 12, "octocat", "main"); err != nil {
			t.Fatal(err)
		}
		assertPRIdentity(t, store, id, "octocat", "main")
		// A retarget replaces the stored target.
		if err := store.SetPRAuthorAndTarget(ctx, id, 12, "octocat", "dev"); err != nil {
			t.Fatal(err)
		}
		assertPRIdentity(t, store, id, "octocat", "dev")
	})

	t.Run("ignores a result for another PR number", func(t *testing.T) {
		store, id := newPRIdentitySession(t)
		if err := store.SetPRAuthorAndTarget(ctx, id, 99, "octocat", "dev"); err != nil {
			t.Fatal(err)
		}
		assertPRIdentity(t, store, id, "", "")
	})

	t.Run("never replaces a stored value with an empty one", func(t *testing.T) {
		store, id := newPRIdentitySession(t)
		if err := store.SetPRAuthorAndTarget(ctx, id, 12, "octocat", "dev"); err != nil {
			t.Fatal(err)
		}
		if err := store.SetPRAuthorAndTarget(ctx, id, 12, "", ""); err != nil {
			t.Fatal(err)
		}
		assertPRIdentity(t, store, id, "octocat", "dev")
		if err := store.SetPRAuthorAndTarget(ctx, id, 12, "", "release"); err != nil {
			t.Fatal(err)
		}
		assertPRIdentity(t, store, id, "octocat", "release")
	})

	t.Run("changing the PR number clears both values", func(t *testing.T) {
		store, id := newPRIdentitySession(t)
		if err := store.SetPRAuthorAndTarget(ctx, id, 12, "octocat", "dev"); err != nil {
			t.Fatal(err)
		}
		// Re-asserting the same PR number keeps the values.
		setPRNumber(t, store, id, 12)
		assertPRIdentity(t, store, id, "octocat", "dev")
		setPRNumber(t, store, id, 13)
		assertPRIdentity(t, store, id, "", "")
	})

	t.Run("an update that leaves the PR number alone keeps the values", func(t *testing.T) {
		store, id := newPRIdentitySession(t)
		if err := store.SetPRAuthorAndTarget(ctx, id, 12, "octocat", "dev"); err != nil {
			t.Fatal(err)
		}
		title := "renamed"
		if _, err := store.Update(ctx, id, UpdateSessionParams{Title: &title}); err != nil {
			t.Fatal(err)
		}
		assertPRIdentity(t, store, id, "octocat", "dev")
	})
}
