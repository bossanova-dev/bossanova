package session

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
)

func TestAggregateChecks(t *testing.T) {
	success := vcs.CheckConclusionSuccess
	failure := vcs.CheckConclusionFailure

	tests := []struct {
		name   string
		checks []vcs.CheckResult
		want   vcs.ChecksOverall
	}{
		{
			name:   "all passed",
			checks: []vcs.CheckResult{{Status: vcs.CheckStatusCompleted, Conclusion: &success}, {Status: vcs.CheckStatusCompleted, Conclusion: &success}},
			want:   vcs.ChecksOverallPassed,
		},
		{
			name:   "one failed",
			checks: []vcs.CheckResult{{Status: vcs.CheckStatusCompleted, Conclusion: &success}, {Status: vcs.CheckStatusCompleted, Conclusion: &failure}},
			want:   vcs.ChecksOverallFailed,
		},
		{
			name:   "still pending",
			checks: []vcs.CheckResult{{Status: vcs.CheckStatusCompleted, Conclusion: &success}, {Status: vcs.CheckStatusInProgress}},
			want:   vcs.ChecksOverallPending,
		},
		{
			name:   "all queued",
			checks: []vcs.CheckResult{{Status: vcs.CheckStatusQueued}, {Status: vcs.CheckStatusQueued}},
			want:   vcs.ChecksOverallPending,
		},
		{
			name:   "failure short-circuits pending",
			checks: []vcs.CheckResult{{Status: vcs.CheckStatusCompleted, Conclusion: &failure}, {Status: vcs.CheckStatusInProgress}},
			want:   vcs.ChecksOverallFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := aggregateChecks(tt.checks)
			if got != tt.want {
				t.Errorf("aggregateChecks() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPollerEmitsChecksPassed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

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

	// Configure mock to return all checks passed.
	success := vcs.CheckConclusionSuccess
	vp.nextCheckResults = []vcs.CheckResult{
		{Status: vcs.CheckStatusCompleted, Conclusion: &success},
	}
	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen}

	poller := NewPoller(sessions, repos, vp, 50*time.Millisecond, DefaultPollTimeout, logger)
	ch := poller.Run(ctx)

	// Wait for the first event.
	select {
	case ev := <-ch:
		if ev.SessionID != "sess-1" {
			t.Errorf("session = %q, want sess-1", ev.SessionID)
		}
		if _, ok := ev.Event.(vcs.ChecksPassed); !ok {
			t.Errorf("event type = %T, want ChecksPassed", ev.Event)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for event")
	}
}

func TestPollerEmitsChecksFailed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

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

	// Configure mock to return a failed check.
	failure := vcs.CheckConclusionFailure
	vp.nextCheckResults = []vcs.CheckResult{
		{Status: vcs.CheckStatusCompleted, Conclusion: &failure},
	}
	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen}

	poller := NewPoller(sessions, repos, vp, 50*time.Millisecond, DefaultPollTimeout, logger)
	ch := poller.Run(ctx)

	select {
	case ev := <-ch:
		if ev.SessionID != "sess-1" {
			t.Errorf("session = %q, want sess-1", ev.SessionID)
		}
		failed, ok := ev.Event.(vcs.ChecksFailed)
		if !ok {
			t.Errorf("event type = %T, want ChecksFailed", ev.Event)
		}
		if len(failed.FailedChecks) != 1 {
			t.Errorf("failed checks = %d, want 1", len(failed.FailedChecks))
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for event")
	}
}

func TestPollerEmitsPRMerged(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

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

	// Configure mock to return PR merged.
	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateMerged}

	poller := NewPoller(sessions, repos, vp, 50*time.Millisecond, DefaultPollTimeout, logger)
	ch := poller.Run(ctx)

	select {
	case ev := <-ch:
		if _, ok := ev.Event.(vcs.PRMerged); !ok {
			t.Errorf("event type = %T, want PRMerged", ev.Event)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for event")
	}
}

func TestPollerEmitsConflictDetected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

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

	// Configure mock to return conflict.
	mergeable := false
	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, Mergeable: &mergeable}

	poller := NewPoller(sessions, repos, vp, 50*time.Millisecond, DefaultPollTimeout, logger)
	ch := poller.Run(ctx)

	select {
	case ev := <-ch:
		if _, ok := ev.Event.(vcs.ConflictDetected); !ok {
			t.Errorf("event type = %T, want ConflictDetected", ev.Event)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for event")
	}
}

// hungVCSProvider wraps mockVCSProvider so GetPRStatus blocks on ctx,
// letting tests verify the per-poll timeout bounds a stuck provider call.
type hungVCSProvider struct {
	*mockVCSProvider
	prStatusCalls int64
}

func (h *hungVCSProvider) GetPRStatus(ctx context.Context, _ string, _ int) (*vcs.PRStatus, error) {
	atomic.AddInt64(&h.prStatusCalls, 1)
	<-ctx.Done()
	return nil, ctx.Err()
}

// syncBuf is a threadsafe buffer for zerolog output.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func TestPollerPollTimeoutBoundsHungProvider(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := &hungVCSProvider{mockVCSProvider: newMockVCSProvider()}

	var logBuf syncBuf
	logger := zerolog.New(&logBuf)

	prNum := 42
	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "owner/repo"}
	sessions.sessions["sess-1"] = &models.Session{
		ID:       "sess-1",
		RepoID:   "repo-1",
		State:    machine.AwaitingChecks,
		PRNumber: &prNum,
	}

	// Tight timeout + interval so we can observe several iterations.
	poller := NewPoller(sessions, repos, vp, 25*time.Millisecond, 20*time.Millisecond, logger)
	_ = poller.Run(ctx)

	// Wait long enough for >=2 ticks to have fired and timed out.
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-poller.Done()

	calls := atomic.LoadInt64(&vp.prStatusCalls)
	if calls < 2 {
		t.Fatalf("expected >=2 GetPRStatus calls after multiple timeouts, got %d", calls)
	}
	if !strings.Contains(logBuf.String(), "exceeded timeout") {
		t.Errorf("expected timeout warning in logs, got %q", logBuf.String())
	}
}

func TestPollerSkipsNonPollableStates(t *testing.T) {
	// Pre-PR and terminal-ish states must never be polled — there is no PR
	// to ask about, or the lifecycle has moved past CI/review (Finalizing,
	// Blocked, Merged, Closed). ImplementingPlan is the canonical pre-PR
	// case; the rest follow from the same filter.
	skipStates := []machine.State{
		machine.CreatingWorktree,
		machine.StartingAgent,
		machine.ImplementingPlan,
		machine.PushingBranch,
		machine.OpeningDraftPR,
		machine.Finalizing,
		machine.Blocked,
	}
	for _, st := range skipStates {
		t.Run(st.String(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()

			sessions := newMockSessionStore()
			repos := newMockRepoStore()
			vp := newMockVCSProvider()
			logger := zerolog.Nop()

			prNum := 42
			repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "owner/repo"}
			sessions.sessions["sess-1"] = &models.Session{
				ID:       "sess-1",
				RepoID:   "repo-1",
				State:    st,
				PRNumber: &prNum,
			}
			mergeable := false
			vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, Mergeable: &mergeable}

			poller := NewPoller(sessions, repos, vp, 50*time.Millisecond, DefaultPollTimeout, logger)
			ch := poller.Run(ctx)

			select {
			case ev := <-ch:
				if ev.Event != nil {
					t.Errorf("unexpected event from %s: %T", st, ev.Event)
				}
			case <-ctx.Done():
				// Expected — no events.
			}
		})
	}
}

// TestPollerEmitsConflictDetectedFromPollableStates verifies the poller
// catches conflicts that appear after checks have passed (GreenDraft /
// ReadyForReview). Without this, conflicts that surface post-AwaitingChecks
// leave the session stuck — the display poller updates display_status but
// the state machine never advances to FixingChecks because no event is
// emitted.
//
// FixingChecks is intentionally not in this set: see
// TestFixingChecksRejectsConflictDetected (state machine) and
// TestPollerSkipsConflictDetectedFromFixingChecks (poller). Re-emitting
// from FixingChecks would self-transition and inflate AttemptCount on
// every poll cycle.
func TestPollerEmitsConflictDetectedFromPollableStates(t *testing.T) {
	pollableNonAwaiting := []machine.State{
		machine.GreenDraft,
		machine.ReadyForReview,
	}
	for _, st := range pollableNonAwaiting {
		t.Run(st.String(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			sessions := newMockSessionStore()
			repos := newMockRepoStore()
			vp := newMockVCSProvider()
			logger := zerolog.Nop()

			prNum := 42
			repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "owner/repo"}
			sessions.sessions["sess-1"] = &models.Session{
				ID:       "sess-1",
				RepoID:   "repo-1",
				State:    st,
				PRNumber: &prNum,
			}
			mergeable := false
			vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, Mergeable: &mergeable}

			poller := NewPoller(sessions, repos, vp, 50*time.Millisecond, DefaultPollTimeout, logger)
			ch := poller.Run(ctx)

			select {
			case ev := <-ch:
				if _, ok := ev.Event.(vcs.ConflictDetected); !ok {
					t.Errorf("event type from %s = %T, want ConflictDetected", st, ev.Event)
				}
			case <-ctx.Done():
				t.Fatalf("timed out waiting for ConflictDetected from %s", st)
			}
		})
	}
}

// TestPollerSkipsEventsNotPermittedByState pins the dispatcher-vs-poller
// invariant: the poller must never emit an event the state machine cannot
// fire. Otherwise the dispatcher logs "fire X failed" on every poll cycle
// (~2min default) and, for self-transitions like ConflictDetected from
// FixingChecks, also inflates AttemptCount until the session is Blocked.
//
// Each subtest sets up a session in a state where the candidate event is
// NOT permitted, configures the mock provider to surface the corresponding
// PR signal, and asserts the poller channel receives nothing.
func TestPollerSkipsEventsNotPermittedByState(t *testing.T) {
	tests := []struct {
		name      string
		state     machine.State
		setupVCS  func(*mockVCSProvider)
		eventDesc string
	}{
		{
			name:  "GreenDraft+passing checks does not emit ChecksPassed",
			state: machine.GreenDraft,
			setupVCS: func(vp *mockVCSProvider) {
				success := vcs.CheckConclusionSuccess
				vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen}
				vp.nextCheckResults = []vcs.CheckResult{
					{Status: vcs.CheckStatusCompleted, Conclusion: &success},
				}
			},
			eventDesc: "ChecksPassed (not permitted from GreenDraft)",
		},
		{
			name:  "ReadyForReview+passing checks does not emit ChecksPassed",
			state: machine.ReadyForReview,
			setupVCS: func(vp *mockVCSProvider) {
				success := vcs.CheckConclusionSuccess
				vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen}
				vp.nextCheckResults = []vcs.CheckResult{
					{Status: vcs.CheckStatusCompleted, Conclusion: &success},
				}
			},
			eventDesc: "ChecksPassed (not permitted from ReadyForReview)",
		},
		{
			name:  "FixingChecks+conflict does not re-emit ConflictDetected",
			state: machine.FixingChecks,
			setupVCS: func(vp *mockVCSProvider) {
				mergeable := false
				vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, Mergeable: &mergeable}
			},
			eventDesc: "ConflictDetected (would inflate AttemptCount on every poll)",
		},
		{
			name:  "FixingChecks+passing checks does not emit ChecksPassed",
			state: machine.FixingChecks,
			setupVCS: func(vp *mockVCSProvider) {
				success := vcs.CheckConclusionSuccess
				vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen}
				vp.nextCheckResults = []vcs.CheckResult{
					{Status: vcs.CheckStatusCompleted, Conclusion: &success},
				}
			},
			eventDesc: "ChecksPassed (FIX_COMPLETE is the legitimate exit, not poll observation)",
		},
		{
			name:  "FixingChecks+failing checks does not re-emit ChecksFailed",
			state: machine.FixingChecks,
			setupVCS: func(vp *mockVCSProvider) {
				failure := vcs.CheckConclusionFailure
				vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen}
				vp.nextCheckResults = []vcs.CheckResult{
					{Status: vcs.CheckStatusCompleted, Conclusion: &failure},
				}
			},
			eventDesc: "ChecksFailed (not permitted from FixingChecks)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()

			sessions := newMockSessionStore()
			repos := newMockRepoStore()
			vp := newMockVCSProvider()
			logger := zerolog.Nop()

			prNum := 42
			repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "owner/repo"}
			sessions.sessions["sess-1"] = &models.Session{
				ID:       "sess-1",
				RepoID:   "repo-1",
				State:    tt.state,
				PRNumber: &prNum,
			}
			tt.setupVCS(vp)

			poller := NewPoller(sessions, repos, vp, 25*time.Millisecond, DefaultPollTimeout, logger)
			ch := poller.Run(ctx)

			select {
			case ev := <-ch:
				if ev.Event != nil {
					t.Errorf("emitted unexpected %T from %s; want no emission for %s", ev.Event, tt.state, tt.eventDesc)
				}
			case <-ctx.Done():
				// Expected — no events.
			}
		})
	}
}
