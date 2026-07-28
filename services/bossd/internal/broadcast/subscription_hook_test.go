package broadcast

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	bcast "github.com/recurser/bossalib/broadcast"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
)

// This file drives the WHOLE standing-subscription chain against real stores:
// a session state write goes through db.RecomputingSessionStore (the single
// transition seam), which notifies the SubscriptionEvaluator, which wins the
// store's CAS and fires a broadcast through the same Sender the SendBroadcast
// RPC uses. The unit tests either side of it prove each link; this proves they
// are actually connected, and that the daemon's one hook is where the wiring
// says it is.

// hookHarness bundles the real stores a wired transition needs.
type hookHarness struct {
	sessions      *db.RecomputingSessionStore
	rawSessions   *db.SQLiteSessionStore
	subscriptions *db.SQLiteBroadcastSubscriptionStore
	broadcasts    *db.SQLiteBroadcastStore
	audience      *fakeAudience
	sessionID     string
}

// noopRecomputer stands in for the display-status computer, which is irrelevant
// here and would drag half the daemon in.
type noopRecomputer struct{}

func (noopRecomputer) Recompute(_ context.Context, _ string) error { return nil }

// newHookHarness migrates a database, seeds one session parked in startState,
// and returns it wired exactly as cmd/main.go wires production: the evaluator
// attached to the session store as its transition observer.
func newHookHarness(t *testing.T, startState machine.State) *hookHarness {
	t.Helper()
	ctx := context.Background()
	sqlDB := broadcastContractDB(t)

	repo, err := db.NewRepoStore(sqlDB).Create(ctx, db.CreateRepoParams{
		DisplayName:       "repo",
		LocalPath:         "/tmp/subscription-hook",
		OriginURL:         "https://github.com/test/repo.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	rawSessions := db.NewSessionStore(sqlDB)
	sess, err := rawSessions.Create(ctx, db.CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "child",
		WorktreePath: "/tmp/subscription-hook-wt",
		BranchName:   "br",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	start := int(startState)
	if _, err := rawSessions.Update(ctx, sess.ID, db.UpdateSessionParams{State: &start}); err != nil {
		t.Fatalf("park session: %v", err)
	}

	subscriptions := db.NewBroadcastSubscriptionStore(sqlDB)
	broadcasts := db.NewBroadcastStore(sqlDB)
	audience := &fakeAudience{targets: []bcast.Target{{ChatID: "chat-coordinator"}}}
	evaluator := NewSubscriptionEvaluator(
		subscriptions,
		NewSender(broadcasts, audience, zerolog.Nop()),
		nil,
		zerolog.Nop(),
	).WithSessionStates(rawSessions)

	return &hookHarness{
		sessions:      db.NewRecomputingSessionStore(rawSessions, noopRecomputer{}).WithTransitionObserver(evaluator),
		rawSessions:   rawSessions,
		subscriptions: subscriptions,
		broadcasts:    broadcasts,
		audience:      audience,
		sessionID:     sess.ID,
	}
}

// evaluator rebuilds an evaluator over the harness's stores, for the sweep tests
// that call ReconcileAll directly rather than through a transition.
func (h *hookHarness) evaluator(t *testing.T) *SubscriptionEvaluator {
	t.Helper()
	return NewSubscriptionEvaluator(
		h.subscriptions,
		NewSender(h.broadcasts, h.audience, zerolog.Nop()),
		nil,
		zerolog.Nop(),
	).WithSessionStates(h.rawSessions)
}

func (h *hookHarness) subscribe(t *testing.T, trigger models.BroadcastTrigger, expiresAt time.Time) string {
	t.Helper()
	origin := "chat-coordinator"
	sub := &models.BroadcastSubscription{
		OwnerSessionID: h.sessionID,
		OriginChatID:   &origin,
		TriggerEvent:   trigger,
		Selector:       "repo:repo-1",
		Message:        secretBody,
		ExpiresAt:      expiresAt,
	}
	if err := h.subscriptions.Create(context.Background(), sub); err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	return sub.ID
}

func (h *hookHarness) transition(t *testing.T, to machine.State) {
	t.Helper()
	state := int(to)
	if _, err := h.sessions.Update(context.Background(), h.sessionID, db.UpdateSessionParams{State: &state}); err != nil {
		t.Fatalf("transition to %v: %v", to, err)
	}
}

func (h *hookHarness) subscription(t *testing.T, id string) *models.BroadcastSubscription {
	t.Helper()
	sub, err := h.subscriptions.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get subscription: %v", err)
	}
	return sub
}

func (h *hookHarness) sessionState(t *testing.T) machine.State {
	t.Helper()
	sess, err := h.sessions.Get(context.Background(), h.sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	return sess.State
}

// assertFiredBroadcastExists checks the subscription's fired_broadcast_id
// actually resolves — the property that breaks if the sender mints its own id
// instead of honouring the one the evaluator recorded during the CAS.
func (h *hookHarness) assertFiredBroadcastExists(t *testing.T, sub *models.BroadcastSubscription) {
	t.Helper()
	if sub.FiredBroadcastID == nil || *sub.FiredBroadcastID == "" {
		t.Fatal("subscription fired without recording a broadcast id")
	}
	b, err := h.broadcasts.Get(context.Background(), *sub.FiredBroadcastID)
	if err != nil {
		t.Fatalf("fired_broadcast_id %q does not resolve: %v", *sub.FiredBroadcastID, err)
	}
	if b.Message != secretBody {
		t.Error("the fired broadcast must carry the subscription's body for the delivery worker")
	}
}

// TestTransitionIntoMergedFiresACompletedSubscription is the headline case: a
// coordinator asked to be told when this child completes, and the child merged.
func TestTransitionIntoMergedFiresACompletedSubscription(t *testing.T) {
	h := newHookHarness(t, machine.ImplementingPlan)
	id := h.subscribe(t, models.BroadcastTriggerCompleted, time.Now().Add(time.Hour))

	h.transition(t, machine.Merged)

	sub := h.subscription(t, id)
	if sub.State != models.BroadcastSubscriptionStateFired {
		t.Fatalf("subscription state = %v, want fired", sub.State)
	}
	h.assertFiredBroadcastExists(t, sub)
}

// TestTransitionIntoBlockedFiresAnErroredSubscription covers the other class.
func TestTransitionIntoBlockedFiresAnErroredSubscription(t *testing.T) {
	h := newHookHarness(t, machine.ImplementingPlan)
	errored := h.subscribe(t, models.BroadcastTriggerErrored, time.Now().Add(time.Hour))
	completed := h.subscribe(t, models.BroadcastTriggerCompleted, time.Now().Add(time.Hour))

	h.transition(t, machine.Blocked)

	if got := h.subscription(t, errored).State; got != models.BroadcastSubscriptionStateFired {
		t.Errorf("errored subscription state = %v, want fired", got)
	}
	if got := h.subscription(t, completed).State; got != models.BroadcastSubscriptionStateActive {
		t.Errorf("completed subscription state = %v, want active (Blocked is not a completion)", got)
	}
}

// TestTransitionIntoANonTriggerStateFiresNothing: the in-flight states are the
// overwhelming majority of transitions and must cost nothing.
func TestTransitionIntoANonTriggerStateFiresNothing(t *testing.T) {
	h := newHookHarness(t, machine.StartingAgent)
	id := h.subscribe(t, models.BroadcastTriggerSettled, time.Now().Add(time.Hour))

	h.transition(t, machine.ImplementingPlan)

	if got := h.subscription(t, id).State; got != models.BroadcastSubscriptionStateActive {
		t.Fatalf("subscription state = %v, want active", got)
	}
}

// TestRepeatedTransitionFiresExactlyOneBroadcast pins the EDGE GATE, and only
// that: the decorator compares against the persisted prior state, so the second
// and third writes never reach the evaluator at all.
//
// It deliberately does not claim to also prove the CAS — it cannot, precisely
// because the gate stops the repeats upstream of it. Reverse fire's CAS/send
// ordering and this test stays green. The CAS is proven separately, and
// non-vacuously, by TestSubscriptionFiresOnce (barriered, two callers) and
// TestBroadcastSubscriptionFiresOnceUnderConcurrency (eight callers, real DB).
func TestRepeatedTransitionFiresExactlyOneBroadcast(t *testing.T) {
	h := newHookHarness(t, machine.ImplementingPlan)
	id := h.subscribe(t, models.BroadcastTriggerSettled, time.Now().Add(time.Hour))

	h.transition(t, machine.Merged)
	h.transition(t, machine.Merged)
	h.transition(t, machine.Merged)

	all, err := h.broadcasts.List(context.Background(), db.ListBroadcastsFilter{})
	if err != nil {
		t.Fatalf("list broadcasts: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("broadcasts = %d, want exactly 1", len(all))
	}
	if got := h.subscription(t, id).State; got != models.BroadcastSubscriptionStateFired {
		t.Errorf("subscription state = %v, want fired", got)
	}
}

// TestBroadcastSendFailureDoesNotFailTheSessionTransition is the acceptance
// criterion: subscriptions are a notification side-channel, and a side-channel
// that can roll back a session transition is a liability, not a feature.
func TestBroadcastSendFailureDoesNotFailTheSessionTransition(t *testing.T) {
	h := newHookHarness(t, machine.ImplementingPlan)
	id := h.subscribe(t, models.BroadcastTriggerCompleted, time.Now().Add(time.Hour))
	h.audience.err = errors.New("resolver is down")

	state := int(machine.Merged)
	sess, err := h.sessions.Update(context.Background(), h.sessionID, db.UpdateSessionParams{State: &state})
	if err != nil {
		t.Fatalf("update returned %v; a failed send must not fail the transition", err)
	}
	if sess.State != machine.Merged {
		t.Fatalf("returned state = %v, want %v", sess.State, machine.Merged)
	}
	if got := h.sessionState(t); got != machine.Merged {
		t.Fatalf("persisted state = %v, want %v", got, machine.Merged)
	}
	// And the subscription stays FIRED rather than rolling back to active: the
	// broadcast's own delivery worker owns retry, and un-firing would risk a
	// duplicate on the next transition.
	if got := h.subscription(t, id).State; got != models.BroadcastSubscriptionStateFired {
		t.Errorf("subscription state = %v, want fired", got)
	}
}

// TestSweepFiresASessionThatSettledWhileTheDaemonWasDown is the reconcile safety
// net against real stores: nothing observed the transition, and the sweep still
// delivers. This is also the net under AdvanceOrphanedSessions, the one state
// writer the decorator deliberately does not intercept.
func TestSweepFiresASessionThatSettledWhileTheDaemonWasDown(t *testing.T) {
	h := newHookHarness(t, machine.ImplementingPlan)
	id := h.subscribe(t, models.BroadcastTriggerSettled, time.Now().Add(time.Hour))

	// Move the session with the RAW store, i.e. behind the hook's back.
	state := int(machine.Orphaned)
	if _, err := h.rawSessions.Update(context.Background(), h.sessionID, db.UpdateSessionParams{State: &state}); err != nil {
		t.Fatalf("unobserved transition: %v", err)
	}
	if got := h.subscription(t, id).State; got != models.BroadcastSubscriptionStateActive {
		t.Fatalf("subscription state = %v, want active (the write must have bypassed the hook)", got)
	}

	if err := h.evaluator(t).ReconcileAll(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	sub := h.subscription(t, id)
	if sub.State != models.BroadcastSubscriptionStateFired {
		t.Fatalf("subscription state = %v, want fired after the sweep", sub.State)
	}
	h.assertFiredBroadcastExists(t, sub)
}

// TestSweepRetiresOverdueSubscriptionsWithoutFiringThem: expiry is the only
// reaper this table has, and a stale registration must never deliver.
func TestSweepRetiresOverdueSubscriptionsWithoutFiringThem(t *testing.T) {
	h := newHookHarness(t, machine.Merged)
	id := h.subscribe(t, models.BroadcastTriggerSettled, time.Now().Add(-time.Minute))

	if err := h.evaluator(t).ReconcileAll(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if got := h.subscription(t, id).State; got != models.BroadcastSubscriptionStateExpired {
		t.Fatalf("subscription state = %v, want expired", got)
	}
	all, err := h.broadcasts.List(context.Background(), db.ListBroadcastsFilter{})
	if err != nil {
		t.Fatalf("list broadcasts: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("broadcasts = %d, want 0 (an expired subscription must never fire)", len(all))
	}
}

// TestFiredBroadcastNeverEchoesTheBodyIntoADiagnostic re-asserts the secret-body
// rule at the seam where a body first crosses from a subscription into a
// broadcast: after a fire it must live in exactly two columns —
// broadcast_subscriptions.message and broadcasts.message — and nowhere else in
// the chain the fire created.
//
// It greps the persisted rows rather than only inspecting LastError: nothing has
// attempted a delivery at this point, so every last_error is NULL and a
// LastError-only check would assert nothing at all. (The populated-last_error
// case is covered non-vacuously by the delivery worker's own tests.) The
// row-count assertion is what keeps this loop live.
func TestFiredBroadcastNeverEchoesTheBodyIntoADiagnostic(t *testing.T) {
	h := newHookHarness(t, machine.ImplementingPlan)
	id := h.subscribe(t, models.BroadcastTriggerCompleted, time.Now().Add(time.Hour))

	h.transition(t, machine.Merged)

	sub := h.subscription(t, id)
	if sub.FiredBroadcastID == nil {
		t.Fatal("the subscription did not fire; the leak assertions below would be vacuous")
	}
	// The body IS carried into the broadcast — the delivery worker's only source
	// — so its absence everywhere else is meaningful.
	fired, err := h.broadcasts.Get(context.Background(), *sub.FiredBroadcastID)
	if err != nil {
		t.Fatalf("get fired broadcast: %v", err)
	}
	if !strings.Contains(fired.Message, secretBody) {
		t.Fatal("the fired broadcast does not carry the body; the leak assertions would be vacuous")
	}
	if strings.Contains(fired.Selector, secretBody) || strings.Contains(fired.State.String(), secretBody) {
		t.Error("the broadcast row echoes the body outside its message column")
	}

	deliveries, err := h.broadcasts.ListDeliveries(context.Background(), *sub.FiredBroadcastID)
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	if len(deliveries) == 0 {
		t.Fatal("no deliveries were materialised; the per-delivery assertions would never run")
	}
	for _, d := range deliveries {
		// A delivery row is pure diagnostics — it must carry no part of the body in
		// ANY column, not merely in last_error.
		for label, field := range map[string]string{
			"target_chat_id":   d.TargetChatID,
			"target_daemon_id": d.TargetDaemonID,
			"state":            d.State.String(),
			"last_error":       derefString(d.LastError),
		} {
			if strings.Contains(field, secretBody) {
				t.Errorf("delivery %s leaked the body: %q", label, field)
			}
		}
	}
}

// derefString reads an optional diagnostic column as "" when unset, so the leak
// scan above covers every column uniformly.
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
