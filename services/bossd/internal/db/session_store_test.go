package db

import (
	"context"
	"testing"
	"time"
)

// TestUpdateRepairDiagnostics_CountResetsOnSuccess pins the semantic that
// last_repair_attempt_count tracks consecutive failures since the last
// clean run, not total attempts. Regression guard for the cursor-bot
// finding that fail → succeed → fail used to render "⚠ repair failed (3×)"
// when only one consecutive failure had occurred.
func TestUpdateRepairDiagnostics_CountResetsOnSuccess(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess, err := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "Repair diagnostics test",
		WorktreePath: "/tmp/wt/repair",
		BranchName:   "feat/repair",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	mustUpdate := func(runnerErr, exitErr string) {
		t.Helper()
		err := sessionStore.UpdateRepairDiagnostics(ctx, UpdateRepairDiagnosticsParams{
			SessionID:   sess.ID,
			StartedAt:   time.Now(),
			RunnerError: runnerErr,
			ExitError:   exitErr,
		})
		if err != nil {
			t.Fatalf("update repair diagnostics: %v", err)
		}
	}

	count := func() int {
		t.Helper()
		got, err := sessionStore.Get(ctx, sess.ID)
		if err != nil {
			t.Fatalf("get session: %v", err)
		}
		return got.LastRepairAttemptCount
	}

	// Two failures in a row → count climbs.
	mustUpdate("claude not on PATH", "")
	if got := count(); got != 1 {
		t.Fatalf("after 1st failure, count=%d, want 1", got)
	}
	mustUpdate("", "exit status 1")
	if got := count(); got != 2 {
		t.Fatalf("after 2nd failure, count=%d, want 2", got)
	}

	// A clean attempt resets the counter.
	mustUpdate("", "")
	if got := count(); got != 0 {
		t.Fatalf("after success, count=%d, want 0", got)
	}

	// The next failure starts a fresh streak — not 3×.
	mustUpdate("", "exit status 2")
	if got := count(); got != 1 {
		t.Fatalf("after fail-after-success, count=%d, want 1 (consecutive failures, not total attempts)", got)
	}
}
