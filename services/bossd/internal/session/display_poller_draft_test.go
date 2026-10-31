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

func TestDisplayPoller_HeldDraft_Sessions_AC160(t *testing.T) {
	for _, tc := range []struct {
		name             string
		state            machine.State
		conflict, failed bool
		want             vcs.DisplayStatus
	}{
		{"held conflict", machine.FixingChecks, true, false, vcs.DisplayStatusConflict},
		{"held failure", machine.AwaitingChecks, false, true, vcs.DisplayStatusFailing},
		{"person conflict", machine.ReadyForReview, true, false, vcs.DisplayStatusDraft},
		{"person failure", machine.ReadyForReview, false, true, vcs.DisplayStatusDraft},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessions, repos, vp := newMockSessionStore(), newMockRepoStore(), newMockVCSProvider()
			tracker, snapshots := status.NewDisplayTracker(), newMockCheckSnapshotStore()
			repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "owner/repo"}
			sessions.sessions["sess-1"] = &models.Session{ID: "sess-1", RepoID: "repo-1", PRNumber: intPtr(10), State: tc.state}
			vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, Draft: true, HeadSHA: "head", Mergeable: boolPtr(!tc.conflict)}
			if tc.failed {
				failure := vcs.CheckConclusionFailure
				vp.nextCheckResults = []vcs.CheckResult{{Name: "ci", Status: vcs.CheckStatusCompleted, Conclusion: &failure}}
			}
			poller := NewDisplayPoller(sessions, repos, vp, tracker, time.Second, zerolog.Nop())
			poller.SetSnapshotStore(snapshots)
			if err := poller.RefreshPR(context.Background(), "owner/repo", 10); err != nil {
				t.Fatal(err)
			}
			got := tracker.Get("sess-1")
			if got == nil || got.Status != tc.want || got.Mergeable == nil || *got.Mergeable == tc.conflict {
				t.Fatalf("entry=%+v, want %v mergeable=%v", got, tc.want, !tc.conflict)
			}
			snaps := snapshots.all()
			if len(snaps) != 1 || snaps[0].ComputedStatus != int(tc.want) {
				t.Fatalf("snapshots=%+v", snaps)
			}
			if vp.getReviewCommentsCalls != 0 {
				t.Fatal("draft reviews were read")
			}
		})
	}
}
