package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/rs/zerolog"
)

type pollerRunsProvider struct {
	*mockVCSProvider
	runs  []vcs.WorkflowRun
	err   error
	calls int
}

func (p *pollerRunsProvider) ListWorkflowRuns(context.Context, string, string) ([]vcs.WorkflowRun, error) {
	p.calls++
	return p.runs, p.err
}

func TestPollerCancelledWorkflow_Sessions_AC246001(t *testing.T) {
	for _, tc := range []struct {
		name               string
		status, conclusion string
		noLister           bool
		err                error
		want               string
	}{
		{name: "success", status: "completed", conclusion: "success", want: "passed"},
		{name: "pending", status: "in_progress"},
		{name: "no lister", noLister: true, want: "failed"},
		{name: "error", err: errors.New("unreadable"), want: "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newMockVCSProvider()
			cancelled, success := vcs.CheckConclusionCancelled, vcs.CheckConclusionSuccess
			p.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, HeadSHA: "head"}
			p.nextCheckResults = []vcs.CheckResult{{Name: "notify", Workflow: "CI", Status: vcs.CheckStatusCompleted, Conclusion: &cancelled}, {Name: "build", Status: vcs.CheckStatusCompleted, Conclusion: &success}}
			withRuns := &pollerRunsProvider{mockVCSProvider: p, runs: []vcs.WorkflowRun{{Name: "CI", HeadSHA: "head", Status: tc.status, Conclusion: tc.conclusion}}, err: tc.err}
			var provider vcs.Provider = withRuns
			if tc.noLister {
				provider = p
			}
			poller := NewPoller(newMockSessionStore(), newMockRepoStore(), provider, time.Minute, DefaultPollTimeout, zerolog.Nop())
			pr := 7
			ch := make(chan Event, 4)
			poller.checkSession(context.Background(), ch, &models.Repo{ID: "repo", OriginURL: "acme/widgets"}, &models.Session{ID: "sess", State: machine.AwaitingChecks, PRNumber: &pr})
			got := ""
			for len(ch) > 0 {
				ev := <-ch
				switch ev.Event.(type) {
				case vcs.ChecksPassed:
					got = "passed"
				case vcs.ChecksFailed:
					got = "failed"
				}
			}
			if got != tc.want {
				t.Fatalf("event=%q want %q", got, tc.want)
			}
			if !tc.noLister && withRuns.calls != 1 {
				t.Fatalf("runs reads=%d want 1", withRuns.calls)
			}
		})
	}
}
