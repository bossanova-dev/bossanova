package callback

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
)

// callbackPolicyByRepo answers RequiredChecks from a fixed map keyed by repo
// reference.
type callbackPolicyByRepo map[string]vcs.RequiredChecks

func (m callbackPolicyByRepo) RequiredChecks(_ context.Context, repoRef string) vcs.RequiredChecks {
	return m[repoRef]
}

func TestEvaluatePR_RequiredChecks_Callbacks_AC223101(t *testing.T) {
	policy := callbackPolicyByRepo{"acme/widgets": {Names: []string{"go-test"}}}
	light := completedCheck("lint", vcs.CheckConclusionSuccess)

	for _, trigger := range []models.GitHubCallbackTrigger{
		models.GitHubCallbackTriggerChecksPassed,
		models.GitHubCallbackTriggerChecksPassedReady,
	} {
		t.Run(string(trigger), func(t *testing.T) {
			store := newStore(t)
			cb := mustCreate(t, store, db.CreateGitHubCallbackParams{
				TargetChatID: "chat-1",
				RepoOwner:    "acme",
				RepoName:     "widgets",
				PRNumber:     7,
				Trigger:      trigger,
				Message:      "hello",
			})
			status := prStatusDraft(vcs.PRStateOpen, false)
			status.BaseBranch = "main"
			prov := &fakeProvider{status: status, checks: []vcs.CheckResult{light}}
			ev := NewEvaluator(store, prov, fixedNow(), zerolog.Nop())
			ev.SetRequiredChecks(policy)

			if _, ready, err := ev.ChecksPassedReady(context.Background(), "acme", "widgets", 7); err != nil || ready {
				t.Fatalf("ChecksPassedReady on light-only head = (%v, %v), want (false, nil)", ready, err)
			}
			if err := ev.EvaluatePR(context.Background(), "acme", "widgets", 7); err != nil {
				t.Fatalf("EvaluatePR: %v", err)
			}
			if got := getState(t, store, cb.ID); got != models.GitHubCallbackStateActive {
				t.Fatalf("light-only head: state = %q, want active", got)
			}

			prov.mu.Lock()
			prov.checks = []vcs.CheckResult{light, completedCheck("go-test", vcs.CheckConclusionSuccess)}
			prov.mu.Unlock()
			if _, ready, err := ev.ChecksPassedReady(context.Background(), "acme", "widgets", 7); err != nil || !ready {
				t.Fatalf("ChecksPassedReady once go-test passed = (%v, %v), want (true, nil)", ready, err)
			}
			if err := ev.EvaluatePR(context.Background(), "acme", "widgets", 7); err != nil {
				t.Fatalf("EvaluatePR: %v", err)
			}
			if got := getState(t, store, cb.ID); got != models.GitHubCallbackStateTriggered {
				t.Fatalf("after go-test succeeded: state = %q, want triggered", got)
			}
		})
	}
}

func TestSatisfiedTriggers_SkippedRequiredCheckHasNotRun(t *testing.T) {
	checks := []vcs.CheckResult{
		completedCheck("lint", vcs.CheckConclusionSuccess),
		completedCheck("go-test", vcs.CheckConclusionSkipped),
	}
	got := satisfiedTriggers(prStatus(vcs.PRStateOpen), checks, []string{"go-test"}, true)
	if got[models.GitHubCallbackTriggerChecksPassed] || got[models.GitHubCallbackTriggerChecksFailed] {
		t.Fatalf("skipped-only required check satisfied %v, want neither checks trigger", got)
	}
}
