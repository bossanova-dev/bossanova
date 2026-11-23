package session

import (
	"context"
	"testing"
	"time"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/status"
	"github.com/rs/zerolog"
)

// TestDisplayPoller_SupersededCancelledPR4630 reproduces PR #4630 (head 96967f58):
// the scripts workflow restarted on the same head, leaving "Notify on nightly failure"
// as Completed/Cancelled while "scripts" is InProgress and every other check passed
// or skipped. The display status must be Checking (not Failing), with HasFailures
// false, on both non-draft and bossd-held draft PRs.
func TestDisplayPoller_SupersededCancelledPR4630(t *testing.T) {
	cancelled := vcs.CheckConclusionCancelled
	success := vcs.CheckConclusionSuccess
	skipped := vcs.CheckConclusionSkipped
	checks4630 := []vcs.CheckResult{
		{Name: "scripts", Workflow: "scripts", Status: vcs.CheckStatusInProgress},
		{Name: "Notify on nightly failure", Workflow: "scripts", Status: vcs.CheckStatusCompleted, Conclusion: &cancelled},
		{Name: "go-test", Workflow: "CI", Status: vcs.CheckStatusCompleted, Conclusion: &success},
		{Name: "nightly-only", Workflow: "CI", Status: vcs.CheckStatusCompleted, Conclusion: &skipped},
	}
	runs4630 := []vcs.WorkflowRun{
		{Name: "scripts", HeadSHA: "96967f58", Status: "completed", Conclusion: "cancelled"},
		{Name: "scripts", HeadSHA: "96967f58", Status: "in_progress"},
		{Name: "CI", HeadSHA: "96967f58", Status: "completed", Conclusion: "success"},
	}

	for _, tc := range []struct {
		name  string
		draft bool
		state machine.State
		want  vcs.DisplayStatus
	}{
		{name: "ready PR", draft: false, state: machine.ReadyForReview, want: vcs.DisplayStatusChecking},
		{name: "bossd-held draft", draft: true, state: machine.AwaitingChecks, want: vcs.DisplayStatusDraft},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessions, repos, base := newMockSessionStore(), newMockRepoStore(), newMockVCSProvider()
			tracker, snapshots := status.NewDisplayTracker(), newMockCheckSnapshotStore()
			repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "owner/repo"}
			sessions.sessions["sess-1"] = &models.Session{ID: "sess-1", RepoID: "repo-1", PRNumber: intPtr(4630), State: tc.state}
			base.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, Draft: tc.draft, HeadSHA: "96967f58", Mergeable: boolPtr(true)}
			base.nextCheckResults = checks4630
			provider := &pollerRunsProvider{mockVCSProvider: base, runs: runs4630}

			poller := NewDisplayPoller(sessions, repos, provider, tracker, time.Second, zerolog.Nop())
			poller.SetSnapshotStore(snapshots)
			if err := poller.RefreshPR(context.Background(), "owner/repo", 4630); err != nil {
				t.Fatalf("RefreshPR: %v", err)
			}
			got := tracker.Get("sess-1")
			if got == nil || got.Status != tc.want || got.HasFailures {
				t.Fatalf("display entry = %+v, want %v with HasFailures=false", got, tc.want)
			}
			snaps := snapshots.all()
			if len(snaps) != 1 || snaps[0].ComputedStatus != int(tc.want) {
				t.Fatalf("snapshots = %+v, want 1 snapshot with %v", snaps, tc.want)
			}
			if provider.calls != 1 {
				t.Fatalf("ListWorkflowRuns calls = %d, want 1", provider.calls)
			}
		})
	}
}
