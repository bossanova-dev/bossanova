package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sessionreason"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
)

// --- Mock AttemptStore ---

type mockAttemptStore struct {
	attempts map[string]*models.Attempt
	nextID   int
}

func newMockAttemptStore() *mockAttemptStore {
	return &mockAttemptStore{
		attempts: make(map[string]*models.Attempt),
	}
}

func (m *mockAttemptStore) Create(_ context.Context, params db.CreateAttemptParams) (*models.Attempt, error) {
	m.nextID++
	id := fmt.Sprintf("attempt-%d", m.nextID)
	a := &models.Attempt{
		ID:        id,
		SessionID: params.SessionID,
		Trigger:   models.AttemptTrigger(params.Trigger),
		Result:    models.AttemptResultUnspecified,
	}
	m.attempts[id] = a
	return a, nil
}

func (m *mockAttemptStore) Get(_ context.Context, id string) (*models.Attempt, error) {
	a, ok := m.attempts[id]
	if !ok {
		return nil, fmt.Errorf("attempt %s not found", id)
	}
	return a, nil
}

func (m *mockAttemptStore) ListBySession(_ context.Context, sessionID string) ([]*models.Attempt, error) {
	var result []*models.Attempt
	for _, a := range m.attempts {
		if a.SessionID == sessionID {
			result = append(result, a)
		}
	}
	return result, nil
}

func (m *mockAttemptStore) Update(_ context.Context, id string, params db.UpdateAttemptParams) (*models.Attempt, error) {
	a, ok := m.attempts[id]
	if !ok {
		return nil, fmt.Errorf("attempt %s not found", id)
	}
	if params.Result != nil {
		a.Result = models.AttemptResult(*params.Result)
	}
	if params.Error != nil {
		a.Error = *params.Error
	}
	return a, nil
}

func (m *mockAttemptStore) Delete(_ context.Context, id string) error {
	delete(m.attempts, id)
	return nil
}

// --- Fix Loop Tests ---

func TestFixLoopHandleCheckFailure(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	attempts := newMockAttemptStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	cr := newMockAgentRunner()
	wt := &mockWorktreeManager{}
	logger := zerolog.Nop()

	prNum := 42
	agentSessionID := "claude-old"
	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:             "sess-1",
		RepoID:         "repo-1",
		State:          machine.FixingChecks,
		AttemptCount:   1,
		PRNumber:       &prNum,
		WorktreePath:   "/tmp/worktrees/test-repo/test",
		BranchName:     "test",
		BaseBranch:     "main",
		AgentSessionID: &agentSessionID,
	}

	fl := NewFixLoop(sessions, attempts, repos, vp, cr, wt, logger)

	failure := vcs.CheckConclusionFailure
	err := fl.HandleCheckFailure(ctx, "sess-1", []vcs.CheckResult{
		{ID: "ci/lint", Name: "lint", Status: vcs.CheckStatusCompleted, Conclusion: &failure},
	})
	if err != nil {
		t.Fatalf("HandleCheckFailure: %v", err)
	}

	// Verify Claude was started with resume.
	if len(cr.started) != 1 {
		t.Fatalf("expected 1 claude start, got %d", len(cr.started))
	}
	if cr.started[0].resume == nil || *cr.started[0].resume != "claude-old" {
		t.Errorf("expected claude resume with 'claude-old', got %v", cr.started[0].resume)
	}

	// Verify branch was pushed.
	if len(wt.pushed) != 1 || wt.pushed[0] != "test" {
		t.Errorf("expected push of test, got %v", wt.pushed)
	}

	// Verify session transitioned to AwaitingChecks (FixComplete).
	sess := sessions.sessions["sess-1"]
	if sess.State != machine.AwaitingChecks {
		t.Errorf("state = %v, want AwaitingChecks", sess.State)
	}

	// Verify attempt was created and marked success.
	if len(attempts.attempts) != 1 {
		t.Fatalf("expected 1 attempt, got %d", len(attempts.attempts))
	}
	for _, a := range attempts.attempts {
		if a.Trigger != models.AttemptTriggerCheckFailure {
			t.Errorf("trigger = %v, want CheckFailure", a.Trigger)
		}
		if a.Result != models.AttemptResultSuccess {
			t.Errorf("result = %v, want Success", a.Result)
		}
	}
}

func TestFixLoopHandleConflict(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	attempts := newMockAttemptStore()
	repos := newMockRepoStore()
	vp := newSequencePRStatusProvider(
		&vcs.PRStatus{
			State:            vcs.PRStateOpen,
			HeadSHA:          "start-head-sha",
			Mergeable:        boolPtr(false),
			MergeStateStatus: vcs.MergeStateStatusDirty,
		},
		&vcs.PRStatus{
			State:            vcs.PRStateOpen,
			HeadSHA:          "pushed-head-sha",
			Mergeable:        boolPtr(true),
			MergeStateStatus: vcs.MergeStateStatusClean,
		},
	)
	cr := newMockAgentRunner()
	wt := &mockWorktreeManager{}
	logger := zerolog.Nop()

	prNum := 9701
	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		State:        machine.FixingChecks,
		PRNumber:     &prNum,
		WorktreePath: "/tmp/worktrees/test-repo/test",
		BranchName:   "test",
		BaseBranch:   "main",
	}

	fl := NewFixLoop(sessions, attempts, repos, vp, cr, wt, logger)

	err := fl.HandleConflict(ctx, "sess-1")
	if err != nil {
		t.Fatalf("HandleConflict: %v", err)
	}

	// Verify Claude was started.
	if len(cr.started) != 1 {
		t.Fatalf("expected 1 claude start, got %d", len(cr.started))
	}

	// Verify session transitioned to AwaitingChecks.
	if sessions.sessions["sess-1"].State != machine.AwaitingChecks {
		t.Errorf("state = %v, want AwaitingChecks", sessions.sessions["sess-1"].State)
	}

	// Verify attempt trigger is Conflict.
	for _, a := range attempts.attempts {
		if a.Trigger != models.AttemptTriggerConflict {
			t.Errorf("trigger = %v, want Conflict", a.Trigger)
		}
	}
}

func TestFixLoopConflictRepairUsesLeaseAndVerifiesRemote(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	attempts := newMockAttemptStore()
	repos := newMockRepoStore()
	vp := newSequencePRStatusProvider(
		&vcs.PRStatus{
			State:            vcs.PRStateOpen,
			HeadSHA:          "start-head-sha",
			Mergeable:        boolPtr(false),
			MergeStateStatus: vcs.MergeStateStatusDirty,
		},
		&vcs.PRStatus{
			State:            vcs.PRStateOpen,
			HeadSHA:          "pushed-head-sha",
			Mergeable:        boolPtr(true),
			MergeStateStatus: vcs.MergeStateStatusClean,
		},
	)
	cr := newMockAgentRunner()
	wt := &conflictRepairWorktreeManager{
		mockWorktreeManager: &mockWorktreeManager{},
		pushWithLeaseHead:   "pushed-head-sha",
	}
	logger := zerolog.Nop()

	prNumber := 9701
	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "freshclaim/fresh-claim",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		State:        machine.FixingChecks,
		PRNumber:     &prNumber,
		WorktreePath: "/tmp/worktree",
		BranchName:   "fix-camera-focus-issues",
		BaseBranch:   "main",
	}

	fl := NewFixLoop(sessions, attempts, repos, vp, cr, wt, logger)

	if err := fl.HandleConflict(ctx, "sess-1"); err != nil {
		t.Fatalf("HandleConflict: %v", err)
	}

	if got := wt.pushWithLeaseBranches; len(got) != 1 || got[0] != "fix-camera-focus-issues" {
		t.Fatalf("PushWithLease branches = %v, want [fix-camera-focus-issues]", got)
	}
	if wt.pushWithLeaseExpectedRemoteSHA != "start-head-sha" {
		t.Fatalf("PushWithLease expected remote SHA = %q, want start-head-sha", wt.pushWithLeaseExpectedRemoteSHA)
	}
	if len(wt.pushed) != 0 {
		t.Fatalf("plain Push branches = %v, want none", wt.pushed)
	}
	// 1 status read before repair + 1 to observe the clean pushed head + 1
	// confirmation re-read that the head has not since moved.
	if got := vp.getPRStatusPRNumbers; len(got) != 3 {
		t.Fatalf("GetPRStatus PR numbers = %v, want 3 calls", got)
	}
	for i, n := range vp.getPRStatusPRNumbers {
		if n != prNumber {
			t.Fatalf("GetPRStatus call %d PR number = %d, want %d", i, n, prNumber)
		}
	}
	if len(cr.started) != 1 {
		t.Fatalf("expected 1 claude start, got %d", len(cr.started))
	}
	prompt := cr.started[0].plan
	for _, want := range []string{
		"Required repair flow:",
		"origin/main",
		"Do not run plain force push",
		"Bossanova will push with --force-with-lease",
		"Report the final local HEAD SHA",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if got := latestAttemptResult(t, attempts); got != models.AttemptResultSuccess {
		t.Fatalf("attempt result = %v, want success", got)
	}
}

func TestFixLoopConflictRepairVerificationUsesRepoMergeStrategy(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	attempts := newMockAttemptStore()
	repos := newMockRepoStore()
	vp := newSequencePRStatusProvider(
		&vcs.PRStatus{
			State:            vcs.PRStateOpen,
			HeadSHA:          "start-head-sha",
			Mergeable:        boolPtr(false),
			MergeStateStatus: vcs.MergeStateStatusDirty,
		},
		&vcs.PRStatus{
			State:            vcs.PRStateOpen,
			HeadSHA:          "pushed-head-sha",
			Mergeable:        boolPtr(true),
			Rebaseable:       boolPtr(false),
			MergeStateStatus: vcs.MergeStateStatusClean,
		},
	)
	vp.allowedStrategies = []string{"merge"}
	cr := newMockAgentRunner()
	wt := &conflictRepairWorktreeManager{
		mockWorktreeManager: &mockWorktreeManager{},
		pushWithLeaseHead:   "pushed-head-sha",
	}
	logger := zerolog.Nop()

	prNumber := 9701
	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "freshclaim/fresh-claim",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		State:        machine.FixingChecks,
		PRNumber:     &prNumber,
		WorktreePath: "/tmp/worktree",
		BranchName:   "fix-camera-focus-issues",
		BaseBranch:   "main",
	}

	fl := NewFixLoop(sessions, attempts, repos, vp, cr, wt, logger)

	if err := fl.HandleConflict(ctx, "sess-1"); err != nil {
		t.Fatalf("HandleConflict: %v", err)
	}
	if got := latestAttemptResult(t, attempts); got != models.AttemptResultSuccess {
		t.Fatalf("attempt result = %v, want success", got)
	}
}

func TestFixLoopConflictRepairVerificationRetriesTransientRemoteStatus(t *testing.T) {
	withoutConflictRepairVerificationDelay(t)

	ctx := context.Background()
	sessions := newMockSessionStore()
	attempts := newMockAttemptStore()
	repos := newMockRepoStore()
	vp := newSequencePRStatusProvider(
		&vcs.PRStatus{
			State:            vcs.PRStateOpen,
			HeadSHA:          "start-head-sha",
			Mergeable:        boolPtr(false),
			MergeStateStatus: vcs.MergeStateStatusDirty,
		},
		&vcs.PRStatus{
			State:            vcs.PRStateOpen,
			HeadSHA:          "newer-head-sha",
			Mergeable:        boolPtr(true),
			MergeStateStatus: vcs.MergeStateStatusClean,
		},
		&vcs.PRStatus{
			State:            vcs.PRStateOpen,
			HeadSHA:          "pushed-head-sha",
			MergeStateStatus: vcs.MergeStateStatusUnknown,
		},
		&vcs.PRStatus{
			State:            vcs.PRStateOpen,
			HeadSHA:          "pushed-head-sha",
			Mergeable:        boolPtr(true),
			MergeStateStatus: vcs.MergeStateStatusClean,
		},
	)
	cr := newMockAgentRunner()
	wt := &conflictRepairWorktreeManager{
		mockWorktreeManager: &mockWorktreeManager{},
		pushWithLeaseHead:   "pushed-head-sha",
	}
	logger := zerolog.Nop()

	prNumber := 9701
	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "freshclaim/fresh-claim"}
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		State:        machine.FixingChecks,
		PRNumber:     &prNumber,
		WorktreePath: "/tmp/worktree",
		BranchName:   "fix-camera-focus-issues",
		BaseBranch:   "main",
	}

	fl := NewFixLoop(sessions, attempts, repos, vp, cr, wt, logger)

	if err := fl.HandleConflict(ctx, "sess-1"); err != nil {
		t.Fatalf("HandleConflict: %v", err)
	}
	// 4 polls to converge on the clean pushed head (skipping the newer and
	// unknown transients) + 1 confirmation re-read.
	if got := len(vp.getPRStatusPRNumbers); got != 5 {
		t.Fatalf("GetPRStatus calls = %d, want 5", got)
	}
	if got := latestAttemptResult(t, attempts); got != models.AttemptResultSuccess {
		t.Fatalf("attempt result = %v, want success", got)
	}
}

func TestFixLoopConflictRepairFailsOnStaleLease(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	attempts := newMockAttemptStore()
	repos := newMockRepoStore()
	vp := newSequencePRStatusProvider(&vcs.PRStatus{
		State:            vcs.PRStateOpen,
		HeadSHA:          "start-head-sha",
		Mergeable:        boolPtr(false),
		MergeStateStatus: vcs.MergeStateStatusDirty,
	})
	cr := newMockAgentRunner()
	wt := &conflictRepairWorktreeManager{
		mockWorktreeManager: &mockWorktreeManager{},
		pushWithLeaseErr:    errors.New("stale lease"),
	}
	logger := zerolog.Nop()

	prNumber := 9701
	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "freshclaim/fresh-claim"}
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		State:        machine.FixingChecks,
		PRNumber:     &prNumber,
		WorktreePath: "/tmp/worktree",
		BranchName:   "fix-camera-focus-issues",
		BaseBranch:   "main",
	}

	fl := NewFixLoop(sessions, attempts, repos, vp, cr, wt, logger)

	err := fl.HandleConflict(ctx, "sess-1")
	if err == nil {
		t.Fatal("HandleConflict error = nil, want stale lease error")
	}
	assertFailedFixAttempt(t, attempts, sessions.sessions["sess-1"], "push branch: stale lease")
}

func TestFixLoopConflictRepairFailsWhenRemoteHeadDiffers(t *testing.T) {
	withoutConflictRepairVerificationDelay(t)

	ctx := context.Background()
	sessions := newMockSessionStore()
	attempts := newMockAttemptStore()
	repos := newMockRepoStore()
	vp := newSequencePRStatusProvider(
		&vcs.PRStatus{
			State:            vcs.PRStateOpen,
			HeadSHA:          "start-head-sha",
			Mergeable:        boolPtr(false),
			MergeStateStatus: vcs.MergeStateStatusDirty,
		},
		&vcs.PRStatus{
			State:            vcs.PRStateOpen,
			HeadSHA:          "newer-head-sha",
			Mergeable:        boolPtr(true),
			MergeStateStatus: vcs.MergeStateStatusClean,
		},
	)
	cr := newMockAgentRunner()
	wt := &conflictRepairWorktreeManager{
		mockWorktreeManager: &mockWorktreeManager{},
		pushWithLeaseHead:   "pushed-head-sha",
	}
	logger := zerolog.Nop()

	prNumber := 9701
	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "freshclaim/fresh-claim"}
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		State:        machine.FixingChecks,
		PRNumber:     &prNumber,
		WorktreePath: "/tmp/worktree",
		BranchName:   "fix-camera-focus-issues",
		BaseBranch:   "main",
	}

	fl := NewFixLoop(sessions, attempts, repos, vp, cr, wt, logger)

	err := fl.HandleConflict(ctx, "sess-1")
	if err == nil {
		t.Fatal("HandleConflict error = nil, want verification error")
	}
	assertFailedFixAttempt(t, attempts, sessions.sessions["sess-1"], "conflict repair remote head = newer-head-sha, want pushed head pushed-head-sha")
}

func TestFixLoopConflictRepairFailsWhenStillRepairable(t *testing.T) {
	withoutConflictRepairVerificationDelay(t)

	ctx := context.Background()
	sessions := newMockSessionStore()
	attempts := newMockAttemptStore()
	repos := newMockRepoStore()
	vp := newSequencePRStatusProvider(
		&vcs.PRStatus{
			State:            vcs.PRStateOpen,
			HeadSHA:          "start-head-sha",
			Mergeable:        boolPtr(false),
			MergeStateStatus: vcs.MergeStateStatusDirty,
		},
		&vcs.PRStatus{
			State:            vcs.PRStateOpen,
			HeadSHA:          "pushed-head-sha",
			Mergeable:        boolPtr(false),
			MergeStateStatus: vcs.MergeStateStatusDirty,
		},
	)
	cr := newMockAgentRunner()
	wt := &conflictRepairWorktreeManager{
		mockWorktreeManager: &mockWorktreeManager{},
		pushWithLeaseHead:   "pushed-head-sha",
	}
	logger := zerolog.Nop()

	prNumber := 9701
	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "freshclaim/fresh-claim"}
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		State:        machine.FixingChecks,
		PRNumber:     &prNumber,
		WorktreePath: "/tmp/worktree",
		BranchName:   "fix-camera-focus-issues",
		BaseBranch:   "main",
	}

	fl := NewFixLoop(sessions, attempts, repos, vp, cr, wt, logger)

	err := fl.HandleConflict(ctx, "sess-1")
	if err == nil {
		t.Fatal("HandleConflict error = nil, want repairable conflict error")
	}
	assertFailedFixAttempt(t, attempts, sessions.sessions["sess-1"], "conflict repair still blocked by merge")
}

func TestFixLoopConflictRepairFailsWhenSupersededAfterClean(t *testing.T) {
	withoutConflictRepairVerificationDelay(t)

	ctx := context.Background()
	sessions := newMockSessionStore()
	attempts := newMockAttemptStore()
	repos := newMockRepoStore()
	// The pushed head verifies clean, but a newer push lands before the
	// confirmation re-read — the repair must not clear conflict state against
	// a SHA that is no longer the branch head.
	vp := newSequencePRStatusProvider(
		&vcs.PRStatus{
			State:            vcs.PRStateOpen,
			HeadSHA:          "start-head-sha",
			Mergeable:        boolPtr(false),
			MergeStateStatus: vcs.MergeStateStatusDirty,
		},
		&vcs.PRStatus{
			State:            vcs.PRStateOpen,
			HeadSHA:          "pushed-head-sha",
			Mergeable:        boolPtr(true),
			MergeStateStatus: vcs.MergeStateStatusClean,
		},
		&vcs.PRStatus{
			State:            vcs.PRStateOpen,
			HeadSHA:          "newer-head-sha",
			Mergeable:        boolPtr(true),
			MergeStateStatus: vcs.MergeStateStatusClean,
		},
	)
	cr := newMockAgentRunner()
	wt := &conflictRepairWorktreeManager{
		mockWorktreeManager: &mockWorktreeManager{},
		pushWithLeaseHead:   "pushed-head-sha",
	}
	logger := zerolog.Nop()

	prNumber := 9701
	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "freshclaim/fresh-claim"}
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		State:        machine.FixingChecks,
		PRNumber:     &prNumber,
		WorktreePath: "/tmp/worktree",
		BranchName:   "fix-camera-focus-issues",
		BaseBranch:   "main",
	}

	fl := NewFixLoop(sessions, attempts, repos, vp, cr, wt, logger)

	err := fl.HandleConflict(ctx, "sess-1")
	if err == nil {
		t.Fatal("HandleConflict error = nil, want superseded error")
	}
	assertFailedFixAttempt(t, attempts, sessions.sessions["sess-1"], "conflict repair superseded: remote head = newer-head-sha, want pushed head pushed-head-sha")
}

func TestFixLoopConflictRepairFailsWhenConfirmationRecomputesConflict(t *testing.T) {
	withoutConflictRepairVerificationDelay(t)

	ctx := context.Background()
	sessions := newMockSessionStore()
	attempts := newMockAttemptStore()
	repos := newMockRepoStore()
	// The pushed head verifies clean, but GitHub can recompute mergeability
	// before the confirmation re-read. The repair must not clear conflict
	// state when the same head is dirty again.
	vp := newSequencePRStatusProvider(
		&vcs.PRStatus{
			State:            vcs.PRStateOpen,
			HeadSHA:          "start-head-sha",
			Mergeable:        boolPtr(false),
			MergeStateStatus: vcs.MergeStateStatusDirty,
		},
		&vcs.PRStatus{
			State:            vcs.PRStateOpen,
			HeadSHA:          "pushed-head-sha",
			Mergeable:        boolPtr(true),
			MergeStateStatus: vcs.MergeStateStatusClean,
		},
		&vcs.PRStatus{
			State:            vcs.PRStateOpen,
			HeadSHA:          "pushed-head-sha",
			Mergeable:        boolPtr(false),
			MergeStateStatus: vcs.MergeStateStatusDirty,
		},
	)
	cr := newMockAgentRunner()
	wt := &conflictRepairWorktreeManager{
		mockWorktreeManager: &mockWorktreeManager{},
		pushWithLeaseHead:   "pushed-head-sha",
	}
	logger := zerolog.Nop()

	prNumber := 9701
	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "freshclaim/fresh-claim"}
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		State:        machine.FixingChecks,
		PRNumber:     &prNumber,
		WorktreePath: "/tmp/worktree",
		BranchName:   "fix-camera-focus-issues",
		BaseBranch:   "main",
	}

	fl := NewFixLoop(sessions, attempts, repos, vp, cr, wt, logger)

	err := fl.HandleConflict(ctx, "sess-1")
	if err == nil {
		t.Fatal("HandleConflict error = nil, want confirmation conflict error")
	}
	assertFailedFixAttempt(t, attempts, sessions.sessions["sess-1"], "conflict repair still blocked by merge")
}

func TestFixLoopConflictRepairFailsWhenConfirmationReturnsUnknown(t *testing.T) {
	withoutConflictRepairVerificationDelay(t)

	ctx := context.Background()
	sessions := newMockSessionStore()
	attempts := newMockAttemptStore()
	repos := newMockRepoStore()
	vp := newSequencePRStatusProvider(
		&vcs.PRStatus{
			State:            vcs.PRStateOpen,
			HeadSHA:          "start-head-sha",
			Mergeable:        boolPtr(false),
			MergeStateStatus: vcs.MergeStateStatusDirty,
		},
		&vcs.PRStatus{
			State:            vcs.PRStateOpen,
			HeadSHA:          "pushed-head-sha",
			Mergeable:        boolPtr(true),
			MergeStateStatus: vcs.MergeStateStatusClean,
		},
		&vcs.PRStatus{
			State:            vcs.PRStateOpen,
			HeadSHA:          "pushed-head-sha",
			MergeStateStatus: vcs.MergeStateStatusUnknown,
		},
	)
	cr := newMockAgentRunner()
	wt := &conflictRepairWorktreeManager{
		mockWorktreeManager: &mockWorktreeManager{},
		pushWithLeaseHead:   "pushed-head-sha",
	}
	logger := zerolog.Nop()

	prNumber := 9701
	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "freshclaim/fresh-claim"}
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		State:        machine.FixingChecks,
		PRNumber:     &prNumber,
		WorktreePath: "/tmp/worktree",
		BranchName:   "fix-camera-focus-issues",
		BaseBranch:   "main",
	}

	fl := NewFixLoop(sessions, attempts, repos, vp, cr, wt, logger)

	err := fl.HandleConflict(ctx, "sess-1")
	if err == nil {
		t.Fatal("HandleConflict error = nil, want confirmation unknown error")
	}
	assertFailedFixAttempt(t, attempts, sessions.sessions["sess-1"], "conflict repair still blocked by unknown")
}

type conflictRepairWorktreeManager struct {
	*mockWorktreeManager

	pushWithLeaseBranches          []string
	pushWithLeaseExpectedRemoteSHA string
	pushWithLeaseHead              string
	pushWithLeaseErr               error
}

func (m *conflictRepairWorktreeManager) PushWithLease(_ context.Context, _ string, branch, expectedRemoteSHA string) (string, error) {
	m.pushWithLeaseBranches = append(m.pushWithLeaseBranches, branch)
	m.pushWithLeaseExpectedRemoteSHA = expectedRemoteSHA
	if m.pushWithLeaseErr != nil {
		return "", m.pushWithLeaseErr
	}
	if m.pushWithLeaseHead != "" {
		return m.pushWithLeaseHead, nil
	}
	return "pushed-head-sha", nil
}

func (m *conflictRepairWorktreeManager) InjectPRNumbers(_ context.Context, _, _ string, _ int, _ string) error {
	return nil
}

type sequencePRStatusProvider struct {
	*mockVCSProvider

	statuses []*vcs.PRStatus
}

func newSequencePRStatusProvider(statuses ...*vcs.PRStatus) *sequencePRStatusProvider {
	return &sequencePRStatusProvider{
		mockVCSProvider: newMockVCSProvider(),
		statuses:        statuses,
	}
}

func (m *sequencePRStatusProvider) GetPRStatus(_ context.Context, _ string, prNumber int) (*vcs.PRStatus, error) {
	m.getPRStatusPRNumbers = append(m.getPRStatusPRNumbers, prNumber)
	if len(m.statuses) == 0 {
		return &vcs.PRStatus{State: vcs.PRStateOpen}, nil
	}
	if len(m.getPRStatusPRNumbers) > len(m.statuses) {
		return m.statuses[len(m.statuses)-1], nil
	}
	return m.statuses[len(m.getPRStatusPRNumbers)-1], nil
}

func latestAttemptResult(t *testing.T, attempts *mockAttemptStore) models.AttemptResult {
	t.Helper()
	return latestAttempt(t, attempts).Result
}

func latestAttempt(t *testing.T, attempts *mockAttemptStore) *models.Attempt {
	t.Helper()
	if len(attempts.attempts) != 1 {
		t.Fatalf("expected 1 attempt, got %d", len(attempts.attempts))
	}
	for _, attempt := range attempts.attempts {
		return attempt
	}
	t.Fatal("attempt store unexpectedly empty")
	return nil
}

func assertFailedFixAttempt(t *testing.T, attempts *mockAttemptStore, sess *models.Session, wantErrorText string) {
	t.Helper()

	attempt := latestAttempt(t, attempts)
	if attempt.Result != models.AttemptResultFailed {
		t.Fatalf("attempt result = %v, want failed", attempt.Result)
	}
	if attempt.Error == nil {
		t.Fatalf("attempt error = nil, want to contain %q", wantErrorText)
	}
	if !strings.Contains(*attempt.Error, wantErrorText) {
		t.Fatalf("attempt error = %q, want to contain %q", *attempt.Error, wantErrorText)
	}
	if sess.State != machine.AwaitingChecks {
		t.Fatalf("session state = %v, want AwaitingChecks", sess.State)
	}
	if sess.AttemptCount != 0 {
		t.Fatalf("attempt count = %d, want 0", sess.AttemptCount)
	}
}

func withoutConflictRepairVerificationDelay(t *testing.T) {
	t.Helper()
	old := conflictRepairVerificationDelay
	conflictRepairVerificationDelay = 0
	t.Cleanup(func() {
		conflictRepairVerificationDelay = old
	})
}

func TestFixLoopHandleReviewFeedback(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	attempts := newMockAttemptStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	cr := newMockAgentRunner()
	wt := &mockWorktreeManager{}
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		State:        machine.FixingChecks,
		WorktreePath: "/tmp/worktrees/test-repo/test",
		BranchName:   "test",
		BaseBranch:   "main",
	}

	fl := NewFixLoop(sessions, attempts, repos, vp, cr, wt, logger)

	comments := []vcs.ReviewComment{
		{Author: "reviewer", Body: "Please fix the formatting", State: vcs.ReviewStateChangesRequested},
	}
	err := fl.HandleReviewFeedback(ctx, "sess-1", comments)
	if err != nil {
		t.Fatalf("HandleReviewFeedback: %v", err)
	}

	// Verify Claude was started.
	if len(cr.started) != 1 {
		t.Fatalf("expected 1 claude start, got %d", len(cr.started))
	}

	// Verify session transitioned to AwaitingChecks.
	if sessions.sessions["sess-1"].State != machine.AwaitingChecks {
		t.Errorf("state = %v, want AwaitingChecks", sessions.sessions["sess-1"].State)
	}

	// Verify attempt trigger is ReviewFeedback.
	for _, a := range attempts.attempts {
		if a.Trigger != models.AttemptTriggerReviewFeedback {
			t.Errorf("trigger = %v, want ReviewFeedback", a.Trigger)
		}
	}
}

func TestFixLoopWrongState(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	attempts := newMockAttemptStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	cr := newMockAgentRunner()
	wt := &mockWorktreeManager{}
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:     "sess-1",
		RepoID: "repo-1",
		State:  machine.ImplementingPlan, // Wrong state for fix loop.
	}

	fl := NewFixLoop(sessions, attempts, repos, vp, cr, wt, logger)

	err := fl.HandleCheckFailure(ctx, "sess-1", nil)
	if err == nil {
		t.Fatal("expected error for wrong state")
	}
}

func TestFixLoopPerSessionMutex(t *testing.T) {
	// Verify that the per-session mutex is correctly scoped — different
	// sessions get different mutexes.
	sessions := newMockSessionStore()
	attempts := newMockAttemptStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	cr := newMockAgentRunner()
	wt := &mockWorktreeManager{}
	logger := zerolog.Nop()

	fl := NewFixLoop(sessions, attempts, repos, vp, cr, wt, logger)

	mu1 := fl.sessionMutex("sess-1")
	mu2 := fl.sessionMutex("sess-2")
	mu1Again := fl.sessionMutex("sess-1")

	if mu1 == mu2 {
		t.Error("different sessions should get different mutexes")
	}
	if mu1 != mu1Again {
		t.Error("same session should get same mutex")
	}
}

// --- Integration: Dispatcher + FixLoop end-to-end ---

func TestIntegrationChecksFailedFixLoop(t *testing.T) {
	// Full cycle: AwaitingChecks → ChecksFailed → FixingChecks → (fix loop) → AwaitingChecks.
	ctx := context.Background()
	sessions := newMockSessionStore()
	attempts := newMockAttemptStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	cr := newMockAgentRunner()
	wt := &mockWorktreeManager{}
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		State:        machine.FixingChecks,
		AttemptCount: 1,
		WorktreePath: "/tmp/worktrees/test-repo/test",
		BranchName:   "test",
		BaseBranch:   "main",
	}

	fl := NewFixLoop(sessions, attempts, repos, vp, cr, wt, logger)

	failure := vcs.CheckConclusionFailure
	err := fl.HandleCheckFailure(ctx, "sess-1", []vcs.CheckResult{
		{ID: "ci/test", Name: "test", Status: vcs.CheckStatusCompleted, Conclusion: &failure},
	})
	if err != nil {
		t.Fatalf("HandleCheckFailure: %v", err)
	}

	// Session should be back in AwaitingChecks.
	sess := sessions.sessions["sess-1"]
	if sess.State != machine.AwaitingChecks {
		t.Errorf("state = %v, want AwaitingChecks", sess.State)
	}

	// Claude should have been started.
	if len(cr.started) != 1 {
		t.Errorf("expected 1 claude start, got %d", len(cr.started))
	}

	// Branch should have been pushed.
	if len(wt.pushed) != 1 {
		t.Errorf("expected 1 push, got %d", len(wt.pushed))
	}
}

func TestIntegrationConflictFixLoop(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	attempts := newMockAttemptStore()
	repos := newMockRepoStore()
	vp := newSequencePRStatusProvider(
		&vcs.PRStatus{
			State:            vcs.PRStateOpen,
			HeadSHA:          "start-head-sha",
			Mergeable:        boolPtr(false),
			MergeStateStatus: vcs.MergeStateStatusDirty,
		},
		&vcs.PRStatus{
			State:            vcs.PRStateOpen,
			HeadSHA:          "pushed-head-sha",
			Mergeable:        boolPtr(true),
			MergeStateStatus: vcs.MergeStateStatusClean,
		},
	)
	cr := newMockAgentRunner()
	wt := &mockWorktreeManager{}
	logger := zerolog.Nop()

	prNum := 9701
	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		State:        machine.FixingChecks,
		PRNumber:     &prNum,
		WorktreePath: "/tmp/worktrees/test-repo/test",
		BranchName:   "test",
		BaseBranch:   "main",
	}

	fl := NewFixLoop(sessions, attempts, repos, vp, cr, wt, logger)

	err := fl.HandleConflict(ctx, "sess-1")
	if err != nil {
		t.Fatalf("HandleConflict: %v", err)
	}

	if sessions.sessions["sess-1"].State != machine.AwaitingChecks {
		t.Errorf("state = %v, want AwaitingChecks", sessions.sessions["sess-1"].State)
	}
}

func TestIntegrationReviewFeedbackFixLoop(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	attempts := newMockAttemptStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	cr := newMockAgentRunner()
	wt := &mockWorktreeManager{}
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		State:        machine.FixingChecks,
		WorktreePath: "/tmp/worktrees/test-repo/test",
		BranchName:   "test",
		BaseBranch:   "main",
	}

	fl := NewFixLoop(sessions, attempts, repos, vp, cr, wt, logger)

	comments := []vcs.ReviewComment{
		{Author: "alice", Body: "Add error handling", State: vcs.ReviewStateChangesRequested},
	}
	err := fl.HandleReviewFeedback(ctx, "sess-1", comments)
	if err != nil {
		t.Fatalf("HandleReviewFeedback: %v", err)
	}

	if sessions.sessions["sess-1"].State != machine.AwaitingChecks {
		t.Errorf("state = %v, want AwaitingChecks", sessions.sessions["sess-1"].State)
	}
}

func TestDispatcherReviewSubmitted(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	sessions.sessions["sess-1"] = &models.Session{
		ID:     "sess-1",
		RepoID: "repo-1",
		State:  machine.ReadyForReview,
	}

	d := NewDispatcher(sessions, repos, vp, nil, logger)

	ch := make(chan SessionEvent, 1)
	ch <- SessionEvent{
		SessionID: "sess-1",
		Event: vcs.ReviewSubmitted{
			PRID:  42,
			State: vcs.ReviewStateChangesRequested,
			Comments: []vcs.ReviewComment{
				{Author: "bob", Body: "Fix this", State: vcs.ReviewStateChangesRequested},
			},
		},
	}
	close(ch)

	d.Run(ctx, ch)

	sess := sessions.sessions["sess-1"]
	if sess.State != machine.FixingChecks {
		t.Errorf("state = %v, want FixingChecks", sess.State)
	}
}

func TestDispatcherReviewSubmittedMaxAttempts(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		State:        machine.ReadyForReview,
		AttemptCount: machine.MaxAttempts - 1,
	}

	d := NewDispatcher(sessions, repos, vp, nil, logger)

	ch := make(chan SessionEvent, 1)
	ch <- SessionEvent{
		SessionID: "sess-1",
		Event:     vcs.ReviewSubmitted{PRID: 42, State: vcs.ReviewStateChangesRequested},
	}
	close(ch)

	d.Run(ctx, ch)

	sess := sessions.sessions["sess-1"]
	if sess.State != machine.Blocked {
		t.Errorf("state = %v, want Blocked", sess.State)
	}
}

// TestFixLoopExhaustedBlockedReason verifies that when a fix attempt drives a
// session to Blocked because MaxAttempts is reached, the persisted BlockedReason
// is exactly sessionreason.FixLoopExhausted() — not the generic machine string.
func TestFixLoopExhaustedBlockedReason(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	attempts := newMockAttemptStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	cr := newMockAgentRunner()
	cr.startErr = errors.New("agent unavailable")
	wt := &mockWorktreeManager{}
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "owner/repo",
	}
	// AttemptCount == MaxAttempts-1 so the next FixFailed fires retryOrBlock → Blocked.
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		State:        machine.FixingChecks,
		AttemptCount: machine.MaxAttempts - 1,
		WorktreePath: "/tmp/worktrees/test-repo/test",
		BranchName:   "test",
		BaseBranch:   "main",
	}

	fl := NewFixLoop(sessions, attempts, repos, vp, cr, wt, logger)

	failure := vcs.CheckConclusionFailure
	_ = fl.HandleCheckFailure(ctx, "sess-1", []vcs.CheckResult{
		{ID: "ci/lint", Name: "lint", Status: vcs.CheckStatusCompleted, Conclusion: &failure},
	})

	sess := sessions.sessions["sess-1"]
	if sess.State != machine.Blocked {
		t.Fatalf("want Blocked, got %v", sess.State)
	}
	if sess.BlockedReason == nil || *sess.BlockedReason != sessionreason.FixLoopExhausted() {
		t.Fatalf("BlockedReason = %v, want %q", sess.BlockedReason, sessionreason.FixLoopExhausted())
	}
}
