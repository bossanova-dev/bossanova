package session

import (
	"context"
	"fmt"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
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
	cr := newMockClaudeRunner()
	wt := &mockWorktreeManager{}
	logger := zerolog.Nop()

	prNum := 42
	claudeID := "claude-old"
	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:              "sess-1",
		RepoID:          "repo-1",
		State:           machine.FixingChecks,
		AttemptCount:    1,
		PRNumber:        &prNum,
		WorktreePath:    "/tmp/worktrees/boss/test",
		BranchName:      "boss/test",
		BaseBranch:      "main",
		ClaudeSessionID: &claudeID,
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
	if len(wt.pushed) != 1 || wt.pushed[0] != "boss/test" {
		t.Errorf("expected push of boss/test, got %v", wt.pushed)
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
	vp := newMockVCSProvider()
	cr := newMockClaudeRunner()
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
		WorktreePath: "/tmp/worktrees/boss/test",
		BranchName:   "boss/test",
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

func TestFixLoopHandleReviewFeedback(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	attempts := newMockAttemptStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	cr := newMockClaudeRunner()
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
		WorktreePath: "/tmp/worktrees/boss/test",
		BranchName:   "boss/test",
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
	cr := newMockClaudeRunner()
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
	cr := newMockClaudeRunner()
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
	cr := newMockClaudeRunner()
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
		WorktreePath: "/tmp/worktrees/boss/test",
		BranchName:   "boss/test",
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
	vp := newMockVCSProvider()
	cr := newMockClaudeRunner()
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
		WorktreePath: "/tmp/worktrees/boss/test",
		BranchName:   "boss/test",
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
	cr := newMockClaudeRunner()
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
		WorktreePath: "/tmp/worktrees/boss/test",
		BranchName:   "boss/test",
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
			PRID: 42,
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
		Event:     vcs.ReviewSubmitted{PRID: 42},
	}
	close(ch)

	d.Run(ctx, ch)

	sess := sessions.sessions["sess-1"]
	if sess.State != machine.Blocked {
		t.Errorf("state = %v, want Blocked", sess.State)
	}
}
