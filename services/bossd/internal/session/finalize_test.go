package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/config"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	libtelemetry "github.com/recurser/bossalib/telemetry"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/tmux"
)

type finalizeTelemetryCapture struct {
	event      libtelemetry.Event
	properties map[string]any
}

type finalizeTelemetryRecorder struct{ captures []finalizeTelemetryCapture }

func (r *finalizeTelemetryRecorder) Capture(_ context.Context, event libtelemetry.Event, _ string, properties map[string]any) {
	r.captures = append(r.captures, finalizeTelemetryCapture{event: event, properties: properties})
}
func (*finalizeTelemetryRecorder) Identify(context.Context, string, map[string]any) {}
func (*finalizeTelemetryRecorder) Alias(context.Context, string, string)            {}
func (*finalizeTelemetryRecorder) Close()                                           {}

func enableFinalizeTelemetry(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("BOSS_SETTINGS_PATH", filepath.Join(t.TempDir(), "settings.json"))
	settings := config.DefaultSettings()
	settings.EventTracingEnabled = true
	if err := config.Save(settings); err != nil {
		t.Fatalf("enable telemetry: %v", err)
	}
}

func assertFinalizeTelemetry(t *testing.T, recorder *finalizeTelemetryRecorder, wantOutcome string) {
	t.Helper()
	if len(recorder.captures) != 1 {
		t.Fatalf("session_finalized captures = %d, want 1", len(recorder.captures))
	}
	capture := recorder.captures[0]
	if capture.event != libtelemetry.EventSessionFinalized {
		t.Fatalf("event = %q, want %q", capture.event, libtelemetry.EventSessionFinalized)
	}
	if got := capture.properties["outcome"]; got != wantOutcome {
		t.Errorf("outcome = %v, want %q", got, wantOutcome)
	}
	for key := range capture.properties {
		if !libtelemetry.IsAllowedProperty(capture.event, key) {
			t.Errorf("captured unregistered property %q for %s", key, capture.event)
		}
		for _, identifier := range []string{"account_id", "account_label", "repo", "repo_name", "branch", "pr_number", "session_id", "chat_id", "job_name", "cron_expression", "prompt", "message_body"} {
			if key == identifier {
				t.Errorf("captured identifier property %q for %s", key, capture.event)
			}
		}
	}
}

func TestTelemetryAgentIsBounded(t *testing.T) {
	for input, want := range map[string]string{"claude": "claude", "codex": "codex", "opencode": "opencode", "customer-plugin-name": "other", "": "other"} {
		if got := telemetryAgent(input); got != want {
			t.Errorf("telemetryAgent(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestFinalizeSessionTelemetryNilClientPreservesNoChangesResult(t *testing.T) {
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", LocalPath: "/tmp/repo-main"}
	sessions.sessions["sess-1"] = &models.Session{
		ID: "sess-1", RepoID: "repo-1", WorktreePath: "/tmp/wt-sess1", State: machine.ImplementingPlan,
	}
	lc := newTestLifecycle(
		sessions, repos, nil, &stubCronJobStore{}, &mockWorktreeManager{statusOut: ""},
		newMockAgentRunner(), nil, newMockVCSProvider(), zerolog.Nop(),
	)
	lc.SetTelemetry(nil)

	result, err := lc.FinalizeSession(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession with nil telemetry: %v", err)
	}
	if result.Outcome != models.CronJobOutcomeDeletedNoChanges {
		t.Fatalf("outcome = %q, want %q", result.Outcome, models.CronJobOutcomeDeletedNoChanges)
	}
}

// TestFinalizeSession_NoOpWhenNotImplementing exercises the idempotency gate:
// the conditional UPDATE transitions ImplementingPlan→Finalizing; any other
// starting state must no-op without side effects. This is the property the
// hook endpoint relies on to return 200 for duplicate Stop events.
func TestFinalizeSession_NoOpWhenNotImplementing(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	cases := []struct {
		name  string
		state machine.State
	}{
		{"already_finalizing", machine.Finalizing},
		{"merged", machine.Merged},
		{"closed", machine.Closed},
		{"awaiting_checks", machine.AwaitingChecks},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sessions := newMockSessionStore()
			repos := newMockRepoStore()
			wt := &mockWorktreeManager{}
			cr := newMockAgentRunner()
			vp := newMockVCSProvider()

			sessions.sessions["sess-1"] = &models.Session{
				ID:     "sess-1",
				RepoID: "repo-1",
				State:  tc.state,
			}

			lc := newTestLifecycle(sessions, repos, nil, &stubCronJobStore{}, wt, cr, nil, vp, logger)
			res, err := lc.FinalizeSession(ctx, "sess-1")
			if err != nil {
				t.Fatalf("FinalizeSession: %v", err)
			}
			if !res.NoOp {
				t.Fatalf("expected NoOp result, got Outcome=%q", res.Outcome)
			}
			if sessions.sessions["sess-1"].State != tc.state {
				t.Fatalf("state should be unchanged; was %s, now %s", tc.state, sessions.sessions["sess-1"].State)
			}
			if len(wt.archived) != 0 {
				t.Errorf("no-op should not archive worktree (archived=%v)", wt.archived)
			}
			if len(vp.createPRCalls) != 0 {
				t.Errorf("no-op should not create PR (calls=%d)", len(vp.createPRCalls))
			}
		})
	}
}

// TestFinalizeSession_DeletedNoChanges covers the empty-git-status branch:
// the worktree must be archived AND the session row deleted. Also confirms
// the outcome is recorded on the cron job row (step 4) and the session's
// pre-existing hook_token is NOT cleared (step 5 clears only on pr_created).
func TestFinalizeSession_DeletedNoChanges(t *testing.T) {
	enableFinalizeTelemetry(t)
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{statusOut: ""} // empty = no changes
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	cron := &recordingCronJobStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main", // different from worktree so Archive runs
	}
	cronJobID := "cron-1"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-sess1",
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
	}

	lc := newTestLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)
	recorder := &finalizeTelemetryRecorder{}
	lc.SetTelemetry(recorder)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomeDeletedNoChanges {
		t.Fatalf("outcome = %q, want %q", res.Outcome, models.CronJobOutcomeDeletedNoChanges)
	}
	assertFinalizeTelemetry(t, recorder, "no_changes")
	if len(wt.archived) != 1 || wt.archived[0] != "/tmp/wt-sess1" {
		t.Errorf("expected worktree archived at /tmp/wt-sess1, got %v", wt.archived)
	}
	if _, ok := sessions.sessions["sess-1"]; ok {
		t.Error("session row should have been deleted")
	}
	if len(cron.lastRunCalls) != 1 {
		t.Fatalf("expected 1 UpdateLastRun call, got %d", len(cron.lastRunCalls))
	}
	if cron.lastRunCalls[0].outcome != models.CronJobOutcomeDeletedNoChanges {
		t.Errorf("recorded outcome = %q, want deleted_no_changes", cron.lastRunCalls[0].outcome)
	}
}

func TestFinalizeSession_ZeroOutputSkipsCheckoutClassification(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{statusOut: " M user-file"}
	cron := &recordingCronJobStore{}
	cronJobID := "cron-zero"
	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", LocalPath: "/tmp/repo"}
	sessions.sessions["sess-1"] = &models.Session{
		ID: "sess-1", RepoID: "repo-1", WorktreePath: "/tmp/repo", State: machine.ImplementingPlan,
		CronJobID: &cronJobID, IsQuickChat: true,
	}
	lc := newTestLifecycle(sessions, repos, nil, cron, wt, newMockAgentRunner(), nil, newMockVCSProvider(), zerolog.Nop())

	result, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}
	if result.Outcome != models.CronJobOutcomeZeroOutput || !result.Deleted {
		t.Fatalf("result = %+v, want deleted zero_output", result)
	}
	if wt.statusCalls != 0 {
		t.Fatalf("worktrees.Status calls = %d, want 0 for repo checkout", wt.statusCalls)
	}
	if _, ok := sessions.sessions["sess-1"]; ok {
		t.Fatal("zero-output session row still exists after teardown")
	}
	if len(cron.lastRunCalls) != 1 || cron.lastRunCalls[0].outcome != models.CronJobOutcomeZeroOutput {
		t.Fatalf("last-run calls = %+v, want one zero_output", cron.lastRunCalls)
	}
}

// TestFinalizeSession_DeletedNoChanges_ReapsBranchWithFlagOff covers BOS-424
// Change 2: a no-change cron hard-delete reaps the orphaned local branch even
// when repo.CanAutoDeleteBranches is false. finalizeNoChanges → hardDeleteSession
// never routes through ArchiveSession, so this is the only place the throwaway
// cron-* branch is reclaimed.
func TestFinalizeSession_DeletedNoChanges_ReapsBranchWithFlagOff(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{statusOut: "", branchSafeToDelete: true} // empty = no changes; branch reapable
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	cron := &recordingCronJobStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:                    "repo-1",
		LocalPath:             "/tmp/repo-main", // different from worktree so Archive runs
		DefaultBaseBranch:     "main",
		CanAutoDeleteBranches: false, // flag OFF — reap must still happen
	}
	cronJobID := "cron-1"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-sess1",
		BranchName:   "cron-abc123",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
	}

	lc := newTestLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomeDeletedNoChanges {
		t.Fatalf("outcome = %q, want %q", res.Outcome, models.CronJobOutcomeDeletedNoChanges)
	}
	if len(wt.deletedLocalBranches) != 1 || wt.deletedLocalBranches[0] != "cron-abc123" {
		t.Errorf("expected branch cron-abc123 reaped, got %v", wt.deletedLocalBranches)
	}
	if _, ok := sessions.sessions["sess-1"]; ok {
		t.Error("session row should have been deleted")
	}
}

// TestFinalizeSession_DeletedNoChanges_KeepsUnmergedBranch covers the shared
// hardDeleteSession commits-no-origin caller (BOS-424): when the branch carries
// unmerged commits (BranchSafeToDelete false), the reap keeps it — no data loss —
// even though the session row is hard-deleted.
func TestFinalizeSession_DeletedNoChanges_KeepsUnmergedBranch(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{statusOut: "", branchSafeToDelete: false} // unmerged: not safe to delete
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	cron := &recordingCronJobStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo-main",
		DefaultBaseBranch: "main",
	}
	cronJobID := "cron-1"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-sess1",
		BranchName:   "cron-unmerged",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
	}

	lc := newTestLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomeDeletedNoChanges {
		t.Fatalf("outcome = %q, want %q", res.Outcome, models.CronJobOutcomeDeletedNoChanges)
	}
	if len(wt.deletedLocalBranches) != 0 {
		t.Errorf("unmerged branch must be kept, but DeleteLocalBranch was called: %v", wt.deletedLocalBranches)
	}
}

// TestFinalizeSession_DeletedNoChanges_ReapErrorStillSucceeds covers the
// best-effort contract (BOS-424): a DeleteLocalBranch failure must not fail the
// hard-delete — the outcome stays deleted_no_changes and the session row is gone.
func TestFinalizeSession_DeletedNoChanges_ReapErrorStillSucceeds(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		statusOut:            "",
		branchSafeToDelete:   true,
		deleteLocalBranchErr: errors.New("git branch -D failed"),
	}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	cron := &recordingCronJobStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo-main",
		DefaultBaseBranch: "main",
	}
	cronJobID := "cron-1"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-sess1",
		BranchName:   "cron-reaperr",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
	}

	lc := newTestLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomeDeletedNoChanges {
		t.Fatalf("outcome = %q, want %q (reap error must not demote)", res.Outcome, models.CronJobOutcomeDeletedNoChanges)
	}
	// The reap must have been ATTEMPTED (so the error path is genuinely exercised,
	// not skipped by an earlier guard) yet still swallowed.
	if len(wt.deletedLocalBranches) != 1 || wt.deletedLocalBranches[0] != "cron-reaperr" {
		t.Errorf("expected DeleteLocalBranch attempted for cron-reaperr, got %v", wt.deletedLocalBranches)
	}
	if !res.Deleted {
		t.Error("expected Deleted = true even when reap errored")
	}
	if _, ok := sessions.sessions["sess-1"]; ok {
		t.Error("session row should have been deleted despite the reap error")
	}
}

// TestFinalizeSession_DeletedNoChanges_NeverReapsBaseBranch covers the
// base-branch guard (BOS-424): a session whose branch equals the base/default
// branch is never force-deleted, even when BranchSafeToDelete would return true
// (a ref is trivially its own ancestor).
func TestFinalizeSession_DeletedNoChanges_NeverReapsBaseBranch(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{statusOut: "", branchSafeToDelete: true} // would delete if unguarded
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	cron := &recordingCronJobStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo-main",
		DefaultBaseBranch: "main",
	}
	cronJobID := "cron-1"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-sess1",
		BranchName:   "main", // == base/default: must never be reaped
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
	}

	lc := newTestLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomeDeletedNoChanges {
		t.Fatalf("outcome = %q, want %q", res.Outcome, models.CronJobOutcomeDeletedNoChanges)
	}
	if len(wt.deletedLocalBranches) != 0 {
		t.Errorf("base branch must never be reaped, but DeleteLocalBranch was called: %v", wt.deletedLocalBranches)
	}
}

// TestFinalizeSessionFrom_AdvancesPushingBranch covers the broadened stranded-
// cron reap entry (BOS-333): finalizeSessionFrom must advance a session
// interrupted in PushingBranch — a state the ImplementingPlan-only Stop-hook
// entry would no-op on — through the shared pipeline to a terminal disposition.
func TestFinalizeSessionFrom_AdvancesPushingBranch(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{statusOut: ""} // empty = no changes -> deleted_no_changes
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	cron := &recordingCronJobStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
	}
	cronJobID := "cron-1"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-sess1",
		State:        machine.PushingBranch,
		CronJobID:    &cronJobID,
	}

	lc := newTestLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)
	res, err := lc.finalizeSessionFrom(ctx, "sess-1", strandedReapStateInts())
	if err != nil {
		t.Fatalf("finalizeSessionFrom: %v", err)
	}
	if res.NoOp {
		t.Fatal("PushingBranch is in the reap set; should have advanced, not no-op'd")
	}
	if res.Outcome != models.CronJobOutcomeDeletedNoChanges {
		t.Fatalf("outcome = %q, want %q", res.Outcome, models.CronJobOutcomeDeletedNoChanges)
	}
	if _, ok := sessions.sessions["sess-1"]; ok {
		t.Error("session row should have been deleted (terminal disposition reached)")
	}
}

// TestFinalizeSession_NoOpOnPushingBranch guards the Stop-hook regression: the
// ImplementingPlan-only FinalizeSession entry must still no-op on a session that
// is NOT in ImplementingPlan, even though the sweep's finalizeSessionFrom entry
// now accepts PushingBranch.
func TestFinalizeSession_NoOpOnPushingBranch(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()

	sessions.sessions["sess-1"] = &models.Session{
		ID:     "sess-1",
		RepoID: "repo-1",
		State:  machine.PushingBranch,
	}

	lc := newTestLifecycle(sessions, repos, nil, &stubCronJobStore{}, wt, cr, nil, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}
	if !res.NoOp {
		t.Fatalf("Stop-hook entry must no-op on PushingBranch, got Outcome=%q", res.Outcome)
	}
	if sessions.sessions["sess-1"].State != machine.PushingBranch {
		t.Fatalf("state changed to %s; want PushingBranch unchanged", sessions.sessions["sess-1"].State)
	}
}

func TestFinalizeSession_DeletedNoChangesNotifiesSessionDeleted(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{statusOut: ""}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	cron := &recordingCronJobStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
	}
	cronJobID := "cron-1"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-sess1",
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
	}

	var deleted []string
	lc := newTestLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)
	lc.SetSessionDeletedNotifier(func(_ context.Context, sessionID string) {
		deleted = append(deleted, sessionID)
	})

	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}
	if res.Outcome != models.CronJobOutcomeDeletedNoChanges {
		t.Fatalf("outcome = %q, want %q", res.Outcome, models.CronJobOutcomeDeletedNoChanges)
	}
	if len(deleted) != 1 || deleted[0] != "sess-1" {
		t.Fatalf("deleted notifications = %v, want [sess-1]", deleted)
	}
}

func TestFinalizeSession_CleanWorktreeExistingBranchPR_AttachesPR(t *testing.T) {
	enableFinalizeTelemetry(t)
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{statusOut: ""}
	vp := newMockVCSProvider()
	vp.nextOpenPRs = []vcs.PRSummary{
		{Number: 77, HeadBranch: "cron-br-1", State: vcs.PRStateOpen},
	}
	chats := &recordingAgentChatStore{}
	cron := &recordingCronJobStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@github.com:owner/repo.git",
	}
	cronJobID := "cron-1"
	hookToken := "secret-token"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-sess1",
		BranchName:   "cron-br-1",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
		HookToken:    &hookToken,
	}

	lc := newFinalizeChatLifecycle(t, sessions, repos, chats, cron, wt, vp, logger)
	recorder := &finalizeTelemetryRecorder{}
	lc.SetTelemetry(recorder)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomePRCreated {
		t.Fatalf("outcome = %q, want %q", res.Outcome, models.CronJobOutcomePRCreated)
	}
	assertFinalizeTelemetry(t, recorder, "pr_opened")
	if len(wt.archived) != 0 {
		t.Fatalf("worktree should be preserved when branch already has PR, got %v", wt.archived)
	}
	if len(vp.createPRCalls) != 0 {
		t.Fatalf("clean branch with existing PR must not create PR, calls=%d", len(vp.createPRCalls))
	}
	// No second "Finalize" chat is spawned; bossd injects the PR tag in-process.
	if len(chats.created) != 0 {
		t.Fatalf("no finalize chat should be created, got %d", len(chats.created))
	}
	if len(wt.injectPRNumbersCalls) != 1 {
		t.Fatalf("InjectPRNumbers calls = %d, want 1", len(wt.injectPRNumbersCalls))
	}
	if got := wt.injectPRNumbersCalls[0].prNumber; got != 77 {
		t.Fatalf("InjectPRNumbers PR = %d, want 77", got)
	}
	if len(vp.markReadyCalls) != 1 || vp.markReadyCalls[0] != 77 {
		t.Fatalf("MarkReadyForReview calls = %v, want [77]", vp.markReadyCalls)
	}
	sess := sessions.sessions["sess-1"]
	if sess == nil {
		t.Fatal("session row should be preserved")
	}
	if sess.PRNumber == nil || *sess.PRNumber != 77 {
		t.Fatalf("PRNumber = %v, want 77", sess.PRNumber)
	}
	if sess.PRURL == nil || *sess.PRURL != "https://github.com/owner/repo/pull/77" {
		t.Fatalf("PRURL = %v, want existing PR URL", sess.PRURL)
	}
	if sess.HookToken != nil {
		t.Fatalf("hook_token = %v, want nil after PR attachment success", sess.HookToken)
	}
	if sess.State != machine.ReadyForReview {
		t.Fatalf("state = %s, want ready_for_review", sess.State)
	}
	if len(cron.lastRunCalls) != 1 {
		t.Fatalf("UpdateLastRun calls = %d, want 1", len(cron.lastRunCalls))
	}
	if cron.lastRunCalls[0].sessionID == nil || *cron.lastRunCalls[0].sessionID != "sess-1" {
		t.Fatalf("recorded session ID = %v, want sess-1", cron.lastRunCalls[0].sessionID)
	}
	if cron.lastRunCalls[0].outcome != models.CronJobOutcomePRCreated {
		t.Fatalf("recorded outcome = %q, want pr_created", cron.lastRunCalls[0].outcome)
	}
}

// TestFinalizeSession_HeadlessNonCronEmptyRun_BlocksNoChanges pins the BOS-179
// empty-vs-real-work signal: a non-cron (headless detach) session whose branch
// carries only the empty draft-PR bootstrap commit — even though a PR exists —
// must NOT be surfaced as a green ready-for-review PR. It records pr_no_changes
// and Blocks the session so a headless /boss-epic driver fail-isolates it instead
// of merging an empty PR.
func TestFinalizeSession_HeadlessNonCronEmptyRun_BlocksNoChanges(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	// Clean worktree, and the only commit vs base is the empty bootstrap commit.
	wt := &mockWorktreeManager{
		statusOut:      "",
		commitSubjects: []string{draftPRPlaceholderCommitSubject},
	}
	vp := newMockVCSProvider()
	vp.nextOpenPRs = []vcs.PRSummary{
		{Number: 88, HeadBranch: "bos-179-br", State: vcs.PRStateOpen},
	}
	chats := &recordingAgentChatStore{}
	cron := &recordingCronJobStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@github.com:owner/repo.git",
	}
	// No CronJobID → non-cron: the real-work gate is active.
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-sess1",
		BranchName:   "bos-179-br",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
	}

	lc := newFinalizeChatLifecycle(t, sessions, repos, chats, cron, wt, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomePRNoChanges {
		t.Fatalf("outcome = %q, want %q", res.Outcome, models.CronJobOutcomePRNoChanges)
	}
	// A no-op run is never surfaced as green: no PR is marked ready, no tags injected.
	if len(vp.markReadyCalls) != 0 {
		t.Fatalf("empty run must not mark any PR ready, got %v", vp.markReadyCalls)
	}
	if len(wt.injectPRNumbersCalls) != 0 {
		t.Fatalf("empty run must not inject PR tags, got %d", len(wt.injectPRNumbersCalls))
	}
	sess := sessions.sessions["sess-1"]
	if sess == nil {
		t.Fatal("session row should be preserved for inspection")
	}
	if sess.State != machine.Blocked {
		t.Fatalf("state = %s, want blocked", sess.State)
	}
	if sess.BlockedReason == nil || *sess.BlockedReason == "" {
		t.Fatalf("blocked reason = %v, want a descriptive no-changes reason", sess.BlockedReason)
	}
}

// TestFinalizeSession_DetachNonCronEmptyRun_BlocksNoChanges is BOS-428's
// preserved-invariant regression: a detach session (Detach=true) joins the
// unattended class for recovery, but the no-real-commits → pr_no_changes → Block
// gate keys on !isCronSession, NOT isUnattendedSession. A detach session never
// sets CronJobID, so a bootstrap-only (empty) run still records pr_no_changes and
// Blocks exactly as a non-detach non-cron run does — it must not merge an empty PR.
func TestFinalizeSession_DetachNonCronEmptyRun_BlocksNoChanges(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	// Clean worktree, and the only commit vs base is the empty bootstrap commit.
	wt := &mockWorktreeManager{
		statusOut:      "",
		commitSubjects: []string{draftPRPlaceholderCommitSubject},
	}
	vp := newMockVCSProvider()
	vp.nextOpenPRs = []vcs.PRSummary{
		{Number: 88, HeadBranch: "bos-428-br", State: vcs.PRStateOpen},
	}
	chats := &recordingAgentChatStore{}
	cron := &recordingCronJobStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@github.com:owner/repo.git",
	}
	// Detach=true but CronJobID nil → still non-cron: the real-work gate is active.
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-sess1",
		BranchName:   "bos-428-br",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
		Detach:       true,
	}

	lc := newFinalizeChatLifecycle(t, sessions, repos, chats, cron, wt, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomePRNoChanges {
		t.Fatalf("outcome = %q, want %q", res.Outcome, models.CronJobOutcomePRNoChanges)
	}
	if len(vp.markReadyCalls) != 0 {
		t.Fatalf("empty detach run must not mark any PR ready, got %v", vp.markReadyCalls)
	}
	sess := sessions.sessions["sess-1"]
	if sess == nil {
		t.Fatal("session row should be preserved for inspection")
	}
	if sess.State != machine.Blocked {
		t.Fatalf("state = %s, want blocked", sess.State)
	}
	if sess.BlockedReason == nil || *sess.BlockedReason == "" {
		t.Fatalf("blocked reason = %v, want a descriptive no-changes reason", sess.BlockedReason)
	}
}

// TestFinalizeSession_PlanningOnlyNoChanges_DoesNotBlockAsImplementationFailure
// pins the BOS-322 planning-only escape: a session explicitly marked IsQuickChat
// (a visible planning/recon/plan-review chat) whose branch made no real changes
// must NOT be surfaced as a failed implementation run with pr_no_changes/Blocked.
// It is expected to produce no repository output, so finalize diverts it to the
// benign deleted_no_changes cleanup path. Same no-real-commits setup as the
// empty-implementation test above — only IsQuickChat differs, proving the persisted
// flag (not the branch state) is what flips the classification.
func TestFinalizeSession_PlanningOnlyNoChanges_DoesNotBlockAsImplementationFailure(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		statusOut:      "",
		commitSubjects: []string{draftPRPlaceholderCommitSubject},
	}
	vp := newMockVCSProvider()
	vp.nextOpenPRs = []vcs.PRSummary{
		{Number: 88, HeadBranch: "bos-322-planning", State: vcs.PRStateOpen},
	}
	chats := &recordingAgentChatStore{}
	cron := &recordingCronJobStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@github.com:owner/repo.git",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-sess1",
		BranchName:   "bos-322-planning",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
		IsQuickChat:  true,
	}

	lc := newFinalizeChatLifecycle(t, sessions, repos, chats, cron, wt, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomeDeletedNoChanges {
		t.Fatalf("outcome = %q, want %q", res.Outcome, models.CronJobOutcomeDeletedNoChanges)
	}
	// A planning-only no-change run is benign cleanup, never a failed
	// implementation run: no PR marked ready, no tags injected, not Blocked.
	if len(vp.markReadyCalls) != 0 {
		t.Fatalf("planning-only run must not mark any PR ready, got %v", vp.markReadyCalls)
	}
	if len(wt.injectPRNumbersCalls) != 0 {
		t.Fatalf("planning-only run must not inject PR tags, got %d", len(wt.injectPRNumbersCalls))
	}
	if sess := sessions.sessions["sess-1"]; sess != nil && sess.State == machine.Blocked {
		t.Fatalf("planning-only no-change session must not be blocked as a failed implementation run")
	}
}

// TestFinalizeSession_HeadlessNonCronRealWork_CreatesPR is the companion: a
// non-cron session that committed real work (a non-placeholder commit) still
// finalizes to a green ready-for-review PR — the gate must not block legitimate
// (even tiny/docs) work.
func TestFinalizeSession_HeadlessNonCronRealWork_CreatesPR(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		statusOut:      "",
		commitSubjects: []string{"docs: tiny but real change"},
	}
	vp := newMockVCSProvider()
	vp.nextOpenPRs = []vcs.PRSummary{
		{Number: 89, HeadBranch: "bos-179-br", State: vcs.PRStateOpen},
	}
	chats := &recordingAgentChatStore{}
	cron := &recordingCronJobStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@github.com:owner/repo.git",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-sess1",
		BranchName:   "bos-179-br",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
	}

	lc := newFinalizeChatLifecycle(t, sessions, repos, chats, cron, wt, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomePRCreated {
		t.Fatalf("outcome = %q, want %q", res.Outcome, models.CronJobOutcomePRCreated)
	}
	if len(vp.markReadyCalls) != 1 || vp.markReadyCalls[0] != 89 {
		t.Fatalf("MarkReadyForReview calls = %v, want [89]", vp.markReadyCalls)
	}
	sess := sessions.sessions["sess-1"]
	if sess.State != machine.ReadyForReview {
		t.Fatalf("state = %s, want ready_for_review", sess.State)
	}
}

// TestFinalizeSession_BranchHasRealCommits_UsesRemoteTrackingBase pins BOS-591's
// fix 3: branchHasRealCommits must ask CommitSubjects for the branch's diff
// against the freshly-fetched remote-tracking base (refs/remotes/origin/<base>),
// not the bare local base branch name. A freshly-created worktree's local base
// can be stale, so commits already merged to the real base since that local ref
// was last updated would otherwise show up as branch work and defeat the empty-
// run guard. The mock returns extra already-merged subjects for the bare local
// base ("main") but only the placeholder for the remote-tracking ref, so a pass
// here proves the guard is asking for the right ref rather than merely happening
// to see the same (empty) answer either way.
func TestFinalizeSession_BranchHasRealCommits_UsesRemoteTrackingBase(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		statusOut: "",
		commitSubjectsFn: func(baseRef string) ([]string, error) {
			if baseRef == "main" {
				// The stale local base: bootstrap placeholder plus commits that
				// already merged into the real (remote) base — must NOT be what
				// the guard consults.
				return []string{draftPRPlaceholderCommitSubject, "fix(x): already merged upstream"}, nil
			}
			return []string{draftPRPlaceholderCommitSubject}, nil
		},
	}
	vp := newMockVCSProvider()
	vp.nextOpenPRs = []vcs.PRSummary{
		{Number: 88, HeadBranch: "bos-591-br", State: vcs.PRStateOpen},
	}
	chats := &recordingAgentChatStore{}
	cron := &recordingCronJobStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@github.com:owner/repo.git",
	}
	// No CronJobID → non-cron: the real-work gate is active.
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-sess1",
		BranchName:   "bos-591-br",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
	}

	lc := newFinalizeChatLifecycle(t, sessions, repos, chats, cron, wt, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomePRNoChanges {
		t.Fatalf("outcome = %q, want %q (a stale-local-base bug would report real work instead)", res.Outcome, models.CronJobOutcomePRNoChanges)
	}
	sess := sessions.sessions["sess-1"]
	if sess == nil {
		t.Fatal("session row should be preserved for inspection")
	}
	if sess.State != machine.Blocked {
		t.Fatalf("state = %s, want blocked", sess.State)
	}

	found := false
	for _, ref := range wt.commitSubjectsBaseRefs {
		if ref == "refs/remotes/origin/main" {
			found = true
		}
	}
	if !found {
		t.Fatalf("CommitSubjects base refs = %v, want one call with refs/remotes/origin/main", wt.commitSubjectsBaseRefs)
	}
	foundStale := false
	for _, ref := range wt.commitSubjectsBaseRefs {
		if ref == "main" {
			foundStale = true
		}
	}
	if foundStale {
		t.Fatalf("CommitSubjects base refs = %v, must not include the bare local base branch name", wt.commitSubjectsBaseRefs)
	}
	fetched := false
	for _, base := range wt.fetchedBases {
		if base == "main" {
			fetched = true
		}
	}
	if !fetched {
		t.Fatalf("FetchBase calls = %v, want a fetch of the base branch before checking commit subjects", wt.fetchedBases)
	}
}

// TestFinalizeSession_BranchHasRealCommits_FetchBaseFailure_FailsOpen pins the
// fail-open contract: when FetchBase fails inside branchHasRealCommits, the
// guard must treat the error as "could not determine real-vs-placeholder" and
// fall back to treating the run as real work (warn-and-continue), never
// silently swallow the error nor fall back to the local base ref. A stale-only
// commit set would otherwise (if the fetch failure were ignored) report
// hasReal == false and Block the session; this proves it does not.
func TestFinalizeSession_BranchHasRealCommits_FetchBaseFailure_FailsOpen(t *testing.T) {
	ctx := context.Background()
	var logBuf bytes.Buffer
	logger := zerolog.New(&logBuf)

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		statusOut:      "",
		commitSubjects: []string{draftPRPlaceholderCommitSubject},
		fetchBaseErr:   fmt.Errorf("network unreachable"),
	}
	vp := newMockVCSProvider()
	vp.nextOpenPRs = []vcs.PRSummary{
		{Number: 88, HeadBranch: "bos-591-fetchfail", State: vcs.PRStateOpen},
	}
	chats := &recordingAgentChatStore{}
	cron := &recordingCronJobStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@github.com:owner/repo.git",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-sess1",
		BranchName:   "bos-591-fetchfail",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
	}

	lc := newFinalizeChatLifecycle(t, sessions, repos, chats, cron, wt, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome == models.CronJobOutcomePRNoChanges {
		t.Fatalf("outcome = %q, a FetchBase failure must fail open (treat as real work), not record pr_no_changes", res.Outcome)
	}
	if len(wt.commitSubjectsBaseRefs) != 0 {
		t.Fatalf("CommitSubjects must not be called after FetchBase fails, got base refs %v", wt.commitSubjectsBaseRefs)
	}
	// The plan's criterion is fail-open AND a logged warning — silent
	// fail-open is what made BOS-591 undiagnosable in the first place.
	if !strings.Contains(logBuf.String(), "real-commit check failed for clean branch with PR") {
		t.Errorf("fail-open must log its warning\nlog:\n%s", logBuf.String())
	}
}

// TestFinalizeSession_BranchHasRealCommits_CommitSubjectsError_FailsOpen mirrors
// the fetch-failure case for a CommitSubjects error against the (successfully
// fetched) remote-tracking ref: the guard must still fail open to "real work".
func TestFinalizeSession_BranchHasRealCommits_CommitSubjectsError_FailsOpen(t *testing.T) {
	ctx := context.Background()
	var logBuf bytes.Buffer
	logger := zerolog.New(&logBuf)

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		statusOut:         "",
		commitSubjectsErr: fmt.Errorf("git log failed: bad object"),
	}
	vp := newMockVCSProvider()
	vp.nextOpenPRs = []vcs.PRSummary{
		{Number: 88, HeadBranch: "bos-591-logfail", State: vcs.PRStateOpen},
	}
	chats := &recordingAgentChatStore{}
	cron := &recordingCronJobStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@github.com:owner/repo.git",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-sess1",
		BranchName:   "bos-591-logfail",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
	}

	lc := newFinalizeChatLifecycle(t, sessions, repos, chats, cron, wt, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome == models.CronJobOutcomePRNoChanges {
		t.Fatalf("outcome = %q, a CommitSubjects failure must fail open (treat as real work), not record pr_no_changes", res.Outcome)
	}
	found := false
	for _, ref := range wt.commitSubjectsBaseRefs {
		if ref == "refs/remotes/origin/main" {
			found = true
		}
	}
	if !found {
		t.Fatalf("CommitSubjects base refs = %v, want one call with refs/remotes/origin/main", wt.commitSubjectsBaseRefs)
	}
	if !strings.Contains(logBuf.String(), "real-commit check failed for clean branch with PR") {
		t.Errorf("fail-open must log its warning\nlog:\n%s", logBuf.String())
	}
}

// TestFinalizeSession_RealCommitGuard_LogsEveryDecision pins BOS-591's fix 4
// part A: the empty-run guard must record its decision on EVERY branch,
// including the previously-silent hasReal == true path — the omission is
// exactly why the incident could not be diagnosed after the fact. The
// real-work line must also carry the real-commit count, so a future bypass
// shows up in the log as "the guard counted N real commits" rather than as
// silence indistinguishable from the guard never running.
func TestFinalizeSession_RealCommitGuard_LogsEveryDecision(t *testing.T) {
	tests := []struct {
		name           string
		commitSubjects []string
		wantSubstrings []string
	}{
		{
			name:           "real work logs the decision and the count",
			commitSubjects: []string{draftPRPlaceholderCommitSubject, "feat(x): real change", "docs: another"},
			wantSubstrings: []string{
				"finalize: clean branch has an existing PR with real commits",
				`"real_commits":2`,
				`"session":"sess-1"`,
			},
		},
		{
			name:           "no real work still logs the no-op decision",
			commitSubjects: []string{draftPRPlaceholderCommitSubject},
			wantSubstrings: []string{
				"finalize: clean branch has an existing PR but no real commits",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			var logBuf bytes.Buffer
			logger := zerolog.New(&logBuf)

			sessions := newMockSessionStore()
			repos := newMockRepoStore()
			wt := &mockWorktreeManager{
				statusOut:      "",
				commitSubjects: tt.commitSubjects,
			}
			vp := newMockVCSProvider()
			vp.nextOpenPRs = []vcs.PRSummary{
				{Number: 91, HeadBranch: "bos-591-log", State: vcs.PRStateOpen},
			}
			chats := &recordingAgentChatStore{}
			cron := &recordingCronJobStore{}

			repos.repos["repo-1"] = &models.Repo{
				ID:        "repo-1",
				LocalPath: "/tmp/repo-main",
				OriginURL: "git@github.com:owner/repo.git",
			}
			sessions.sessions["sess-1"] = &models.Session{
				ID:           "sess-1",
				RepoID:       "repo-1",
				WorktreePath: "/tmp/wt-sess1",
				BranchName:   "bos-591-log",
				BaseBranch:   "main",
				State:        machine.ImplementingPlan,
			}

			lc := newFinalizeChatLifecycle(t, sessions, repos, chats, cron, wt, vp, logger)
			if _, err := lc.FinalizeSession(ctx, "sess-1"); err != nil {
				t.Fatalf("FinalizeSession: %v", err)
			}

			got := logBuf.String()
			for _, want := range tt.wantSubstrings {
				if !strings.Contains(got, want) {
					t.Errorf("guard log missing %q\nlog:\n%s", want, got)
				}
			}
		})
	}
}

// TestFinalizeSession_MarkReadyBackstop_RefusesEmptyDiff pins BOS-591's fix 4
// part B: markFinalizePRReady is the single funnel for every mark-ready call
// site, so an empty diff against the base must stop the write there regardless
// of how the code arrived. This is the last line of defence behind the guard:
// the branch here clears the guard outright (its commit subject is real work,
// not a placeholder) yet still changes nothing relative to the base — the
// residual shape of any future guard bypass — and is refused anyway.
func TestFinalizeSession_MarkReadyBackstop_RefusesEmptyDiff(t *testing.T) {
	ctx := context.Background()
	var logBuf bytes.Buffer
	logger := zerolog.New(&logBuf)

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		statusOut: "",
		// Clears the guard (a genuine, non-placeholder subject) yet the branch's
		// diff against the base is empty — the residue of any future bypass.
		commitSubjects:       []string{"feat(x): passes the guard but changes nothing"},
		hasDiffAgainstBase:   false,
		hasDiffAgainstBaseOK: true,
	}
	vp := newMockVCSProvider()
	vp.nextOpenPRs = []vcs.PRSummary{
		{Number: 92, HeadBranch: "bos-591-backstop", State: vcs.PRStateOpen},
	}
	chats := &recordingAgentChatStore{}
	cron := &recordingCronJobStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@github.com:owner/repo.git",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-sess1",
		BranchName:   "bos-591-backstop",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
	}

	lc := newFinalizeChatLifecycle(t, sessions, repos, chats, cron, wt, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	// pr_no_changes, not pr_failed: nothing broke — the PR was opened and pushed
	// fine, the run simply produced nothing. Both Block the session, but the
	// recorded reason has to be truthful.
	if res.Outcome != models.CronJobOutcomePRNoChanges {
		t.Fatalf("outcome = %q, want %q", res.Outcome, models.CronJobOutcomePRNoChanges)
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "empty diff") {
		t.Fatalf("error = %v, want one naming the empty diff", res.Err)
	}
	if !errors.Is(res.Err, errEmptyDiffRefusedReady) {
		t.Fatalf("error = %v, want one wrapping errEmptyDiffRefusedReady", res.Err)
	}
	if !needsAttention(res.Outcome) {
		t.Fatalf("outcome %q must still route the session to Blocked", res.Outcome)
	}
	if len(vp.markReadyCalls) != 0 {
		t.Fatalf("empty-diff PR must never be marked ready, got %v", vp.markReadyCalls)
	}
	// The refusal must be diagnosable: session, branch, PR number and base ref.
	// The PR number is asserted as the zerolog JSON field ("pr":92), not a bare
	// "92" substring, so it can't accidentally match an unrelated number (e.g.
	// a session/branch name) elsewhere in the buffer.
	for _, want := range []string{"sess-1", "bos-591-backstop", `"pr":92`, "refs/remotes/origin/main"} {
		if !strings.Contains(logBuf.String(), want) {
			t.Errorf("refusal log missing %q\nlog:\n%s", want, logBuf.String())
		}
	}
	if got := wt.hasDiffAgainstBaseRefs; len(got) == 0 || got[len(got)-1] != "refs/remotes/origin/main" {
		t.Fatalf("HasDiffAgainstBase base refs = %v, want the remote-tracking base ref", got)
	}
}

// TestFinalizeSession_MarkReadyBackstop_AllowsNonEmptyDiff is the companion:
// a branch with a real diff against the base still marks the PR ready.
func TestFinalizeSession_MarkReadyBackstop_AllowsNonEmptyDiff(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		statusOut:            "",
		commitSubjects:       []string{"feat(x): real change"},
		hasDiffAgainstBase:   true,
		hasDiffAgainstBaseOK: true,
	}
	vp := newMockVCSProvider()
	vp.nextOpenPRs = []vcs.PRSummary{
		{Number: 93, HeadBranch: "bos-591-allow", State: vcs.PRStateOpen},
	}
	chats := &recordingAgentChatStore{}
	cron := &recordingCronJobStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@github.com:owner/repo.git",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-sess1",
		BranchName:   "bos-591-allow",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
	}

	lc := newFinalizeChatLifecycle(t, sessions, repos, chats, cron, wt, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomePRCreated {
		t.Fatalf("outcome = %q, want %q", res.Outcome, models.CronJobOutcomePRCreated)
	}
	if len(vp.markReadyCalls) != 1 || vp.markReadyCalls[0] != 93 {
		t.Fatalf("MarkReadyForReview calls = %v, want [93]", vp.markReadyCalls)
	}
	// The backstop deliberately does NOT fetch: it relies on injectPRTagsAndPush
	// having just refreshed refs/remotes/origin/<base> at every mark-ready call
	// site. Nothing in the type system enforces that, and if it ever stopped
	// holding, the missing ref would make `git diff` error and the backstop would
	// fail open — a silent bypass of the replacement for a silently-bypassed
	// guard. Pin the ordering: a fetch of this base must precede the diff check.
	if len(wt.fetchedBases) == 0 {
		t.Fatalf("HasDiffAgainstBase ran with no prior FetchBase; the backstop's freshness invariant is broken")
	}
	if wt.fetchedBases[0] != "main" {
		t.Fatalf("fetched bases = %v, want the session's base fetched first", wt.fetchedBases)
	}
	if len(wt.hasDiffAgainstBaseRefs) == 0 {
		t.Fatalf("HasDiffAgainstBase was never called; the backstop did not run")
	}
}

// TestFinalizeSession_MarkReadyProviderFailure_IsPRFailedNotNoChanges is the
// negative half of markReadyFailureOutcome: only the BOS-591 empty-diff refusal
// maps to pr_no_changes. A branch with a real diff whose MarkReadyForReview call
// genuinely fails (GitHub error, bad token) must still record pr_failed —
// without this, the helper could return pr_no_changes unconditionally and every
// other test would still pass.
func TestFinalizeSession_MarkReadyProviderFailure_IsPRFailedNotNoChanges(t *testing.T) {
	enableFinalizeTelemetry(t)
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		statusOut:            "",
		commitSubjects:       []string{"feat(x): real change"},
		hasDiffAgainstBase:   true,
		hasDiffAgainstBaseOK: true,
	}
	vp := newMockVCSProvider()
	vp.nextOpenPRs = []vcs.PRSummary{
		{Number: 94, HeadBranch: "bos-591-provider-fail", State: vcs.PRStateOpen},
	}
	vp.markReadyErr = errors.New("github: 403 forbidden")
	chats := &recordingAgentChatStore{}
	cron := &recordingCronJobStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@github.com:owner/repo.git",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-sess1",
		BranchName:   "bos-591-provider-fail",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
	}

	lc := newFinalizeChatLifecycle(t, sessions, repos, chats, cron, wt, vp, logger)
	recorder := &finalizeTelemetryRecorder{}
	lc.SetTelemetry(recorder)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomePRFailed {
		t.Fatalf("outcome = %q, want %q", res.Outcome, models.CronJobOutcomePRFailed)
	}
	assertFinalizeTelemetry(t, recorder, "error")
	if errors.Is(res.Err, errEmptyDiffRefusedReady) {
		t.Fatalf("a provider failure must not be classified as the empty-diff refusal: %v", res.Err)
	}
}

// TestFinalizeSession_MarkReadyBackstop_DiffErrorFailsOpen pins the fail-open
// half of the backstop: an empty diff is definite evidence of a no-op run, but
// an UNREADABLE diff is not — a git failure must never block a legitimate run,
// matching this file's established convention (see branchHasRealCommits).
func TestFinalizeSession_MarkReadyBackstop_DiffErrorFailsOpen(t *testing.T) {
	ctx := context.Background()
	var logBuf bytes.Buffer
	logger := zerolog.New(&logBuf)

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		statusOut:             "",
		commitSubjects:        []string{"feat(x): real change"},
		hasDiffAgainstBaseOK:  true,
		hasDiffAgainstBaseErr: fmt.Errorf("git diff: bad object"),
	}
	vp := newMockVCSProvider()
	vp.nextOpenPRs = []vcs.PRSummary{
		{Number: 94, HeadBranch: "bos-591-failopen", State: vcs.PRStateOpen},
	}
	chats := &recordingAgentChatStore{}
	cron := &recordingCronJobStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@github.com:owner/repo.git",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-sess1",
		BranchName:   "bos-591-failopen",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
	}

	lc := newFinalizeChatLifecycle(t, sessions, repos, chats, cron, wt, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomePRCreated {
		t.Fatalf("outcome = %q, want %q (a diff-computation error must fail open)", res.Outcome, models.CronJobOutcomePRCreated)
	}
	if len(vp.markReadyCalls) != 1 || vp.markReadyCalls[0] != 94 {
		t.Fatalf("MarkReadyForReview calls = %v, want [94]", vp.markReadyCalls)
	}
	// Assert the actual fail-open message, not a bare "warn" substring — the
	// level word alone would match vacuously against any unrelated WARN that
	// enters the finalize flow.
	if !strings.Contains(logBuf.String(), "empty-diff check failed before mark ready") {
		t.Errorf("fail-open must log the empty-diff-check-failed WARN, log:\n%s", logBuf.String())
	}
}

// TestFinalizeSession_CronCleanBranchRealWork_BackstopLeavesFlowUnchanged pins
// decision D2: the backstop lives inside markFinalizePRReady so it also covers
// cron, but a cron run with genuine work must be byte-identical to today —
// same outcome, same provider calls, same tag injection.
func TestFinalizeSession_CronCleanBranchRealWork_BackstopLeavesFlowUnchanged(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		statusOut:            "",
		commitSubjects:       []string{"feat(x): cron did real work"},
		hasDiffAgainstBase:   true,
		hasDiffAgainstBaseOK: true,
	}
	vp := newMockVCSProvider()
	vp.nextOpenPRs = []vcs.PRSummary{
		{Number: 95, HeadBranch: "cron-real-br", State: vcs.PRStateOpen},
	}
	chats := &recordingAgentChatStore{}
	cron := &recordingCronJobStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@github.com:owner/repo.git",
	}
	cronJobID := "cron-1"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-sess1",
		BranchName:   "cron-real-br",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
	}

	lc := newFinalizeChatLifecycle(t, sessions, repos, chats, cron, wt, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomePRCreated {
		t.Fatalf("outcome = %q, want %q", res.Outcome, models.CronJobOutcomePRCreated)
	}
	if len(vp.markReadyCalls) != 1 || vp.markReadyCalls[0] != 95 {
		t.Fatalf("MarkReadyForReview calls = %v, want [95]", vp.markReadyCalls)
	}
	if len(wt.injectPRNumbersCalls) != 1 {
		t.Fatalf("InjectPRNumbers calls = %d, want 1", len(wt.injectPRNumbersCalls))
	}
}

func TestFinalizeSession_CleanWorktreeExistingBranchPR_TagInjectionFailureBlocksSession(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		statusOut:          "",
		injectPRNumbersErr: fmt.Errorf("force-push rewritten commits: stale lease"),
	}
	vp := newMockVCSProvider()
	vp.nextOpenPRs = []vcs.PRSummary{
		{Number: 77, HeadBranch: "cron-br-1", State: vcs.PRStateOpen},
	}
	chats := &recordingAgentChatStore{}
	cron := &recordingCronJobStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@github.com:owner/repo.git",
	}
	cronJobID := "cron-1"
	hookToken := "secret-token"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-sess1",
		BranchName:   "cron-br-1",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
		HookToken:    &hookToken,
	}

	lc := newFinalizeChatLifecycle(t, sessions, repos, chats, cron, wt, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomePRFailed {
		t.Fatalf("outcome = %q, want pr_failed", res.Outcome)
	}
	if len(wt.injectPRNumbersCalls) != 1 {
		t.Fatalf("InjectPRNumbers calls = %d, want 1", len(wt.injectPRNumbersCalls))
	}
	if sessions.sessions["sess-1"].HookToken == nil {
		t.Fatalf("hook_token was cleared despite tag injection failure")
	}
	if got := sessions.sessions["sess-1"].State; got != machine.Blocked {
		t.Fatalf("state = %s, want blocked", got)
	}
	if len(cron.lastRunCalls) != 1 || cron.lastRunCalls[0].outcome != models.CronJobOutcomePRFailed {
		t.Fatalf("outcome recording: got %+v, want single pr_failed entry", cron.lastRunCalls)
	}
}

func TestFinalizeSession_CleanWorktreeExistingBranchPRTitleUpdateFailure_BlocksSession(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{statusOut: ""}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	vp.nextOpenPRs = []vcs.PRSummary{
		{Number: 77, HeadBranch: "cron-br-1", State: vcs.PRStateOpen},
	}
	vp.updatePRTitleErr = fmt.Errorf("gh pr edit failed")
	cron := &recordingCronJobStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@github.com:owner/repo.git",
	}
	cronJobID := "cron-1"
	hookToken := "secret-token"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		Title:        "Cron job",
		WorktreePath: "/tmp/wt-sess1",
		BranchName:   "cron-br-1",
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
		HookToken:    &hookToken,
	}

	lc := newTestLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomePRFailed {
		t.Fatalf("outcome = %q, want %q", res.Outcome, models.CronJobOutcomePRFailed)
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "update attached PR title") {
		t.Fatalf("Err = %v, want title update failure", res.Err)
	}
	if len(vp.updatePRTitleCalls) != 1 {
		t.Fatalf("UpdatePRTitle calls = %d, want 1", len(vp.updatePRTitleCalls))
	}
	if len(wt.archived) != 0 {
		t.Fatalf("worktree should be preserved on pr_failed, got %v", wt.archived)
	}
	sess := sessions.sessions["sess-1"]
	if sess == nil {
		t.Fatal("session row should be preserved")
	}
	if sess.State != machine.Blocked {
		t.Fatalf("state = %s, want blocked", sess.State)
	}
	if sess.BlockedReason == nil ||
		!strings.Contains(*sess.BlockedReason, "pr_failed") ||
		!strings.Contains(*sess.BlockedReason, "gh pr edit failed") {
		t.Fatalf("BlockedReason = %v, want finalize failure reason", sess.BlockedReason)
	}
	if sess.PRNumber != nil {
		t.Fatalf("PRNumber = %v, want nil after title update failure", sess.PRNumber)
	}
	if len(cron.lastRunCalls) != 1 {
		t.Fatalf("UpdateLastRun calls = %d, want 1", len(cron.lastRunCalls))
	}
	if cron.lastRunCalls[0].outcome != models.CronJobOutcomePRFailed {
		t.Fatalf("recorded outcome = %q, want %q",
			cron.lastRunCalls[0].outcome, models.CronJobOutcomePRFailed)
	}
	if cron.lastRunCalls[0].sessionID == nil || *cron.lastRunCalls[0].sessionID != "sess-1" {
		t.Fatalf("recorded session ID = %v, want sess-1", cron.lastRunCalls[0].sessionID)
	}
}

// TestFinalizeSession_MissingWorktree_BenignWorktreeGone proves that finalizing
// a session whose worktree is already gone (archived/removed) yields the benign
// worktree_gone outcome — NOT pr_failed — and does not transition the session to
// Blocked or persist a scary blocked reason. This is the defensive fix for the
// spurious "finalize failed (pr_failed)" errors on cron/headless sessions.
func TestFinalizeSession_MissingWorktree_BenignWorktreeGone(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	// Status fails because the worktree path is gone: git reports it is not a
	// git repository. Mirrors Manager.Status wrapping the runGit error.
	wt := &mockWorktreeManager{
		statusErr: fmt.Errorf("git status: fatal: not a git repository (or any of the parent directories): .git"),
	}
	cr := newMockAgentRunner()
	cron := &recordingCronJobStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@github.com:owner/repo.git",
	}
	cronJobID := "cron-1"
	hookToken := "secret-token"
	// A worktree path that genuinely does not exist on disk (never created).
	gonePath := t.TempDir() + "/removed-worktree"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		WorktreePath: gonePath,
		BranchName:   "cron-br-1",
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
		HookToken:    &hookToken,
	}

	lc := newTestLifecycle(sessions, repos, nil, cron, wt, cr, nil, nil, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomeWorktreeGone {
		t.Fatalf("outcome = %q, want %q (benign, not pr_failed)",
			res.Outcome, models.CronJobOutcomeWorktreeGone)
	}
	sess := sessions.sessions["sess-1"]
	if sess == nil {
		t.Fatal("session row should be preserved")
	}
	if sess.State == machine.Blocked {
		t.Fatalf("state = %s, want NOT blocked (worktree_gone is benign)", sess.State)
	}
	if sess.BlockedReason != nil {
		t.Fatalf("BlockedReason = %v, want nil (no pr_failed reason persisted)", *sess.BlockedReason)
	}
	// The recorded cron outcome is worktree_gone, not pr_failed.
	if len(cron.lastRunCalls) != 1 || cron.lastRunCalls[0].outcome != models.CronJobOutcomeWorktreeGone {
		t.Fatalf("recorded outcome = %+v, want single worktree_gone entry", cron.lastRunCalls)
	}
}

// TestNeedsAttention_WorktreeGone_False pins that worktree_gone is a
// non-attention outcome, so a finalize against a gone worktree never routes the
// session to Blocked.
func TestNeedsAttention_WorktreeGone_False(t *testing.T) {
	if needsAttention(models.CronJobOutcomeWorktreeGone) {
		t.Fatal("needsAttention(worktree_gone) = true, want false (benign no-op)")
	}
}

// TestFinalizeSession_LiveWorktreeStatusError_PRFailed is the non-regression
// twin of the worktree_gone test: when `git status` fails but the worktree path
// still exists on disk, the failure is a genuine one against a live (corrupt or
// de-initialized) worktree — so it must stay the attention-worthy pr_failed
// (Blocked, scary reason), NOT be swallowed as benign worktree_gone. The status
// error text here even names "not a git repository" (the exact string
// worktreeIsMissing matches as a fallback), so this pins that os.Stat confirming
// the path is present wins over the error-text fallback: a live-but-corrupt
// worktree must never be reclassified as gone.
func TestFinalizeSession_LiveWorktreeStatusError_PRFailed(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	// Status fails with a git error whose text DOES name a missing repository —
	// the fallback substring — but the worktree path below is present on disk
	// (e.g. a corrupt/de-initialized worktree whose .git was removed). The
	// present-path check must win, keeping this a genuine pr_failed.
	wt := &mockWorktreeManager{
		statusErr: fmt.Errorf("git status: fatal: not a git repository (or any of the parent directories): .git"),
	}
	cr := newMockAgentRunner()
	cron := &recordingCronJobStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@github.com:owner/repo.git",
	}
	cronJobID := "cron-1"
	hookToken := "secret-token"
	// A worktree path that genuinely exists on disk (t.TempDir), so os.Stat
	// confirms presence and the missing-worktree fallback must not apply.
	livePath := t.TempDir()
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		WorktreePath: livePath,
		BranchName:   "cron-br-1",
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
		HookToken:    &hookToken,
	}

	lc := newTestLifecycle(sessions, repos, nil, cron, wt, cr, nil, nil, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomePRFailed {
		t.Fatalf("outcome = %q, want %q (live path, generic git error is not benign)",
			res.Outcome, models.CronJobOutcomePRFailed)
	}
	sess := sessions.sessions["sess-1"]
	if sess == nil {
		t.Fatal("session row should be preserved")
	}
	if sess.State != machine.Blocked {
		t.Fatalf("state = %s, want blocked (genuine finalize failure)", sess.State)
	}
	if sess.BlockedReason == nil || !strings.Contains(*sess.BlockedReason, "pr_failed") {
		t.Fatalf("BlockedReason = %v, want a pr_failed reason persisted", sess.BlockedReason)
	}
	if len(cron.lastRunCalls) != 1 || cron.lastRunCalls[0].outcome != models.CronJobOutcomePRFailed {
		t.Fatalf("recorded outcome = %+v, want single pr_failed entry", cron.lastRunCalls)
	}
}

// TestWorktreeIsMissing gives the nuanced worktreeIsMissing helper direct
// table-driven coverage of every documented branch — the integration tests only
// exercise the present-path and absent-path happy branches. The precedence under
// test: os.Stat confirming PRESENCE wins (a live-but-corrupt worktree stays a
// genuine failure regardless of error text); os.IsNotExist is the authoritative
// "gone" signal; and the git-error-text fallback ("not a git repository" / "no
// such file or directory") only stands in when the path cannot be confirmed
// present (empty path). Pinning these makes the empty-path fallback an
// intentional, tested design choice rather than an accident.
func TestWorktreeIsMissing(t *testing.T) {
	present := t.TempDir()             // a path that exists on disk
	absent := t.TempDir() + "/gone-wt" // never created → os.Stat IsNotExist
	notARepo := fmt.Errorf("fatal: not a git repository (or any of the parent directories): .git")
	noSuchFile := fmt.Errorf("chdir /x: no such file or directory")
	unrelated := fmt.Errorf("fatal: unable to read tree object")

	tests := []struct {
		name      string
		path      string
		statusErr error
		want      bool
	}{
		// Present path wins over the error-text fallback: a corrupt/de-initialized
		// live worktree is a genuine failure (pr_failed), never benign.
		{"present path + not-a-git-repo text -> live failure", present, notARepo, false},
		{"present path + no-such-file text -> live failure", present, noSuchFile, false},
		{"present path + nil err -> live failure", present, nil, false},
		// Absent path is the authoritative gone signal, regardless of error text.
		{"absent path + unrelated err -> gone", absent, unrelated, true},
		{"absent path + nil err -> gone", absent, nil, true},
		// Empty path can't name a live worktree: the error-text fallback stands in.
		{"empty path + not-a-git-repo text -> gone (fallback)", "", notARepo, true},
		{"empty path + no-such-file text -> gone (fallback)", "", noSuchFile, true},
		{"empty path + unrelated err -> live failure", "", unrelated, false},
		{"empty path + nil err -> live failure", "", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := worktreeIsMissing(tc.path, tc.statusErr); got != tc.want {
				t.Fatalf("worktreeIsMissing(%q, %v) = %v, want %v", tc.path, tc.statusErr, got, tc.want)
			}
		})
	}
}

func TestFinalizeSession_CleanWorktreeNoBranchPR_DeletesSession(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{statusOut: ""}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	vp.nextOpenPRs = []vcs.PRSummary{
		{Number: 77, HeadBranch: "other-branch", State: vcs.PRStateOpen},
	}
	cron := &recordingCronJobStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@github.com:owner/repo.git",
	}
	cronJobID := "cron-1"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-sess1",
		BranchName:   "cron-br-1",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
	}

	lc := newTestLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomeDeletedNoChanges {
		t.Fatalf("outcome = %q, want deleted_no_changes", res.Outcome)
	}
	if _, ok := sessions.sessions["sess-1"]; ok {
		t.Fatal("session row should be deleted when no matching branch PR exists")
	}
	if len(wt.archived) != 1 || wt.archived[0] != "/tmp/wt-sess1" {
		t.Fatalf("archived = %v, want /tmp/wt-sess1", wt.archived)
	}
}

func TestFinalizeSession_CleanCommittedBranchNoPR_CreatesPR(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		statusOut:           "",
		latestCommitSubject: "fix(cron): preserve PR-backed clean sessions",
		isAncestorFn: func(_, ref, target string) (bool, error) {
			if ref == "HEAD" && target == "refs/remotes/origin/main" {
				return false, nil
			}
			return true, nil
		},
	}
	vp := newMockVCSProvider()
	cron := &recordingCronJobStore{}
	chats := &recordingAgentChatStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@github.com:owner/repo.git",
	}
	cronJobID := "cron-1"
	hookToken := "secret-token"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		Title:        "Cron job",
		Plan:         "Do thing",
		WorktreePath: "/tmp/wt-sess1",
		BranchName:   "cron-br-1",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
		HookToken:    &hookToken,
	}

	lc := newFinalizeChatLifecycle(t, sessions, repos, chats, cron, wt, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomePRCreated {
		t.Fatalf("outcome = %q, want pr_created", res.Outcome)
	}
	if len(wt.archived) != 0 {
		t.Fatalf("worktree should be preserved when committed branch needs PR, got %v", wt.archived)
	}
	if len(wt.emptyCommits) != 0 {
		t.Fatalf("clean committed branch should not get placeholder commit, got %v", wt.emptyCommits)
	}
	if len(wt.fetchedBases) == 0 || wt.fetchedBases[0] != "main" {
		t.Fatalf("fetched bases = %v, want main fetched", wt.fetchedBases)
	}
	if len(wt.pushed) != 1 || wt.pushed[0] != "cron-br-1" {
		t.Fatalf("pushed branches = %v, want [cron-br-1]", wt.pushed)
	}
	if len(vp.createPRCalls) != 1 {
		t.Fatalf("CreateDraftPR calls = %d, want 1", len(vp.createPRCalls))
	}
	if vp.createPRCalls[0].Title != "Preserve PR-backed clean sessions" {
		t.Fatalf("CreateDraftPR title = %q, want %q", vp.createPRCalls[0].Title, "Preserve PR-backed clean sessions")
	}
	// bossd injects the PR tag in-process instead of spawning a finalize chat.
	if len(wt.injectPRNumbersCalls) != 1 {
		t.Fatalf("InjectPRNumbers calls = %d, want 1", len(wt.injectPRNumbersCalls))
	}
	if len(vp.markReadyCalls) != 1 || vp.markReadyCalls[0] != 42 {
		t.Fatalf("MarkReadyForReview calls = %v, want [42]", vp.markReadyCalls)
	}
	if len(chats.created) != 0 {
		t.Fatalf("no finalize chat should be created, got %d", len(chats.created))
	}
	if sessions.sessions["sess-1"].HookToken != nil {
		t.Fatalf("hook_token = %v, want nil after PR creation success", sessions.sessions["sess-1"].HookToken)
	}
	if len(cron.lastRunCalls) != 1 || cron.lastRunCalls[0].outcome != models.CronJobOutcomePRCreated {
		t.Fatalf("outcome recording: got %+v, want single pr_created entry", cron.lastRunCalls)
	}
}

func TestFinalizeSession_CleanCommittedBranchEmptyOrigin_RedetectsBeforeCreatingPR(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		statusOut:           "",
		latestCommitSubject: "fix(cron): preserve PR-backed clean sessions",
		originURL:           "git@github.com:owner/repo.git",
		isAncestorFn: func(_, ref, target string) (bool, error) {
			if ref == "HEAD" && target == "refs/remotes/origin/main" {
				return false, nil
			}
			return true, nil
		},
	}
	vp := newMockVCSProvider()
	cron := &recordingCronJobStore{}
	chats := &recordingAgentChatStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "",
	}
	cronJobID := "cron-1"
	hookToken := "secret-token"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		Title:        "Cron job",
		Plan:         "Do thing",
		WorktreePath: "/tmp/wt-sess1",
		BranchName:   "cron-br-1",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
		HookToken:    &hookToken,
	}

	lc := newFinalizeChatLifecycle(t, sessions, repos, chats, cron, wt, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomePRCreated {
		t.Fatalf("outcome = %q, want pr_created", res.Outcome)
	}
	if repos.repos["repo-1"].OriginURL != "git@github.com:owner/repo.git" {
		t.Fatalf("repo origin = %q, want re-detected GitHub origin", repos.repos["repo-1"].OriginURL)
	}
	if len(vp.createPRCalls) != 1 {
		t.Fatalf("CreateDraftPR calls = %d, want 1", len(vp.createPRCalls))
	}
	if len(wt.archived) != 0 {
		t.Fatalf("worktree should be preserved when creating PR, got %v", wt.archived)
	}
	if len(wt.injectPRNumbersCalls) != 1 {
		t.Fatalf("InjectPRNumbers calls = %d, want 1", len(wt.injectPRNumbersCalls))
	}
	if len(vp.markReadyCalls) != 1 || vp.markReadyCalls[0] != 42 {
		t.Fatalf("MarkReadyForReview calls = %v, want [42]", vp.markReadyCalls)
	}
	if len(cron.lastRunCalls) != 1 || cron.lastRunCalls[0].outcome != models.CronJobOutcomePRCreated {
		t.Fatalf("outcome recording: got %+v, want single pr_created entry", cron.lastRunCalls)
	}
}

func TestFinalizeSession_CleanCommittedBranchNoOriginNoBranchWorkDeletesSession(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		statusOut:    "",
		fetchBaseErr: fmt.Errorf("remote origin does not exist"),
		isAncestorFn: func(_, ref, target string) (bool, error) {
			if ref == "HEAD" && target == "main" {
				return true, nil
			}
			t.Fatalf("unexpected ancestor check %q -> %q", ref, target)
			return false, nil
		},
	}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	cron := &recordingCronJobStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "",
	}
	cronJobID := "cron-1"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-sess1",
		BranchName:   "cron-br-1",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
	}

	lc := newTestLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomeDeletedNoChanges {
		t.Fatalf("outcome = %q, want %q", res.Outcome, models.CronJobOutcomeDeletedNoChanges)
	}
	if len(wt.fetchedBases) != 0 {
		t.Fatalf("FetchBase should not run for no-origin clean branch, got %v", wt.fetchedBases)
	}
	if _, ok := sessions.sessions["sess-1"]; ok {
		t.Error("session row should have been deleted")
	}
	if len(vp.createPRCalls) != 0 {
		t.Fatalf("CreateDraftPR calls = %d, want 0", len(vp.createPRCalls))
	}
}

// A cron session that committed all its work cleanly (empty git status) in a
// repo with no GitHub origin reaches createPRIfCleanBranchHasCommittedWork —
// the common healthy-cron flow, distinct from the dirty-output path. It must be
// hard-deleted (no PR is possible) without a FetchBase against the missing
// origin.
func TestFinalizeSession_CleanCommittedBranchNoOriginDeletesCronSessionWithoutFetch(t *testing.T) {
	enableFinalizeTelemetry(t)
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		statusOut:    "",
		fetchBaseErr: fmt.Errorf("remote origin does not exist"),
		isAncestorFn: func(_, ref, target string) (bool, error) {
			if ref == "HEAD" && target == "main" {
				return false, nil
			}
			t.Fatalf("unexpected ancestor check %q -> %q", ref, target)
			return false, nil
		},
	}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	cron := &recordingCronJobStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "",
	}
	cronJobID := "cron-1"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-sess1",
		BranchName:   "cron-br-1",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
	}

	lc := newTestLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)
	recorder := &finalizeTelemetryRecorder{}
	lc.SetTelemetry(recorder)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomePRSkippedNoGitHub {
		t.Fatalf("outcome = %q, want %q", res.Outcome, models.CronJobOutcomePRSkippedNoGitHub)
	}
	if !res.Deleted {
		t.Error("Deleted should be true for hard-deleted clean-committed cron session")
	}
	assertFinalizeTelemetry(t, recorder, "error")
	if len(wt.fetchedBases) != 0 {
		t.Fatalf("FetchBase should not run for no-origin clean branch, got %v", wt.fetchedBases)
	}
	// Cron session must be hard-deleted: worktree archived and row gone.
	if len(wt.archived) != 1 || wt.archived[0] != "/tmp/wt-sess1" {
		t.Errorf("expected worktree archived at /tmp/wt-sess1, got archived=%v", wt.archived)
	}
	if _, ok := sessions.sessions["sess-1"]; ok {
		t.Error("cron session row should be deleted on clean-committed pr_skipped_no_github")
	}
	if len(vp.createPRCalls) != 0 {
		t.Fatalf("CreateDraftPR calls = %d, want 0", len(vp.createPRCalls))
	}
	if len(cron.lastRunCalls) != 1 || cron.lastRunCalls[0].outcome != models.CronJobOutcomePRSkippedNoGitHub {
		t.Fatalf("outcome recording: got %+v, want single pr_skipped_no_github entry", cron.lastRunCalls)
	}
	// Step 4 FK guard: deleted session ID must NOT be recorded.
	if cron.lastRunCalls[0].sessionID != nil {
		t.Errorf("UpdateLastRun sessionID should be nil for deleted session, got %v", *cron.lastRunCalls[0].sessionID)
	}
}

// The interactive (non-cron) counterpart of the clean-committed no-origin path:
// it must be PRESERVED. Deleting interactive sessions is a data-loss footgun and
// is gated out by isCronSession on this branch too, not only the dirty path.
func TestFinalizeSession_CleanCommittedBranchNoOriginInteractivePreserved(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		statusOut: "",
		isAncestorFn: func(_, ref, target string) (bool, error) {
			if ref == "HEAD" && target == "main" {
				return false, nil
			}
			t.Fatalf("unexpected ancestor check %q -> %q", ref, target)
			return false, nil
		},
	}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	cron := &recordingCronJobStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "",
	}
	// No CronJobID → interactive session.
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-sess1",
		BranchName:   "br-1",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
	}

	lc := newTestLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomePRSkippedNoGitHub {
		t.Fatalf("outcome = %q, want %q", res.Outcome, models.CronJobOutcomePRSkippedNoGitHub)
	}
	if res.Deleted {
		t.Error("Deleted must be false for an interactive session")
	}
	if len(wt.archived) != 0 {
		t.Errorf("interactive worktree must be preserved, got archived=%v", wt.archived)
	}
	if _, ok := sessions.sessions["sess-1"]; !ok {
		t.Error("interactive session row must be preserved")
	}
}

func TestFinalizeSession_CleanWorktreePRLookupFailure_DeletesNoChanges(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{statusOut: ""}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	vp.listOpenPRErr = fmt.Errorf("github unavailable")
	cron := &recordingCronJobStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@github.com:owner/repo.git",
	}
	cronJobID := "cron-1"
	hookToken := "secret-token"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-sess1",
		BranchName:   "cron-br-1",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
		HookToken:    &hookToken,
	}

	lc := newTestLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomeDeletedNoChanges {
		t.Fatalf("outcome = %q, want %q", res.Outcome, models.CronJobOutcomeDeletedNoChanges)
	}
	if res.Err != nil {
		t.Fatalf("Err = %v, want nil", res.Err)
	}
	if len(wt.archived) != 1 || wt.archived[0] != "/tmp/wt-sess1" {
		t.Fatalf("expected worktree archived at /tmp/wt-sess1, got %v", wt.archived)
	}
	if _, ok := sessions.sessions["sess-1"]; ok {
		t.Fatal("session row should have been deleted")
	}
	if len(cron.lastRunCalls) != 1 {
		t.Fatalf("UpdateLastRun calls = %d, want 1", len(cron.lastRunCalls))
	}
	if cron.lastRunCalls[0].outcome != models.CronJobOutcomeDeletedNoChanges {
		t.Fatalf("recorded outcome = %q, want %q",
			cron.lastRunCalls[0].outcome, models.CronJobOutcomeDeletedNoChanges)
	}
	if cron.lastRunCalls[0].sessionID != nil {
		t.Fatalf("recorded session ID = %v, want nil after session delete", cron.lastRunCalls[0].sessionID)
	}
	if cron.lastRunCalls[0].expectedSessionID == nil || *cron.lastRunCalls[0].expectedSessionID != "sess-1" {
		t.Fatalf("expected session guard = %v, want sess-1", cron.lastRunCalls[0].expectedSessionID)
	}
}

// TestFinalizeSession_HookConfigOnly_DeletedNoChanges proves that the
// bossd-managed Claude files must NOT be classified as Claude-authored
// changes. Without this filtering, a "do nothing" cron run lands in
// pr_failed → Blocked because git status reports bossd-owned files as
// untracked, kicking the finalize pipeline down the EnsurePR path; this
// was observed in prod for cron job "Another Cron Test". The hook config
// also contains a bearer token, so it must be ignored on the no-changes
// branch (and never staged for commit by anything downstream).
func TestFinalizeSession_HookConfigOnly_DeletedNoChanges(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	cases := []struct {
		name      string
		statusOut string
	}{
		{"hook_config_alone", "?? .claude/settings.local.json\n"},
		{"hook_config_with_trailing_whitespace", "?? .claude/settings.local.json  \n"},
		{"hook_config_among_blank_lines", "\n?? .claude/settings.local.json\n\n"},
		{"scheduled_tasks_lock_alone", "?? .claude/scheduled_tasks.lock\n"},
		{"hook_config_and_scheduled_tasks_lock", "?? .claude/settings.local.json\n?? .claude/scheduled_tasks.lock\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sessions := newMockSessionStore()
			repos := newMockRepoStore()
			wt := &mockWorktreeManager{statusOut: tc.statusOut}
			cr := newMockAgentRunner()
			vp := newMockVCSProvider()
			cron := &recordingCronJobStore{}

			repos.repos["repo-1"] = &models.Repo{
				ID:        "repo-1",
				LocalPath: "/tmp/repo-main",
				// GitHub origin so that, without the filter, the failure mode
				// is pr_failed (EnsurePR is attempted) rather than the
				// non-GitHub branch — matches the prod failure shape.
				OriginURL: "git@github.com:owner/repo.git",
			}
			cronJobID := "cron-1"
			sessions.sessions["sess-1"] = &models.Session{
				ID:           "sess-1",
				RepoID:       "repo-1",
				WorktreePath: "/tmp/wt-sess1",
				State:        machine.ImplementingPlan,
				CronJobID:    &cronJobID,
			}

			lc := newTestLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)
			res, err := lc.FinalizeSession(ctx, "sess-1")
			if err != nil {
				t.Fatalf("FinalizeSession: %v", err)
			}
			if res.Outcome != models.CronJobOutcomeDeletedNoChanges {
				t.Fatalf("outcome = %q, want %q (statusOut=%q)",
					res.Outcome, models.CronJobOutcomeDeletedNoChanges, tc.statusOut)
			}
			if len(vp.createPRCalls) != 0 {
				t.Errorf("EnsurePR must NOT be called when only the hook config is dirty (calls=%d)",
					len(vp.createPRCalls))
			}
			if _, ok := sessions.sessions["sess-1"]; ok {
				t.Error("session row should have been deleted on no-changes branch")
			}
		})
	}
}

func TestFinalizeSession_LoadedAgentKeepsBossdManagedFiles_DeletedNoChanges(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{statusOut: "?? .superpowers/\n"}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	cron := &recordingCronJobStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@github.com:owner/repo.git",
	}
	cronJobID := "cron-1"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-sess1",
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
		AgentName:    "codex",
	}

	lc := newTestLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)
	lc.SetAgents(map[string]agent.AgentRunnerClient{"codex": newFakeAgent()})

	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}
	if res.Outcome != models.CronJobOutcomeDeletedNoChanges {
		t.Fatalf("outcome = %q, want %q", res.Outcome, models.CronJobOutcomeDeletedNoChanges)
	}
	if len(vp.createPRCalls) != 0 {
		t.Fatalf("CreateDraftPR calls = %d, want 0", len(vp.createPRCalls))
	}
	if _, ok := sessions.sessions["sess-1"]; ok {
		t.Fatal("session row should have been deleted on no-changes branch")
	}
}

// TestFinalizeSession_PRSkippedNoGitHub covers the non-GitHub origin branch
// for a cron session: changes exist but origin is not GitHub, so the cron
// session is hard-deleted (worktree archived, session row removed).
// The outcome is still pr_skipped_no_github (enum kept stable); Deleted=true
// ensures the caller does not try to record the deleted session ID or
// transition the (now-gone) row to Blocked.
func TestFinalizeSession_PRSkippedNoGitHub(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{statusOut: "?? new.txt"} // untracked file
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	cron := &recordingCronJobStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@gitlab.example.com:owner/repo.git", // not github.com
	}
	cronJobID := "cron-1"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-sess1",
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
	}

	lc := newTestLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomePRSkippedNoGitHub {
		t.Fatalf("outcome = %q, want %q", res.Outcome, models.CronJobOutcomePRSkippedNoGitHub)
	}
	// Cron session must be hard-deleted: worktree archived and row gone.
	if len(wt.archived) != 1 || wt.archived[0] != "/tmp/wt-sess1" {
		t.Errorf("expected worktree archived at /tmp/wt-sess1, got archived=%v", wt.archived)
	}
	if _, ok := sessions.sessions["sess-1"]; ok {
		t.Error("cron session row should be deleted on pr_skipped_no_github")
	}
	// Deleted=true means Step 4 must not pass the session ID (FK guard) and
	// Step 6 must not try to transition the gone row to Blocked.
	if !res.Deleted {
		t.Error("Deleted should be true for hard-deleted cron session")
	}
	if len(cron.lastRunCalls) != 1 || cron.lastRunCalls[0].outcome != models.CronJobOutcomePRSkippedNoGitHub {
		t.Errorf("outcome recording: got %+v, want single pr_skipped_no_github entry", cron.lastRunCalls)
	}
	// Step 4 FK guard: deleted session ID must NOT be recorded.
	if cron.lastRunCalls[0].sessionID != nil {
		t.Errorf("UpdateLastRun sessionID should be nil for deleted session, got %v", *cron.lastRunCalls[0].sessionID)
	}
}

// TestFinalizeSession_PRSkippedNoGitHub_Interactive is the critical negative
// guard: an interactive (non-cron, CronJobID==nil) session in a no-GitHub
// repo must NOT be auto-deleted. The session row must be preserved and the
// outcome is pr_skipped_no_github. Because this is an interactive session
// (no CronJobID), the cron-job outcome recording (Step 4) is skipped entirely.
func TestFinalizeSession_PRSkippedNoGitHub_Interactive(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{statusOut: "?? new.txt"} // untracked file
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	cron := &recordingCronJobStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@gitlab.example.com:owner/repo.git", // not github.com
	}
	// CronJobID is nil: interactive session, not spawned by cron.
	sessions.sessions["sess-interactive"] = &models.Session{
		ID:           "sess-interactive",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-interactive",
		State:        machine.ImplementingPlan,
		CronJobID:    nil,
	}

	lc := newTestLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-interactive")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomePRSkippedNoGitHub {
		t.Fatalf("outcome = %q, want %q", res.Outcome, models.CronJobOutcomePRSkippedNoGitHub)
	}
	// Interactive session must NOT be archived — deleting it would be data loss.
	if len(wt.archived) != 0 {
		t.Errorf("interactive session worktree must NOT be archived, got archived=%v", wt.archived)
	}
	// Session row must still exist.
	if _, ok := sessions.sessions["sess-interactive"]; !ok {
		t.Error("interactive session row must be preserved on pr_skipped_no_github")
	}
	// Not deleted: Deleted must be false.
	if res.Deleted {
		t.Error("Deleted should be false for preserved interactive session")
	}
	// No cron recording (CronJobID == nil skips Step 4).
	if len(cron.lastRunCalls) != 0 {
		t.Errorf("expected no UpdateLastRun calls for interactive session, got %d", len(cron.lastRunCalls))
	}
}

func TestFinalizeSession_DirtyUncommittedCronOutputOpensPRThenBlocksBeforeTagInjection(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		statusOut:           " M services/bossd/internal/session/finalize.go\n",
		latestCommitSubject: "fix(bossd): preserve dirty cron output",
	}
	vp := newMockVCSProvider()
	cron := &recordingCronJobStore{}
	chats := &recordingAgentChatStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@github.com:owner/repo.git",
	}
	cronJobID := "cron-1"
	hookToken := "secret-token"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		Title:        "Cron job",
		Plan:         "Do thing",
		WorktreePath: "/tmp/wt-sess1",
		BranchName:   "cron-br-1",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
		HookToken:    &hookToken,
	}

	lc := newFinalizeChatLifecycle(t, sessions, repos, chats, cron, wt, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomePRFailed {
		t.Fatalf("outcome = %q, want %q", res.Outcome, models.CronJobOutcomePRFailed)
	}
	// Dirty cron output must get a PR opened so the run is reviewable.
	if len(vp.createPRCalls) != 1 {
		t.Fatalf("CreateDraftPR calls = %d, want 1", len(vp.createPRCalls))
	}
	// This branch has no commits beyond base (mock IsAncestor defaults to
	// true), so a placeholder commit must be added so GitHub accepts the PR.
	if wt.emptyCommitCalls != 1 {
		t.Fatalf("EmptyCommit calls = %d, want 1 (placeholder for zero-commit dirty branch)", wt.emptyCommitCalls)
	}
	// No finalize chat is spawned, and bossd must not run the rebase-based PR
	// tag injection while preserved dirty output remains in the worktree.
	if len(chats.created) != 0 {
		t.Fatalf("no finalize chat should be created, got %d", len(chats.created))
	}
	if len(wt.injectPRNumbersCalls) != 0 {
		t.Fatalf("InjectPRNumbers calls = %d, want 0", len(wt.injectPRNumbersCalls))
	}
	if len(vp.markReadyCalls) != 0 {
		t.Fatalf("MarkReadyForReview calls = %v, want none", vp.markReadyCalls)
	}
}

// TestFinalizeSession_DirtyUncommittedCronOutput_UntrackedOnlyEmptyDiff_BackstopRefuses
// pins the OTHER dirty-cron-output shape (the tracked-changes variant is
// TestFinalizeSession_DirtyUncommittedCronOutputOpensPRThenBlocksBeforeTagInjection
// above, which blocks at injectPRTagsAndPush before markFinalizePRReady is ever
// reached). Untracked-only leftovers (e.g. a stray plan file) do NOT block
// PR-tag injection (see the "only untracked leftovers remain" comment in
// injectPRTagsAndPush), so this run reaches markFinalizePRReady with zero real
// commits and only bossd's --allow-empty placeholder — a diff-against-base that
// is deterministically empty. Without the BOS-591 backstop this used to end
// pr_created with a vacuously green PR; mockWorktreeManager defaults
// hasDiffAgainstBase to true, so nothing pinned this outcome until this test.
//
// The outcome is pr_no_changes, not pr_failed: the placeholder commit, the PR
// and the tag injection all succeeded — the run just produced nothing. This is
// the deterministic cron shape whose finalize outcome BOS-591 deliberately
// changes (any cron path reaching markFinalizePRReady with an empty diff is
// affected — e.g. commits that net to zero — but this one is empty by
// construction). The plan's "cron finalize is byte-identical" wording does not
// survive the universal backstop, and narrowing the backstop would leave cron
// able to advertise the exact vacuous-green PR this ticket exists to stop.
func TestFinalizeSession_DirtyUncommittedCronOutput_UntrackedOnlyEmptyDiff_BackstopRefuses(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		statusOut: "?? scratch/notes.md\n",
		// Zero real commits beyond base (IsAncestor defaults to true, so
		// cleanBranchHasCommittedWork reports false and the placeholder commit
		// path runs). The placeholder itself carries no file diff, matching
		// production: an --allow-empty commit changes nothing relative to base.
		hasDiffAgainstBase:   false,
		hasDiffAgainstBaseOK: true,
	}
	vp := newMockVCSProvider()
	cron := &recordingCronJobStore{}
	chats := &recordingAgentChatStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@github.com:owner/repo.git",
	}
	cronJobID := "cron-1"
	hookToken := "secret-token"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		Title:        "Cron job",
		Plan:         "Do thing",
		WorktreePath: "/tmp/wt-sess1",
		BranchName:   "cron-br-1",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
		HookToken:    &hookToken,
	}

	lc := newFinalizeChatLifecycle(t, sessions, repos, chats, cron, wt, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomePRNoChanges {
		t.Fatalf("outcome = %q, want %q", res.Outcome, models.CronJobOutcomePRNoChanges)
	}
	if !errors.Is(res.Err, errEmptyDiffRefusedReady) {
		t.Fatalf("error = %v, want one wrapping errEmptyDiffRefusedReady", res.Err)
	}
	// A placeholder commit is still made (GitHub requires one to open the PR),
	// and PR-tag injection still runs (untracked-only leftovers don't block
	// it) — but the backstop must refuse the mark-ready write.
	if wt.emptyCommitCalls != 1 {
		t.Fatalf("EmptyCommit calls = %d, want 1 (placeholder for zero-commit dirty branch)", wt.emptyCommitCalls)
	}
	if len(wt.injectPRNumbersCalls) != 1 {
		t.Fatalf("InjectPRNumbers calls = %d, want 1 (untracked-only leftovers don't block tag injection)", len(wt.injectPRNumbersCalls))
	}
	if len(vp.markReadyCalls) != 0 {
		t.Fatalf("MarkReadyForReview calls = %v, want none", vp.markReadyCalls)
	}
}

func TestFinalizeSession_DirtyUncommittedCronOutput_TagInjectionFailureBlocksSession(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		statusOut:           " M services/bossd/internal/session/finalize.go\n",
		latestCommitSubject: "fix(bossd): preserve dirty cron output",
	}
	vp := newMockVCSProvider()
	cron := &recordingCronJobStore{}
	chats := &recordingAgentChatStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@github.com:owner/repo.git",
	}
	cronJobID := "cron-1"
	hookToken := "secret-token"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		Title:        "Cron job",
		Plan:         "Do thing",
		WorktreePath: "/tmp/wt-sess1",
		BranchName:   "cron-br-1",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
		HookToken:    &hookToken,
	}

	lc := newFinalizeChatLifecycle(t, sessions, repos, chats, cron, wt, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomePRFailed {
		t.Fatalf("outcome = %q, want pr_failed", res.Outcome)
	}
	if len(vp.createPRCalls) != 1 {
		t.Fatalf("CreateDraftPR calls = %d, want 1", len(vp.createPRCalls))
	}
	if len(wt.injectPRNumbersCalls) != 0 {
		t.Fatalf("InjectPRNumbers calls = %d, want 0", len(wt.injectPRNumbersCalls))
	}
	if len(vp.markReadyCalls) != 0 {
		t.Fatalf("MarkReadyForReview calls = %v, want none", vp.markReadyCalls)
	}
	if sessions.sessions["sess-1"].HookToken == nil {
		t.Fatalf("hook_token was cleared despite tag injection failure")
	}
	if got := sessions.sessions["sess-1"].State; got != machine.Blocked {
		t.Fatalf("state = %s, want blocked", got)
	}
	if len(cron.lastRunCalls) != 1 || cron.lastRunCalls[0].outcome != models.CronJobOutcomePRFailed {
		t.Fatalf("outcome recording: got %+v, want single pr_failed entry", cron.lastRunCalls)
	}
}

func TestPorcelainHasTrackedChanges(t *testing.T) {
	cases := []struct {
		name      string
		porcelain string
		want      bool
	}{
		{"empty", "", false},
		{"untracked only", "?? .superpowers/\n?? docs/plans/2026-06-27-x.md", false},
		{"modified tracked", " M services/bossd/internal/session/finalize.go", true},
		{"staged add", "A  newfile.go", true},
		{"deleted tracked", " D oldfile.go", true},
		{"mixed tracked + untracked", "?? .superpowers/\n M finalize.go", true},
		{"blank lines among untracked", "?? a\n\n?? b\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := porcelainHasTrackedChanges(tc.porcelain); got != tc.want {
				t.Fatalf("porcelainHasTrackedChanges(%q) = %v, want %v", tc.porcelain, got, tc.want)
			}
		})
	}
}

// TestFinalizeSession_UntrackedOnlyLeftovers_InjectsTagsAndMarksReady
// covers a cron run whose only leftover dirty entries are UNTRACKED scratch
// artifacts (a plan file the agent forgot to commit and the .superpowers/
// framework scratch dir). These are not implementation work, so finalize must
// still inject PR tags and mark the PR ready (pr_created) rather than publishing
// a tagless PR or routing the session to pr_failed → Blocked as a tracked change
// would.
func TestFinalizeSession_UntrackedOnlyLeftovers_InjectsTagsAndMarksReady(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		statusOut:           "?? .superpowers/\n?? docs/plans/2026-06-27-some-ticket.md\n",
		latestCommitSubject: "feat(bossd): implement the ticket",
	}
	vp := newMockVCSProvider()
	cron := &recordingCronJobStore{}
	chats := &recordingAgentChatStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@github.com:owner/repo.git",
	}
	cronJobID := "cron-1"
	hookToken := "secret-token"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		Title:        "Cron job",
		Plan:         "Do thing",
		WorktreePath: "/tmp/wt-sess1",
		BranchName:   "cron-br-1",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
		HookToken:    &hookToken,
	}

	lc := newFinalizeChatLifecycle(t, sessions, repos, chats, cron, wt, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomePRCreated {
		t.Fatalf("outcome = %q, want pr_created", res.Outcome)
	}
	if len(vp.createPRCalls) != 1 {
		t.Fatalf("CreateDraftPR calls = %d, want 1", len(vp.createPRCalls))
	}
	if len(wt.injectPRNumbersCalls) != 1 {
		t.Fatalf("InjectPRNumbers calls = %d, want 1", len(wt.injectPRNumbersCalls))
	}
	if got := wt.injectPRNumbersCalls[0].prNumber; got != 42 {
		t.Fatalf("InjectPRNumbers PR = %d, want 42", got)
	}
	if got := wt.injectPRNumbersCalls[0].baseRef; got != "refs/remotes/origin/main" {
		t.Fatalf("InjectPRNumbers baseRef = %q, want refs/remotes/origin/main", got)
	}
	if len(vp.markReadyCalls) != 1 {
		t.Fatalf("MarkReadyForReview calls = %d, want 1", len(vp.markReadyCalls))
	}
	if got := sessions.sessions["sess-1"].State; got == machine.Blocked {
		t.Fatalf("state = %s, want non-blocked terminal state", got)
	}
	// The session title is synced to the freshly-created draft PR's title
	// (normalized latest commit subject), not left as the cron job name.
	if got := sessions.sessions["sess-1"].Title; got != "Implement the ticket" {
		t.Fatalf("title = %q, want %q (synced from draft PR)", got, "Implement the ticket")
	}
	if len(cron.lastRunCalls) != 1 || cron.lastRunCalls[0].outcome != models.CronJobOutcomePRCreated {
		t.Fatalf("outcome recording: got %+v, want single pr_created entry", cron.lastRunCalls)
	}
}

// TestFinalizeSession_DirtyUncommittedCronOutputPRFailed covers the dirty cron
// output path when opening the PR genuinely fails (here GitHub rejects the
// create even after the placeholder commit). The outcome is pr_failed and the
// worktree is preserved.
func TestFinalizeSession_DirtyUncommittedCronOutputPRFailed(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		statusOut:           " M services/bossd/internal/session/finalize.go\n",
		latestCommitSubject: "fix(bossd): preserve dirty cron output",
	}
	vp := newMockVCSProvider()
	vp.createPRErr = fmt.Errorf("github 422: no commits between main and cron-br-1")
	cron := &recordingCronJobStore{}
	chats := &recordingAgentChatStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@github.com:owner/repo.git",
	}
	cronJobID := "cron-1"
	hookToken := "secret-token"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		Title:        "Cron job",
		Plan:         "Do thing",
		WorktreePath: "/tmp/wt-sess1",
		BranchName:   "cron-br-1",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
		HookToken:    &hookToken,
	}

	lc := newFinalizeChatLifecycle(t, sessions, repos, chats, cron, wt, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomePRFailed {
		t.Fatalf("outcome = %q, want %q", res.Outcome, models.CronJobOutcomePRFailed)
	}
	if len(chats.created) != 0 {
		t.Fatalf("finalize chats created = %d, want 0 when no PR could be opened", len(chats.created))
	}
}

// TestFinalizeSession_DirtyOnlyCronUsesSessionTitleNotPlaceholder verifies that
// when a dirty-only cron run gets its placeholder commit as HEAD, the draft PR
// title falls back to the cron/session title rather than being derived from the
// placeholder commit subject ("create pull request"). bossd does not repair PR
// titles after creation, so the title set at creation must be correct.
func TestFinalizeSession_DirtyOnlyCronUsesSessionTitleNotPlaceholder(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	// Simulate the placeholder commit being HEAD (what EnsurePR/openDraftPRForBranch
	// would read after the dirty path adds it for a zero-commit branch).
	wt := &mockWorktreeManager{
		statusOut:           " M services/bossd/internal/session/finalize.go\n",
		latestCommitSubject: draftPRPlaceholderCommitSubject,
	}
	vp := newMockVCSProvider()
	cron := &recordingCronJobStore{}
	chats := &recordingAgentChatStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@github.com:owner/repo.git",
	}
	cronJobID := "cron-1"
	hookToken := "secret-token"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		Title:        "Nightly mutation tests",
		Plan:         "Do thing",
		WorktreePath: "/tmp/wt-sess1",
		BranchName:   "cron-br-1",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
		HookToken:    &hookToken,
	}

	lc := newFinalizeChatLifecycle(t, sessions, repos, chats, cron, wt, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomePRFailed {
		t.Fatalf("outcome = %q, want %q", res.Outcome, models.CronJobOutcomePRFailed)
	}
	if len(vp.createPRCalls) != 1 {
		t.Fatalf("CreateDraftPR calls = %d, want 1", len(vp.createPRCalls))
	}
	if got := vp.createPRCalls[0].Title; got != "Nightly mutation tests" {
		t.Errorf("draft PR title = %q, want %q (session title, not the placeholder subject)", got, "Nightly mutation tests")
	}
	if len(wt.injectPRNumbersCalls) != 0 {
		t.Fatalf("InjectPRNumbers calls = %d, want 0", len(wt.injectPRNumbersCalls))
	}
	if len(vp.markReadyCalls) != 0 {
		t.Fatalf("MarkReadyForReview calls = %v, want none", vp.markReadyCalls)
	}
}

// TestFinalizeSession_PRCreated covers the happy path: a clean branch contains
// committed work on a GitHub-linked repo, EnsurePR opens the PR, and bossd
// injects the PR number into the commit subjects in-process (no second chat).
// Outcome must be pr_created, the session moves to ReadyForReview after the
// PR is marked ready, and hook_token is cleared so a replayed Stop event can
// no longer authenticate.
func TestFinalizeSession_PRCreated(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		statusOut:           "",
		latestCommitSubject: "test(mutate): add tests for surviving mutants",
		isAncestorFn: func(_, ref, target string) (bool, error) {
			if ref == "HEAD" && target == "refs/remotes/origin/main" {
				return false, nil
			}
			return true, nil
		},
	}
	vp := newMockVCSProvider()
	cron := &recordingCronJobStore{}
	chats := &recordingAgentChatStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@github.com:owner/repo.git",
	}
	cronJobID := "cron-1"
	hookToken := "secret-token"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		Title:        "Cron job",
		Plan:         "Do thing",
		WorktreePath: "/tmp/wt-sess1",
		BranchName:   "cron-br-1",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
		HookToken:    &hookToken,
	}

	lc := newFinalizeChatLifecycle(t, sessions, repos, chats, cron, wt, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomePRCreated {
		t.Fatalf("outcome = %q, want %q", res.Outcome, models.CronJobOutcomePRCreated)
	}
	if len(vp.createPRCalls) != 1 {
		t.Errorf("expected 1 createPR call, got %d", len(vp.createPRCalls))
	}
	if len(vp.createPRCalls) == 1 && vp.createPRCalls[0].Title != "Add tests for surviving mutants" {
		t.Errorf("CreateDraftPR title = %q, want %q", vp.createPRCalls[0].Title, "Add tests for surviving mutants")
	}
	// bossd injects the PR tag in-process; no second "Finalize" chat is created.
	if len(wt.injectPRNumbersCalls) != 1 {
		t.Fatalf("InjectPRNumbers calls = %d, want 1", len(wt.injectPRNumbersCalls))
	}
	if len(vp.markReadyCalls) != 1 || vp.markReadyCalls[0] != 42 {
		t.Fatalf("MarkReadyForReview calls = %v, want [42]", vp.markReadyCalls)
	}
	if len(chats.created) != 0 {
		t.Fatalf("no finalize chat should be created, got %d", len(chats.created))
	}
	// hook_token must be cleared on pr_created (step 5).
	if sessions.sessions["sess-1"].HookToken != nil {
		t.Errorf("hook_token = %v, want nil (cleared on pr_created)", *sessions.sessions["sess-1"].HookToken)
	}
	if got := sessions.sessions["sess-1"].State; got != machine.ReadyForReview {
		t.Errorf("state after pr_created = %s, want ready_for_review", got)
	}
}

// TestFinalizeSession_PRFailed covers the clean committed branch +
// EnsurePR-fails branch: repo has a GitHub origin and committed work, but the
// draft PR creation errored. Outcome must be pr_failed, the worktree is
// preserved (no archive), hook_token stays set, and the session lands in
// Blocked for an operator.
func TestFinalizeSession_PRFailed(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		statusOut:           "",
		latestCommitSubject: "fix(cron): preserve PR-backed clean sessions",
		isAncestorFn: func(_, ref, target string) (bool, error) {
			if ref == "HEAD" && target == "refs/remotes/origin/main" {
				return false, nil
			}
			return true, nil
		},
	}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	vp.createPRErr = fmt.Errorf("github 503: service unavailable")
	cron := &recordingCronJobStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@github.com:owner/repo.git",
	}
	cronJobID := "cron-1"
	hookToken := "secret-token"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		Title:        "Cron job",
		Plan:         "Do thing",
		WorktreePath: "/tmp/wt-sess1",
		BranchName:   "cron-br-1",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
		HookToken:    &hookToken,
	}

	lc := newTestLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomePRFailed {
		t.Fatalf("outcome = %q, want pr_failed", res.Outcome)
	}
	if res.Err == nil {
		t.Error("Err should carry the underlying CreateDraftPR failure for logging")
	}
	if len(wt.archived) != 0 {
		t.Errorf("worktree should be preserved on pr_failed, got archived=%v", wt.archived)
	}
	if sessions.sessions["sess-1"].HookToken == nil {
		t.Error("hook_token should be preserved on pr_failed (clear only on success)")
	}
	if got := sessions.sessions["sess-1"].State; got != machine.Blocked {
		t.Errorf("state after pr_failed = %s, want blocked", got)
	}
	if cron.lastRunCalls[0].outcome != models.CronJobOutcomePRFailed {
		t.Errorf("recorded outcome = %q, want pr_failed", cron.lastRunCalls[0].outcome)
	}
}

// TestRecoverFinalizingSessions_SetsBlockedReason asserts that daemon-restart
// recovery persists a descriptive BlockedReason on each recovered session so
// the UI explains why the session is blocked.
func TestRecoverFinalizingSessions_SetsBlockedReason(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	cron := &recordingCronJobStore{}

	cronJobID := "cron-1"
	sessions.sessions["sess-stuck"] = &models.Session{
		ID:           "sess-stuck",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-stuck",
		State:        machine.Finalizing,
		CronJobID:    &cronJobID,
	}

	lc := newTestLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)
	n, err := lc.RecoverFinalizingSessions(ctx)
	if err != nil {
		t.Fatalf("RecoverFinalizingSessions: %v", err)
	}
	if n != 1 {
		t.Fatalf("recovered count = %d, want 1", n)
	}

	sess := sessions.sessions["sess-stuck"]
	if sess.State != machine.Blocked {
		t.Fatalf("state = %v, want Blocked", sess.State)
	}
	if sess.BlockedReason == nil || *sess.BlockedReason == "" {
		t.Fatalf("BlockedReason = %v, want non-empty recovery reason", sess.BlockedReason)
	}
}

// TestRecoverFinalizingSessions_GuardsLastRunWrite asserts that daemon-restart
// recovery records the failed_recovered outcome with an ExpectedSessionID guard
// pinned to the recovered session. Without the guard, a newer run that fired
// while this session sat stranded in Finalizing (RunActive treats Finalizing as
// inactive) and already moved last_run_session_id forward would be clobbered,
// pointing the overlap check back at this now-Blocked session and letting more
// runs launch concurrently with the live newer run.
func TestRecoverFinalizingSessions_GuardsLastRunWrite(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	cron := &recordingCronJobStore{}

	cronJobID := "cron-1"
	sessions.sessions["sess-stuck"] = &models.Session{
		ID:           "sess-stuck",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-stuck",
		State:        machine.Finalizing,
		CronJobID:    &cronJobID,
	}

	lc := newTestLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)
	if _, err := lc.RecoverFinalizingSessions(ctx); err != nil {
		t.Fatalf("RecoverFinalizingSessions: %v", err)
	}

	if len(cron.lastRunCalls) != 1 {
		t.Fatalf("UpdateLastRun calls = %d, want 1", len(cron.lastRunCalls))
	}
	call := cron.lastRunCalls[0]
	if call.expectedSessionID == nil || *call.expectedSessionID != "sess-stuck" {
		t.Fatalf("ExpectedSessionID = %v, want guard pinned to \"sess-stuck\"", call.expectedSessionID)
	}
	if call.sessionID == nil || *call.sessionID != "sess-stuck" {
		t.Fatalf("SessionID = %v, want \"sess-stuck\"", call.sessionID)
	}
	if call.outcome != models.CronJobOutcomeFailedRecovered {
		t.Fatalf("outcome = %v, want failed_recovered", call.outcome)
	}
}

// TestFinalizeSession_CleanupFailed covers the no-changes branch where
// worktree archival errors out: the session row must be PRESERVED (so the
// operator can see the failed cleanup), the worktree path is unchanged, and
// the outcome must be cleanup_failed with the session transitioning to
// Blocked. This is the safety net for the otherwise-destructive
// deleted_no_changes path.
func TestFinalizeSession_CleanupFailed(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		statusOut:  "", // no changes — would normally trigger deleted_no_changes
		archiveErr: fmt.Errorf("permission denied removing worktree"),
	}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	cron := &recordingCronJobStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
	}
	cronJobID := "cron-1"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-sess1",
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
	}

	lc := newTestLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomeCleanupFailed {
		t.Fatalf("outcome = %q, want cleanup_failed", res.Outcome)
	}
	if _, ok := sessions.sessions["sess-1"]; !ok {
		t.Error("session row should be preserved on cleanup_failed (operator inspects it)")
	}
	if got := sessions.sessions["sess-1"].State; got != machine.Blocked {
		t.Errorf("state after cleanup_failed = %s, want blocked", got)
	}
	if cron.lastRunCalls[0].outcome != models.CronJobOutcomeCleanupFailed {
		t.Errorf("recorded outcome = %q, want cleanup_failed", cron.lastRunCalls[0].outcome)
	}
}

// TestFinalizeSession_Idempotency exercises the conditional-UPDATE
// idempotency gate under concurrent load: 10 goroutines fire FinalizeSession
// for the same session simultaneously, and exactly one must perform the
// side effects (worktree removal, cron outcome write). The other nine must
// no-op via the rows_affected==0 path. This guards the Stop-hook endpoint
// against duplicate-event storms (network retries, double-fire from claude
// CLI restarts, etc.).
func TestFinalizeSession_Idempotency(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{statusOut: ""} // empty → deleted_no_changes path
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	cron := &recordingCronJobStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
	}
	cronJobID := "cron-1"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-sess1",
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
	}

	lc := newTestLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)

	const n = 10
	var wg sync.WaitGroup
	results := make(chan *FinalizeResult, n)
	errs := make(chan error, n)

	startGate := make(chan struct{})
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-startGate
			res, err := lc.FinalizeSession(ctx, "sess-1")
			if err != nil {
				errs <- err
				return
			}
			results <- res
		}()
	}
	close(startGate)
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Fatalf("FinalizeSession returned an error: %v", err)
	}

	// Exactly one call must have performed the side effects; the other
	// n-1 must have observed NoOp via the rows_affected==0 gate.
	noOps, sideEffects := 0, 0
	var winner *FinalizeResult
	for res := range results {
		if res.NoOp {
			noOps++
		} else {
			sideEffects++
			winner = res
		}
	}
	if sideEffects != 1 {
		t.Fatalf("side-effect calls = %d, want 1 (rest must NoOp)", sideEffects)
	}
	if noOps != n-1 {
		t.Fatalf("noOp calls = %d, want %d", noOps, n-1)
	}
	if winner.Outcome != models.CronJobOutcomeDeletedNoChanges {
		t.Errorf("winner outcome = %q, want deleted_no_changes", winner.Outcome)
	}
	// Worktree archived exactly once, cron row written exactly once.
	if len(wt.archived) != 1 {
		t.Errorf("worktree archive calls = %d, want 1", len(wt.archived))
	}
	if len(cron.lastRunCalls) != 1 {
		t.Errorf("UpdateLastRun calls = %d, want 1", len(cron.lastRunCalls))
	}
}

// TestRecoverFinalizingSessions covers the daemon-startup recovery path:
// sessions left in Finalizing from a previous daemon crash get recorded as
// failed_recovered and transitioned to Blocked. The worktree is preserved
// (we don't archive or push), and hook_token is left intact so an operator
// can re-fire the cron job manually if needed.
func TestRecoverFinalizingSessions(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	cron := &recordingCronJobStore{}

	cronJobID := "cron-1"
	hookToken := "secret-token"
	// Stuck Finalizing session: should be recovered.
	sessions.sessions["sess-stuck"] = &models.Session{
		ID:           "sess-stuck",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-stuck",
		State:        machine.Finalizing,
		CronJobID:    &cronJobID,
		HookToken:    &hookToken,
	}
	// Untouched: a session in a non-Finalizing state must not be moved.
	sessions.sessions["sess-implementing"] = &models.Session{
		ID:    "sess-implementing",
		State: machine.ImplementingPlan,
	}

	lc := newTestLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)
	n, err := lc.RecoverFinalizingSessions(ctx)
	if err != nil {
		t.Fatalf("RecoverFinalizingSessions: %v", err)
	}
	if n != 1 {
		t.Fatalf("recovered count = %d, want 1", n)
	}

	// Stuck session: state advanced to Blocked, hook_token preserved,
	// worktree NOT archived.
	stuck := sessions.sessions["sess-stuck"]
	if stuck.State != machine.Blocked {
		t.Errorf("stuck session state = %s, want blocked", stuck.State)
	}
	if stuck.HookToken == nil || *stuck.HookToken != "secret-token" {
		t.Errorf("hook_token = %v, want preserved (recovery doesn't clear)", stuck.HookToken)
	}
	if len(wt.archived) != 0 {
		t.Errorf("worktree should be preserved on recovery, got archived=%v", wt.archived)
	}

	// Untouched session: still ImplementingPlan.
	if got := sessions.sessions["sess-implementing"].State; got != machine.ImplementingPlan {
		t.Errorf("untouched session state = %s, want implementing_plan", got)
	}

	// failed_recovered written on the cron job.
	if len(cron.lastRunCalls) != 1 {
		t.Fatalf("UpdateLastRun calls = %d, want 1", len(cron.lastRunCalls))
	}
	if cron.lastRunCalls[0].id != cronJobID {
		t.Errorf("UpdateLastRun id = %q, want %q", cron.lastRunCalls[0].id, cronJobID)
	}
	if cron.lastRunCalls[0].outcome != models.CronJobOutcomeFailedRecovered {
		t.Errorf("recorded outcome = %q, want failed_recovered", cron.lastRunCalls[0].outcome)
	}
}

// TestRecoverFinalizingSessions_Archived_Skipped pins the symmetric archived
// guard: a benign worktree_gone finalize leaves an archived/removed session in
// Finalizing (FinalizeSession steps 5 and 6 are both skipped for that benign
// outcome). ListByState returns archived rows, so without the guard this
// restart-recovery pass would record failed_recovered and transition the row to
// Blocked — resurrecting the exact scary framing BOS-384 kills. The pass must
// skip archived sessions entirely, mirroring recoverStrandedCronSessions.
func TestRecoverFinalizingSessions_Archived_Skipped(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	cron := &recordingCronJobStore{}

	cronJobID := "cron-1"
	hookToken := "secret-token"
	archivedAt := time.Now().Add(-time.Minute)
	sessions.sessions["sess-archived"] = &models.Session{
		ID:           "sess-archived",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-archived",
		State:        machine.Finalizing,
		CronJobID:    &cronJobID,
		HookToken:    &hookToken,
		ArchivedAt:   &archivedAt,
	}

	lc := newTestLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)
	n, err := lc.RecoverFinalizingSessions(ctx)
	if err != nil {
		t.Fatalf("RecoverFinalizingSessions: %v", err)
	}
	if n != 0 {
		t.Fatalf("recovered count = %d, want 0 (archived session skipped)", n)
	}
	// The archived row is left untouched: still Finalizing, no Blocked
	// transition, and no failed_recovered cron write clobbering worktree_gone.
	sess := sessions.sessions["sess-archived"]
	if sess.State != machine.Finalizing {
		t.Fatalf("archived session state = %s, want unchanged (finalizing)", sess.State)
	}
	if len(cron.lastRunCalls) != 0 {
		t.Fatalf("UpdateLastRun calls = %d, want 0 (archived session never reclassified)", len(cron.lastRunCalls))
	}
}

// TestRecoverFinalizingSessions_NoneStuck guards the no-op case: if no
// session is in Finalizing, the recovery path returns 0 with no side effects
// (no spurious cron writes, no state churn). This is the steady-state path
// on every clean daemon restart.
func TestRecoverFinalizingSessions_NoneStuck(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	cron := &recordingCronJobStore{}

	sessions.sessions["sess-1"] = &models.Session{
		ID:    "sess-1",
		State: machine.ImplementingPlan,
	}

	lc := newTestLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)
	n, err := lc.RecoverFinalizingSessions(ctx)
	if err != nil {
		t.Fatalf("RecoverFinalizingSessions: %v", err)
	}
	if n != 0 {
		t.Errorf("recovered count = %d, want 0", n)
	}
	if len(cron.lastRunCalls) != 0 {
		t.Errorf("expected no UpdateLastRun calls, got %d", len(cron.lastRunCalls))
	}
}

// TestFinalizeSession_DirtyOutput_PRNumberSetAfterEnsurePRRace covers the race
// where EnsurePR runs and creates/pushes the PR but the session row's PRNumber
// is not persisted (e.g. a network drop after the GitHub create succeeded but
// before the DB write committed). The fallback post-EnsurePR association must
// detect PRNumber==nil and call attachOpenPRForBranch to recover the number
// from the live branch PR list so the UI shows "#N" instead of "-".
func TestFinalizeSession_DirtyOutput_PRNumberSetAfterEnsurePRRace(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	inner := newMockSessionStore()
	// suppressPRNumberStore wraps the inner store and silently drops the first
	// PRNumber update, simulating the race where EnsurePR's openDraftPRForBranch
	// succeeds on GitHub but the DB write that persists the PR number is lost.
	sessions := &suppressPRNumberStore{mockSessionStore: inner}

	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		statusOut:           " M services/bossd/internal/session/finalize.go\n",
		latestCommitSubject: "fix(bossd): cron job run",
	}
	vp := newMockVCSProvider()
	// Branch has an open PR on GitHub; the fallback should find it.
	vp.nextOpenPRs = []vcs.PRSummary{
		{Number: 55, HeadBranch: "cron-br-1", State: vcs.PRStateOpen},
	}
	cron := &recordingCronJobStore{}
	chats := &recordingAgentChatStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@github.com:owner/repo.git",
	}
	cronJobID := "cron-1"
	hookToken := "secret-token"
	inner.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		Title:        "Nightly cron",
		Plan:         "Do thing",
		WorktreePath: "/tmp/wt-sess1",
		BranchName:   "cron-br-1",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
		HookToken:    &hookToken,
		AgentName:    "claude",
	}

	runner := newRecordingFinalizeAgentRunner()
	tmuxFake := newFakeTmux()
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(tmuxFake.factory))
	lc := newTestLifecycle(sessions, repos, chats, cron, wt, newMockAgentRunner(), tmuxClient, vp, logger)
	lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": runner})
	lc.SetAgentLogsDir(t.TempDir())

	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomePRFailed {
		t.Fatalf("outcome = %q, want pr_failed", res.Outcome)
	}
	sess, getErr := inner.Get(ctx, "sess-1")
	if getErr != nil {
		t.Fatalf("Get session: %v", getErr)
	}
	if sess.PRNumber == nil {
		t.Fatalf("PRNumber still nil after finalize; expected branch PR (55) to be associated")
	}
	if *sess.PRNumber != 55 {
		t.Fatalf("PRNumber = %d, want 55", *sess.PRNumber)
	}
	if len(wt.injectPRNumbersCalls) != 0 {
		t.Fatalf("InjectPRNumbers calls = %d, want 0", len(wt.injectPRNumbersCalls))
	}
	if len(vp.markReadyCalls) != 0 {
		t.Fatalf("MarkReadyForReview calls = %v, want none", vp.markReadyCalls)
	}
}

// TestFinalizeSession_CleanCommittedBranch_PRNumberSetAfterEnsurePRRace covers
// the same race for the createPRIfCleanBranchHasCommittedWork path: a clean
// branch with committed work calls EnsurePR, but the PRNumber write is lost.
// The post-EnsurePR fallback must recover the number from the branch PR list.
//
// The mock uses a deferredPRVCSProvider that starts with no open PRs (so
// attachExistingPRIfCleanBranchHasOne returns nil and the committed-work path
// runs), then exposes the new PR only after CreateDraftPR fires (simulating the
// race: GitHub accepted the PR, but the DB write was dropped).
func TestFinalizeSession_CleanCommittedBranch_PRNumberSetAfterEnsurePRRace(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	inner := newMockSessionStore()
	sessions := &suppressPRNumberStore{mockSessionStore: inner}

	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		statusOut:           "",
		latestCommitSubject: "fix(cron): do the thing",
		isAncestorFn: func(_, ref, target string) (bool, error) {
			if ref == "HEAD" && target == "refs/remotes/origin/main" {
				return false, nil // branch HAS committed work
			}
			return true, nil
		},
	}
	// deferredPRVCSProvider starts with no open PRs so that
	// attachExistingPRIfCleanBranchHasOne does not intercept. After
	// CreateDraftPR fires (simulating GitHub accepting the PR), it exposes PR
	// 77 so the post-EnsurePR fallback can recover the number.
	base := newMockVCSProvider()
	vp := &deferredPRVCSProvider{
		mockVCSProvider: base,
		deferred: []vcs.PRSummary{
			{Number: 77, HeadBranch: "cron-br-1", State: vcs.PRStateOpen},
		},
	}
	cron := &recordingCronJobStore{}
	chats := &recordingAgentChatStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@github.com:owner/repo.git",
	}
	cronJobID := "cron-1"
	hookToken := "secret-token"
	inner.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		Title:        "Nightly cron",
		Plan:         "Do thing",
		WorktreePath: "/tmp/wt-sess1",
		BranchName:   "cron-br-1",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
		HookToken:    &hookToken,
		AgentName:    "claude",
	}

	runner := newRecordingFinalizeAgentRunner()
	tmuxFake := newFakeTmux()
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(tmuxFake.factory))
	lc := newTestLifecycle(sessions, repos, chats, cron, wt, newMockAgentRunner(), tmuxClient, vp, logger)
	lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": runner})
	lc.SetAgentLogsDir(t.TempDir())

	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomePRCreated {
		t.Fatalf("outcome = %q, want pr_created", res.Outcome)
	}
	sess, getErr := inner.Get(ctx, "sess-1")
	if getErr != nil {
		t.Fatalf("Get session: %v", getErr)
	}
	if sess.PRNumber == nil {
		t.Fatalf("PRNumber still nil after finalize; expected branch PR (77) to be associated")
	}
	if *sess.PRNumber != 77 {
		t.Fatalf("PRNumber = %d, want 77", *sess.PRNumber)
	}
}

// TestFinalizeSession_CleanCommittedBranch_AttachesPRAfterPRInfoUpdateError
// covers the production-shaped failure where CreateDraftPR succeeds on GitHub
// but persisting PR metadata returns an error. Finalize must recover by finding
// the now-visible branch PR, attach it, and continue to the finalize chat
// instead of reporting pr_failed.
func TestFinalizeSession_CleanCommittedBranch_AttachesPRAfterPRInfoUpdateError(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	firstPRNumberUpdate := true
	sessions.updateHook = func(_ string, params db.UpdateSessionParams) error {
		if params.PRNumber != nil && firstPRNumberUpdate {
			firstPRNumberUpdate = false
			return fmt.Errorf("db write failed after PR create")
		}
		return nil
	}

	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		statusOut:           "",
		latestCommitSubject: "fix(cron): do the thing",
		isAncestorFn: func(_, ref, target string) (bool, error) {
			if ref == "HEAD" && target == "refs/remotes/origin/main" {
				return false, nil
			}
			return true, nil
		},
	}
	base := newMockVCSProvider()
	vp := &deferredPRVCSProvider{
		mockVCSProvider: base,
		deferred: []vcs.PRSummary{
			{Number: 88, HeadBranch: "cron-br-1", State: vcs.PRStateOpen},
		},
	}
	cron := &recordingCronJobStore{}
	chats := &recordingAgentChatStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@github.com:owner/repo.git",
	}
	cronJobID := "cron-1"
	hookToken := "secret-token"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		Title:        "Nightly cron",
		Plan:         "Do thing",
		WorktreePath: "/tmp/wt-sess1",
		BranchName:   "cron-br-1",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
		HookToken:    &hookToken,
		AgentName:    "claude",
	}

	runner := newRecordingFinalizeAgentRunner()
	tmuxFake := newFakeTmux()
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(tmuxFake.factory))
	lc := newTestLifecycle(sessions, repos, chats, cron, wt, newMockAgentRunner(), tmuxClient, vp, logger)
	lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": runner})
	lc.SetAgentLogsDir(t.TempDir())

	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomePRCreated {
		t.Fatalf("outcome = %q, want pr_created", res.Outcome)
	}
	if firstPRNumberUpdate {
		t.Fatal("test did not exercise failing first PRNumber update")
	}
	sess, getErr := sessions.Get(ctx, "sess-1")
	if getErr != nil {
		t.Fatalf("Get session: %v", getErr)
	}
	if sess.PRNumber == nil {
		t.Fatalf("PRNumber still nil after finalize; expected branch PR (88) to be associated")
	}
	if *sess.PRNumber != 88 {
		t.Fatalf("PRNumber = %d, want 88", *sess.PRNumber)
	}
	if len(base.createPRCalls) != 1 {
		t.Fatalf("CreateDraftPR calls = %d, want 1", len(base.createPRCalls))
	}
}

// deferredPRVCSProvider wraps mockVCSProvider and starts with no open PRs.
// After the first CreateDraftPR call (simulating GitHub accepting the PR while
// the DB write is about to be dropped), it exposes deferred PRs on subsequent
// ListOpenPRs calls — the post-EnsurePR fallback can then recover the number.
type deferredPRVCSProvider struct {
	*mockVCSProvider
	deferred  []vcs.PRSummary
	prCreated bool
}

func (d *deferredPRVCSProvider) CreateDraftPR(ctx context.Context, opts vcs.CreatePROpts) (*vcs.PRInfo, error) {
	d.prCreated = true
	return d.mockVCSProvider.CreateDraftPR(ctx, opts)
}

func (d *deferredPRVCSProvider) ListOpenPRs(ctx context.Context, repoPath string) ([]vcs.PRSummary, error) {
	if d.prCreated {
		return d.deferred, nil
	}
	return nil, nil // no PRs until CreateDraftPR has fired
}

// suppressPRNumberStore wraps a mockSessionStore and silently drops the FIRST
// PRNumber write, simulating a lost local write after EnsurePR creates the PR
// on GitHub.
// Subsequent PRNumber writes (e.g. from the fallback attachOpenPRForBranch)
// are allowed through so the fallback can demonstrate it recovered the number.
type suppressPRNumberStore struct {
	*mockSessionStore
	prNumberDropped bool
}

func (s *suppressPRNumberStore) Update(ctx context.Context, id string, params db.UpdateSessionParams) (*models.Session, error) {
	// Drop only the first PRNumber update (the one from EnsurePR's
	// openDraftPRForBranch), leaving the fallback write intact.
	if params.PRNumber != nil && !s.prNumberDropped {
		s.prNumberDropped = true
		params.PRNumber = nil
	}
	return s.mockSessionStore.Update(ctx, id, params)
}

// --- helpers ---

type recordingFinalizeAgentRunner struct {
	*fakeAgentForLifecycle
	lastInput ChatInput
}

func newRecordingFinalizeAgentRunner() *recordingFinalizeAgentRunner {
	return &recordingFinalizeAgentRunner{fakeAgentForLifecycle: newFakeAgent()}
}

func (r *recordingFinalizeAgentRunner) BuildInteractiveCommand(ctx context.Context, req *bossanovav1.BuildInteractiveCommandRequest) (*bossanovav1.BuildInteractiveCommandResponse, error) {
	r.lastInput = ChatInput{
		Prompt:  req.GetInitialPrompt(),
		Command: req.GetInitialCommand(),
	}
	return r.fakeAgentForLifecycle.BuildInteractiveCommand(ctx, req)
}

func newFinalizeChatLifecycle(
	t *testing.T,
	sessions *mockSessionStore,
	repos *mockRepoStore,
	chats db.AgentChatStore,
	cron db.CronJobStore,
	wt *mockWorktreeManager,
	vp *mockVCSProvider,
	logger zerolog.Logger,
) *Lifecycle {
	t.Helper()

	for _, sess := range sessions.sessions {
		if sess.AgentName == "" {
			sess.AgentName = "claude"
		}
	}

	runner := newRecordingFinalizeAgentRunner()
	tmuxFake := newFakeTmux()
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(tmuxFake.factory))
	lc := newTestLifecycle(sessions, repos, chats, cron, wt, newMockAgentRunner(), tmuxClient, vp, logger)
	lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": runner})
	lc.SetAgentLogsDir(t.TempDir())
	return lc
}

// recordingAgentChatStore captures Create calls so tests can assert that
// the finalize chat row was written with the right session/claude IDs.
type recordingAgentChatStore struct {
	mockAgentChatStore
	created []db.CreateAgentChatParams
}

func (r *recordingAgentChatStore) Create(_ context.Context, params db.CreateAgentChatParams) (*models.AgentChat, error) {
	r.created = append(r.created, params)
	return &models.AgentChat{ID: "chat-" + params.AgentSessionID, SessionID: params.SessionID, AgentSessionID: params.AgentSessionID, ProviderSessionID: params.ProviderSessionID, Title: params.Title}, nil
}

// recordingCronJobStore captures UpdateLastRun calls so tests can assert
// that FinalizeSession wrote the correct outcome.
type recordingCronJobStore struct {
	stubCronJobStore
	lastRunCalls []lastRunCall
}

type lastRunCall struct {
	id                string
	sessionID         *string
	expectedSessionID *string
	outcome           models.CronJobOutcome
}

func (r *recordingCronJobStore) UpdateLastRun(_ context.Context, id string, params db.UpdateCronJobLastRunParams) error {
	r.lastRunCalls = append(r.lastRunCalls, lastRunCall{
		id:                id,
		sessionID:         params.SessionID,
		expectedSessionID: params.ExpectedSessionID,
		outcome:           params.Outcome,
	})
	return nil
}

// stubCronJobStore is a zero-behavior CronJobStore so tests that don't care
// about outcome persistence can still construct a Lifecycle. FL4-5 replaces
// this with a mock that records every call.
type stubCronJobStore struct{}

func (s *stubCronJobStore) Create(context.Context, db.CreateCronJobParams) (*models.CronJob, error) {
	return nil, nil
}
func (s *stubCronJobStore) Get(context.Context, string) (*models.CronJob, error) { return nil, nil }
func (s *stubCronJobStore) List(context.Context) ([]*models.CronJob, error)      { return nil, nil }
func (s *stubCronJobStore) ListByRepo(context.Context, string) ([]*models.CronJob, error) {
	return nil, nil
}
func (s *stubCronJobStore) ListEnabled(context.Context) ([]*models.CronJob, error) { return nil, nil }
func (s *stubCronJobStore) Update(context.Context, string, db.UpdateCronJobParams) (*models.CronJob, error) {
	return nil, nil
}
func (s *stubCronJobStore) MarkFireStarted(context.Context, string, string, time.Time, *time.Time) error {
	return nil
}
func (s *stubCronJobStore) UpdateLastRun(context.Context, string, db.UpdateCronJobLastRunParams) error {
	return nil
}
func (s *stubCronJobStore) Delete(context.Context, string) error { return nil }

// TestStripBossdManagedFiles_UsesPluginContributions verifies that the
// parameterised helper correctly filters both hardcoded and plugin-supplied
// paths, leaving unmanaged entries intact.
func TestStripBossdManagedFiles_UsesPluginContributions(t *testing.T) {
	porcelain := strings.Join([]string{
		"?? .claude/settings.local.json",
		"?? README.md",
		"?? plugin/custom-token.json",
	}, "\n")
	got := stripBossdManagedFilesWith(porcelain, []string{
		".claude/settings.local.json",
		"plugin/custom-token.json",
	})
	want := "?? README.md"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
