package callback

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
)

func prStatus(state vcs.PRState) *vcs.PRStatus {
	return &vcs.PRStatus{State: state}
}

// TestEvaluatePR_TriggerMapping verifies that a single ungrouped active callback
// is triggered only when the authoritative PR/check state satisfies exactly its
// requested trigger — and, crucially, that a webhook-suggested event the
// authoritative state denies produces no false trigger.
func TestEvaluatePR_TriggerMapping(t *testing.T) {
	cases := []struct {
		name        string
		trigger     models.GithubCallbackTrigger
		status      *vcs.PRStatus
		checks      []vcs.CheckResult
		wantTrigger bool
	}{
		{
			name:        "merged pr, merged trigger",
			trigger:     models.GithubCallbackTriggerMerged,
			status:      prStatus(vcs.PRStateMerged),
			wantTrigger: true,
		},
		{
			name:        "closed unmerged pr, closed trigger",
			trigger:     models.GithubCallbackTriggerClosed,
			status:      prStatus(vcs.PRStateClosed),
			wantTrigger: true,
		},
		{
			// Authoritative state denies the merge: PR is still open even if a
			// merge signal was observed. No false trigger.
			name:        "open pr, merged trigger -> no false trigger",
			trigger:     models.GithubCallbackTriggerMerged,
			status:      prStatus(vcs.PRStateOpen),
			wantTrigger: false,
		},
		{
			// Merged is distinct from closed: a merged PR must not satisfy closed.
			name:        "merged pr, closed trigger -> no false trigger",
			trigger:     models.GithubCallbackTriggerClosed,
			status:      prStatus(vcs.PRStateMerged),
			wantTrigger: false,
		},
		{
			name:        "all checks passed, checks_passed trigger",
			trigger:     models.GithubCallbackTriggerChecksPassed,
			status:      prStatus(vcs.PRStateOpen),
			checks:      []vcs.CheckResult{completedCheck("build", vcs.CheckConclusionSuccess)},
			wantTrigger: true,
		},
		{
			name:        "a failing check, checks_failed trigger",
			trigger:     models.GithubCallbackTriggerChecksFailed,
			status:      prStatus(vcs.PRStateOpen),
			checks:      []vcs.CheckResult{completedCheck("build", vcs.CheckConclusionFailure)},
			wantTrigger: true,
		},
		{
			name:        "pending check, checks_passed trigger -> not yet",
			trigger:     models.GithubCallbackTriggerChecksPassed,
			status:      prStatus(vcs.PRStateOpen),
			checks:      []vcs.CheckResult{completedCheck("a", vcs.CheckConclusionSuccess), pendingCheck("b")},
			wantTrigger: false,
		},
		{
			name:        "pending check, checks_failed trigger -> not yet",
			trigger:     models.GithubCallbackTriggerChecksFailed,
			status:      prStatus(vcs.PRStateOpen),
			checks:      []vcs.CheckResult{pendingCheck("b")},
			wantTrigger: false,
		},
		{
			name:        "no checks, checks_passed trigger -> not satisfied",
			trigger:     models.GithubCallbackTriggerChecksPassed,
			status:      prStatus(vcs.PRStateOpen),
			checks:      nil,
			wantTrigger: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			cb := mustCreate(t, store, db.CreateGithubCallbackParams{
				TargetChatID: "chat-1",
				RepoOwner:    "acme",
				RepoName:     "widgets",
				PRNumber:     7,
				Trigger:      tc.trigger,
				Message:      "hello",
			})
			prov := &fakeProvider{status: tc.status, checks: tc.checks}
			ev := NewEvaluator(store, prov, fixedNow(), zerolog.Nop())

			if err := ev.EvaluatePR(context.Background(), "acme", "widgets", 7); err != nil {
				t.Fatalf("EvaluatePR: %v", err)
			}
			got := getState(t, store, cb.ID)
			if tc.wantTrigger && got != models.GithubCallbackStateTriggered {
				t.Errorf("state = %q, want triggered", got)
			}
			if !tc.wantTrigger && got != models.GithubCallbackStateActive {
				t.Errorf("state = %q, want active (no trigger)", got)
			}
		})
	}
}

// TestEvaluatePR_GroupSiblingsCanceled verifies that when one callback in a group
// fires, its still-active siblings are canceled and never delivered.
func TestEvaluatePR_GroupSiblingsCanceled(t *testing.T) {
	store := newStore(t)
	group := "release-grp"
	winner := mustCreate(t, store, db.CreateGithubCallbackParams{
		GroupID:      &group,
		TargetChatID: "chat-1",
		RepoOwner:    "acme",
		RepoName:     "widgets",
		PRNumber:     7,
		Trigger:      models.GithubCallbackTriggerMerged,
		Message:      "on merge",
	})
	sibling := mustCreate(t, store, db.CreateGithubCallbackParams{
		GroupID:      &group,
		TargetChatID: "chat-1",
		RepoOwner:    "acme",
		RepoName:     "widgets",
		PRNumber:     7,
		Trigger:      models.GithubCallbackTriggerClosed,
		Message:      "on close",
	})

	prov := &fakeProvider{status: prStatus(vcs.PRStateMerged)}
	ev := NewEvaluator(store, prov, fixedNow(), zerolog.Nop())
	if err := ev.EvaluatePR(context.Background(), "acme", "widgets", 7); err != nil {
		t.Fatalf("EvaluatePR: %v", err)
	}

	if got := getState(t, store, winner.ID); got != models.GithubCallbackStateTriggered {
		t.Errorf("winner state = %q, want triggered", got)
	}
	if got := getState(t, store, sibling.ID); got != models.GithubCallbackStateCanceled {
		t.Errorf("sibling state = %q, want canceled", got)
	}
}

// TestEvaluatePR_ProviderErrorHoldsNothingOpen verifies a provider failure is
// returned to the caller and leaves the callback active for a later retry.
func TestEvaluatePR_ProviderErrorHoldsNothingOpen(t *testing.T) {
	store := newStore(t)
	cb := mustCreate(t, store, db.CreateGithubCallbackParams{
		TargetChatID: "chat-1",
		RepoOwner:    "acme",
		RepoName:     "widgets",
		PRNumber:     7,
		Trigger:      models.GithubCallbackTriggerMerged,
		Message:      "hi",
	})
	prov := &fakeProvider{statusErr: errors.New("gh boom")}
	ev := NewEvaluator(store, prov, fixedNow(), zerolog.Nop())

	if err := ev.EvaluatePR(context.Background(), "acme", "widgets", 7); err == nil {
		t.Fatal("expected error from provider failure, got nil")
	}
	if got := getState(t, store, cb.ID); got != models.GithubCallbackStateActive {
		t.Errorf("state = %q, want active after provider error", got)
	}
}

// TestEvaluatePR_NoCallbacksSkipsProvider verifies the fast path: no matching
// callbacks means the provider is never queried.
func TestEvaluatePR_NoCallbacksSkipsProvider(t *testing.T) {
	store := newStore(t)
	prov := &countingProvider{}
	ev := NewEvaluator(store, prov, fixedNow(), zerolog.Nop())
	if err := ev.EvaluatePR(context.Background(), "acme", "widgets", 99); err != nil {
		t.Fatalf("EvaluatePR: %v", err)
	}
	if prov.statusCalls != 0 {
		t.Errorf("provider queried %d times, want 0", prov.statusCalls)
	}
}

// TestReconcileAll_TriggersEnduringStatesPerDistinctPR verifies ReconcileAll
// evaluates every distinct PR with an active callback and fires the satisfied
// ones.
func TestReconcileAll_TriggersEnduringStatesPerDistinctPR(t *testing.T) {
	store := newStore(t)
	a := mustCreate(t, store, db.CreateGithubCallbackParams{
		TargetChatID: "chat-1", RepoOwner: "acme", RepoName: "widgets", PRNumber: 1,
		Trigger: models.GithubCallbackTriggerMerged, Message: "a",
	})
	b := mustCreate(t, store, db.CreateGithubCallbackParams{
		TargetChatID: "chat-2", RepoOwner: "acme", RepoName: "gadgets", PRNumber: 2,
		Trigger: models.GithubCallbackTriggerMerged, Message: "b",
	})

	prov := &fakeProvider{status: prStatus(vcs.PRStateMerged)}
	ev := NewEvaluator(store, prov, fixedNow(), zerolog.Nop())
	if err := ev.ReconcileAll(context.Background()); err != nil {
		t.Fatalf("ReconcileAll: %v", err)
	}
	if got := getState(t, store, a.ID); got != models.GithubCallbackStateTriggered {
		t.Errorf("a state = %q, want triggered", got)
	}
	if got := getState(t, store, b.ID); got != models.GithubCallbackStateTriggered {
		t.Errorf("b state = %q, want triggered", got)
	}
}

// countingProvider counts GetPRStatus calls for fast-path assertions.
type countingProvider struct {
	statusCalls int
}

func (c *countingProvider) GetPRStatus(_ context.Context, _ string, _ int) (*vcs.PRStatus, error) {
	c.statusCalls++
	return prStatus(vcs.PRStateOpen), nil
}

func (c *countingProvider) GetCheckResults(_ context.Context, _ string, _ int) ([]vcs.CheckResult, error) {
	return nil, nil
}

// fixedNow returns a deterministic clock function.
func fixedNow() func() time.Time {
	t := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return t }
}
