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

// prStatusDraft builds a PRStatus with an explicit draft flag, for the
// draft-aware ready_for_review / checks_passed_ready triggers.
func prStatusDraft(state vcs.PRState, draft bool) *vcs.PRStatus {
	return &vcs.PRStatus{State: state, Draft: draft}
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
			name:        "all checks skipped, checks_passed trigger still fires",
			trigger:     models.GithubCallbackTriggerChecksPassed,
			status:      prStatus(vcs.PRStateOpen),
			checks:      []vcs.CheckResult{completedCheck("docs", vcs.CheckConclusionSkipped)},
			wantTrigger: true,
		},
		{
			name:    "completed nil conclusion, checks_passed trigger -> not satisfied",
			trigger: models.GithubCallbackTriggerChecksPassed,
			status:  prStatus(vcs.PRStateOpen),
			checks: []vcs.CheckResult{
				{ID: "build", Name: "build", Status: vcs.CheckStatusCompleted},
			},
			wantTrigger: false,
		},
		{
			name:    "unclassified check, checks_passed trigger -> not satisfied",
			trigger: models.GithubCallbackTriggerChecksPassed,
			status:  prStatus(vcs.PRStateOpen),
			checks: []vcs.CheckResult{
				{ID: "build", Name: "build", Status: vcs.CheckStatusCompleted, Unclassified: true},
			},
			wantTrigger: false,
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
		{
			// Draft "commit CI" noise: checks_passed is trigger-blind to draft
			// status and still fires, but the new draft-aware triggers must not.
			name:        "draft pr, all-green checks, checks_passed trigger -> still fires",
			trigger:     models.GithubCallbackTriggerChecksPassed,
			status:      prStatusDraft(vcs.PRStateOpen, true),
			checks:      []vcs.CheckResult{completedCheck("build", vcs.CheckConclusionSuccess)},
			wantTrigger: true,
		},
		{
			name:        "draft pr, all-green checks, checks_passed_ready trigger -> no false trigger",
			trigger:     models.GithubCallbackTriggerChecksPassedReady,
			status:      prStatusDraft(vcs.PRStateOpen, true),
			checks:      []vcs.CheckResult{completedCheck("build", vcs.CheckConclusionSuccess)},
			wantTrigger: false,
		},
		{
			name:        "draft pr, all-green checks, ready_for_review trigger -> no false trigger",
			trigger:     models.GithubCallbackTriggerReadyForReview,
			status:      prStatusDraft(vcs.PRStateOpen, true),
			checks:      []vcs.CheckResult{completedCheck("build", vcs.CheckConclusionSuccess)},
			wantTrigger: false,
		},
		{
			name:        "not-draft open pr, pending checks, ready_for_review trigger -> fires",
			trigger:     models.GithubCallbackTriggerReadyForReview,
			status:      prStatusDraft(vcs.PRStateOpen, false),
			checks:      []vcs.CheckResult{pendingCheck("build")},
			wantTrigger: true,
		},
		{
			name:        "not-draft open pr, pending checks, checks_passed_ready trigger -> not yet",
			trigger:     models.GithubCallbackTriggerChecksPassedReady,
			status:      prStatusDraft(vcs.PRStateOpen, false),
			checks:      []vcs.CheckResult{pendingCheck("build")},
			wantTrigger: false,
		},
		{
			name:        "not-draft open pr, all-green checks, ready_for_review trigger -> fires",
			trigger:     models.GithubCallbackTriggerReadyForReview,
			status:      prStatusDraft(vcs.PRStateOpen, false),
			checks:      []vcs.CheckResult{completedCheck("build", vcs.CheckConclusionSuccess)},
			wantTrigger: true,
		},
		{
			name:        "not-draft open pr, all-green checks, checks_passed_ready trigger -> fires",
			trigger:     models.GithubCallbackTriggerChecksPassedReady,
			status:      prStatusDraft(vcs.PRStateOpen, false),
			checks:      []vcs.CheckResult{completedCheck("build", vcs.CheckConclusionSuccess)},
			wantTrigger: true,
		},
		{
			// Closed/merged PRs report Draft == false, so draft-negation alone
			// cannot exclude them; state == open is required too.
			name:        "closed pr, all-green checks, ready_for_review trigger -> no false trigger",
			trigger:     models.GithubCallbackTriggerReadyForReview,
			status:      prStatusDraft(vcs.PRStateClosed, false),
			checks:      []vcs.CheckResult{completedCheck("build", vcs.CheckConclusionSuccess)},
			wantTrigger: false,
		},
		{
			name:        "closed pr, all-green checks, checks_passed_ready trigger -> no false trigger",
			trigger:     models.GithubCallbackTriggerChecksPassedReady,
			status:      prStatusDraft(vcs.PRStateClosed, false),
			checks:      []vcs.CheckResult{completedCheck("build", vcs.CheckConclusionSuccess)},
			wantTrigger: false,
		},
		{
			name:        "merged pr, all-green checks, ready_for_review trigger -> no false trigger",
			trigger:     models.GithubCallbackTriggerReadyForReview,
			status:      prStatusDraft(vcs.PRStateMerged, false),
			checks:      []vcs.CheckResult{completedCheck("build", vcs.CheckConclusionSuccess)},
			wantTrigger: false,
		},
		{
			name:        "merged pr, all-green checks, checks_passed_ready trigger -> no false trigger",
			trigger:     models.GithubCallbackTriggerChecksPassedReady,
			status:      prStatusDraft(vcs.PRStateMerged, false),
			checks:      []vcs.CheckResult{completedCheck("build", vcs.CheckConclusionSuccess)},
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

func TestEvaluatePR_TransitionRequiredAlreadySatisfiedObservesBaselineBeforeTrigger(t *testing.T) {
	store := newStore(t)
	group := "transition-group"
	target := mustCreate(t, store, db.CreateGithubCallbackParams{
		GroupID:                 &group,
		TargetChatID:            "chat-1",
		RepoOwner:               "acme",
		RepoName:                "widgets",
		PRNumber:                7,
		Trigger:                 models.GithubCallbackTriggerChecksPassed,
		Message:                 "on green",
		ShouldRequireTransition: true,
	})
	sibling := mustCreate(t, store, db.CreateGithubCallbackParams{
		GroupID:      &group,
		TargetChatID: "chat-1",
		RepoOwner:    "acme",
		RepoName:     "widgets",
		PRNumber:     7,
		Trigger:      models.GithubCallbackTriggerChecksFailed,
		Message:      "on red",
	})

	prov := &scriptedProvider{}
	ev := NewEvaluator(store, prov, fixedNow(), zerolog.Nop())

	prov.set(prStatus(vcs.PRStateOpen), []vcs.CheckResult{completedCheck("build", vcs.CheckConclusionSuccess)})
	if err := ev.EvaluatePR(context.Background(), "acme", "widgets", 7); err != nil {
		t.Fatalf("first EvaluatePR: %v", err)
	}
	afterFirst, err := store.Get(context.Background(), target.ID)
	if err != nil {
		t.Fatalf("get target: %v", err)
	}
	if afterFirst.State != models.GithubCallbackStateActive {
		t.Fatalf("first state = %q, want active", afterFirst.State)
	}
	if !afterFirst.HasObservedBaseline {
		t.Fatal("first evaluation should mark has_observed_baseline")
	}
	if afterFirst.TriggeredAt != nil {
		t.Fatalf("first triggered_at = %v, want nil", afterFirst.TriggeredAt)
	}
	if got := getState(t, store, sibling.ID); got != models.GithubCallbackStateActive {
		t.Fatalf("sibling state after baseline = %q, want active", got)
	}

	prov.set(prStatus(vcs.PRStateOpen), []vcs.CheckResult{pendingCheck("build")})
	if err := ev.EvaluatePR(context.Background(), "acme", "widgets", 7); err != nil {
		t.Fatalf("second EvaluatePR: %v", err)
	}
	if got := getState(t, store, target.ID); got != models.GithubCallbackStateActive {
		t.Fatalf("second state = %q, want active", got)
	}

	prov.set(prStatus(vcs.PRStateOpen), []vcs.CheckResult{completedCheck("build", vcs.CheckConclusionSuccess)})
	if err := ev.EvaluatePR(context.Background(), "acme", "widgets", 7); err != nil {
		t.Fatalf("third EvaluatePR: %v", err)
	}
	if got := getState(t, store, target.ID); got != models.GithubCallbackStateTriggered {
		t.Fatalf("third state = %q, want triggered", got)
	}
	if got := getState(t, store, sibling.ID); got != models.GithubCallbackStateCanceled {
		t.Fatalf("sibling state after trigger = %q, want canceled", got)
	}
}

func TestEvaluatePR_TransitionRequiredInitiallyUnsatisfiedFiresOnFirstSatisfiedObservation(t *testing.T) {
	store := newStore(t)
	cb := mustCreate(t, store, db.CreateGithubCallbackParams{
		TargetChatID:            "chat-1",
		RepoOwner:               "acme",
		RepoName:                "widgets",
		PRNumber:                7,
		Trigger:                 models.GithubCallbackTriggerChecksPassed,
		Message:                 "on green",
		ShouldRequireTransition: true,
	})

	prov := &scriptedProvider{}
	ev := NewEvaluator(store, prov, fixedNow(), zerolog.Nop())

	prov.set(prStatus(vcs.PRStateOpen), []vcs.CheckResult{pendingCheck("build")})
	if err := ev.EvaluatePR(context.Background(), "acme", "widgets", 7); err != nil {
		t.Fatalf("first EvaluatePR: %v", err)
	}
	afterFirst, err := store.Get(context.Background(), cb.ID)
	if err != nil {
		t.Fatalf("get callback: %v", err)
	}
	if afterFirst.State != models.GithubCallbackStateActive || !afterFirst.HasObservedBaseline {
		t.Fatalf("first state/baseline = %q/%v, want active/true", afterFirst.State, afterFirst.HasObservedBaseline)
	}

	prov.set(prStatus(vcs.PRStateOpen), []vcs.CheckResult{completedCheck("build", vcs.CheckConclusionSuccess)})
	if err := ev.EvaluatePR(context.Background(), "acme", "widgets", 7); err != nil {
		t.Fatalf("second EvaluatePR: %v", err)
	}
	if got := getState(t, store, cb.ID); got != models.GithubCallbackStateTriggered {
		t.Fatalf("second state = %q, want triggered", got)
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

// TestFlow_DraftNoiseSuppressedThenSingleUnDraftFire proves the headline
// draft-aware acceptance criteria end-to-end through evaluator -> store ->
// DeliveryWorker: repeated "draft commit CI" rounds (draft PR, all checks
// green) must not fire either new trigger, and only the `gh pr ready` flip
// (Draft -> false, still open) fires each callback exactly once — with no
// re-fire on a subsequent no-op round.
func TestFlow_DraftNoiseSuppressedThenSingleUnDraftFire(t *testing.T) {
	store := newStore(t)
	clk := newClock(time.Now())

	// Distinct groups (none, here) so the store's group-sibling cancellation
	// does not cancel one callback when the other fires.
	readyCB := mustCreate(t, store, db.CreateGithubCallbackParams{
		TargetChatID: "chat-1", RepoOwner: "acme", RepoName: "widgets", PRNumber: 7,
		Trigger: models.GithubCallbackTriggerReadyForReview, Message: "ready for review",
	})
	checksReadyCB := mustCreate(t, store, db.CreateGithubCallbackParams{
		TargetChatID: "chat-2", RepoOwner: "acme", RepoName: "widgets", PRNumber: 7,
		Trigger: models.GithubCallbackTriggerChecksPassedReady, Message: "checks passed and ready",
	})

	greenChecks := []vcs.CheckResult{completedCheck("build", vcs.CheckConclusionSuccess)}
	prov := &scriptedProvider{}
	ev := NewEvaluator(store, prov, clk.now, zerolog.Nop())
	deliverer := newCaptureDeliverer(nil)
	w := newWorker(store, deliverer, clk.now, "worker-1")

	// Rounds 1-3: draft PR, all checks green -- the "draft commit CI" noise.
	// Neither new trigger should ever fire while the PR remains a draft.
	prov.set(prStatusDraft(vcs.PRStateOpen, true), greenChecks)
	for round := 1; round <= 3; round++ {
		if err := ev.EvaluatePR(context.Background(), "acme", "widgets", 7); err != nil {
			t.Fatalf("round %d: EvaluatePR: %v", round, err)
		}
		w.scan(context.Background())

		if got := getState(t, store, readyCB.ID); got != models.GithubCallbackStateActive {
			t.Errorf("round %d: ready_for_review state = %q, want active", round, got)
		}
		if got := getState(t, store, checksReadyCB.ID); got != models.GithubCallbackStateActive {
			t.Errorf("round %d: checks_passed_ready state = %q, want active", round, got)
		}
		if n := deliverer.count(); n != 0 {
			t.Errorf("round %d: deliverer count = %d, want 0", round, n)
		}
	}

	// Round 4: the `gh pr ready` flip -- Draft -> false, still open, checks
	// still green. Both callbacks should fire exactly once.
	prov.set(prStatusDraft(vcs.PRStateOpen, false), greenChecks)
	if err := ev.EvaluatePR(context.Background(), "acme", "widgets", 7); err != nil {
		t.Fatalf("round 4: EvaluatePR: %v", err)
	}
	w.scan(context.Background())

	if got := getState(t, store, readyCB.ID); got != models.GithubCallbackStateDelivered {
		t.Errorf("round 4: ready_for_review state = %q, want delivered", got)
	}
	if got := getState(t, store, checksReadyCB.ID); got != models.GithubCallbackStateDelivered {
		t.Errorf("round 4: checks_passed_ready state = %q, want delivered", got)
	}
	if n := deliverer.count(); n != 2 {
		t.Fatalf("round 4: deliverer count = %d, want exactly 2", n)
	}

	// Round 5: nothing changed. One-shot semantics -- no re-fire.
	if err := ev.EvaluatePR(context.Background(), "acme", "widgets", 7); err != nil {
		t.Fatalf("round 5: EvaluatePR: %v", err)
	}
	w.scan(context.Background())
	if n := deliverer.count(); n != 2 {
		t.Errorf("round 5: deliverer count = %d, want still exactly 2 (no re-fire)", n)
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
