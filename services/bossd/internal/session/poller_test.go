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

	poller := NewPoller(sessions, repos, vp, 50*time.Millisecond, logger)
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

	poller := NewPoller(sessions, repos, vp, 50*time.Millisecond, logger)
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

	poller := NewPoller(sessions, repos, vp, 50*time.Millisecond, logger)
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

	poller := NewPoller(sessions, repos, vp, 50*time.Millisecond, logger)
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

func TestPollerSkipsNonAwaitingChecks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:     "sess-1",
		RepoID: "repo-1",
		State:  machine.ImplementingPlan, // Not AwaitingChecks.
	}

	poller := NewPoller(sessions, repos, vp, 50*time.Millisecond, logger)
	ch := poller.Run(ctx)

	// Should not receive any events.
	select {
	case ev := <-ch:
		if ev.Event != nil {
			t.Errorf("unexpected event: %T", ev.Event)
		}
	case <-ctx.Done():
		// Expected — no events.
	}
}
