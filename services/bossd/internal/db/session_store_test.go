package db

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
)

// TestClaimUnarchivedOrphan_RequiresMatchingReason ensures a candidate from a
// stale orphan-resume sweep cannot be claimed after another writer clears or
// changes its daemon-restart marker.
func TestClaimUnarchivedOrphan_RequiresMatchingReason(t *testing.T) {
	database := setupTestDB(t)
	repoStore := NewRepoStore(database)
	store := NewSessionStore(database)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess, err := store.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "claim orphan marker",
		WorktreePath: "/tmp/wt/claim-orphan-marker",
		BranchName:   "feat/claim-orphan-marker",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	setSessionState(t, store, sess.ID, machine.Orphaned)
	marker := "headless run orphaned: daemon restart"
	markerPtr := &marker
	if _, err := store.Update(ctx, sess.ID, UpdateSessionParams{BlockedReason: &markerPtr}); err != nil {
		t.Fatalf("set orphan marker: %v", err)
	}

	changedMarker := "cleared by retry"
	changedMarkerPtr := &changedMarker
	if _, err := store.Update(ctx, sess.ID, UpdateSessionParams{BlockedReason: &changedMarkerPtr}); err != nil {
		t.Fatalf("change orphan marker: %v", err)
	}

	advanced, err := store.ClaimUnarchivedOrphan(ctx, sess.ID, marker)
	if err != nil {
		t.Fatalf("claim with stale marker: %v", err)
	}
	if advanced {
		t.Fatal("claim with stale marker advanced, want false")
	}
	got, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get after stale claim: %v", err)
	}
	if got.State != machine.Orphaned {
		t.Fatalf("state after stale claim = %v, want Orphaned", got.State)
	}
	if got.BlockedReason == nil || *got.BlockedReason != changedMarker {
		t.Fatalf("marker after stale claim = %v, want %q", got.BlockedReason, changedMarker)
	}

	if _, err := store.Update(ctx, sess.ID, UpdateSessionParams{BlockedReason: &markerPtr}); err != nil {
		t.Fatalf("restore orphan marker: %v", err)
	}

	advanced, err = store.ClaimUnarchivedOrphan(ctx, sess.ID, marker)
	if err != nil {
		t.Fatalf("claim with matching marker: %v", err)
	}
	if !advanced {
		t.Fatal("claim with matching marker did not advance")
	}
}

func TestOrphanResumeStore_ConditionalHandoff(t *testing.T) {
	database := setupTestDB(t)
	repoStore := NewRepoStore(database)
	store := NewSessionStore(database)
	ctx := context.Background()
	repo := createTestRepo(t, repoStore)
	marker := "headless run orphaned: daemon restart"

	newCandidate := func(t *testing.T, priorAgentSession *string) *models.Session {
		t.Helper()
		sess, err := store.Create(ctx, CreateSessionParams{
			RepoID:       repo.ID,
			Title:        "orphan resume handoff",
			WorktreePath: "/tmp/wt/orphan-resume-handoff",
			BranchName:   "feat/orphan-resume-handoff",
			BaseBranch:   "main",
		})
		if err != nil {
			t.Fatalf("create session: %v", err)
		}
		setSessionState(t, store, sess.ID, machine.Orphaned)
		markerPtr := &marker
		if _, err := store.Update(ctx, sess.ID, UpdateSessionParams{
			AgentSessionID: &priorAgentSession,
			BlockedReason:  &markerPtr,
		}); err != nil {
			t.Fatalf("prepare orphan candidate: %v", err)
		}
		return sess
	}

	t.Run("commits and reparks a nil prior agent session", func(t *testing.T) {
		sess := newCandidate(t, nil)
		if ok, err := store.ClaimUnarchivedOrphan(ctx, sess.ID, marker); err != nil || !ok {
			t.Fatalf("claim = %v, %v; want true, nil", ok, err)
		}
		if ok, err := store.CommitOrphanResume(ctx, sess.ID, marker, nil, "agent-new"); err != nil || !ok {
			t.Fatalf("commit = %v, %v; want true, nil", ok, err)
		}
		if ok, err := store.ReparkOrphanResume(ctx, sess.ID, marker, nil, "agent-new"); err != nil || !ok {
			t.Fatalf("repark = %v, %v; want true, nil", ok, err)
		}
		got, err := store.Get(ctx, sess.ID)
		if err != nil {
			t.Fatalf("get reparking result: %v", err)
		}
		if got.State != machine.Orphaned || got.AgentSessionID != nil || got.BlockedReason == nil || *got.BlockedReason != marker {
			t.Fatalf("reparked session = %+v, want original nil-agent orphan shape", got)
		}
	})

	t.Run("does not commit after archive or prior agent change", func(t *testing.T) {
		prior := "agent-old"
		sess := newCandidate(t, &prior)
		if ok, err := store.ClaimUnarchivedOrphan(ctx, sess.ID, marker); err != nil || !ok {
			t.Fatalf("claim = %v, %v; want true, nil", ok, err)
		}
		if err := store.Archive(ctx, sess.ID); err != nil {
			t.Fatalf("archive: %v", err)
		}
		if ok, err := store.CommitOrphanResume(ctx, sess.ID, marker, &prior, "agent-new"); err != nil || ok {
			t.Fatalf("commit after archive = %v, %v; want false, nil", ok, err)
		}

		other := "agent-other"
		otherPtr := &other
		sess = newCandidate(t, &prior)
		if ok, err := store.ClaimUnarchivedOrphan(ctx, sess.ID, marker); err != nil || !ok {
			t.Fatalf("claim = %v, %v; want true, nil", ok, err)
		}
		if _, err := store.Update(ctx, sess.ID, UpdateSessionParams{AgentSessionID: &otherPtr}); err != nil {
			t.Fatalf("replace prior agent: %v", err)
		}
		if ok, err := store.CommitOrphanResume(ctx, sess.ID, marker, &prior, "agent-new"); err != nil || ok {
			t.Fatalf("commit after agent replacement = %v, %v; want false, nil", ok, err)
		}
	})

	t.Run("releases a claimed orphan when retry cleared its marker", func(t *testing.T) {
		prior := "agent-old"
		sess := newCandidate(t, &prior)
		if ok, err := store.ClaimUnarchivedOrphan(ctx, sess.ID, marker); err != nil || !ok {
			t.Fatalf("claim = %v, %v; want true, nil", ok, err)
		}
		var nilReason *string
		if _, err := store.Update(ctx, sess.ID, UpdateSessionParams{BlockedReason: &nilReason}); err != nil {
			t.Fatalf("clear orphan marker for retry: %v", err)
		}
		if ok, err := store.ReleaseOrphanResumeClaim(ctx, sess.ID, marker, &prior); err != nil || !ok {
			t.Fatalf("release markerless retry claim = %v, %v; want true, nil", ok, err)
		}
		got, err := store.Get(ctx, sess.ID)
		if err != nil {
			t.Fatalf("get released retry claim: %v", err)
		}
		if got.State != machine.Orphaned || got.BlockedReason != nil || got.AgentSessionID == nil || *got.AgentSessionID != prior {
			t.Fatalf("released retry claim = %+v, want markerless orphan with original agent", got)
		}
	})

	t.Run("does not release a markerless claim after archive or manual agent replacement", func(t *testing.T) {
		prior := "agent-old"
		clearMarker := func(t *testing.T, sess *models.Session) {
			t.Helper()
			if ok, err := store.ClaimUnarchivedOrphan(ctx, sess.ID, marker); err != nil || !ok {
				t.Fatalf("claim = %v, %v; want true, nil", ok, err)
			}
			var nilReason *string
			if _, err := store.Update(ctx, sess.ID, UpdateSessionParams{BlockedReason: &nilReason}); err != nil {
				t.Fatalf("clear orphan marker: %v", err)
			}
		}

		archived := newCandidate(t, &prior)
		clearMarker(t, archived)
		if err := store.Archive(ctx, archived.ID); err != nil {
			t.Fatalf("archive claimed orphan: %v", err)
		}
		if ok, err := store.ReleaseOrphanResumeClaim(ctx, archived.ID, marker, &prior); err != nil || ok {
			t.Fatalf("release after archive = %v, %v; want false, nil", ok, err)
		}

		replaced := newCandidate(t, &prior)
		clearMarker(t, replaced)
		other := "agent-manual"
		otherPtr := &other
		if _, err := store.Update(ctx, replaced.ID, UpdateSessionParams{AgentSessionID: &otherPtr}); err != nil {
			t.Fatalf("replace claimed agent: %v", err)
		}
		if ok, err := store.ReleaseOrphanResumeClaim(ctx, replaced.ID, marker, &prior); err != nil || ok {
			t.Fatalf("release after manual agent replacement = %v, %v; want false, nil", ok, err)
		}
	})
}

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

// TestUpdateRotationFields round-trips the BOS-174 rotation_attempt_count and
// rotation_resume_at columns: defaults, set, and clear-to-NULL.
func TestUpdateRotationFields(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess, err := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "rotation fields test",
		WorktreePath: "/tmp/wt/rotation",
		BranchName:   "feat/rotation",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Fresh session: defaults.
	if sess.RotationAttemptCount != 0 {
		t.Fatalf("fresh RotationAttemptCount = %d, want 0", sess.RotationAttemptCount)
	}
	if sess.RotationResumeAt != nil {
		t.Fatalf("fresh RotationResumeAt = %v, want nil", *sess.RotationResumeAt)
	}

	// Set both fields.
	count := 2
	resumeAt := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	resumeAtStr := resumeAt.UTC().Format("2006-01-02T15:04:05.000Z")
	resumeAtPtr := &resumeAtStr
	got, err := sessionStore.Update(ctx, sess.ID, UpdateSessionParams{
		RotationAttemptCount: &count,
		RotationResumeAt:     &resumeAtPtr,
	})
	if err != nil {
		t.Fatalf("update set: %v", err)
	}
	if got.RotationAttemptCount != 2 {
		t.Fatalf("RotationAttemptCount = %d, want 2", got.RotationAttemptCount)
	}
	if got.RotationResumeAt == nil || !got.RotationResumeAt.Equal(resumeAt) {
		t.Fatalf("RotationResumeAt = %v, want %v", got.RotationResumeAt, resumeAt)
	}

	// Re-fetch to verify persistence.
	reread, err := sessionStore.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if reread.RotationAttemptCount != 2 {
		t.Fatalf("reread RotationAttemptCount = %d, want 2", reread.RotationAttemptCount)
	}
	if reread.RotationResumeAt == nil || !reread.RotationResumeAt.Equal(resumeAt) {
		t.Fatalf("reread RotationResumeAt = %v, want %v", reread.RotationResumeAt, resumeAt)
	}

	// Clear RotationResumeAt to NULL (*nil inner pointer).
	var clear *string
	got, err = sessionStore.Update(ctx, sess.ID, UpdateSessionParams{RotationResumeAt: &clear})
	if err != nil {
		t.Fatalf("update clear: %v", err)
	}
	if got.RotationResumeAt != nil {
		t.Fatalf("RotationResumeAt = %v after clear, want nil", *got.RotationResumeAt)
	}
	// RotationAttemptCount must be untouched by the clear (nil = don't touch).
	if got.RotationAttemptCount != 2 {
		t.Fatalf("RotationAttemptCount = %d after unrelated update, want unchanged 2", got.RotationAttemptCount)
	}
}

// TestSessionStore_AccountIDRoundTrip pins the BOS-170 nullable account_id
// column: a session created with an explicit account binding reads it back
// (through Get and the join-based ListWithRepo scan), and one created without
// a binding reads back nil (system-default account 0).
func TestSessionStore_AccountIDRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)

	// Bound session: account_id persists and round-trips.
	acct := "a1"
	bound, err := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "bound",
		WorktreePath: "/tmp/wt/acct-bound",
		BranchName:   "feat/acct-bound",
		BaseBranch:   "main",
		AccountID:    &acct,
	})
	if err != nil {
		t.Fatalf("create bound: %v", err)
	}
	if bound.AccountID == nil || *bound.AccountID != "a1" {
		t.Fatalf("create AccountID = %v, want a1", bound.AccountID)
	}
	got, err := sessionStore.Get(ctx, bound.ID)
	if err != nil {
		t.Fatalf("get bound: %v", err)
	}
	if got.AccountID == nil || *got.AccountID != "a1" {
		t.Fatalf("get AccountID = %v, want a1", got.AccountID)
	}

	// Unbound session: no account_id → nil (system-default account 0).
	unbound, err := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "unbound",
		WorktreePath: "/tmp/wt/acct-unbound",
		BranchName:   "feat/acct-unbound",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create unbound: %v", err)
	}
	if unbound.AccountID != nil {
		t.Fatalf("unbound create AccountID = %q, want nil", *unbound.AccountID)
	}
	gotUnbound, err := sessionStore.Get(ctx, unbound.ID)
	if err != nil {
		t.Fatalf("get unbound: %v", err)
	}
	if gotUnbound.AccountID != nil {
		t.Fatalf("unbound get AccountID = %q, want nil", *gotUnbound.AccountID)
	}

	// The join-based ListWithRepo path has its own scan order — confirm the
	// bound session's account_id survives it too.
	rows, err := sessionStore.ListWithRepo(ctx, repo.ID)
	if err != nil {
		t.Fatalf("list with repo: %v", err)
	}
	var sawBound bool
	for _, r := range rows {
		if r.ID == bound.ID {
			sawBound = true
			if r.AccountID == nil || *r.AccountID != "a1" {
				t.Errorf("list with repo AccountID = %v, want a1", r.AccountID)
			}
		}
	}
	if !sawBound {
		t.Error("bound session missing from ListWithRepo")
	}
}

// TestUpdateSessionAccountID round-trips the BOS-171 manual-switch rebind
// through UpdateSessionParams.AccountID: bind to an account, then clear it back
// to NULL (system-default account 0) via the nullable-string double-pointer
// convention, leaving unrelated fields untouched.
func TestUpdateSessionAccountID(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess, err := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "account rebind test",
		WorktreePath: "/tmp/wt/acct-rebind",
		BranchName:   "feat/acct-rebind",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Fresh session: no binding.
	if sess.AccountID != nil {
		t.Fatalf("fresh AccountID = %q, want nil", *sess.AccountID)
	}

	// Bind to an account.
	acct := "acct-42"
	acctPtr := &acct
	got, err := sessionStore.Update(ctx, sess.ID, UpdateSessionParams{AccountID: &acctPtr})
	if err != nil {
		t.Fatalf("update bind: %v", err)
	}
	if got.AccountID == nil || *got.AccountID != "acct-42" {
		t.Fatalf("AccountID = %v, want acct-42", got.AccountID)
	}
	reread, err := sessionStore.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if reread.AccountID == nil || *reread.AccountID != "acct-42" {
		t.Fatalf("reread AccountID = %v, want acct-42", reread.AccountID)
	}

	// Clear back to NULL (system-default account 0) via the *nil inner pointer.
	var clear *string
	got, err = sessionStore.Update(ctx, sess.ID, UpdateSessionParams{AccountID: &clear})
	if err != nil {
		t.Fatalf("update clear: %v", err)
	}
	if got.AccountID != nil {
		t.Fatalf("AccountID = %q after clear, want nil", *got.AccountID)
	}

	// A nil AccountID pointer must not touch the column.
	acct2 := "acct-99"
	acct2Ptr := &acct2
	if _, err := sessionStore.Update(ctx, sess.ID, UpdateSessionParams{AccountID: &acct2Ptr}); err != nil {
		t.Fatalf("re-bind: %v", err)
	}
	title := "renamed"
	got, err = sessionStore.Update(ctx, sess.ID, UpdateSessionParams{Title: &title})
	if err != nil {
		t.Fatalf("unrelated update: %v", err)
	}
	if got.AccountID == nil || *got.AccountID != "acct-99" {
		t.Fatalf("AccountID = %v after unrelated update, want unchanged acct-99", got.AccountID)
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

// TestSessionDetachRoundTrip pins the persistence contract for the detach
// column added in BOS-428: it defaults false on creation, an explicit Update
// sets it, and a nil-valued Update (the "don't touch this field" convention
// used throughout UpdateSessionParams) leaves the previously-set value
// unchanged. Detach marks a durable, tmux-hosted --detach autonomous run that
// must survive a daemon restart via unattended-class recovery.
func TestSessionDetachRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess, err := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "Detach round trip",
		WorktreePath: "/tmp/wt/detach",
		BranchName:   "feat/detach",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if sess.Detach {
		t.Fatalf("Detach = true on creation, want false (default)")
	}

	detach := true
	if _, err := sessionStore.Update(ctx, sess.ID, UpdateSessionParams{
		Detach: &detach,
	}); err != nil {
		t.Fatalf("update session detach: %v", err)
	}
	gotSess, err := sessionStore.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if !gotSess.Detach {
		t.Fatalf("Detach = false after Update(true), want true")
	}

	// An Update with a nil Detach pointer must not touch the column.
	renamedTitle := "Detach round trip (renamed)"
	if _, err := sessionStore.Update(ctx, sess.ID, UpdateSessionParams{
		Title: &renamedTitle,
	}); err != nil {
		t.Fatalf("update session title: %v", err)
	}
	gotSess, err = sessionStore.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get session after unrelated update: %v", err)
	}
	if !gotSess.Detach {
		t.Fatalf("Detach = false after unrelated Update, want true (unchanged)")
	}
}

// TestSessionQuickChatRoundTrip pins the persistence contract for the
// quick_chat column added in BOS-322: it defaults false on creation, an
// explicit Update sets it, and a nil-valued Update (the "don't touch this
// field" convention used throughout UpdateSessionParams) leaves the
// previously-set value unchanged. This is the persisted planning-only signal
// the finalize backstop keys on.
func TestSessionQuickChatRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess, err := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "Quick chat round trip",
		WorktreePath: "/tmp/wt/quick-chat",
		BranchName:   "feat/quick-chat",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if sess.QuickChat {
		t.Fatalf("QuickChat = true on creation, want false (default)")
	}

	quickChat := true
	if _, err := sessionStore.Update(ctx, sess.ID, UpdateSessionParams{
		QuickChat: &quickChat,
	}); err != nil {
		t.Fatalf("update session quick_chat: %v", err)
	}
	gotSess, err := sessionStore.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if !gotSess.QuickChat {
		t.Fatalf("QuickChat = false after Update(true), want true")
	}

	// An Update with a nil QuickChat pointer must not touch the column.
	renamedTitle := "Quick chat round trip (renamed)"
	if _, err := sessionStore.Update(ctx, sess.ID, UpdateSessionParams{
		Title: &renamedTitle,
	}); err != nil {
		t.Fatalf("update session title: %v", err)
	}
	gotSess, err = sessionStore.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get session after unrelated update: %v", err)
	}
	if !gotSess.QuickChat {
		t.Fatalf("QuickChat = false after unrelated Update, want true (unchanged)")
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

// setSessionState forces a session row into a specific state, since Create
// always starts in CreatingWorktree.
func setSessionState(t *testing.T, store *SQLiteSessionStore, id string, state machine.State) {
	t.Helper()
	s := int(state)
	if _, err := store.Update(context.Background(), id, UpdateSessionParams{State: &s}); err != nil {
		t.Fatalf("set session %s state: %v", id, err)
	}
}

func TestSessionStore_ListByStates(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	store := NewSessionStore(db)
	ctx := context.Background()
	repo := createTestRepo(t, repoStore)

	mk := func(title string, state machine.State) string {
		sess, err := store.Create(ctx, CreateSessionParams{
			RepoID:       repo.ID,
			Title:        title,
			WorktreePath: "/tmp/wt/" + title,
			BranchName:   "feat/" + title,
			BaseBranch:   "main",
		})
		if err != nil {
			t.Fatalf("create %s: %v", title, err)
		}
		setSessionState(t, store, sess.ID, state)
		return sess.ID
	}

	pushing := mk("pushing", machine.PushingBranch)
	implementing := mk("implementing", machine.ImplementingPlan)
	_ = mk("orphaned", machine.Orphaned) // must NOT be returned when not requested

	// Empty set: no query, nil result.
	got, err := store.ListByStates(ctx, nil)
	if err != nil {
		t.Fatalf("ListByStates(nil): %v", err)
	}
	if got != nil {
		t.Fatalf("ListByStates(nil) = %v, want nil", got)
	}

	// Multi-state set returns matching sessions across states.
	got, err = store.ListByStates(ctx, []int{int(machine.PushingBranch), int(machine.ImplementingPlan)})
	if err != nil {
		t.Fatalf("ListByStates: %v", err)
	}
	ids := map[string]bool{}
	for _, s := range got {
		ids[s.ID] = true
	}
	if len(got) != 2 || !ids[pushing] || !ids[implementing] {
		t.Fatalf("ListByStates returned %d sessions (%v), want [pushing implementing]", len(got), ids)
	}
}

func TestSessionStore_UpdateStateConditionalFrom(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	store := NewSessionStore(db)
	ctx := context.Background()
	repo := createTestRepo(t, repoStore)

	sess, err := store.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "conditional-from",
		WorktreePath: "/tmp/wt/cond",
		BranchName:   "feat/cond",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	setSessionState(t, store, sess.ID, machine.PushingBranch)

	expected := []int{int(machine.ImplementingPlan), int(machine.PushingBranch), int(machine.OpeningDraftPR)}

	// Empty expected set: no query, no-op false.
	ok, err := store.UpdateStateConditionalFrom(ctx, sess.ID, int(machine.Finalizing), nil)
	if err != nil {
		t.Fatalf("UpdateStateConditionalFrom(empty): %v", err)
	}
	if ok {
		t.Fatal("empty expected set should no-op (false)")
	}

	// Current state in the set: transitions, returns true.
	ok, err = store.UpdateStateConditionalFrom(ctx, sess.ID, int(machine.Finalizing), expected)
	if err != nil {
		t.Fatalf("UpdateStateConditionalFrom(in-set): %v", err)
	}
	if !ok {
		t.Fatal("state in set should transition (true)")
	}
	got, _ := store.Get(ctx, sess.ID)
	if got.State != machine.Finalizing {
		t.Fatalf("state = %v, want Finalizing", got.State)
	}

	// Current state (now Finalizing) not in the set: no-op false, unchanged.
	ok, err = store.UpdateStateConditionalFrom(ctx, sess.ID, int(machine.Blocked), expected)
	if err != nil {
		t.Fatalf("UpdateStateConditionalFrom(not-in-set): %v", err)
	}
	if ok {
		t.Fatal("state not in set should no-op (false)")
	}
	got, _ = store.Get(ctx, sess.ID)
	if got.State != machine.Finalizing {
		t.Fatalf("state = %v after no-op, want Finalizing unchanged", got.State)
	}
}

// TestClampInt32 is the BOS-413 boundary table-test for the package-local
// gosec-G115 clamp helper: normal values pass through; out-of-range int inputs
// clamp to the int32 extremes instead of wrapping. int32 range is
// [-2147483648, 2147483647].
func TestClampInt32(t *testing.T) {
	const (
		maxI32 = 2147483647
		minI32 = -2147483648
	)
	tests := []struct {
		name string
		in   int
		want int32
	}{
		{"normal", 42, 42},
		{"zero", 0, 0},
		{"max", maxI32, maxI32},
		{"min", minI32, minI32},
		{"clampsHigh", maxI32 + 1, maxI32},
		{"clampsLow", minI32 - 1, minI32},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clampInt32(tt.in); got != tt.want {
				t.Errorf("clampInt32(%d) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}
