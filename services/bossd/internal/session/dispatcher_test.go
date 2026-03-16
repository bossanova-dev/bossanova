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

func TestDispatcherChecksPassed(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	prNum := 42
	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:       "sess-1",
		RepoID:   "repo-1",
		State:    machine.AwaitingChecks,
		PRNumber: &prNum,
	}

	d := NewDispatcher(sessions, repos, vp, logger)

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

	d := NewDispatcher(sessions, repos, vp, logger)

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

	d := NewDispatcher(sessions, repos, vp, logger)

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

	d := NewDispatcher(sessions, repos, vp, logger)

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

	d := NewDispatcher(sessions, repos, vp, logger)

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

	d := NewDispatcher(sessions, repos, vp, logger)

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

	d := NewDispatcher(sessions, repos, vp, logger)

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
