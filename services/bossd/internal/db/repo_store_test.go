package db

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/recurser/bossalib/models"
)

// TestRepoStore_MergeStrategyRoundTrip verifies each valid strategy round-trips
// through the TEXT column as the typed enum, and that an out-of-band unknown or
// empty stored value is normalized back to the default MergeStrategyMerge by the
// scan path (ParseMergeStrategy), not surfaced as a raw invalid enum.
func TestRepoStore_MergeStrategyRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	store := NewRepoStore(db)
	ctx := context.Background()

	for _, strategy := range []models.MergeStrategy{
		models.MergeStrategyMerge,
		models.MergeStrategyRebase,
		models.MergeStrategySquash,
	} {
		repo := createTestRepo(t, store)
		s := strategy
		updated, err := store.Update(ctx, repo.ID, UpdateRepoParams{MergeStrategy: &s})
		if err != nil {
			t.Fatalf("update merge_strategy=%q: %v", strategy, err)
		}
		if updated.MergeStrategy != strategy {
			t.Errorf("update returned merge_strategy = %q, want %q", updated.MergeStrategy, strategy)
		}
		got, err := store.Get(ctx, repo.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.MergeStrategy != strategy {
			t.Errorf("stored merge_strategy = %q, want %q", got.MergeStrategy, strategy)
		}
		_ = store.Delete(ctx, repo.ID)
	}
}

func TestRepoStore_MergeStrategyNormalizesUnknown(t *testing.T) {
	db := setupTestDB(t)
	store := NewRepoStore(db)
	ctx := context.Background()

	for _, raw := range []string{"", "bogus", "MERGE"} {
		repo := createTestRepo(t, store)
		// Inject an out-of-band value directly, bypassing the typed writer.
		if _, err := db.ExecContext(ctx, `UPDATE repos SET merge_strategy = ? WHERE id = ?`, raw, repo.ID); err != nil {
			t.Fatalf("inject merge_strategy=%q: %v", raw, err)
		}
		got, err := store.Get(ctx, repo.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.MergeStrategy != models.MergeStrategyMerge {
			t.Errorf("stored raw %q read back as %q, want normalized %q", raw, got.MergeStrategy, models.MergeStrategyMerge)
		}
		_ = store.Delete(ctx, repo.ID)
	}
}

func TestRepoStore_SecretActionParams(t *testing.T) {
	dbConn := setupTestDB(t)
	store := NewRepoStore(dbConn)
	ctx := context.Background()
	repo := createTestRepo(t, store)

	// &"value" sets the key.
	set := "lin_value"
	if _, err := store.Update(ctx, repo.ID, UpdateRepoParams{LinearAPIKey: &set}); err != nil {
		t.Fatalf("set key: %v", err)
	}
	if got, _ := store.Get(ctx, repo.ID); got.LinearAPIKey != "lin_value" {
		t.Fatalf("after set, LinearAPIKey = %q, want %q", got.LinearAPIKey, "lin_value")
	}

	// nil leaves the stored key untouched (a save that does not re-supply it).
	other := "renamed"
	if _, err := store.Update(ctx, repo.ID, UpdateRepoParams{DisplayName: &other}); err != nil {
		t.Fatalf("update without key: %v", err)
	}
	if got, _ := store.Get(ctx, repo.ID); got.LinearAPIKey != "lin_value" {
		t.Fatalf("after nil, LinearAPIKey = %q, want unchanged %q", got.LinearAPIKey, "lin_value")
	}

	// &"" clears the key.
	empty := ""
	if _, err := store.Update(ctx, repo.ID, UpdateRepoParams{LinearAPIKey: &empty}); err != nil {
		t.Fatalf("clear key: %v", err)
	}
	if got, _ := store.Get(ctx, repo.ID); got.LinearAPIKey != "" {
		t.Fatalf("after clear, LinearAPIKey = %q, want empty", got.LinearAPIKey)
	}
}

func TestRepoStore_OptimisticConcurrency_MatchingToken(t *testing.T) {
	dbConn := setupTestDB(t)
	store := NewRepoStore(dbConn)
	ctx := context.Background()
	repo := createTestRepo(t, store)

	// Ensure the next write lands in a later millisecond so the bump is observable.
	time.Sleep(2 * time.Millisecond)

	token := repo.UpdatedAt
	name := "renamed"
	updated, err := store.Update(ctx, repo.ID, UpdateRepoParams{
		DisplayName:       &name,
		ExpectedUpdatedAt: &token,
	})
	if err != nil {
		t.Fatalf("update with matching token: %v", err)
	}
	if updated.DisplayName != "renamed" {
		t.Errorf("display_name = %q, want %q", updated.DisplayName, "renamed")
	}
	if !updated.UpdatedAt.After(token) {
		t.Errorf("updated_at %v not bumped past token %v", updated.UpdatedAt, token)
	}
}

func TestRepoStore_OptimisticConcurrency_StaleToken(t *testing.T) {
	dbConn := setupTestDB(t)
	store := NewRepoStore(dbConn)
	ctx := context.Background()
	repo := createTestRepo(t, store)

	// A token that does not match the stored updated_at.
	stale := repo.UpdatedAt.Add(-time.Hour)
	name := "should-not-apply"
	linear := "should-not-write"
	_, err := store.Update(ctx, repo.ID, UpdateRepoParams{
		DisplayName:       &name,
		LinearAPIKey:      &linear,
		ExpectedUpdatedAt: &stale,
	})
	if !errors.Is(err, ErrStaleRepoUpdate) {
		t.Fatalf("stale update err = %v, want ErrStaleRepoUpdate", err)
	}

	// Row must be fully untouched (atomic rollback — no field changed).
	got, err := store.Get(ctx, repo.ID)
	if err != nil {
		t.Fatalf("get after stale: %v", err)
	}
	if got.DisplayName != repo.DisplayName {
		t.Errorf("display_name changed to %q, want %q", got.DisplayName, repo.DisplayName)
	}
	if got.LinearAPIKey != repo.LinearAPIKey {
		t.Errorf("linear_api_key changed to %q, want %q", got.LinearAPIKey, repo.LinearAPIKey)
	}
	if !got.UpdatedAt.Equal(repo.UpdatedAt) {
		t.Errorf("updated_at changed to %v, want %v", got.UpdatedAt, repo.UpdatedAt)
	}
}

func TestRepoStore_OptimisticConcurrency_MissingRow(t *testing.T) {
	dbConn := setupTestDB(t)
	store := NewRepoStore(dbConn)
	ctx := context.Background()

	token := time.Now().UTC()
	name := "x"
	_, err := store.Update(ctx, "does-not-exist", UpdateRepoParams{
		DisplayName:       &name,
		ExpectedUpdatedAt: &token,
	})
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing-row update err = %v, want sql.ErrNoRows", err)
	}
}

// TestRepoStore_ArchiveSessionsAfterMerge_DefaultsTrue verifies a freshly created
// repo has the archive-after-merge flag ON (the double-default: migration DEFAULT 1
// plus the repos INSERT literal), since proto3's bool zero value is false.
func TestRepoStore_ArchiveSessionsAfterMerge_DefaultsTrue(t *testing.T) {
	store := NewRepoStore(setupTestDB(t))
	repo := createTestRepo(t, store)
	if !repo.ArchiveSessionsAfterMerge {
		t.Fatal("new repos should default to archive-after-merge ON")
	}
}

// TestRepoStore_ArchiveSessionsAfterMerge_Update verifies the flag toggles off
// through UpdateRepoParams and reads back false.
func TestRepoStore_ArchiveSessionsAfterMerge_Update(t *testing.T) {
	store := NewRepoStore(setupTestDB(t))
	ctx := context.Background()
	repo := createTestRepo(t, store)

	off := false
	updated, err := store.Update(ctx, repo.ID, UpdateRepoParams{ArchiveSessionsAfterMerge: &off})
	if err != nil {
		t.Fatalf("update archive_sessions_after_merge: %v", err)
	}
	if updated.ArchiveSessionsAfterMerge {
		t.Error("update returned archive_sessions_after_merge = true, want false")
	}
	got, err := store.Get(ctx, repo.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ArchiveSessionsAfterMerge {
		t.Error("stored archive_sessions_after_merge = true, want false")
	}
}

// TestRepoStore_CanAutoDeleteBranches_DefaultsTrue verifies a freshly created
// repo has the auto-delete-branches flag ON (the double-default: migration
// DEFAULT 1 plus the repos INSERT literal), since proto3's bool zero value is
// false.
func TestRepoStore_CanAutoDeleteBranches_DefaultsTrue(t *testing.T) {
	store := NewRepoStore(setupTestDB(t))
	repo := createTestRepo(t, store)
	if !repo.CanAutoDeleteBranches {
		t.Fatal("new repos should default to auto-delete-branches ON")
	}
}

// TestRepoStore_CanAutoDeleteBranches_Update verifies the flag toggles off
// through UpdateRepoParams and reads back false.
func TestRepoStore_CanAutoDeleteBranches_Update(t *testing.T) {
	store := NewRepoStore(setupTestDB(t))
	ctx := context.Background()
	repo := createTestRepo(t, store)

	off := false
	updated, err := store.Update(ctx, repo.ID, UpdateRepoParams{CanAutoDeleteBranches: &off})
	if err != nil {
		t.Fatalf("update can_auto_delete_branches: %v", err)
	}
	if updated.CanAutoDeleteBranches {
		t.Error("update returned can_auto_delete_branches = true, want false")
	}
	got, err := store.Get(ctx, repo.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.CanAutoDeleteBranches {
		t.Error("stored can_auto_delete_branches = true, want false")
	}
}
