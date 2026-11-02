package session

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/status"
)

func TestDisplayPollerRequiredChecks_Sessions_AC223101(t *testing.T) {
	success := vcs.CheckConclusionSuccess
	light := vcs.CheckResult{Name: "lint", Status: vcs.CheckStatusCompleted, Conclusion: &success}
	goTest := vcs.CheckResult{Name: "go-test", Status: vcs.CheckStatusCompleted, Conclusion: &success}
	policy := pollerPolicyByPath{"/repos/widgets": {Names: []string{"go-test"}}}

	for _, tc := range []struct {
		name   string
		checks []vcs.CheckResult
		want   vcs.DisplayStatus
	}{
		{"light-only green", []vcs.CheckResult{light}, vcs.DisplayStatusIdle},
		{"required check succeeded", []vcs.CheckResult{light, goTest}, vcs.DisplayStatusPassing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessions := newMockSessionStore()
			repos := newMockRepoStore()
			vp := newMockVCSProvider()
			tracker := status.NewDisplayTracker()
			prNum := 42
			repos.repos["repo-1"] = &models.Repo{ID: "repo-1", LocalPath: "/repos/widgets", OriginURL: "owner/widgets"}
			sessions.sessions["sess-1"] = &models.Session{ID: "sess-1", RepoID: "repo-1", PRNumber: &prNum}
			vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, Mergeable: boolPtr(true), MergeStateStatus: vcs.MergeStateStatusClean, HeadSHA: "abc123", BaseBranch: "main"}
			vp.nextCheckResults = tc.checks

			poller := NewDisplayPoller(sessions, repos, vp, tracker, time.Minute, zerolog.Nop())
			poller.SetRequiredChecks(policy)
			poller.pollSession(context.Background(), repos.repos["repo-1"], sessions.sessions["sess-1"], prNum)
			if e := tracker.Get("sess-1"); e == nil || e.Status != tc.want {
				t.Fatalf("entry = %+v, want status %d", e, tc.want)
			}
		})
	}
}
