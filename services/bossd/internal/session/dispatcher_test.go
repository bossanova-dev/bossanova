package session

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
)

// --- Mock FixHandler ---

type mockFixHandler struct {
	checkFailureCalls []string
	conflictCalls     []string
	reviewCalls       []string
	done              chan struct{} // closed after a handler call, to sync with async goroutine
}

func newMockFixHandler() *mockFixHandler {
	return &mockFixHandler{done: make(chan struct{}, 1)}
}

func (m *mockFixHandler) HandleCheckFailure(_ context.Context, sessionID string, _ []vcs.CheckResult) error {
	m.checkFailureCalls = append(m.checkFailureCalls, sessionID)
	select {
	case m.done <- struct{}{}:
	default:
	}
	return nil
}

func (m *mockFixHandler) HandleConflict(_ context.Context, sessionID string) error {
	m.conflictCalls = append(m.conflictCalls, sessionID)
	select {
	case m.done <- struct{}{}:
	default:
	}
	return nil
}

func (m *mockFixHandler) HandleReviewFeedback(_ context.Context, sessionID string, _ []vcs.ReviewComment) error {
	m.reviewCalls = append(m.reviewCalls, sessionID)
	select {
	case m.done <- struct{}{}:
	default:
	}
	return nil
}

func TestDispatcherChecksPassed(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	prNum := 42
	repos.repos["repo-1"] = &models.Repo{
		ID:           "repo-1",
		OriginURL:    "owner/repo",
		CanAutoMerge: true,
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:       "sess-1",
		RepoID:   "repo-1",
		State:    machine.AwaitingChecks,
		PRNumber: &prNum,
	}

	d := NewDispatcher(sessions, repos, vp, nil, logger)

	ch := make(chan SessionEvent, 1)
	ch <- SessionEvent{SessionID: "sess-1", Event: vcs.ChecksPassed{PRID: 42}}
	close(ch)

	d.Run(ctx, ch)

	sess := sessions.sessions["sess-1"]
	// Should transition to ReadyForReview (via GreenDraft → MarkReadyForReview → ReadyForReview).
	if sess.State != machine.ReadyForReview {
		t.Errorf("state = %v, want ReadyForReview", sess.State)
	}

	// Should have called MarkReadyForReview.
	if len(vp.markReadyCalls) != 1 || vp.markReadyCalls[0] != 42 {
		t.Errorf("markReadyCalls = %v, want [42]", vp.markReadyCalls)
	}
}

func TestDispatcherChecksFailed(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	sessions.sessions["sess-1"] = &models.Session{
		ID:     "sess-1",
		RepoID: "repo-1",
		State:  machine.AwaitingChecks,
	}

	d := NewDispatcher(sessions, repos, vp, nil, logger)

	failure := vcs.CheckConclusionFailure
	ch := make(chan SessionEvent, 1)
	ch <- SessionEvent{
		SessionID: "sess-1",
		Event: vcs.ChecksFailed{
			PRID:         42,
			FailedChecks: []vcs.CheckResult{{Conclusion: &failure}},
		},
	}
	close(ch)

	d.Run(ctx, ch)

	sess := sessions.sessions["sess-1"]
	if sess.State != machine.FixingChecks {
		t.Errorf("state = %v, want FixingChecks", sess.State)
	}
}

func TestDispatcherChecksFailedMaxAttempts(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		State:        machine.AwaitingChecks,
		AttemptCount: machine.MaxAttempts - 1, // One attempt away from max.
	}

	d := NewDispatcher(sessions, repos, vp, nil, logger)

	ch := make(chan SessionEvent, 1)
	ch <- SessionEvent{SessionID: "sess-1", Event: vcs.ChecksFailed{PRID: 42}}
	close(ch)

	d.Run(ctx, ch)

	sess := sessions.sessions["sess-1"]
	if sess.State != machine.Blocked {
		t.Errorf("state = %v, want Blocked", sess.State)
	}
	if sess.BlockedReason == nil {
		t.Error("expected blocked reason to be set")
	}
}

func TestDispatcherConflictDetected(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	sessions.sessions["sess-1"] = &models.Session{
		ID:     "sess-1",
		RepoID: "repo-1",
		State:  machine.AwaitingChecks,
	}

	d := NewDispatcher(sessions, repos, vp, nil, logger)

	ch := make(chan SessionEvent, 1)
	ch <- SessionEvent{SessionID: "sess-1", Event: vcs.ConflictDetected{PRID: 42}}
	close(ch)

	d.Run(ctx, ch)

	sess := sessions.sessions["sess-1"]
	if sess.State != machine.FixingChecks {
		t.Errorf("state = %v, want FixingChecks", sess.State)
	}
}

func TestDispatcherPRMerged(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	sessions.sessions["sess-1"] = &models.Session{
		ID:     "sess-1",
		RepoID: "repo-1",
		State:  machine.AwaitingChecks,
	}

	d := NewDispatcher(sessions, repos, vp, nil, logger)

	ch := make(chan SessionEvent, 1)
	ch <- SessionEvent{SessionID: "sess-1", Event: vcs.PRMerged{PRID: 42}}
	close(ch)

	d.Run(ctx, ch)

	if sessions.sessions["sess-1"].State != machine.Merged {
		t.Errorf("state = %v, want Merged", sessions.sessions["sess-1"].State)
	}
}

func TestDispatcherPRClosed(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	sessions.sessions["sess-1"] = &models.Session{
		ID:     "sess-1",
		RepoID: "repo-1",
		State:  machine.AwaitingChecks,
	}

	d := NewDispatcher(sessions, repos, vp, nil, logger)

	ch := make(chan SessionEvent, 1)
	ch <- SessionEvent{SessionID: "sess-1", Event: vcs.PRClosed{PRID: 42}}
	close(ch)

	d.Run(ctx, ch)

	if sessions.sessions["sess-1"].State != machine.Closed {
		t.Errorf("state = %v, want Closed", sessions.sessions["sess-1"].State)
	}
}

func TestDispatcherContextCancellation(t *testing.T) {
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	d := NewDispatcher(sessions, repos, vp, nil, logger)

	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan SessionEvent)

	done := make(chan struct{})
	go func() {
		d.Run(ctx, ch)
		close(done)
	}()

	// Cancel context should stop the dispatcher.
	cancel()
	select {
	case <-done:
		// Expected.
	case <-time.After(time.Second):
		t.Fatal("dispatcher did not stop on context cancellation")
	}
}

func TestDispatcherChecksPassedAutoMergeDisabled(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	prNum := 42
	repos.repos["repo-1"] = &models.Repo{
		ID:           "repo-1",
		OriginURL:    "owner/repo",
		CanAutoMerge: false,
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:       "sess-1",
		RepoID:   "repo-1",
		State:    machine.AwaitingChecks,
		PRNumber: &prNum,
	}

	d := NewDispatcher(sessions, repos, vp, nil, logger)

	ch := make(chan SessionEvent, 1)
	ch <- SessionEvent{SessionID: "sess-1", Event: vcs.ChecksPassed{PRID: 42}}
	close(ch)

	d.Run(ctx, ch)

	sess := sessions.sessions["sess-1"]
	// Should stop at GreenDraft — not transition to ReadyForReview.
	if sess.State != machine.GreenDraft {
		t.Errorf("state = %v, want GreenDraft", sess.State)
	}
	if len(vp.markReadyCalls) != 0 {
		t.Errorf("markReadyCalls = %v, want none", vp.markReadyCalls)
	}
}

func TestDispatcherChecksFailedAutomationDisabled(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	fh := newMockFixHandler()
	logger := zerolog.Nop()

	sessions.sessions["sess-1"] = &models.Session{
		ID:                "sess-1",
		RepoID:            "repo-1",
		State:             machine.AwaitingChecks,
		AutomationEnabled: false,
	}

	d := NewDispatcher(sessions, repos, vp, fh, logger)

	failure := vcs.CheckConclusionFailure
	ch := make(chan SessionEvent, 1)
	ch <- SessionEvent{
		SessionID: "sess-1",
		Event:     vcs.ChecksFailed{PRID: 42, FailedChecks: []vcs.CheckResult{{Conclusion: &failure}}},
	}
	close(ch)

	d.Run(ctx, ch)

	// State should still transition to FixingChecks (state machine doesn't check automation).
	sess := sessions.sessions["sess-1"]
	if sess.State != machine.FixingChecks {
		t.Errorf("state = %v, want FixingChecks", sess.State)
	}
	// But fix loop should NOT have been called.
	if len(fh.checkFailureCalls) != 0 {
		t.Errorf("expected 0 check failure calls, got %d", len(fh.checkFailureCalls))
	}
}

func TestDispatcherChecksFailedAutomationEnabled(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	fh := newMockFixHandler()
	logger := zerolog.Nop()

	sessions.sessions["sess-1"] = &models.Session{
		ID:                "sess-1",
		RepoID:            "repo-1",
		State:             machine.AwaitingChecks,
		AutomationEnabled: true,
	}

	d := NewDispatcher(sessions, repos, vp, fh, logger)

	failure := vcs.CheckConclusionFailure
	ch := make(chan SessionEvent, 1)
	ch <- SessionEvent{
		SessionID: "sess-1",
		Event:     vcs.ChecksFailed{PRID: 42, FailedChecks: []vcs.CheckResult{{Conclusion: &failure}}},
	}
	close(ch)

	d.Run(ctx, ch)

	// Wait for async fix loop goroutine.
	select {
	case <-fh.done:
	case <-time.After(2 * time.Second):
		t.Fatal("fix loop was not invoked within timeout")
	}

	if len(fh.checkFailureCalls) != 1 {
		t.Errorf("expected 1 check failure call, got %d", len(fh.checkFailureCalls))
	}
}

func TestDispatcherConflictAutoResolveDisabled(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	fh := newMockFixHandler()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                      "repo-1",
		CanAutoResolveConflicts: false,
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:     "sess-1",
		RepoID: "repo-1",
		State:  machine.AwaitingChecks,
	}

	d := NewDispatcher(sessions, repos, vp, fh, logger)

	ch := make(chan SessionEvent, 1)
	ch <- SessionEvent{SessionID: "sess-1", Event: vcs.ConflictDetected{PRID: 42}}
	close(ch)

	d.Run(ctx, ch)

	sess := sessions.sessions["sess-1"]
	if sess.State != machine.FixingChecks {
		t.Errorf("state = %v, want FixingChecks", sess.State)
	}
	if len(fh.conflictCalls) != 0 {
		t.Errorf("expected 0 conflict calls, got %d", len(fh.conflictCalls))
	}
}

func TestDispatcherConflictAutoResolveEnabled(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	fh := newMockFixHandler()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                      "repo-1",
		CanAutoResolveConflicts: true,
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:     "sess-1",
		RepoID: "repo-1",
		State:  machine.AwaitingChecks,
	}

	d := NewDispatcher(sessions, repos, vp, fh, logger)

	ch := make(chan SessionEvent, 1)
	ch <- SessionEvent{SessionID: "sess-1", Event: vcs.ConflictDetected{PRID: 42}}
	close(ch)

	d.Run(ctx, ch)

	select {
	case <-fh.done:
	case <-time.After(2 * time.Second):
		t.Fatal("fix loop was not invoked within timeout")
	}

	if len(fh.conflictCalls) != 1 {
		t.Errorf("expected 1 conflict call, got %d", len(fh.conflictCalls))
	}
}

func TestDispatcherReviewAutoAddressDisabled(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	fh := newMockFixHandler()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                    "repo-1",
		CanAutoAddressReviews: false,
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:     "sess-1",
		RepoID: "repo-1",
		State:  machine.ReadyForReview,
	}

	d := NewDispatcher(sessions, repos, vp, fh, logger)

	ch := make(chan SessionEvent, 1)
	ch <- SessionEvent{
		SessionID: "sess-1",
		Event:     vcs.ReviewSubmitted{PRID: 42, Comments: []vcs.ReviewComment{{Body: "fix this"}}},
	}
	close(ch)

	d.Run(ctx, ch)

	sess := sessions.sessions["sess-1"]
	if sess.State != machine.FixingChecks {
		t.Errorf("state = %v, want FixingChecks", sess.State)
	}
	if len(fh.reviewCalls) != 0 {
		t.Errorf("expected 0 review calls, got %d", len(fh.reviewCalls))
	}
}

func TestDispatcherReviewAutoAddressEnabled(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	fh := newMockFixHandler()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                    "repo-1",
		CanAutoAddressReviews: true,
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:     "sess-1",
		RepoID: "repo-1",
		State:  machine.ReadyForReview,
	}

	d := NewDispatcher(sessions, repos, vp, fh, logger)

	ch := make(chan SessionEvent, 1)
	ch <- SessionEvent{
		SessionID: "sess-1",
		Event:     vcs.ReviewSubmitted{PRID: 42, Comments: []vcs.ReviewComment{{Body: "fix this"}}},
	}
	close(ch)

	d.Run(ctx, ch)

	select {
	case <-fh.done:
	case <-time.After(2 * time.Second):
		t.Fatal("fix loop was not invoked within timeout")
	}

	if len(fh.reviewCalls) != 1 {
		t.Errorf("expected 1 review call, got %d", len(fh.reviewCalls))
	}
}
