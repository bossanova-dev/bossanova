package session

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/tmux"
)

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

			lc := NewLifecycle(sessions, repos, nil, &stubCronJobStore{}, wt, cr, nil, vp, logger)
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

	lc := NewLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomeDeletedNoChanges {
		t.Fatalf("outcome = %q, want %q", res.Outcome, models.CronJobOutcomeDeletedNoChanges)
	}
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

func TestFinalizeSession_CleanWorktreeExistingBranchPR_AttachesPR(t *testing.T) {
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
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
		HookToken:    &hookToken,
	}

	lc, runner := newFinalizeChatLifecycle(t, sessions, repos, chats, cron, wt, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomePRCreated {
		t.Fatalf("outcome = %q, want %q", res.Outcome, models.CronJobOutcomePRCreated)
	}
	if len(wt.archived) != 0 {
		t.Fatalf("worktree should be preserved when branch already has PR, got %v", wt.archived)
	}
	if len(vp.createPRCalls) != 0 {
		t.Fatalf("clean branch with existing PR must not create PR, calls=%d", len(vp.createPRCalls))
	}
	if got := runner.lastInput.Command; got != "boss-finalize" {
		t.Fatalf("finalize command = %q, want boss-finalize without / or $ prefix", got)
	}
	if len(chats.created) != 1 {
		t.Fatalf("expected 1 chat row created, got %d", len(chats.created))
	}
	if chats.created[0].SessionID != "sess-1" {
		t.Errorf("chat row session = %q, want sess-1", chats.created[0].SessionID)
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
	if sess.State != machine.Finalizing {
		t.Fatalf("state = %s, want finalizing", sess.State)
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

	lc := NewLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)
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

	lc, runner := newFinalizeChatLifecycle(t, sessions, repos, chats, cron, wt, vp, logger)
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
	if len(wt.fetchedBases) != 1 || wt.fetchedBases[0] != "main" {
		t.Fatalf("fetched bases = %v, want [main]", wt.fetchedBases)
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
	if got := runner.lastInput.Command; got != "boss-finalize" {
		t.Fatalf("finalize command = %q, want boss-finalize without / or $ prefix", got)
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

	lc, runner := newFinalizeChatLifecycle(t, sessions, repos, chats, cron, wt, vp, logger)
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
	if got := runner.lastInput.Command; got != "boss-finalize" {
		t.Fatalf("finalize command = %q, want boss-finalize without / or $ prefix", got)
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

	lc := NewLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)
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

func TestFinalizeSession_CleanCommittedBranchNoOriginSkipsPRWithoutFetch(t *testing.T) {
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

	lc := NewLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomePRSkippedNoGitHub {
		t.Fatalf("outcome = %q, want %q", res.Outcome, models.CronJobOutcomePRSkippedNoGitHub)
	}
	if len(wt.fetchedBases) != 0 {
		t.Fatalf("FetchBase should not run for no-origin clean branch, got %v", wt.fetchedBases)
	}
	if len(wt.archived) != 0 {
		t.Fatalf("worktree should be preserved, got archived=%v", wt.archived)
	}
	if len(vp.createPRCalls) != 0 {
		t.Fatalf("CreateDraftPR calls = %d, want 0", len(vp.createPRCalls))
	}
	if len(cron.lastRunCalls) != 1 || cron.lastRunCalls[0].outcome != models.CronJobOutcomePRSkippedNoGitHub {
		t.Fatalf("outcome recording: got %+v, want single pr_skipped_no_github entry", cron.lastRunCalls)
	}
}

func TestFinalizeSession_CleanWorktreePRLookupFailure_PreservesSession(t *testing.T) {
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
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
		HookToken:    &hookToken,
	}

	lc := NewLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomePRFailed {
		t.Fatalf("outcome = %q, want pr_failed", res.Outcome)
	}
	if res.Err == nil {
		t.Fatal("Err should carry PR lookup failure")
	}
	if len(wt.archived) != 0 {
		t.Fatalf("worktree should be preserved on PR lookup failure, got %v", wt.archived)
	}
	sess := sessions.sessions["sess-1"]
	if sess == nil {
		t.Fatal("session row should be preserved")
	}
	if sess.HookToken == nil {
		t.Fatal("hook_token should be preserved on pr_failed")
	}
	if sess.State != machine.Blocked {
		t.Fatalf("state = %s, want blocked", sess.State)
	}
	if cron.lastRunCalls[0].outcome != models.CronJobOutcomePRFailed {
		t.Fatalf("recorded outcome = %q, want pr_failed", cron.lastRunCalls[0].outcome)
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

			lc := NewLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)
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

// TestFinalizeSession_PRSkippedNoGitHub covers the non-GitHub origin branch:
// changes exist but there's no GitHub to push to, so the worktree is
// preserved and the session transitions to Blocked (attention-needed),
// mirroring the "needs manual action" semantics of a preserved worktree.
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

	lc := NewLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomePRSkippedNoGitHub {
		t.Fatalf("outcome = %q, want %q", res.Outcome, models.CronJobOutcomePRSkippedNoGitHub)
	}
	if len(wt.archived) != 0 {
		t.Errorf("worktree should be preserved on pr_skipped_no_github, got archived=%v", wt.archived)
	}
	if _, ok := sessions.sessions["sess-1"]; !ok {
		t.Error("session row should be preserved on pr_skipped_no_github")
	}
	// Step 6: failure outcomes transition Finalizing → Blocked so the
	// session surfaces as attention-needed in the UI.
	if got := sessions.sessions["sess-1"].State; got != machine.Blocked {
		t.Errorf("state after pr_skipped_no_github = %s, want blocked", got)
	}
	if len(cron.lastRunCalls) != 1 || cron.lastRunCalls[0].outcome != models.CronJobOutcomePRSkippedNoGitHub {
		t.Errorf("outcome recording: got %+v, want single pr_skipped_no_github entry", cron.lastRunCalls)
	}
}

func TestFinalizeSession_DirtyUncommittedCronOutputStartsFinalizeChat(t *testing.T) {
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

	lc, _ := newFinalizeChatLifecycle(t, sessions, repos, chats, cron, wt, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomePRCreated {
		t.Fatalf("outcome = %q, want %q", res.Outcome, models.CronJobOutcomePRCreated)
	}
	// The /boss-finalize skill has no `gh pr create` step, so dirty cron
	// output must get a PR opened before the chat starts — otherwise the chat
	// has nothing for `gh pr view` / `gh pr ready` to operate on.
	if len(vp.createPRCalls) != 1 {
		t.Fatalf("CreateDraftPR calls = %d, want 1 (PR opened before the finalize chat)", len(vp.createPRCalls))
	}
	// This branch has no commits beyond base (mock IsAncestor defaults to
	// true), so a placeholder commit must be added so GitHub accepts the PR —
	// otherwise dirty-only runs could never reach the finalizer chat.
	if wt.emptyCommitCalls != 1 {
		t.Fatalf("EmptyCommit calls = %d, want 1 (placeholder for zero-commit dirty branch)", wt.emptyCommitCalls)
	}
	if len(chats.created) != 1 {
		t.Fatalf("finalize chats created = %d, want 1", len(chats.created))
	}
}

// TestFinalizeSession_DirtyUncommittedCronOutputPRFailed covers the dirty cron
// output path when opening the PR genuinely fails (here GitHub rejects the
// create even after the placeholder commit). Because the /boss-finalize skill
// assumes a PR already exists, we must not start the chat without one: the
// outcome is pr_failed, the worktree is preserved, and no finalize chat is
// spawned. (The zero-commit case itself no longer fails — a placeholder commit
// makes the PR openable; see TestFinalizeSession_DirtyUncommittedCronOutputStartsFinalizeChat.)
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

	lc, _ := newFinalizeChatLifecycle(t, sessions, repos, chats, cron, wt, vp, logger)
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
// placeholder commit subject ("create pull request"). The /boss-finalize skill
// does not repair PR titles, so the title set at creation must be correct.
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

	lc, _ := newFinalizeChatLifecycle(t, sessions, repos, chats, cron, wt, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomePRCreated {
		t.Fatalf("outcome = %q, want %q", res.Outcome, models.CronJobOutcomePRCreated)
	}
	if len(vp.createPRCalls) != 1 {
		t.Fatalf("CreateDraftPR calls = %d, want 1", len(vp.createPRCalls))
	}
	if got := vp.createPRCalls[0].Title; got != "Nightly mutation tests" {
		t.Errorf("draft PR title = %q, want %q (session title, not the placeholder subject)", got, "Nightly mutation tests")
	}
}

// TestFinalizeSession_PRCreated covers the happy path: a clean branch contains
// committed work on a GitHub-linked repo, EnsurePR opens the PR, and
// StartFinalizeChat spawns the /boss-finalize Claude conversation. Outcome must
// be pr_created, the session stays in Finalizing (the chat drives it onward to
// PRMerged/PRClosed), and hook_token is cleared so a replayed Stop event can no
// longer authenticate.
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

	lc, runner := newFinalizeChatLifecycle(t, sessions, repos, chats, cron, wt, vp, logger)
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
	if got := runner.lastInput.Command; got != "boss-finalize" {
		t.Fatalf("finalize command = %q, want boss-finalize without / or $ prefix", got)
	}
	// A claude_chats row must be created so the UI lists the finalize chat
	// alongside the prior implementing chat.
	if len(chats.created) != 1 {
		t.Fatalf("expected 1 chat row created, got %d", len(chats.created))
	}
	if chats.created[0].SessionID != "sess-1" {
		t.Errorf("chat row session = %q, want sess-1", chats.created[0].SessionID)
	}
	// hook_token must be cleared on pr_created (step 5).
	if sessions.sessions["sess-1"].HookToken != nil {
		t.Errorf("hook_token = %v, want nil (cleared on pr_created)", *sessions.sessions["sess-1"].HookToken)
	}
	// State stays at Finalizing — the finalize chat drives the session forward.
	if got := sessions.sessions["sess-1"].State; got != machine.Finalizing {
		t.Errorf("state after pr_created = %s, want finalizing", got)
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

	lc := NewLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)
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

// TestFinalizeSession_ChatSpawnFailed covers the clean committed branch where
// EnsurePR succeeds but chat spawn fails: the PR is opened (so we can't redo it
// cleanly), but the /boss-finalize claude process won't start. Outcome must be
// chat_spawn_failed with the worktree + hook_token preserved and the session in
// Blocked.
func TestFinalizeSession_ChatSpawnFailed(t *testing.T) {
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
	cr.startErr = fmt.Errorf("claude binary not found")
	vp := newMockVCSProvider()
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

	lc := NewLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	if res.Outcome != models.CronJobOutcomeChatSpawnFailed {
		t.Fatalf("outcome = %q, want chat_spawn_failed", res.Outcome)
	}
	if len(vp.createPRCalls) != 1 {
		t.Errorf("EnsurePR should have been reached and PR created (calls=%d)", len(vp.createPRCalls))
	}
	if sessions.sessions["sess-1"].HookToken == nil {
		t.Error("hook_token should be preserved on chat_spawn_failed (clear only on success)")
	}
	if got := sessions.sessions["sess-1"].State; got != machine.Blocked {
		t.Errorf("state after chat_spawn_failed = %s, want blocked", got)
	}
	if cron.lastRunCalls[0].outcome != models.CronJobOutcomeChatSpawnFailed {
		t.Errorf("recorded outcome = %q, want chat_spawn_failed", cron.lastRunCalls[0].outcome)
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

	lc := NewLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)
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

	lc := NewLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)

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

	lc := NewLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)
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

	lc := NewLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)
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
) (*Lifecycle, *recordingFinalizeAgentRunner) {
	t.Helper()

	for _, sess := range sessions.sessions {
		if sess.AgentName == "" {
			sess.AgentName = "claude"
		}
	}

	runner := newRecordingFinalizeAgentRunner()
	tmuxFake := newFakeTmux()
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(tmuxFake.factory))
	lc := NewLifecycle(sessions, repos, chats, cron, wt, newMockAgentRunner(), tmuxClient, vp, logger)
	lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": runner})
	lc.SetAgentLogsDir(t.TempDir())
	return lc, runner
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
	id        string
	sessionID *string
	outcome   models.CronJobOutcome
}

func (r *recordingCronJobStore) UpdateLastRun(_ context.Context, id string, params db.UpdateCronJobLastRunParams) error {
	r.lastRunCalls = append(r.lastRunCalls, lastRunCall{id: id, sessionID: params.SessionID, outcome: params.Outcome})
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
