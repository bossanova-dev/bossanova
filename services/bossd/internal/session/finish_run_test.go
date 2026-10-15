package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
)

// TestFinishUnattendedRun_WorktreeNoChangesFinalizesThroughCronPipeline: an
// explicit finish of a no-change worktree cron run takes the sweep's finalize —
// the worktree is removed, the row hard-deleted, and the cron job records
// deleted_no_changes.
func TestFinishUnattendedRun_WorktreeNoChangesFinalizesThroughCronPipeline(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{statusOut: ""}
	cron := &recordingCronJobStore{}
	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", LocalPath: "/tmp/repo-main"}
	cronJobID := "cron-1"
	sessions.sessions["sess-1"] = &models.Session{
		ID: "sess-1", RepoID: "repo-1", WorktreePath: "/tmp/wt-sess1",
		State: machine.ImplementingPlan, CronJobID: &cronJobID,
	}
	lc := newTestLifecycle(sessions, repos, nil, cron, wt, newMockAgentRunner(), nil, newMockVCSProvider(), zerolog.Nop())

	res, err := lc.FinishUnattendedRun(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinishUnattendedRun: %v", err)
	}
	if res.NoOp || res.Outcome != models.CronJobOutcomeDeletedNoChanges || !res.Deleted {
		t.Fatalf("result = %+v, want deleted deleted_no_changes", res)
	}
	if _, ok := sessions.sessions["sess-1"]; ok {
		t.Error("session row should have been hard-deleted")
	}
	if len(wt.archived) != 1 || wt.archived[0] != "/tmp/wt-sess1" {
		t.Errorf("archived worktrees = %v, want [/tmp/wt-sess1]", wt.archived)
	}
	if len(cron.lastRunCalls) != 1 || cron.lastRunCalls[0].outcome != models.CronJobOutcomeDeletedNoChanges {
		t.Fatalf("last-run calls = %+v, want one deleted_no_changes", cron.lastRunCalls)
	}
}

// TestFinishUnattendedRun_ZeroOutputFinalizesFromWidenedStates: a zero-output
// session is finished from the widened reap set (here a stray AwaitingChecks),
// exactly as the sweep would, and records zero_output.
func TestFinishUnattendedRun_ZeroOutputFinalizesFromWidenedStates(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{statusOut: " M user-file"}
	cron := &recordingCronJobStore{}
	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", LocalPath: "/tmp/repo"}
	cronJobID := "cron-zero"
	sessions.sessions["sess-1"] = &models.Session{
		ID: "sess-1", RepoID: "repo-1", WorktreePath: "/tmp/repo",
		State: machine.AwaitingChecks, CronJobID: &cronJobID, IsQuickChat: true,
	}
	lc := newTestLifecycle(sessions, repos, nil, cron, wt, newMockAgentRunner(), nil, newMockVCSProvider(), zerolog.Nop())

	res, err := lc.FinishUnattendedRun(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinishUnattendedRun: %v", err)
	}
	if res.Outcome != models.CronJobOutcomeZeroOutput || !res.Deleted {
		t.Fatalf("result = %+v, want deleted zero_output", res)
	}
	if wt.statusCalls != 0 {
		t.Errorf("worktrees.Status calls = %d, want 0 for the repo checkout", wt.statusCalls)
	}
	if len(cron.lastRunCalls) != 1 || cron.lastRunCalls[0].outcome != models.CronJobOutcomeZeroOutput {
		t.Fatalf("last-run calls = %+v, want one zero_output", cron.lastRunCalls)
	}
}

// TestFinishUnattendedRun_WorktreeSessionOnPRLifecycleIsNoOp: the widened
// states are a zero-output session's only. A worktree run already on its PR's
// lifecycle is not finalized again.
func TestFinishUnattendedRun_WorktreeSessionOnPRLifecycleIsNoOp(t *testing.T) {
	sessions := newMockSessionStore()
	cronJobID := "cron-1"
	sessions.sessions["sess-1"] = &models.Session{
		ID: "sess-1", RepoID: "repo-1", State: machine.AwaitingChecks, CronJobID: &cronJobID,
	}
	lc := newTestLifecycle(sessions, newMockRepoStore(), nil, &recordingCronJobStore{}, &mockWorktreeManager{}, newMockAgentRunner(), nil, newMockVCSProvider(), zerolog.Nop())

	res, err := lc.FinishUnattendedRun(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("FinishUnattendedRun: %v", err)
	}
	if !res.NoOp {
		t.Fatalf("result = %+v, want NoOp", res)
	}
	if got := sessions.sessions["sess-1"].State; got != machine.AwaitingChecks {
		t.Errorf("state = %s, want AwaitingChecks unchanged", got)
	}
}

// TestFinishUnattendedRun_RacedByFinalizeIsNoOp: the sweep or the Stop gate
// finalized the run first. While that finalize holds the row in Finalizing, the
// finish's conditional transition no-ops.
func TestFinishUnattendedRun_RacedByFinalizeIsNoOp(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{statusOut: ""}
	cron := &recordingCronJobStore{}
	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", LocalPath: "/tmp/repo-main"}
	cronJobID := "cron-1"
	sessions.sessions["sess-1"] = &models.Session{
		ID: "sess-1", RepoID: "repo-1", WorktreePath: "/tmp/wt-sess1",
		State: machine.Finalizing, CronJobID: &cronJobID,
	}
	lc := newTestLifecycle(sessions, repos, nil, cron, wt, newMockAgentRunner(), nil, newMockVCSProvider(), zerolog.Nop())

	res, err := lc.FinishUnattendedRun(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinishUnattendedRun: %v", err)
	}
	if !res.NoOp {
		t.Fatalf("result = %+v, want NoOp while another finalize owns the row", res)
	}
	if len(cron.lastRunCalls) != 0 || len(wt.archived) != 0 {
		t.Fatalf("a no-op finish recorded %d outcomes and archived %v", len(cron.lastRunCalls), wt.archived)
	}

	// The sweep finalizes a fresh run first; the finish that follows finds
	// the row gone and reports the lookup error (the server maps a missing
	// row to done), touching nothing.
	sessions.sessions["sess-2"] = &models.Session{
		ID: "sess-2", RepoID: "repo-1", WorktreePath: "/tmp/wt-sess2",
		State: machine.ImplementingPlan, CronJobID: &cronJobID,
	}
	if _, err := lc.finalizeSessionFrom(ctx, "sess-2", periodicReapStateInts()); err != nil {
		t.Fatalf("sweep finalize: %v", err)
	}
	if _, err := lc.FinishUnattendedRun(ctx, "sess-2"); err == nil {
		t.Fatal("FinishUnattendedRun on a hard-deleted row returned nil error")
	}
	if len(cron.lastRunCalls) != 1 {
		t.Fatalf("last-run calls = %d, want only the sweep's", len(cron.lastRunCalls))
	}
}

func TestFinishUnattendedRun_RefusesAttendedSession(t *testing.T) {
	sessions := newMockSessionStore()
	sessions.sessions["sess-1"] = &models.Session{ID: "sess-1", RepoID: "repo-1", State: machine.ImplementingPlan}
	lc := newTestLifecycle(sessions, newMockRepoStore(), nil, &recordingCronJobStore{}, &mockWorktreeManager{}, newMockAgentRunner(), nil, newMockVCSProvider(), zerolog.Nop())

	_, err := lc.FinishUnattendedRun(context.Background(), "sess-1")
	if !errors.Is(err, ErrNotUnattendedRun) {
		t.Fatalf("err = %v, want ErrNotUnattendedRun", err)
	}
	if got := sessions.sessions["sess-1"].State; got != machine.ImplementingPlan {
		t.Errorf("state = %s, want ImplementingPlan unchanged", got)
	}
}

func TestFinishUnattendedRun_ArchivedSessionIsNoOp(t *testing.T) {
	sessions := newMockSessionStore()
	cronJobID := "cron-1"
	archivedAt := time.Now()
	sessions.sessions["sess-1"] = &models.Session{
		ID: "sess-1", RepoID: "repo-1", State: machine.ImplementingPlan, CronJobID: &cronJobID, ArchivedAt: &archivedAt,
	}
	lc := newTestLifecycle(sessions, newMockRepoStore(), nil, &recordingCronJobStore{}, &mockWorktreeManager{}, newMockAgentRunner(), nil, newMockVCSProvider(), zerolog.Nop())

	res, err := lc.FinishUnattendedRun(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("FinishUnattendedRun: %v", err)
	}
	if !res.NoOp {
		t.Fatalf("result = %+v, want NoOp for an archived row", res)
	}
	if got := sessions.sessions["sess-1"].State; got != machine.ImplementingPlan {
		t.Errorf("state = %s, want ImplementingPlan unchanged", got)
	}
}

// TestUnattendedRunQuiet_RecentLogBlocks pins the background-subagent guard: a
// chat whose log was written inside finishQuietWindow blocks, whatever the
// tracker says; quiet and absent logs do not.
func TestUnattendedRunQuiet_RecentLogBlocks(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeLog := func(id string, mtime time.Time) {
		t.Helper()
		p := filepath.Join(dir, id+".log")
		if err := os.WriteFile(p, []byte("output\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	writeLog("chat-quiet", now.Add(-2*finishQuietWindow))
	writeLog("chat-busy", now.Add(-finishQuietWindow/2))
	// chat-nolog has no log at all (pipe-pane failed).

	chats := &mockAgentChatStore{chatsBySession: map[string][]*models.AgentChat{
		"sess-1": {
			{SessionID: "sess-1", AgentSessionID: "chat-quiet"},
			{SessionID: "sess-1", AgentSessionID: "chat-nolog"},
			{SessionID: "sess-1", AgentSessionID: "chat-busy"},
		},
	}}
	lc := newTestLifecycle(newMockSessionStore(), newMockRepoStore(), chats, &recordingCronJobStore{}, &mockWorktreeManager{}, newMockAgentRunner(), nil, newMockVCSProvider(), zerolog.Nop())
	lc.SetAgentLogsDir(dir)

	got, err := lc.unattendedRunQuietAt(context.Background(), "sess-1", now)
	if err != nil {
		t.Fatalf("unattendedRunQuietAt: %v", err)
	}
	if got != "chat-busy" {
		t.Fatalf("blocking = %q, want chat-busy (log written inside the quiet window)", got)
	}

	// Once the window has passed since the last write, nothing blocks.
	got, err = lc.unattendedRunQuietAt(context.Background(), "sess-1", now.Add(finishQuietWindow))
	if err != nil {
		t.Fatalf("unattendedRunQuietAt: %v", err)
	}
	if got != "" {
		t.Fatalf("blocking = %q, want \"\" once every log is quiet", got)
	}

	// The exported entry reads the wall clock: the fresh log still blocks.
	if got, err := lc.UnattendedRunQuiet(context.Background(), "sess-1"); err != nil || got != "chat-busy" {
		t.Fatalf("UnattendedRunQuiet = %q, %v; want chat-busy", got, err)
	}

	// A chat-list failure surfaces rather than reading as quiet.
	chats.listBySessionErr = errors.New("db down")
	if _, err := lc.unattendedRunQuietAt(context.Background(), "sess-1", now); err == nil {
		t.Fatal("chat-list failure read as quiet")
	}
}

func TestUnattendedRunQuiet_NoLogsDirIsQuiet(t *testing.T) {
	chats := &mockAgentChatStore{chatsBySession: map[string][]*models.AgentChat{
		"sess-1": {{SessionID: "sess-1", AgentSessionID: "chat-a"}},
	}}
	lc := newTestLifecycle(newMockSessionStore(), newMockRepoStore(), chats, &recordingCronJobStore{}, &mockWorktreeManager{}, newMockAgentRunner(), nil, newMockVCSProvider(), zerolog.Nop())
	got, err := lc.UnattendedRunQuiet(context.Background(), "sess-1")
	if err != nil || got != "" {
		t.Fatalf("UnattendedRunQuiet = %q, %v; want quiet with no logs dir", got, err)
	}
}

// TestReportRunBlocked_RefusesWhatItCannotAffect: only an unattended run that
// finalize has not reached yet can report BLOCKED; anything else is refused
// and leaves run_blocked_reason untouched.
func TestReportRunBlocked_RefusesWhatItCannotAffect(t *testing.T) {
	cronJobID := "cron-1"
	for name, tc := range map[string]struct {
		sess *models.Session
		want error
	}{
		"attended session":   {&models.Session{ID: "s", State: machine.ImplementingPlan}, ErrNotUnattendedRun},
		"already finalizing": {&models.Session{ID: "s", State: machine.Finalizing, CronJobID: &cronJobID}, ErrRunAlreadyFinalized},
		"already ready":      {&models.Session{ID: "s", State: machine.ReadyForReview, CronJobID: &cronJobID}, ErrRunAlreadyFinalized},
	} {
		t.Run(name, func(t *testing.T) {
			sessions := newMockSessionStore()
			sessions.sessions["s"] = tc.sess
			lc := newTestLifecycle(sessions, nil, nil, nil, nil, newMockAgentRunner(), nil, nil, zerolog.Nop())
			if err := lc.ReportRunBlocked(context.Background(), "s", "why"); !errors.Is(err, tc.want) {
				t.Fatalf("ReportRunBlocked = %v, want %v", err, tc.want)
			}
			if got := sessions.sessions["s"].RunBlockedReason; got != nil {
				t.Fatalf("run_blocked_reason = %q, want untouched", *got)
			}
		})
	}
}
