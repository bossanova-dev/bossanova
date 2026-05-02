package testharness_test

// e2e_cron_test.go — cron E2E flows 1-4.
//
// Each test drives the full cron lifecycle:
//   register repo → create cron job → AddJob → Tick → session spawned →
//   PostStopHook → finalize → assert last_run_outcome + worktree presence.
//
// Determinism: Tick is called with a specific time that is after the job's
// next scheduled time (which starts from zero, so any time in the past
// triggers a fire). time.Sleep is only used inside polling helpers (clearly
// labelled), never as a bare wait.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/cron"
	"github.com/recurser/bossd/internal/db"
	gitpkg "github.com/recurser/bossd/internal/git"
	"github.com/recurser/bossd/internal/taskorchestrator"
	"github.com/recurser/bossd/internal/testharness"
	"github.com/rs/zerolog"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// waitForCronOutcome polls the CronJobStore until the job's LastRunOutcome
// matches want or the deadline expires. A short sleep inside the loop is
// acceptable here: we are waiting for an asynchronous finalize pipeline to
// write the DB row, and polling avoids a fixed sleep that would bloat the
// runtime.
func waitForCronOutcome(t *testing.T, store db.CronJobStore, id string, want models.CronJobOutcome) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		job, err := store.Get(ctx, id)
		if err == nil && job != nil && job.LastRunOutcome != nil && *job.LastRunOutcome == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Fetch final state for the error message.
	job, _ := store.Get(ctx, id)
	var got string
	if job != nil && job.LastRunOutcome != nil {
		got = string(*job.LastRunOutcome)
	}
	t.Fatalf("timeout waiting for cron outcome=%q; got=%q", want, got)
}

// waitForSessionDeleted polls the SessionStore until the session row is gone
// (Get returns a not-found error) or the deadline expires.
func waitForSessionDeleted(t *testing.T, store db.SessionStore, id string, timeout time.Duration) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		sess, err := store.Get(ctx, id)
		if err != nil || sess == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for session %s to be deleted", id)
}

// useTempWorktrees overrides the mock git's Create to return real temp
// directories so WriteHookConfig (which calls os.MkdirAll inside the path)
// succeeds. Without this, the fake /tmp/worktrees/... path from the default
// mock doesn't exist on disk, and WriteHookConfig fails with ENOENT.
func useTempWorktrees(t *testing.T, h *testharness.Harness) {
	t.Helper()
	h.Git.CreateFunc = func(_ context.Context, opts gitpkg.CreateOpts) (*gitpkg.CreateResult, error) {
		// Mirror MockWorktreeManager.Create's BaseBranch invariant — real
		// git fails with `invalid reference: origin/` otherwise.
		if opts.BaseBranch == "" {
			t.Errorf("CreateFunc: BaseBranch is empty (would fail with 'invalid reference: origin/' in production)")
		}
		dir := t.TempDir()
		branch := opts.Title
		if branch == "" {
			branch = "cron-branch"
		}
		return &gitpkg.CreateResult{
			WorktreePath: dir,
			BranchName:   branch,
		}, nil
	}
}

// newCronScheduler builds a Scheduler wired to the harness's stores and
// lifecycle. It uses MaxConcurrent=1 for predictable behaviour in these tests.
func newCronScheduler(h *testharness.Harness) *cron.Scheduler {
	logger := zerolog.New(os.Stderr).With().Timestamp().Logger()
	creator := taskorchestrator.NewSessionCreator(h.Sessions, h.Lifecycle, logger)
	return cron.New(cron.Config{
		Store:         h.CronJobs,
		Sessions:      h.Sessions,
		Repos:         h.Repos,
		Creator:       creator,
		MaxConcurrent: 1,
		Logger:        logger,
	})
}

// sessionFromCronJob returns the session spawned by the cron job identified
// by jobID. It inspects the job's LastRunSessionID (written by MarkFireStarted
// inside Tick → fire). Tick is synchronous so this is available immediately
// after Tick returns.
func sessionFromCronJob(t *testing.T, h *testharness.Harness, jobID string) *models.Session {
	t.Helper()
	ctx := context.Background()
	job, err := h.CronJobs.Get(ctx, jobID)
	if err != nil || job == nil {
		t.Fatalf("sessionFromCronJob: get cron job %s: %v", jobID, err)
	}
	if job.LastRunSessionID == nil || *job.LastRunSessionID == "" {
		t.Fatalf("sessionFromCronJob: job %s has no LastRunSessionID after Tick", jobID)
	}
	sess, err := h.Sessions.Get(ctx, *job.LastRunSessionID)
	if err != nil || sess == nil {
		t.Fatalf("sessionFromCronJob: get session %s: %v", *job.LastRunSessionID, err)
	}
	return sess
}

// tickPast fires the scheduler for all jobs whose next run is at or before
// a time safely in the past (1970 epoch + 1 year). Using a fixed past time
// guarantees every job with a non-zero schedule fires exactly once.
var tickTime = time.Date(1971, 1, 1, 0, 0, 0, 0, time.UTC)

// cronTestHarness builds a Harness wired with a CronReadyTmuxFake so the
// cron lifecycle's startCronTmuxChat path (which requires tmux Available()
// and a successful SendPlan) can run end-to-end without a real tmux. The
// fake is returned alongside the harness so tests that want to assert on
// tmux call history (e.g. did kill-session fire during finalize) can do so.
func cronTestHarness(t *testing.T) (*testharness.Harness, *testharness.CronReadyTmuxFake) {
	t.Helper()
	fake := testharness.NewCronReadyTmuxFake()
	h := testharness.NewWithOptions(t, testharness.Options{
		TmuxCommandFactory: fake.Factory(),
	})
	return h, fake
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestE2ECron_HappyPath_PRCreated exercises the full happy path:
//
//	create job → Tick → session spawned → mock claude writes a file →
//	PostStopHook → finalize creates PR → outcome=pr_created.
//
// Assertions:
//   - last_run_outcome == "pr_created"
//   - a ClaudeChat row (finalize chat) was created for the session
//   - the worktree directory still exists (preserved after PR)
func TestE2ECron_HappyPath_PRCreated(t *testing.T) {
	h, fake := cronTestHarness(t)
	ctx := context.Background()

	// Register repo — default DetectOriginURLResult is github.com, which is
	// what EnsurePR needs to open a PR.
	repoID := registerTestRepo(t, h, ctx, withWorktreeBaseDir(t.TempDir()))

	// Use real temp directories so WriteHookConfig succeeds.
	useTempWorktrees(t, h)

	// Claude writes a file so git Status returns non-empty (changes present).
	h.Claude.WithChanges("cron-output.txt", "hello from cron")
	// Override Status to simulate dirty worktree after the implementing chat.
	h.Git.StatusFunc = func(_ context.Context, _ string) (string, error) {
		return "M cron-output.txt\n", nil
	}

	// Create the cron job.
	job, err := h.CronJobs.Create(ctx, db.CreateCronJobParams{
		RepoID:   repoID,
		Name:     "happy-path",
		Prompt:   "do the thing",
		Schedule: "* * * * *", // every minute
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create cron job: %v", err)
	}

	// Build scheduler and register the job.
	sched := newCronScheduler(h)
	if err := sched.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Tick → session is spawned synchronously.
	sched.Tick(tickTime)

	// Retrieve the session created by the scheduler.
	sess := sessionFromCronJob(t, h, job.ID)
	if sess.HookToken == nil || *sess.HookToken == "" {
		t.Fatal("expected HookToken to be set on cron-spawned session")
	}
	hookToken := *sess.HookToken

	// Assert: a claude_chats row was created for the cron-spawned session
	// during startCronTmuxChat, with TmuxSessionName populated and a
	// `Run "<cron name>"` title. This is the chat the user attaches to in
	// boss to watch the cron-fired Claude work autonomously.
	cronChats, err := h.ClaudeChats.ListBySession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("list claude chats after Tick: %v", err)
	}
	if len(cronChats) != 1 {
		t.Fatalf("expected exactly 1 claude_chats row after cron fire, got %d", len(cronChats))
	}
	cronChat := cronChats[0]
	if cronChat.TmuxSessionName == nil || *cronChat.TmuxSessionName == "" {
		t.Fatal("expected cron chat to have a non-nil TmuxSessionName populated by startCronTmuxChat")
	}
	if !strings.HasPrefix(*cronChat.TmuxSessionName, "boss-") {
		t.Errorf("expected TmuxSessionName to start with %q, got %q", "boss-", *cronChat.TmuxSessionName)
	}
	if want := `Run "happy-path"`; cronChat.Title != want {
		t.Errorf("cron chat title = %q, want %q", cronChat.Title, want)
	}
	// The fake's HasLiveSession reflects every new-session/kill-session
	// observation. The harness's tmuxClient driving HasSession reads from
	// the same fake, so both views stay consistent.
	if !h.Tmux.HasSession(ctx, *cronChat.TmuxSessionName) {
		t.Errorf("expected tmux to report session %q as alive after startCronTmuxChat", *cronChat.TmuxSessionName)
	}
	if !fake.HasLiveSession(*cronChat.TmuxSessionName) {
		t.Errorf("expected fake to report session %q as live after new-session", *cronChat.TmuxSessionName)
	}

	// POST the stop hook to trigger the finalize pipeline.
	resp, err := h.PostStopHook(sess.ID, hookToken)
	if err != nil {
		t.Fatalf("PostStopHook: %v", err)
	}
	resp.Body.Close() //nolint:errcheck // test helper: body drain error is not meaningful here
	if resp.StatusCode != 200 {
		t.Fatalf("PostStopHook: unexpected status %d", resp.StatusCode)
	}

	// Wait for the cron job row to reflect the outcome.
	waitForCronOutcome(t, h.CronJobs, job.ID, models.CronJobOutcomePRCreated)

	// Assert: at least two chat rows exist now — the cron-spawned one
	// (created during startCronTmuxChat) and the finalize chat sibling
	// (created by StartFinalizeChat on the pr_created path).
	chats, err := h.ClaudeChats.ListBySession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("list claude chats: %v", err)
	}
	if len(chats) < 2 {
		t.Errorf("expected at least 2 ClaudeChat rows (cron + finalize) after pr_created, got %d", len(chats))
	}

	// Assert: worktree directory is still present.
	if sess.WorktreePath == "" {
		t.Fatal("expected non-empty WorktreePath on cron session")
	}
	if _, statErr := os.Stat(sess.WorktreePath); os.IsNotExist(statErr) {
		t.Errorf("expected worktree directory %s to still exist after pr_created", sess.WorktreePath)
	}
}

// TestE2ECron_NoChanges_DeletesSession exercises the no-changes path:
//
//	create job → Tick → session spawned → mock claude exits clean (no files) →
//	PostStopHook → finalize finds empty status → outcome=deleted_no_changes.
//
// Assertions:
//   - last_run_outcome == "deleted_no_changes"
//   - session row is deleted from the store
//   - worktree directory is removed (Archive is called)
//   - hook_token on the job is irrelevant (session row gone)
func TestE2ECron_NoChanges_DeletesSession(t *testing.T) {
	h, fake := cronTestHarness(t)
	ctx := context.Background()

	repoID := registerTestRepo(t, h, ctx, withWorktreeBaseDir(t.TempDir()))
	useTempWorktrees(t, h)

	// NoChanges: mock claude exits without writing files.
	// The default Status mock returns "" (empty) which triggers the
	// deleted_no_changes branch in the finalize pipeline.
	h.Claude.NoChanges()

	// Create the cron job.
	job, err := h.CronJobs.Create(ctx, db.CreateCronJobParams{
		RepoID:   repoID,
		Name:     "no-changes",
		Prompt:   "do nothing",
		Schedule: "* * * * *",
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create cron job: %v", err)
	}

	sched := newCronScheduler(h)
	if err := sched.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	sched.Tick(tickTime)

	sess := sessionFromCronJob(t, h, job.ID)
	if sess.HookToken == nil || *sess.HookToken == "" {
		t.Fatal("expected HookToken to be set on cron-spawned session")
	}
	sessionID := sess.ID
	worktreePath := sess.WorktreePath

	// Capture the cron chat's tmux session name BEFORE the stop hook so we
	// can assert that finalizeNoChanges tears it down. After session-row
	// deletion the claude_chats row is cascaded away and the name is lost.
	cronChatsBefore, err := h.ClaudeChats.ListBySession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("list claude chats before finalize: %v", err)
	}
	if len(cronChatsBefore) != 1 {
		t.Fatalf("expected exactly 1 cron-spawned chat row before finalize, got %d", len(cronChatsBefore))
	}
	if cronChatsBefore[0].TmuxSessionName == nil || *cronChatsBefore[0].TmuxSessionName == "" {
		t.Fatal("expected cron chat to have non-nil TmuxSessionName before finalize")
	}
	cronTmuxName := *cronChatsBefore[0].TmuxSessionName
	if !fake.HasLiveSession(cronTmuxName) {
		t.Fatalf("expected fake to report tmux session %q as live before finalize", cronTmuxName)
	}

	// POST the stop hook to trigger the async finalize pipeline.
	resp, err := h.PostStopHook(sessionID, *sess.HookToken)
	if err != nil {
		t.Fatalf("PostStopHook: %v", err)
	}
	resp.Body.Close() //nolint:errcheck // test helper: body drain error is not meaningful here
	if resp.StatusCode != 200 {
		t.Fatalf("PostStopHook: unexpected status %d", resp.StatusCode)
	}

	// Wait for outcome.
	waitForCronOutcome(t, h.CronJobs, job.ID, models.CronJobOutcomeDeletedNoChanges)

	// Assert: session row is deleted.
	waitForSessionDeleted(t, h.Sessions, sessionID, 5*time.Second)

	// Assert: the cron chat's tmux session was killed during finalize so
	// no orphaned `claude` process is left behind. killAllChatTmuxSessions
	// runs from finalizeNoChanges before the session-delete cascade strips
	// the row that holds the tmux name. The fake's liveSessions tracking
	// drops the name on kill-session, so HasLiveSession is the cleanest
	// post-condition check.
	if fake.HasLiveSession(cronTmuxName) {
		t.Errorf("expected tmux session %q to be killed during finalize; still live", cronTmuxName)
	}
	if h.Tmux.HasSession(ctx, cronTmuxName) {
		t.Errorf("expected tmux client HasSession(%q) = false after finalize", cronTmuxName)
	}

	// Assert: worktree was archived (removed). The mock's Archive appends to
	// ArchiveCalls; if the worktree path is non-empty, it should appear there.
	if worktreePath != "" {
		found := false
		for _, p := range h.Git.ArchiveCalls {
			if p == worktreePath {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected worktree %s to appear in Git.ArchiveCalls; got %v",
				worktreePath, h.Git.ArchiveCalls)
		}
	}
}

// TestE2ECron_NoGitHub_SkipsPR exercises the no-github branch:
//
//	register repo with non-GitHub origin → create job → Tick → claude writes
//	changes → PostStopHook → finalize sees non-GitHub origin →
//	outcome=pr_skipped_no_github, worktree preserved.
//
// The key setup: set DetectOriginURLResult to a non-GitHub URL BEFORE
// registering the repo, so the stored OriginURL in the DB is non-GitHub.
// The finalize pipeline checks IsGitHubURL(repo.OriginURL).
func TestE2ECron_NoGitHub_SkipsPR(t *testing.T) {
	h, _ := cronTestHarness(t)
	ctx := context.Background()

	// Override origin URL to non-GitHub BEFORE repo registration so the DB
	// stores a non-GitHub OriginURL. IsGitHubURL checks this stored value.
	h.Git.DetectOriginURLResult = "file:///tmp/local-only-repo"

	repoID := registerTestRepo(t, h, ctx, withWorktreeBaseDir(t.TempDir()))
	useTempWorktrees(t, h)

	// Claude writes a file so Status returns non-empty (changes exist).
	h.Claude.WithChanges("anything.txt", "x")
	h.Git.StatusFunc = func(_ context.Context, _ string) (string, error) {
		return "M anything.txt\n", nil
	}

	job, err := h.CronJobs.Create(ctx, db.CreateCronJobParams{
		RepoID:   repoID,
		Name:     "no-github",
		Prompt:   "do stuff",
		Schedule: "* * * * *",
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create cron job: %v", err)
	}

	sched := newCronScheduler(h)
	if err := sched.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	sched.Tick(tickTime)

	sess := sessionFromCronJob(t, h, job.ID)
	if sess.HookToken == nil || *sess.HookToken == "" {
		t.Fatal("expected HookToken to be set on cron-spawned session")
	}

	resp, err := h.PostStopHook(sess.ID, *sess.HookToken)
	if err != nil {
		t.Fatalf("PostStopHook: %v", err)
	}
	resp.Body.Close() //nolint:errcheck // test helper: body drain error is not meaningful here

	waitForCronOutcome(t, h.CronJobs, job.ID, models.CronJobOutcomePRSkippedNoGitHub)

	// Assert: worktree is preserved (no Archive call for this path).
	if sess.WorktreePath != "" {
		for _, p := range h.Git.ArchiveCalls {
			if p == sess.WorktreePath {
				t.Errorf("worktree %s should be preserved but was archived", sess.WorktreePath)
			}
		}
	}
}

// TestE2ECron_Overlap_SkipsSecondTick verifies overlap suppression:
//
//  1. First Tick fires; session is created and stays in ImplementingPlan
//     (default mock Start: running=true, no StartFunc override).
//  2. Second Tick: scheduler detects previousRunActive → skips.
//     No second session is created.
//  3. First session is closed (terminal state) via the daemon's CloseSession
//     RPC. Third Tick fires and creates a new session.
//
// Overlap is verified by comparing session counts and checking
// job.LastRunSessionID changes after Tick 3.
func TestE2ECron_Overlap_SkipsSecondTick(t *testing.T) {
	h, _ := cronTestHarness(t)
	ctx := context.Background()

	repoID := registerTestRepo(t, h, ctx, withWorktreeBaseDir(t.TempDir()))
	useTempWorktrees(t, h)

	// Tick 1 uses the default Start (running=true): session stays in
	// ImplementingPlan so previousRunActive blocks Tick 2.
	// No StartFunc override → h.Claude has StartFunc=nil by default.

	job, err := h.CronJobs.Create(ctx, db.CreateCronJobParams{
		RepoID:   repoID,
		Name:     "overlap",
		Prompt:   "do something",
		Schedule: "* * * * *",
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create cron job: %v", err)
	}

	sched := newCronScheduler(h)
	if err := sched.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// WithRunningSession keeps StartFunc=nil so the mock's default behaviour
	// applies: the session is registered as running=true and never exits.
	// This is what keeps the session alive so Tick 2 sees previousRunActive.
	h.Claude.WithRunningSession()

	// --- Tick 1: session spawned with running=true. ---
	sched.Tick(tickTime)

	firstJob, err := h.CronJobs.Get(ctx, job.ID)
	if err != nil || firstJob.LastRunSessionID == nil {
		t.Fatal("Tick 1: expected LastRunSessionID to be set after fire")
	}
	firstSessionID := *firstJob.LastRunSessionID

	allSessions1, err := h.Sessions.List(ctx, repoID)
	if err != nil {
		t.Fatalf("list sessions after tick 1: %v", err)
	}
	if len(allSessions1) != 1 {
		t.Fatalf("Tick 1: expected 1 session, got %d", len(allSessions1))
	}

	// --- Tick 2: must be skipped (overlap). ---
	tick2Time := tickTime.Add(2 * time.Minute)
	sched.Tick(tick2Time)

	allSessions2, err := h.Sessions.List(ctx, repoID)
	if err != nil {
		t.Fatalf("list sessions after tick 2: %v", err)
	}
	if len(allSessions2) != 1 {
		t.Fatalf("Tick 2: expected overlap skip (still 1 session), got %d sessions", len(allSessions2))
	}
	// Confirm the cron job still points at the first session.
	job2, err := h.CronJobs.Get(ctx, job.ID)
	if err != nil || job2.LastRunSessionID == nil || *job2.LastRunSessionID != firstSessionID {
		t.Fatalf("Tick 2: expected LastRunSessionID=%s still; got %v", firstSessionID, job2.LastRunSessionID)
	}

	// --- Drive first session to a terminal state via CloseSession RPC. ---
	// Closed is a terminal state; previousRunActive returns false for it.
	if _, err := h.Client.CloseSession(ctx, connect.NewRequest(&pb.CloseSessionRequest{
		Id: firstSessionID,
	})); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}

	// --- Tick 3: previous session is Closed (terminal) → should fire. ---
	// Switch Claude to NoChanges so the third session exits quickly
	// (not required for the overlap assertion, but keeps the test clean).
	h.Claude.NoChanges()
	tick3Time := tickTime.Add(4 * time.Minute)
	sched.Tick(tick3Time)

	// A new session must have been created with a different ID.
	job3, err := h.CronJobs.Get(ctx, job.ID)
	if err != nil {
		t.Fatalf("get cron job after tick 3: %v", err)
	}
	if job3.LastRunSessionID == nil {
		t.Fatal("Tick 3: expected LastRunSessionID to be set")
	}
	if *job3.LastRunSessionID == firstSessionID {
		t.Fatalf("Tick 3: expected a NEW session (different from %s); got same ID — tick did not fire",
			firstSessionID)
	}
}
