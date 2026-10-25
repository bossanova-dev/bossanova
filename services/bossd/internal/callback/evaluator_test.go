package callback

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
)

// testHeadSHA is the head SHA every test PRStatus carries; checks_passed
// requires one, because it reads the head's workflow runs.
const testHeadSHA = "0123456789abcdef0123456789abcdef01234567"

func prStatus(state vcs.PRState) *vcs.PRStatus {
	return &vcs.PRStatus{State: state, HeadSHA: testHeadSHA}
}

// prStatusDraft builds a PRStatus with an explicit draft flag, for the
// draft-aware ready_for_review / checks_passed_ready triggers.
func prStatusDraft(state vcs.PRState, draft bool) *vcs.PRStatus {
	return &vcs.PRStatus{State: state, Draft: draft, HeadSHA: testHeadSHA}
}

// TestEvaluatePR_TriggerMapping verifies that a single ungrouped active callback
// is triggered only when the authoritative PR/check state satisfies exactly its
// requested trigger — and, crucially, that a webhook-suggested event the
// authoritative state denies produces no false trigger.
func TestEvaluatePR_TriggerMapping(t *testing.T) {
	cases := []struct {
		name        string
		trigger     models.GitHubCallbackTrigger
		status      *vcs.PRStatus
		checks      []vcs.CheckResult
		wantTrigger bool
	}{
		{
			name:        "merged pr, merged trigger",
			trigger:     models.GitHubCallbackTriggerMerged,
			status:      prStatus(vcs.PRStateMerged),
			wantTrigger: true,
		},
		{
			name:        "closed unmerged pr, closed trigger",
			trigger:     models.GitHubCallbackTriggerClosed,
			status:      prStatus(vcs.PRStateClosed),
			wantTrigger: true,
		},
		{
			// Authoritative state denies the merge: PR is still open even if a
			// merge signal was observed. No false trigger.
			name:        "open pr, merged trigger -> no false trigger",
			trigger:     models.GitHubCallbackTriggerMerged,
			status:      prStatus(vcs.PRStateOpen),
			wantTrigger: false,
		},
		{
			// Merged is distinct from closed: a merged PR must not satisfy closed.
			name:        "merged pr, closed trigger -> no false trigger",
			trigger:     models.GitHubCallbackTriggerClosed,
			status:      prStatus(vcs.PRStateMerged),
			wantTrigger: false,
		},
		{
			name:        "all checks passed, checks_passed trigger",
			trigger:     models.GitHubCallbackTriggerChecksPassed,
			status:      prStatus(vcs.PRStateOpen),
			checks:      []vcs.CheckResult{completedCheck("build", vcs.CheckConclusionSuccess)},
			wantTrigger: true,
		},
		{
			name:        "all checks skipped, checks_passed trigger still fires",
			trigger:     models.GitHubCallbackTriggerChecksPassed,
			status:      prStatus(vcs.PRStateOpen),
			checks:      []vcs.CheckResult{completedCheck("docs", vcs.CheckConclusionSkipped)},
			wantTrigger: true,
		},
		{
			name:    "completed nil conclusion, checks_passed trigger -> not satisfied",
			trigger: models.GitHubCallbackTriggerChecksPassed,
			status:  prStatus(vcs.PRStateOpen),
			checks: []vcs.CheckResult{
				{ID: "build", Name: "build", Status: vcs.CheckStatusCompleted},
			},
			wantTrigger: false,
		},
		{
			name:    "unclassified check, checks_passed trigger -> not satisfied",
			trigger: models.GitHubCallbackTriggerChecksPassed,
			status:  prStatus(vcs.PRStateOpen),
			checks: []vcs.CheckResult{
				{ID: "build", Name: "build", Status: vcs.CheckStatusCompleted, Unclassified: true},
			},
			wantTrigger: false,
		},
		{
			name:        "a failing check, checks_failed trigger",
			trigger:     models.GitHubCallbackTriggerChecksFailed,
			status:      prStatus(vcs.PRStateOpen),
			checks:      []vcs.CheckResult{completedCheck("build", vcs.CheckConclusionFailure)},
			wantTrigger: true,
		},
		{
			name:        "pending check, checks_passed trigger -> not yet",
			trigger:     models.GitHubCallbackTriggerChecksPassed,
			status:      prStatus(vcs.PRStateOpen),
			checks:      []vcs.CheckResult{completedCheck("a", vcs.CheckConclusionSuccess), pendingCheck("b")},
			wantTrigger: false,
		},
		{
			name:        "pending check, checks_failed trigger -> not yet",
			trigger:     models.GitHubCallbackTriggerChecksFailed,
			status:      prStatus(vcs.PRStateOpen),
			checks:      []vcs.CheckResult{pendingCheck("b")},
			wantTrigger: false,
		},
		{
			name:        "no checks, checks_passed trigger -> not satisfied",
			trigger:     models.GitHubCallbackTriggerChecksPassed,
			status:      prStatus(vcs.PRStateOpen),
			checks:      nil,
			wantTrigger: false,
		},
		{
			// Draft "commit CI" noise: checks_passed is trigger-blind to draft
			// status and still fires, but the new draft-aware triggers must not.
			name:        "draft pr, all-green checks, checks_passed trigger -> still fires",
			trigger:     models.GitHubCallbackTriggerChecksPassed,
			status:      prStatusDraft(vcs.PRStateOpen, true),
			checks:      []vcs.CheckResult{completedCheck("build", vcs.CheckConclusionSuccess)},
			wantTrigger: true,
		},
		{
			name:        "draft pr, all-green checks, checks_passed_ready trigger -> no false trigger",
			trigger:     models.GitHubCallbackTriggerChecksPassedReady,
			status:      prStatusDraft(vcs.PRStateOpen, true),
			checks:      []vcs.CheckResult{completedCheck("build", vcs.CheckConclusionSuccess)},
			wantTrigger: false,
		},
		{
			name:        "draft pr, all-green checks, ready_for_review trigger -> no false trigger",
			trigger:     models.GitHubCallbackTriggerReadyForReview,
			status:      prStatusDraft(vcs.PRStateOpen, true),
			checks:      []vcs.CheckResult{completedCheck("build", vcs.CheckConclusionSuccess)},
			wantTrigger: false,
		},
		{
			name:        "not-draft open pr, pending checks, ready_for_review trigger -> fires",
			trigger:     models.GitHubCallbackTriggerReadyForReview,
			status:      prStatusDraft(vcs.PRStateOpen, false),
			checks:      []vcs.CheckResult{pendingCheck("build")},
			wantTrigger: true,
		},
		{
			name:        "not-draft open pr, pending checks, checks_passed_ready trigger -> not yet",
			trigger:     models.GitHubCallbackTriggerChecksPassedReady,
			status:      prStatusDraft(vcs.PRStateOpen, false),
			checks:      []vcs.CheckResult{pendingCheck("build")},
			wantTrigger: false,
		},
		{
			name:        "not-draft open pr, all-green checks, ready_for_review trigger -> fires",
			trigger:     models.GitHubCallbackTriggerReadyForReview,
			status:      prStatusDraft(vcs.PRStateOpen, false),
			checks:      []vcs.CheckResult{completedCheck("build", vcs.CheckConclusionSuccess)},
			wantTrigger: true,
		},
		{
			name:        "not-draft open pr, all-green checks, checks_passed_ready trigger -> fires",
			trigger:     models.GitHubCallbackTriggerChecksPassedReady,
			status:      prStatusDraft(vcs.PRStateOpen, false),
			checks:      []vcs.CheckResult{completedCheck("build", vcs.CheckConclusionSuccess)},
			wantTrigger: true,
		},
		{
			// Closed/merged PRs report Draft == false, so draft-negation alone
			// cannot exclude them; state == open is required too.
			name:        "closed pr, all-green checks, ready_for_review trigger -> no false trigger",
			trigger:     models.GitHubCallbackTriggerReadyForReview,
			status:      prStatusDraft(vcs.PRStateClosed, false),
			checks:      []vcs.CheckResult{completedCheck("build", vcs.CheckConclusionSuccess)},
			wantTrigger: false,
		},
		{
			name:        "closed pr, all-green checks, checks_passed_ready trigger -> no false trigger",
			trigger:     models.GitHubCallbackTriggerChecksPassedReady,
			status:      prStatusDraft(vcs.PRStateClosed, false),
			checks:      []vcs.CheckResult{completedCheck("build", vcs.CheckConclusionSuccess)},
			wantTrigger: false,
		},
		{
			name:        "merged pr, all-green checks, ready_for_review trigger -> no false trigger",
			trigger:     models.GitHubCallbackTriggerReadyForReview,
			status:      prStatusDraft(vcs.PRStateMerged, false),
			checks:      []vcs.CheckResult{completedCheck("build", vcs.CheckConclusionSuccess)},
			wantTrigger: false,
		},
		{
			name:        "merged pr, all-green checks, checks_passed_ready trigger -> no false trigger",
			trigger:     models.GitHubCallbackTriggerChecksPassedReady,
			status:      prStatusDraft(vcs.PRStateMerged, false),
			checks:      []vcs.CheckResult{completedCheck("build", vcs.CheckConclusionSuccess)},
			wantTrigger: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			cb := mustCreate(t, store, db.CreateGitHubCallbackParams{
				TargetChatID: "chat-1",
				RepoOwner:    "acme",
				RepoName:     "widgets",
				PRNumber:     7,
				Trigger:      tc.trigger,
				Message:      "hello",
			})
			prov := &fakeProvider{status: tc.status, checks: tc.checks}
			ev := NewEvaluator(store, prov, fixedNow(), zerolog.Nop())
			head, ready, err := ev.ChecksPassedReady(context.Background(), "acme", "widgets", 7)
			if err != nil {
				t.Fatalf("ChecksPassedReady: %v", err)
			}
			wantReady := satisfiedTriggers(tc.status, tc.checks, true)[models.GitHubCallbackTriggerChecksPassedReady]
			if ready != wantReady || head != tc.status.HeadSHA {
				t.Fatalf("ChecksPassedReady = (%q, %v), want (%q, %v)", head, ready, tc.status.HeadSHA, wantReady)
			}

			if err := ev.EvaluatePR(context.Background(), "acme", "widgets", 7); err != nil {
				t.Fatalf("EvaluatePR: %v", err)
			}
			got := getState(t, store, cb.ID)
			if tc.wantTrigger && got != models.GitHubCallbackStateTriggered {
				t.Errorf("state = %q, want triggered", got)
			}
			// Not triggered means still waiting — or, on a merged PR whose
			// state can no longer satisfy the trigger, retired as canceled
			// (see TestEvaluatePR_CancelsCallbacksAMergedPRCanNoLongerSatisfy).
			// Either way it must never have fired.
			wantIdle := models.GitHubCallbackStateActive
			if unreachableAfterMerge(tc.status, vcs.EvaluateChecks("", tc.checks, nil).State, tc.trigger) {
				wantIdle = models.GitHubCallbackStateCanceled
			}
			if !tc.wantTrigger && got != wantIdle {
				t.Errorf("state = %q, want %q (no trigger)", got, wantIdle)
			}
		})
	}
}

// TestEvaluatePR_GroupSiblingsCanceled verifies that when one callback in a group
// fires, its still-active siblings are canceled and never delivered.
func TestEvaluatePR_GroupSiblingsCanceled(t *testing.T) {
	store := newStore(t)
	group := "release-grp"
	winner := mustCreate(t, store, db.CreateGitHubCallbackParams{
		GroupID:      &group,
		TargetChatID: "chat-1",
		RepoOwner:    "acme",
		RepoName:     "widgets",
		PRNumber:     7,
		Trigger:      models.GitHubCallbackTriggerMerged,
		Message:      "on merge",
	})
	sibling := mustCreate(t, store, db.CreateGitHubCallbackParams{
		GroupID:      &group,
		TargetChatID: "chat-1",
		RepoOwner:    "acme",
		RepoName:     "widgets",
		PRNumber:     7,
		Trigger:      models.GitHubCallbackTriggerClosed,
		Message:      "on close",
	})

	prov := &fakeProvider{status: prStatus(vcs.PRStateMerged)}
	ev := NewEvaluator(store, prov, fixedNow(), zerolog.Nop())
	if err := ev.EvaluatePR(context.Background(), "acme", "widgets", 7); err != nil {
		t.Fatalf("EvaluatePR: %v", err)
	}

	if got := getState(t, store, winner.ID); got != models.GitHubCallbackStateTriggered {
		t.Errorf("winner state = %q, want triggered", got)
	}
	if got := getState(t, store, sibling.ID); got != models.GitHubCallbackStateCanceled {
		t.Errorf("sibling state = %q, want canceled", got)
	}
}

func TestEvaluatePR_TransitionRequiredAlreadySatisfiedObservesBaselineBeforeTrigger(t *testing.T) {
	store := newStore(t)
	group := "transition-group"
	target := mustCreate(t, store, db.CreateGitHubCallbackParams{
		GroupID:                 &group,
		TargetChatID:            "chat-1",
		RepoOwner:               "acme",
		RepoName:                "widgets",
		PRNumber:                7,
		Trigger:                 models.GitHubCallbackTriggerChecksPassed,
		Message:                 "on green",
		ShouldRequireTransition: true,
	})
	sibling := mustCreate(t, store, db.CreateGitHubCallbackParams{
		GroupID:      &group,
		TargetChatID: "chat-1",
		RepoOwner:    "acme",
		RepoName:     "widgets",
		PRNumber:     7,
		Trigger:      models.GitHubCallbackTriggerChecksFailed,
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
	if afterFirst.State != models.GitHubCallbackStateActive {
		t.Fatalf("first state = %q, want active", afterFirst.State)
	}
	if afterFirst.TriggeredAt != nil {
		t.Fatalf("first triggered_at = %v, want nil", afterFirst.TriggeredAt)
	}
	if got := getState(t, store, sibling.ID); got != models.GitHubCallbackStateActive {
		t.Fatalf("sibling state after baseline = %q, want active", got)
	}

	prov.set(prStatus(vcs.PRStateOpen), []vcs.CheckResult{pendingCheck("build")})
	if err := ev.EvaluatePR(context.Background(), "acme", "widgets", 7); err != nil {
		t.Fatalf("second EvaluatePR: %v", err)
	}
	if got := getState(t, store, target.ID); got != models.GitHubCallbackStateActive {
		t.Fatalf("second state = %q, want active", got)
	}

	prov.set(prStatus(vcs.PRStateOpen), []vcs.CheckResult{completedCheck("build", vcs.CheckConclusionSuccess)})
	if err := ev.EvaluatePR(context.Background(), "acme", "widgets", 7); err != nil {
		t.Fatalf("third EvaluatePR: %v", err)
	}
	if got := getState(t, store, target.ID); got != models.GitHubCallbackStateTriggered {
		t.Fatalf("third state = %q, want triggered", got)
	}
	if got := getState(t, store, sibling.ID); got != models.GitHubCallbackStateCanceled {
		t.Fatalf("sibling state after trigger = %q, want canceled", got)
	}
}

// TestEvaluatePR_TransitionRequiredHeldSatisfiedNeverFires pins the
// madverts-core#6672 shape: a verify re-arm on a green PR that stays green woke
// its chat on the second poll although nothing transitioned.
func TestEvaluatePR_TransitionRequiredHeldSatisfiedNeverFires(t *testing.T) {
	store := newStore(t)
	cb := mustCreate(t, store, db.CreateGitHubCallbackParams{
		TargetChatID:            "chat-1",
		RepoOwner:               "acme",
		RepoName:                "widgets",
		PRNumber:                7,
		Trigger:                 models.GitHubCallbackTriggerChecksPassedReady,
		Message:                 "/boss-verify 7",
		ShouldRequireTransition: true,
	})

	prov := &scriptedProvider{}
	ev := NewEvaluator(store, prov, fixedNow(), zerolog.Nop())
	prov.set(prStatus(vcs.PRStateOpen), []vcs.CheckResult{completedCheck("build", vcs.CheckConclusionSuccess)})
	for i := range 3 {
		if err := ev.EvaluatePR(context.Background(), "acme", "widgets", 7); err != nil {
			t.Fatalf("EvaluatePR %d: %v", i, err)
		}
		if got := getState(t, store, cb.ID); got != models.GitHubCallbackStateActive {
			t.Fatalf("evaluation %d state = %q, want active while the PR stays green", i, got)
		}
	}

	prov.set(prStatus(vcs.PRStateOpen), []vcs.CheckResult{pendingCheck("build")})
	if err := ev.EvaluatePR(context.Background(), "acme", "widgets", 7); err != nil {
		t.Fatalf("pending EvaluatePR: %v", err)
	}
	prov.set(prStatus(vcs.PRStateOpen), []vcs.CheckResult{completedCheck("build", vcs.CheckConclusionSuccess)})
	if err := ev.EvaluatePR(context.Background(), "acme", "widgets", 7); err != nil {
		t.Fatalf("green again EvaluatePR: %v", err)
	}
	if got := getState(t, store, cb.ID); got != models.GitHubCallbackStateTriggered {
		t.Fatalf("state after red-to-green = %q, want triggered", got)
	}
}

func TestEvaluatePR_TransitionRequiredInitiallyUnsatisfiedFiresOnFirstSatisfiedObservation(t *testing.T) {
	store := newStore(t)
	cb := mustCreate(t, store, db.CreateGitHubCallbackParams{
		TargetChatID:            "chat-1",
		RepoOwner:               "acme",
		RepoName:                "widgets",
		PRNumber:                7,
		Trigger:                 models.GitHubCallbackTriggerChecksPassed,
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
	if afterFirst.State != models.GitHubCallbackStateActive || !afterFirst.HasObservedBaseline {
		t.Fatalf("first state/baseline = %q/%v, want active/true", afterFirst.State, afterFirst.HasObservedBaseline)
	}

	prov.set(prStatus(vcs.PRStateOpen), []vcs.CheckResult{completedCheck("build", vcs.CheckConclusionSuccess)})
	if err := ev.EvaluatePR(context.Background(), "acme", "widgets", 7); err != nil {
		t.Fatalf("second EvaluatePR: %v", err)
	}
	if got := getState(t, store, cb.ID); got != models.GitHubCallbackStateTriggered {
		t.Fatalf("second state = %q, want triggered", got)
	}
}

// TestEvaluatePR_ProviderErrorHoldsNothingOpen verifies a provider failure is
// returned to the caller and leaves the callback active for a later retry.
func TestEvaluatePR_ProviderErrorHoldsNothingOpen(t *testing.T) {
	store := newStore(t)
	cb := mustCreate(t, store, db.CreateGitHubCallbackParams{
		TargetChatID: "chat-1",
		RepoOwner:    "acme",
		RepoName:     "widgets",
		PRNumber:     7,
		Trigger:      models.GitHubCallbackTriggerMerged,
		Message:      "hi",
	})
	prov := &fakeProvider{statusErr: errors.New("gh boom")}
	ev := NewEvaluator(store, prov, fixedNow(), zerolog.Nop())

	if err := ev.EvaluatePR(context.Background(), "acme", "widgets", 7); err == nil {
		t.Fatal("expected error from provider failure, got nil")
	}
	if got := getState(t, store, cb.ID); got != models.GitHubCallbackStateActive {
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
	a := mustCreate(t, store, db.CreateGitHubCallbackParams{
		TargetChatID: "chat-1", RepoOwner: "acme", RepoName: "widgets", PRNumber: 1,
		Trigger: models.GitHubCallbackTriggerMerged, Message: "a",
	})
	b := mustCreate(t, store, db.CreateGitHubCallbackParams{
		TargetChatID: "chat-2", RepoOwner: "acme", RepoName: "gadgets", PRNumber: 2,
		Trigger: models.GitHubCallbackTriggerMerged, Message: "b",
	})

	prov := &fakeProvider{status: prStatus(vcs.PRStateMerged)}
	ev := NewEvaluator(store, prov, fixedNow(), zerolog.Nop())
	if err := ev.ReconcileAll(context.Background()); err != nil {
		t.Fatalf("ReconcileAll: %v", err)
	}
	if got := getState(t, store, a.ID); got != models.GitHubCallbackStateTriggered {
		t.Errorf("a state = %q, want triggered", got)
	}
	if got := getState(t, store, b.ID); got != models.GitHubCallbackStateTriggered {
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
	readyCB := mustCreate(t, store, db.CreateGitHubCallbackParams{
		TargetChatID: "chat-1", RepoOwner: "acme", RepoName: "widgets", PRNumber: 7,
		Trigger: models.GitHubCallbackTriggerReadyForReview, Message: "ready for review",
	})
	checksReadyCB := mustCreate(t, store, db.CreateGitHubCallbackParams{
		TargetChatID: "chat-2", RepoOwner: "acme", RepoName: "widgets", PRNumber: 7,
		Trigger: models.GitHubCallbackTriggerChecksPassedReady, Message: "checks passed and ready",
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

		if got := getState(t, store, readyCB.ID); got != models.GitHubCallbackStateActive {
			t.Errorf("round %d: ready_for_review state = %q, want active", round, got)
		}
		if got := getState(t, store, checksReadyCB.ID); got != models.GitHubCallbackStateActive {
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

	if got := getState(t, store, readyCB.ID); got != models.GitHubCallbackStateDelivered {
		t.Errorf("round 4: ready_for_review state = %q, want delivered", got)
	}
	if got := getState(t, store, checksReadyCB.ID); got != models.GitHubCallbackStateDelivered {
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

// TestSatisfiedTriggers_RunsSettledGatesChecksPassed verifies that a green
// check set satisfies checks_passed / checks_passed_ready only once every head
// workflow run has completed, and that the gate touches no other trigger.
func TestSatisfiedTriggers_RunsSettledGatesChecksPassed(t *testing.T) {
	green := []vcs.CheckResult{completedCheck("build", vcs.CheckConclusionSuccess)}
	red := []vcs.CheckResult{completedCheck("build", vcs.CheckConclusionFailure)}
	open := prStatusDraft(vcs.PRStateOpen, false)

	held := satisfiedTriggers(open, green, false)
	if held[models.GitHubCallbackTriggerChecksPassed] || held[models.GitHubCallbackTriggerChecksPassedReady] {
		t.Errorf("unsettled runs: checks_passed pair satisfied = %v, want neither", held)
	}
	if !held[models.GitHubCallbackTriggerReadyForReview] {
		t.Errorf("unsettled runs: ready_for_review must still be satisfied")
	}
	settled := satisfiedTriggers(open, green, true)
	if !settled[models.GitHubCallbackTriggerChecksPassed] || !settled[models.GitHubCallbackTriggerChecksPassedReady] {
		t.Errorf("settled runs: checks_passed pair = %v, want both", settled)
	}
	if !satisfiedTriggers(open, red, false)[models.GitHubCallbackTriggerChecksFailed] {
		t.Errorf("unsettled runs: checks_failed must still be satisfied by a completed failure")
	}
	if !satisfiedTriggers(prStatus(vcs.PRStateMerged), green, false)[models.GitHubCallbackTriggerMerged] {
		t.Errorf("unsettled runs: merged must still be satisfied")
	}
}

// TestSatisfiedTriggers_PendingVerifyDoesNotHoldOrdinaryCI pins BOS-1382: a
// pending boss/verify commit status (a live claim, an unclaimed head, or a park
// awaiting a human) is not ordinary CI, so it never holds checks_passed /
// checks_passed_ready once ordinary checks and workflow runs settled, while a
// completed boss/verify failure is an ordinary failed check.
func TestSatisfiedTriggers_PendingVerifyDoesNotHoldOrdinaryCI(t *testing.T) {
	open := prStatusDraft(vcs.PRStateOpen, false)
	for _, desc := range []string{"verifying… 3f2a", "waiting: checks-pending", "needs human: always-human-path", ""} {
		t.Run("pending "+desc, func(t *testing.T) {
			checks := []vcs.CheckResult{
				completedCheck("build", vcs.CheckConclusionSuccess),
				{ID: "/" + vcs.VerifyStatusContext, Name: vcs.VerifyStatusContext, Status: vcs.CheckStatusQueued, Description: desc},
			}
			got := satisfiedTriggers(open, checks, true)
			if !got[models.GitHubCallbackTriggerChecksPassed] || !got[models.GitHubCallbackTriggerChecksPassedReady] {
				t.Errorf("checks_passed pair = %v, want both satisfied beside a pending boss/verify", got)
			}
			if got[models.GitHubCallbackTriggerChecksFailed] {
				t.Errorf("checks_failed satisfied by a pending boss/verify")
			}
		})
	}
	t.Run("completed verify failure", func(t *testing.T) {
		checks := []vcs.CheckResult{
			completedCheck("build", vcs.CheckConclusionSuccess),
			{ID: "/" + vcs.VerifyStatusContext, Name: vcs.VerifyStatusContext, Status: vcs.CheckStatusCompleted, Conclusion: conclusion(vcs.CheckConclusionFailure), Description: "defect: tests red"},
		}
		got := satisfiedTriggers(open, checks, true)
		if !got[models.GitHubCallbackTriggerChecksFailed] {
			t.Errorf("checks_failed = false, want a completed boss/verify failure to satisfy it")
		}
		if got[models.GitHubCallbackTriggerChecksPassed] {
			t.Errorf("checks_passed satisfied beside a completed boss/verify failure")
		}
	})
}

// TestEvaluatePR_WorkflowRunsGateChecksPassed drives EvaluatePR end to end
// against the head workflow-run read.
func TestEvaluatePR_WorkflowRunsGateChecksPassed(t *testing.T) {
	green := []vcs.CheckResult{completedCheck("build", vcs.CheckConclusionSuccess)}
	inProgress := []vcs.WorkflowRun{
		{Name: "test", Status: "completed", Conclusion: "success", HeadSHA: testHeadSHA},
		{Name: "bazel", Status: "in_progress", HeadSHA: testHeadSHA},
	}
	completed := []vcs.WorkflowRun{
		{Name: "test", Status: "completed", Conclusion: "success", HeadSHA: testHeadSHA},
		// A run for another SHA never holds this head.
		{Name: "bazel", Status: "queued", HeadSHA: "ffffffffffffffffffffffffffffffffffffffff"},
	}
	cases := []struct {
		name     string
		status   *vcs.PRStatus
		runs     []vcs.WorkflowRun
		runsErr  error
		trigger  models.GitHubCallbackTrigger
		wantFire bool
	}{
		{"waiting run, checks_passed", prStatusDraft(vcs.PRStateOpen, false), []vcs.WorkflowRun{{Name: "deploy", Status: "waiting", Event: "pull_request", HeadSHA: testHeadSHA}}, nil, models.GitHubCallbackTriggerChecksPassed, false},
		{"action required, checks_passed", prStatusDraft(vcs.PRStateOpen, false), []vcs.WorkflowRun{{Name: "deploy", Status: "action_required", Event: "pull_request", HeadSHA: testHeadSHA}}, nil, models.GitHubCallbackTriggerChecksPassed, false},
		{"push run, checks_passed", prStatusDraft(vcs.PRStateOpen, false), []vcs.WorkflowRun{{Name: "deploy", Status: "queued", Event: "push", HeadSHA: testHeadSHA}}, nil, models.GitHubCallbackTriggerChecksPassed, false},
		{"schedule run, checks_passed", prStatusDraft(vcs.PRStateOpen, false), []vcs.WorkflowRun{{Name: "deploy", Status: "pending", Event: "schedule", HeadSHA: testHeadSHA}}, nil, models.GitHubCallbackTriggerChecksPassed, false},
		{"run in progress, checks_passed", prStatusDraft(vcs.PRStateOpen, false), inProgress, nil, models.GitHubCallbackTriggerChecksPassed, false},
		{"run in progress, checks_passed_ready", prStatusDraft(vcs.PRStateOpen, false), inProgress, nil, models.GitHubCallbackTriggerChecksPassedReady, false},
		{"run in progress, merged pr, merged", prStatus(vcs.PRStateMerged), inProgress, nil, models.GitHubCallbackTriggerMerged, true},
		{"all runs completed, checks_passed", prStatusDraft(vcs.PRStateOpen, false), completed, nil, models.GitHubCallbackTriggerChecksPassed, true},
		{"all runs completed, checks_passed_ready", prStatusDraft(vcs.PRStateOpen, false), completed, nil, models.GitHubCallbackTriggerChecksPassedReady, true},
		{"runs read error, checks_passed", prStatusDraft(vcs.PRStateOpen, false), nil, errors.New("gh api 502"), models.GitHubCallbackTriggerChecksPassed, false},
		{"runs read error, checks_passed_ready", prStatusDraft(vcs.PRStateOpen, false), nil, errors.New("gh api 502"), models.GitHubCallbackTriggerChecksPassedReady, false},
		{"runs read error, ready_for_review", prStatusDraft(vcs.PRStateOpen, false), nil, errors.New("gh api 502"), models.GitHubCallbackTriggerReadyForReview, true},
		{"runs read error, merged pr, merged", prStatus(vcs.PRStateMerged), nil, errors.New("gh api 502"), models.GitHubCallbackTriggerMerged, true},
		{"empty head sha, checks_passed", &vcs.PRStatus{State: vcs.PRStateOpen}, nil, nil, models.GitHubCallbackTriggerChecksPassed, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			cb := mustCreate(t, store, db.CreateGitHubCallbackParams{
				TargetChatID: "chat-1",
				RepoOwner:    "acme",
				RepoName:     "widgets",
				PRNumber:     7,
				Trigger:      tc.trigger,
				Message:      "hello",
			})
			prov := &fakeProvider{status: tc.status, checks: green, runs: tc.runs, runsErr: tc.runsErr}
			ev := NewEvaluator(store, prov, fixedNow(), zerolog.Nop())
			head, ready, err := ev.ChecksPassedReady(context.Background(), "acme", "widgets", 7)
			if err != nil {
				t.Fatalf("ChecksPassedReady: %v", err)
			}
			if head != tc.status.HeadSHA {
				t.Errorf("head = %q, want %q", head, tc.status.HeadSHA)
			}
			if tc.trigger == models.GitHubCallbackTriggerChecksPassedReady && ready != tc.wantFire {
				t.Errorf("ready = %v, want %v", ready, tc.wantFire)
			}
			if err := ev.EvaluatePR(context.Background(), "acme", "widgets", 7); err != nil {
				t.Fatalf("EvaluatePR: %v (a runs-read error must not fail the evaluation)", err)
			}
			got := getState(t, store, cb.ID)
			want := models.GitHubCallbackStateActive
			if tc.wantFire {
				want = models.GitHubCallbackStateTriggered
			}
			if got != want {
				t.Errorf("state = %q, want %q", got, want)
			}
			if tc.status.HeadSHA != "" && prov.runsSHA != tc.status.HeadSHA {
				t.Errorf("runs read for %q, want the head SHA %q", prov.runsSHA, tc.status.HeadSHA)
			}
		})
	}
}

// TestEvaluatePR_RunsNotReadWhenChecksNotGreen verifies the runs read is
// skipped when the attached set is not green, sparing the API quota.
func TestEvaluatePR_RunsNotReadWhenChecksNotGreen(t *testing.T) {
	store := newStore(t)
	mustCreate(t, store, db.CreateGitHubCallbackParams{
		TargetChatID: "chat-1",
		RepoOwner:    "acme",
		RepoName:     "widgets",
		PRNumber:     7,
		Trigger:      models.GitHubCallbackTriggerChecksPassed,
		Message:      "hello",
	})
	prov := &fakeProvider{status: prStatusDraft(vcs.PRStateOpen, false), checks: []vcs.CheckResult{pendingCheck("build")}}
	ev := NewEvaluator(store, prov, fixedNow(), zerolog.Nop())
	if err := ev.EvaluatePR(context.Background(), "acme", "widgets", 7); err != nil {
		t.Fatalf("EvaluatePR: %v", err)
	}
	if prov.runsCalls != 0 {
		t.Errorf("runs read %d times for a pending set, want 0", prov.runsCalls)
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

func (c *countingProvider) ListWorkflowRuns(_ context.Context, _, _ string) ([]vcs.WorkflowRun, error) {
	return nil, nil
}

// fixedNow returns a deterministic clock function.
func fixedNow() func() time.Time {
	t := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return t }
}

// TestEvaluatePR_CancelsCallbacksAMergedPRCanNoLongerSatisfy pins the cleanup
// that keeps a merged PR from being polled for a day: boss-build arms
// checks_passed and checks_failed in separate groups, and once the PR merges
// green the checks_failed half can never fire. Left active it was re-read from
// GitHub on every reconcile until its 24h expiry.
func TestEvaluatePR_CancelsCallbacksAMergedPRCanNoLongerSatisfy(t *testing.T) {
	cases := []struct {
		name       string
		trigger    models.GitHubCallbackTrigger
		checks     []vcs.CheckResult
		wantCancel bool
	}{
		{"checks_failed after a green merge", models.GitHubCallbackTriggerChecksFailed,
			[]vcs.CheckResult{completedCheck("build", vcs.CheckConclusionSuccess)}, true},
		{"checks_passed after a red merge", models.GitHubCallbackTriggerChecksPassed,
			[]vcs.CheckResult{completedCheck("build", vcs.CheckConclusionFailure)}, true},
		{"ready_for_review on a merged PR", models.GitHubCallbackTriggerReadyForReview, nil, true},
		{"checks_passed_ready on a merged PR", models.GitHubCallbackTriggerChecksPassedReady, nil, true},
		{"closed on a merged PR", models.GitHubCallbackTriggerClosed, nil, true},
		// Checks still running can finish after the merge, either way.
		{"checks_failed while checks are pending", models.GitHubCallbackTriggerChecksFailed,
			[]vcs.CheckResult{pendingCheck("build")}, false},
		{"checks_passed while checks are pending", models.GitHubCallbackTriggerChecksPassed,
			[]vcs.CheckResult{pendingCheck("build")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			cb := mustCreate(t, store, db.CreateGitHubCallbackParams{
				TargetChatID: "chat-1", RepoOwner: "acme", RepoName: "widgets", PRNumber: 7,
				Trigger: tc.trigger, Message: "wait",
			})
			prov := &fakeProvider{status: prStatus(vcs.PRStateMerged), checks: tc.checks}
			ev := NewEvaluator(store, prov, fixedNow(), zerolog.Nop())
			if err := ev.EvaluatePR(context.Background(), "acme", "widgets", 7); err != nil {
				t.Fatalf("EvaluatePR: %v", err)
			}
			want := models.GitHubCallbackStateActive
			if tc.wantCancel {
				want = models.GitHubCallbackStateCanceled
			}
			if got := getState(t, store, cb.ID); got != want {
				t.Fatalf("state = %q, want %q", got, want)
			}
		})
	}
}

// TestEvaluatePR_KeepsCallbacksOnAClosedUnmergedPR pins the other side: a
// closed PR can be reopened, so its unsatisfied callbacks are left to expire.
func TestEvaluatePR_KeepsCallbacksOnAClosedUnmergedPR(t *testing.T) {
	store := newStore(t)
	cb := mustCreate(t, store, db.CreateGitHubCallbackParams{
		TargetChatID: "chat-1", RepoOwner: "acme", RepoName: "widgets", PRNumber: 7,
		Trigger: models.GitHubCallbackTriggerReadyForReview, Message: "wait",
	})
	prov := &fakeProvider{status: prStatus(vcs.PRStateClosed)}
	ev := NewEvaluator(store, prov, fixedNow(), zerolog.Nop())
	if err := ev.EvaluatePR(context.Background(), "acme", "widgets", 7); err != nil {
		t.Fatalf("EvaluatePR: %v", err)
	}
	if got := getState(t, store, cb.ID); got != models.GitHubCallbackStateActive {
		t.Fatalf("state = %q, want active", got)
	}
}

type stubWebhookHealth map[string]bool

func (s stubWebhookHealth) WebhookDeliveryHealthy(repo string) bool { return s[repo] }

// TestReconcileAll_SafetyNetForWebhookHealthyRepos pins that the periodic
// reconcile stops re-reading PRs whose repo's webhooks reach the daemon (each
// webhook already runs EvaluatePR) except once per safety-net interval, while
// a repo without that evidence is re-read every pass.
func TestReconcileAll_SafetyNetForWebhookHealthyRepos(t *testing.T) {
	for _, tc := range []struct {
		name    string
		healthy bool
		want    int
	}{
		{"webhook-healthy repo", true, 2},
		{"repo without webhook evidence", false, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			mustCreate(t, store, db.CreateGitHubCallbackParams{
				TargetChatID: "chat-1", RepoOwner: "acme", RepoName: "widgets", PRNumber: 7,
				Trigger: models.GitHubCallbackTriggerMerged, Message: "wait",
			})
			now := time.Now()
			prov := &fakeProvider{status: prStatus(vcs.PRStateOpen)}
			ev := NewEvaluator(store, prov, func() time.Time { return now }, zerolog.Nop())
			ev.SetWebhookHealth(stubWebhookHealth{"acme/widgets": tc.healthy})

			// Passes at t=0, 5m, 10m, 15m: the healthy repo is due at 0 and 10m.
			for i := 0; i < 4; i++ {
				if err := ev.ReconcileAll(context.Background()); err != nil {
					t.Fatalf("ReconcileAll: %v", err)
				}
				now = now.Add(5 * time.Minute)
			}
			if prov.statusCalls != tc.want {
				t.Fatalf("PR read %d times over four passes, want %d", prov.statusCalls, tc.want)
			}
		})
	}
}

func TestRunsSettled_HoldLogs(t *testing.T) {
	for _, tc := range []struct {
		status string
		level  string
		class  string
	}{
		{"waiting", "warn", "awaiting-approval"},
		{"ACTION_REQUIRED", "warn", "awaiting-approval"},
		{"in_progress", "debug", "in-flight"},
		{"queued", "debug", "in-flight"},
	} {
		t.Run(tc.status, func(t *testing.T) {
			var buf bytes.Buffer
			prov := &fakeProvider{runs: []vcs.WorkflowRun{{Name: "deploy", Status: tc.status, Event: "push", HeadSHA: testHeadSHA}}}
			ev := NewEvaluator(nil, prov, fixedNow(), zerolog.New(&buf))
			if settled, _ := ev.runsSettled(context.Background(), "acme/widgets", 7, prStatus(vcs.PRStateOpen)); settled {
				t.Fatal("held run must not settle")
			}
			var record struct {
				Level   string `json:"level"`
				Repo    string `json:"repo"`
				PR      int    `json:"pr"`
				HeadSHA string `json:"head_sha"`
				Runs    []struct {
					Name   string `json:"name"`
					Status string `json:"status"`
					Class  string `json:"class"`
					Event  string `json:"event"`
				} `json:"held_runs"`
			}
			dec := json.NewDecoder(&buf)
			if err := dec.Decode(&record); err != nil {
				t.Fatalf("decode hold log: %v", err)
			}
			if record.Level != tc.level || record.Repo != "acme/widgets" || record.PR != 7 || record.HeadSHA != testHeadSHA {
				t.Fatalf("hold log context = %+v", record)
			}
			if len(record.Runs) != 1 || record.Runs[0].Name != "deploy" || record.Runs[0].Status != tc.status || record.Runs[0].Class != tc.class || record.Runs[0].Event != "push" {
				t.Fatalf("held runs = %+v", record.Runs)
			}
			var extra any
			if err := dec.Decode(&extra); err != io.EOF {
				t.Fatalf("expected exactly one hold log, got %v (%v)", extra, err)
			}
		})
	}
}

func TestRunsSettled_ClassifiesAndSortsHeldRuns(t *testing.T) {
	prov := &fakeProvider{runs: []vcs.WorkflowRun{
		{Name: "z-deploy", Status: "WAITING", Event: "schedule", HeadSHA: testHeadSHA},
		{Name: "done", Status: "COMPLETED", HeadSHA: testHeadSHA},
		{Name: "other-head", Status: "waiting", HeadSHA: "other"},
		{Name: "a-build", Status: "queued", Event: "push", HeadSHA: testHeadSHA},
		{Name: "approval", Status: "action_required", Event: "pull_request", HeadSHA: testHeadSHA},
		{Name: "unknown", Status: "unexpected", HeadSHA: testHeadSHA},
	}}
	var buf bytes.Buffer
	ev := NewEvaluator(nil, prov, fixedNow(), zerolog.New(&buf))
	settled, held := ev.runsSettled(context.Background(), "acme/widgets", 7, prStatus(vcs.PRStateOpen))
	want := []heldWorkflowRun{
		{Name: "a-build", Status: "queued", Event: "push", Class: "in-flight"},
		{Name: "approval", Status: "action_required", Event: "pull_request", Class: "awaiting-approval"},
		{Name: "unknown", Status: "unexpected", Class: "in-flight"},
		{Name: "z-deploy", Status: "WAITING", Event: "schedule", Class: "awaiting-approval"},
	}
	if settled || !reflect.DeepEqual(held, want) {
		t.Fatalf("settled=%v held=%+v, want %+v", settled, held, want)
	}
	var record struct {
		Level string            `json:"level"`
		Runs  []heldWorkflowRun `json:"held_runs"`
	}
	if err := json.NewDecoder(&buf).Decode(&record); err != nil {
		t.Fatal(err)
	}
	if record.Level != "warn" || !reflect.DeepEqual(record.Runs, want) {
		t.Fatalf("mixed hold log = %+v", record)
	}
}

// cachedStateProvider checks that every authoritative read can use the shared
// dispatch cache, including the workflow run read needed to settle green CI.
type cachedStateProvider struct {
	t     *testing.T
	reads int
}

func (p *cachedStateProvider) read(ctx context.Context, repo string) {
	p.t.Helper()
	if !vcs.CachedReadsAllowed(ctx) || repo != "acme/widgets" {
		p.t.Fatalf("read: cached=%v repo=%q", vcs.CachedReadsAllowed(ctx), repo)
	}
	p.reads++
}

func (p *cachedStateProvider) GetPRStatus(ctx context.Context, repo string, _ int) (*vcs.PRStatus, error) {
	p.read(ctx, repo)
	return prStatus(vcs.PRStateOpen), nil
}

func (p *cachedStateProvider) GetCheckResults(ctx context.Context, repo string, _ int) ([]vcs.CheckResult, error) {
	p.read(ctx, repo)
	return []vcs.CheckResult{completedCheck("build", vcs.CheckConclusionSuccess)}, nil
}

func (p *cachedStateProvider) ListWorkflowRuns(ctx context.Context, repo, head string) ([]vcs.WorkflowRun, error) {
	p.read(ctx, repo)
	if head != testHeadSHA {
		p.t.Fatalf("workflow head = %q, want %q", head, testHeadSHA)
	}
	return nil, nil
}

func TestChecksPassedReady_CachesAllReadsWithoutCallbacks(t *testing.T) {
	p := &cachedStateProvider{t: t}
	e := NewEvaluator(nil, p, fixedNow(), zerolog.Nop())
	head, ready, err := e.ChecksPassedReady(context.Background(), "acme", "widgets", 7)
	if err != nil || !ready || head != testHeadSHA || p.reads != 3 {
		t.Fatalf("ChecksPassedReady = (%q, %v, %v), reads=%d", head, ready, err, p.reads)
	}
}

func TestChecksPassedReady_ReadErrors(t *testing.T) {
	failure := errors.New("GitHub unavailable")
	for _, tc := range []struct {
		name     string
		provider *fakeProvider
	}{
		{"status", &fakeProvider{statusErr: failure}},
		{"checks", &fakeProvider{status: prStatus(vcs.PRStateOpen), checksErr: failure}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := NewEvaluator(nil, tc.provider, fixedNow(), zerolog.Nop())
			head, ready, err := e.ChecksPassedReady(context.Background(), "acme", "widgets", 7)
			if !errors.Is(err, failure) || ready || head != "" {
				t.Fatalf("ChecksPassedReady = (%q, %v, %v), want unreadable state", head, ready, err)
			}
		})
	}
}
