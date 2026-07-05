package db

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/recurser/bossalib/models"
)

// TestUpdateLastAttemptHeadSHA round-trips the BOS-235 last_attempt_head_sha
// column through the store: set a value, then clear it to NULL via the
// nullable-string double-pointer convention.
func TestUpdateLastAttemptHeadSHA(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess, err := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "last attempt head sha test",
		WorktreePath: "/tmp/wt/las",
		BranchName:   "feat/las",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Fresh session: column is NULL → nil.
	if sess.LastAttemptHeadSHA != nil {
		t.Fatalf("fresh LastAttemptHeadSHA = %q, want nil", *sess.LastAttemptHeadSHA)
	}

	// Set a value.
	head := "deadbeef"
	headPtr := &head
	got, err := sessionStore.Update(ctx, sess.ID, UpdateSessionParams{LastAttemptHeadSHA: &headPtr})
	if err != nil {
		t.Fatalf("update set: %v", err)
	}
	if got.LastAttemptHeadSHA == nil || *got.LastAttemptHeadSHA != "deadbeef" {
		t.Fatalf("LastAttemptHeadSHA = %v, want deadbeef", got.LastAttemptHeadSHA)
	}

	// Clear it to NULL (*nil inner pointer).
	var clear *string
	got, err = sessionStore.Update(ctx, sess.ID, UpdateSessionParams{LastAttemptHeadSHA: &clear})
	if err != nil {
		t.Fatalf("update clear: %v", err)
	}
	if got.LastAttemptHeadSHA != nil {
		t.Fatalf("LastAttemptHeadSHA = %q after clear, want nil", *got.LastAttemptHeadSHA)
	}
}

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
			SessionID:     sess.ID,
			StartedAt:     time.Now(),
			RunnerError:   runnerErr,
			ExitError:     exitErr,
			HeadSHA:       "abc123",
			DisplayStatus: 3,
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

func TestSessionPRMetadataAndCronLastRunRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	cronStore := NewCronJobStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess, err := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "Cron PR repair",
		WorktreePath: "/tmp/wt/cron-pr-repair",
		BranchName:   "cron/pr-repair",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	cron, err := cronStore.Create(ctx, CreateCronJobParams{
		RepoID:   repo.ID,
		Name:     "cron-pr-repair",
		Prompt:   "repair",
		Schedule: "* * * * *",
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create cron job: %v", err)
	}

	prNumber := 77
	prURL := "https://github.com/test/repo/pull/77"
	prNumberPtr := &prNumber
	prURLPtr := &prURL
	if _, err := sessionStore.Update(ctx, sess.ID, UpdateSessionParams{
		PRNumber: &prNumberPtr,
		PRURL:    &prURLPtr,
	}); err != nil {
		t.Fatalf("update session PR metadata: %v", err)
	}
	if err := cronStore.UpdateLastRun(ctx, cron.ID, UpdateCronJobLastRunParams{
		SessionID: &sess.ID,
		RanAt:     time.Now(),
		Outcome:   models.CronJobOutcomePRCreated,
	}); err != nil {
		t.Fatalf("update cron last run: %v", err)
	}

	gotSess, err := sessionStore.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if gotSess.PRNumber == nil || *gotSess.PRNumber != 77 {
		t.Fatalf("PRNumber = %v, want 77", gotSess.PRNumber)
	}
	if gotSess.PRURL == nil || *gotSess.PRURL != prURL {
		t.Fatalf("PRURL = %v, want %s", gotSess.PRURL, prURL)
	}
	gotCron, err := cronStore.Get(ctx, cron.ID)
	if err != nil {
		t.Fatalf("get cron job: %v", err)
	}
	if gotCron.LastRunSessionID == nil || *gotCron.LastRunSessionID != sess.ID {
		t.Fatalf("LastRunSessionID = %v, want %s", gotCron.LastRunSessionID, sess.ID)
	}
	if gotCron.LastRunOutcome == nil || *gotCron.LastRunOutcome != models.CronJobOutcomePRCreated {
		t.Fatalf("LastRunOutcome = %v, want pr_created", gotCron.LastRunOutcome)
	}
}

// TestSessionTmuxUnattendedRoundTrip pins the persistence contract for the
// tmux_unattended column added in BOS-208 Task 1: it defaults false on
// creation, an explicit Update sets it, and a nil-valued Update (the "don't
// touch this field" convention used throughout UpdateSessionParams) leaves
// the previously-set value unchanged.
func TestSessionTmuxUnattendedRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess, err := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "Tmux unattended round trip",
		WorktreePath: "/tmp/wt/tmux-unattended",
		BranchName:   "feat/tmux-unattended",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if sess.TmuxUnattended {
		t.Fatalf("TmuxUnattended = true on creation, want false (default)")
	}

	tmuxUnattended := true
	if _, err := sessionStore.Update(ctx, sess.ID, UpdateSessionParams{
		TmuxUnattended: &tmuxUnattended,
	}); err != nil {
		t.Fatalf("update session tmux_unattended: %v", err)
	}
	gotSess, err := sessionStore.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if !gotSess.TmuxUnattended {
		t.Fatalf("TmuxUnattended = false after Update(true), want true")
	}

	// An Update with a nil TmuxUnattended pointer must not touch the column.
	renamedTitle := "Tmux unattended round trip (renamed)"
	if _, err := sessionStore.Update(ctx, sess.ID, UpdateSessionParams{
		Title: &renamedTitle,
	}); err != nil {
		t.Fatalf("update session title: %v", err)
	}
	gotSess, err = sessionStore.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get session after unrelated update: %v", err)
	}
	if !gotSess.TmuxUnattended {
		t.Fatalf("TmuxUnattended = false after unrelated Update, want true (unchanged)")
	}
}

func TestUpdateRepairDiagnostics_RoundTripsAttemptIdentity(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess, err := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "Repair identity test",
		WorktreePath: "/tmp/wt/repair-identity",
		BranchName:   "feat/repair-identity",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	err = sessionStore.UpdateRepairDiagnostics(ctx, UpdateRepairDiagnosticsParams{
		SessionID:     sess.ID,
		StartedAt:     time.Now(),
		ExitError:     "exit status 1",
		HeadSHA:       "def456",
		DisplayStatus: 4,
	})
	if err != nil {
		t.Fatalf("update repair diagnostics: %v", err)
	}

	got, err := sessionStore.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.LastRepairHeadSHA != "def456" {
		t.Fatalf("LastRepairHeadSHA=%q, want def456", got.LastRepairHeadSHA)
	}
	if got.LastRepairDisplayStatus != 4 {
		t.Fatalf("LastRepairDisplayStatus=%d, want 4", got.LastRepairDisplayStatus)
	}
}

func TestUpdateRepairDiagnosticsPersistsReviewFingerprint(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess, err := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "Repair review fingerprint test",
		WorktreePath: "/tmp/wt/repair-review-fingerprint",
		BranchName:   "feat/repair-review-fingerprint",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	reviewFingerprint := "review-fp-1"
	err = sessionStore.UpdateRepairDiagnostics(ctx, UpdateRepairDiagnosticsParams{
		SessionID:         sess.ID,
		StartedAt:         time.Unix(1779238800, 0),
		HeadSHA:           "abc123",
		DisplayStatus:     5,
		ReviewFingerprint: &reviewFingerprint,
	})
	if err != nil {
		t.Fatalf("UpdateRepairDiagnostics: %v", err)
	}

	got, err := sessionStore.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.LastRepairReviewFingerprint != "review-fp-1" {
		t.Fatalf("LastRepairReviewFingerprint=%q, want review-fp-1", got.LastRepairReviewFingerprint)
	}
}

// TestRepairBlocked_WriteSetsReasonNotCount verifies that the blocked-refusal
// lane records the reason + timestamp WITHOUT bumping the consecutive-failure
// counter — a start-refusal must never count as a repair failure (BOS-153).
func TestRepairBlocked_WriteSetsReasonNotCount(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess, err := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "Repair blocked reason test",
		WorktreePath: "/tmp/wt/repair-blocked-reason",
		BranchName:   "feat/repair-blocked-reason",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Seed a real consecutive-failure count of 3.
	for i := 0; i < 3; i++ {
		if err := sessionStore.UpdateRepairDiagnostics(ctx, UpdateRepairDiagnosticsParams{
			SessionID:   sess.ID,
			StartedAt:   time.Now(),
			RunnerError: "claude not on PATH",
		}); err != nil {
			t.Fatalf("seed failure %d: %v", i, err)
		}
	}

	blockedAt := time.Unix(1779238800, 0)
	if err := sessionStore.UpdateRepairBlocked(ctx, sess.ID, blockedAt, "displace blocked: chat working"); err != nil {
		t.Fatalf("UpdateRepairBlocked: %v", err)
	}

	got, err := sessionStore.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.LastRepairBlockedReason != "displace blocked: chat working" {
		t.Fatalf("LastRepairBlockedReason=%q, want %q", got.LastRepairBlockedReason, "displace blocked: chat working")
	}
	if got.LastRepairBlockedAt == nil || !got.LastRepairBlockedAt.Equal(blockedAt) {
		t.Fatalf("LastRepairBlockedAt=%v, want %v", got.LastRepairBlockedAt, blockedAt)
	}
	if got.LastRepairAttemptCount != 3 {
		t.Fatalf("LastRepairAttemptCount=%d, want 3 (blocked write must not bump count)", got.LastRepairAttemptCount)
	}
}

// TestRepairBlocked_RealOutcomeClearsBlocked verifies that a real repair
// outcome (a failed run here) supersedes and clears the blocked pair while
// bumping the failure count (BOS-153).
func TestRepairBlocked_RealOutcomeClearsBlocked(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess, err := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "Repair blocked cleared test",
		WorktreePath: "/tmp/wt/repair-blocked-cleared",
		BranchName:   "feat/repair-blocked-cleared",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	if err := sessionStore.UpdateRepairBlocked(ctx, sess.ID, time.Unix(1779238800, 0), "displace blocked: tracker warming"); err != nil {
		t.Fatalf("UpdateRepairBlocked: %v", err)
	}
	if err := sessionStore.UpdateRepairDiagnostics(ctx, UpdateRepairDiagnosticsParams{
		SessionID: sess.ID,
		StartedAt: time.Now(),
		ExitError: "exit status 1",
	}); err != nil {
		t.Fatalf("UpdateRepairDiagnostics: %v", err)
	}

	got, err := sessionStore.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.LastRepairBlockedReason != "" {
		t.Fatalf("LastRepairBlockedReason=%q, want empty after real outcome", got.LastRepairBlockedReason)
	}
	if got.LastRepairBlockedAt != nil {
		t.Fatalf("LastRepairBlockedAt=%v, want nil after real outcome", got.LastRepairBlockedAt)
	}
	if got.LastRepairAttemptCount != 1 {
		t.Fatalf("LastRepairAttemptCount=%d, want 1", got.LastRepairAttemptCount)
	}
}

// TestRepairBlocked_CleanOutcomeClearsBlockedAndCount verifies that a clean
// repair outcome clears both the blocked pair and the failure count (BOS-153).
func TestRepairBlocked_CleanOutcomeClearsBlockedAndCount(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess, err := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "Repair blocked clean test",
		WorktreePath: "/tmp/wt/repair-blocked-clean",
		BranchName:   "feat/repair-blocked-clean",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Seed a failure and a blocked state.
	if err := sessionStore.UpdateRepairDiagnostics(ctx, UpdateRepairDiagnosticsParams{
		SessionID: sess.ID, StartedAt: time.Now(), ExitError: "exit status 1",
	}); err != nil {
		t.Fatalf("seed failure: %v", err)
	}
	if err := sessionStore.UpdateRepairBlocked(ctx, sess.ID, time.Unix(1779238800, 0), "displace blocked: chat working"); err != nil {
		t.Fatalf("UpdateRepairBlocked: %v", err)
	}

	// A clean run resets both.
	if err := sessionStore.UpdateRepairDiagnostics(ctx, UpdateRepairDiagnosticsParams{
		SessionID: sess.ID, StartedAt: time.Now(),
	}); err != nil {
		t.Fatalf("UpdateRepairDiagnostics clean: %v", err)
	}

	got, err := sessionStore.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.LastRepairAttemptCount != 0 {
		t.Fatalf("LastRepairAttemptCount=%d, want 0 after clean run", got.LastRepairAttemptCount)
	}
	if got.LastRepairBlockedReason != "" || got.LastRepairBlockedAt != nil {
		t.Fatalf("blocked pair not cleared: reason=%q at=%v", got.LastRepairBlockedReason, got.LastRepairBlockedAt)
	}
}

// TestArchiveContract locks the contract that the handler relies on (BOS-76):
// Archive returns sql.ErrNoRows for a missing id, and nil (with archived_at set)
// for an existing un-archived row.
func TestArchiveContract(t *testing.T) {
	ctx := context.Background()

	t.Run("missing id returns sql.ErrNoRows", func(t *testing.T) {
		db := setupTestDB(t)
		sessionStore := NewSessionStore(db)

		err := sessionStore.Archive(ctx, "nonexistent-id")
		if !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("Archive missing id: got %v, want sql.ErrNoRows", err)
		}
	})

	t.Run("existing un-archived row returns nil and sets archived_at", func(t *testing.T) {
		db := setupTestDB(t)
		repoStore := NewRepoStore(db)
		sessionStore := NewSessionStore(db)

		repo := createTestRepo(t, repoStore)
		sess, err := sessionStore.Create(ctx, CreateSessionParams{
			RepoID:       repo.ID,
			Title:        "archive contract test",
			WorktreePath: "/tmp/wt/archive-contract",
			BranchName:   "feat/archive-contract",
			BaseBranch:   "main",
		})
		if err != nil {
			t.Fatalf("create session: %v", err)
		}

		if err := sessionStore.Archive(ctx, sess.ID); err != nil {
			t.Fatalf("Archive: %v", err)
		}

		got, err := sessionStore.Get(ctx, sess.ID)
		if err != nil {
			t.Fatalf("Get after Archive: %v", err)
		}
		if got.ArchivedAt == nil {
			t.Error("ArchivedAt should be set after Archive, got nil")
		}
	})
}
