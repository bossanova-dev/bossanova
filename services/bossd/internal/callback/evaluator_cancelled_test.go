package callback

import (
	"context"
	"errors"
	"testing"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/rs/zerolog"
)

func TestEvaluateCancelledWorkflow_Callbacks_AC246002(t *testing.T) {
	for _, tc := range []struct {
		name           string
		runs           []vcs.WorkflowRun
		err            error
		passed, failed bool
	}{
		{name: "success", runs: []vcs.WorkflowRun{{Name: "CI", Status: "completed", Conclusion: "success", HeadSHA: testHeadSHA}}, passed: true},
		{name: "in flight", runs: []vcs.WorkflowRun{{Name: "CI", Status: "in_progress", HeadSHA: testHeadSHA}}},
		{name: "read error", err: errors.New("unreadable"), failed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			c := completedCheck("notify", vcs.CheckConclusionCancelled)
			c.Workflow = "CI"
			p := &fakeProvider{status: prStatus(vcs.PRStateOpen), checks: []vcs.CheckResult{completedCheck("build", vcs.CheckConclusionSuccess), c}, runs: tc.runs, runsErr: tc.err}
			e := NewEvaluator(store, p, nil, zerolog.Nop())
			expected := map[models.GitHubCallbackTrigger]bool{models.GitHubCallbackTriggerChecksPassed: tc.passed, models.GitHubCallbackTriggerChecksPassedReady: tc.passed, models.GitHubCallbackTriggerChecksFailed: tc.failed}
			callbacks := make(map[models.GitHubCallbackTrigger]string)
			for trigger := range expected {
				cb := mustCreate(t, store, db.CreateGitHubCallbackParams{TargetChatID: "chat", RepoOwner: "acme", RepoName: "widgets", PRNumber: 7, Trigger: trigger, Message: "checks"})
				callbacks[trigger] = cb.ID
			}
			if err := e.EvaluatePR(context.Background(), "acme", "widgets", 7); err != nil {
				t.Fatal(err)
			}
			for trigger, want := range expected {
				got := getState(t, store, callbacks[trigger])
				state := models.GitHubCallbackStateActive
				if want {
					state = models.GitHubCallbackStateTriggered
				}
				if got != state {
					t.Errorf("%s state=%s want %s", trigger, got, state)
				}
			}
			if p.runsCalls != 1 {
				t.Fatalf("runs reads=%d want 1", p.runsCalls)
			}
		})
	}
}
