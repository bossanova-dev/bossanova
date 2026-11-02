package session

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
)

// pollerPolicyByPath answers RequiredChecks from a fixed map keyed by repo
// reference.
type pollerPolicyByPath map[string]vcs.RequiredChecks

func (m pollerPolicyByPath) RequiredChecks(_ context.Context, repoRef string) vcs.RequiredChecks {
	return m[repoRef]
}

func TestPollerChecksPassedHonoursRequiredChecks(t *testing.T) {
	success := vcs.CheckConclusionSuccess
	skipped := vcs.CheckConclusionSkipped
	light := vcs.CheckResult{Name: "lint", Status: vcs.CheckStatusCompleted, Conclusion: &success}
	policy := pollerPolicyByPath{"/repos/widgets": {Names: []string{"go-test"}, ExemptBases: []string{"production"}}}

	for _, tc := range []struct {
		name   string
		policy RequiredChecksSource
		base   string
		checks []vcs.CheckResult
		want   bool
	}{
		{"light-only green draft with requirement", policy, "main", []vcs.CheckResult{light}, false},
		{"skipped required check", policy, "main", []vcs.CheckResult{light, {Name: "go-test", Status: vcs.CheckStatusCompleted, Conclusion: &skipped}}, false},
		{"required check succeeded", policy, "main", []vcs.CheckResult{light, {Name: "go-test", Status: vcs.CheckStatusCompleted, Conclusion: &success}}, true},
		{"exempt base", policy, "production", []vcs.CheckResult{light}, true},
		{"no policy wired", nil, "main", []vcs.CheckResult{light}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vp := newMockVCSProvider()
			vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, Draft: true, HeadSHA: "sha-light", BaseBranch: tc.base}
			vp.nextCheckResults = tc.checks
			prNum := 42
			sess := &models.Session{ID: "sess-1", RepoID: "repo-1", State: machine.AwaitingChecks, PRNumber: &prNum}
			poller := NewPoller(newMockSessionStore(), newMockRepoStore(), vp, time.Minute, DefaultPollTimeout, zerolog.Nop())
			poller.SetRequiredChecks(tc.policy)
			ch := make(chan Event, 4)
			poller.checkSession(context.Background(), ch, &models.Repo{ID: "repo-1", LocalPath: "/repos/widgets", OriginURL: "owner/widgets"}, sess)
			close(ch)

			var got bool
			for ev := range ch {
				if _, ok := ev.Event.(vcs.ChecksPassed); ok {
					got = true
				}
			}
			if got != tc.want {
				t.Fatalf("emitted ChecksPassed = %v, want %v", got, tc.want)
			}
		})
	}
}
