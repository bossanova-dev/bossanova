package broadcast

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
)

// secretBody is planted in every subscription these tests fire. No error string
// or log line the evaluator produces may contain it — the message is a secret at
// rest (see models.BroadcastSubscription).
const secretBody = "PLANTED-SECRET-BODY-a7f3"

// fakeSubscriptionStore is an in-memory subscriptionStore reproducing the CAS
// semantics of db.SQLiteBroadcastSubscriptionStore that the evaluator depends
// on: MarkFired is a real compare-and-swap on state = active, so a losing caller
// gets (false, nil) and only the winner may send.
type fakeSubscriptionStore struct {
	mu   sync.Mutex
	subs map[string]*models.BroadcastSubscription
	// order preserves creation order for ListActiveForSession.
	order []string

	listErr      error
	markFiredErr error
	expireErr    error
	ownersErr    error

	// listBarrier, when non-nil, holds every ListActiveForSession caller until
	// all of its participants have listed. It is what turns TestSubscriptionFiresOnce
	// from a probabilistic race into a DETERMINISTIC proof of the CAS-before-send
	// ORDERING: without it, a schedule where the first caller completes MarkFired
	// before the second reaches the list makes the second see an empty list and
	// send nothing, so a reversed implementation (send, then CAS) would still
	// report one send and the test would pass. With the barrier both callers
	// necessarily hold the same pre-CAS snapshot, so only the CAS can separate
	// them.
	listBarrier *sync.WaitGroup

	expireCalls int
}

func newFakeSubscriptionStore(subs ...*models.BroadcastSubscription) *fakeSubscriptionStore {
	s := &fakeSubscriptionStore{subs: make(map[string]*models.BroadcastSubscription, len(subs))}
	for _, sub := range subs {
		s.subs[sub.ID] = sub
		s.order = append(s.order, sub.ID)
	}
	return s
}

func (s *fakeSubscriptionStore) ListActiveForSession(_ context.Context, sessionID string) ([]models.BroadcastSubscription, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	s.mu.Lock()
	var out []models.BroadcastSubscription
	for _, id := range s.order {
		sub := s.subs[id]
		if sub.OwnerSessionID != sessionID || sub.State != models.BroadcastSubscriptionStateActive {
			continue
		}
		out = append(out, *sub)
	}
	s.mu.Unlock()
	// Released the lock FIRST: the barrier blocks until every participant has
	// listed, and holding s.mu across it would deadlock the participants still
	// trying to read.
	if s.listBarrier != nil {
		s.listBarrier.Done()
		s.listBarrier.Wait()
	}
	return out, nil
}

func (s *fakeSubscriptionStore) MarkFired(_ context.Context, id string, broadcastID string, now time.Time) (bool, error) {
	if s.markFiredErr != nil {
		return false, s.markFiredErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sub, ok := s.subs[id]
	if !ok {
		return false, sql.ErrNoRows
	}
	if sub.State != models.BroadcastSubscriptionStateActive {
		return false, nil
	}
	sub.State = models.BroadcastSubscriptionStateFired
	sub.FiredAt = &now
	sub.FiredBroadcastID = &broadcastID
	return true, nil
}

func (s *fakeSubscriptionStore) ExpireOverdue(_ context.Context, now time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireCalls++
	if s.expireErr != nil {
		return 0, s.expireErr
	}
	var n int64
	for _, sub := range s.subs {
		if sub.State == models.BroadcastSubscriptionStateActive && !sub.ExpiresAt.After(now) {
			sub.State = models.BroadcastSubscriptionStateExpired
			n++
		}
	}
	return n, nil
}

func (s *fakeSubscriptionStore) ListActiveOwnerSessionIDs(_ context.Context) ([]string, error) {
	if s.ownersErr != nil {
		return nil, s.ownersErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := make(map[string]struct{}, len(s.order))
	var out []string
	for _, id := range s.order {
		sub := s.subs[id]
		if sub.State != models.BroadcastSubscriptionStateActive {
			continue
		}
		if _, ok := seen[sub.OwnerSessionID]; ok {
			continue
		}
		seen[sub.OwnerSessionID] = struct{}{}
		out = append(out, sub.OwnerSessionID)
	}
	return out, nil
}

func (s *fakeSubscriptionStore) get(t *testing.T, id string) models.BroadcastSubscription {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	sub, ok := s.subs[id]
	if !ok {
		t.Fatalf("subscription %s not found", id)
	}
	return *sub
}

// fakeSender records every broadcast the evaluator asks it to send.
type fakeSender struct {
	mu   sync.Mutex
	sent []SubscriptionBroadcast
	err  error
}

func (f *fakeSender) SendBroadcast(_ context.Context, b SubscriptionBroadcast) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, b)
	return nil
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

// fakeSessionStates is an in-memory sessionStateReader.
type fakeSessionStates struct {
	sessions map[string]*models.Session
	err      error
}

func (f *fakeSessionStates) Get(_ context.Context, id string) (*models.Session, error) {
	if f.err != nil {
		return nil, f.err
	}
	s, ok := f.sessions[id]
	if !ok {
		return nil, sql.ErrNoRows
	}
	return s, nil
}

func testTime() time.Time { return time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC) }

func activeSub(id, sessionID string, trigger models.BroadcastTrigger) *models.BroadcastSubscription {
	return &models.BroadcastSubscription{
		ID:             id,
		OwnerSessionID: sessionID,
		TriggerEvent:   trigger,
		Selector:       "repo:repo-1",
		Message:        secretBody,
		State:          models.BroadcastSubscriptionStateActive,
		ExpiresAt:      testTime().Add(time.Hour),
		CreatedAt:      testTime().Add(-time.Hour),
	}
}

func newTestEvaluator(store subscriptionStore, sender broadcastSender) *SubscriptionEvaluator {
	return NewSubscriptionEvaluator(store, sender, testTime, zerolog.Nop())
}

// wantSubscriptionClasses is the authoritative expectation for every current
// machine.State. TestTriggerClassForIsExhaustive asserts it covers the whole
// enum, so adding a state without deciding its class fails here rather than
// silently never firing.
var wantSubscriptionClasses = map[machine.State]struct {
	class Class
	ok    bool
}{
	machine.CreatingWorktree: {"", false},
	machine.StartingAgent:    {"", false},
	machine.PushingBranch:    {"", false},
	machine.OpeningDraftPR:   {"", false},
	machine.ImplementingPlan: {"", false},
	machine.AwaitingChecks:   {"", false},
	machine.FixingChecks:     {"", false},
	machine.Finalizing:       {"", false},
	machine.GreenDraft:       {ClassCompleted, true},
	machine.ReadyForReview:   {ClassCompleted, true},
	machine.Merged:           {ClassCompleted, true},
	machine.Closed:           {ClassCompleted, true},
	machine.Blocked:          {ClassErrored, true},
	machine.Orphaned:         {ClassErrored, true},
}

func TestTriggerClassForTable(t *testing.T) {
	for state, want := range wantSubscriptionClasses {
		got, ok := TriggerClassFor(state)
		if ok != want.ok {
			t.Fatalf("TriggerClassFor(%s) ok = %v, want %v", state, ok, want.ok)
		}
		if got != want.class {
			t.Fatalf("TriggerClassFor(%s) = %q, want %q", state, got, want.class)
		}
	}
}

// TestTriggerClassForIsExhaustive is the guard the plan requires: it fails
// loudly when a machine.State is added without a classification.
// It iterates machine.AllStates rather than a numeric range or a String()-based
// sentinel. Both of those had the same hole: a state appended to the const block
// WITHOUT a String() case rendered as "unknown", tripped nothing, and left
// TriggerClassFor silently returning "not a trigger" for it — a standing
// subscription that never fires, which is the worst failure this feature has.
// machine.AllStates is pinned against its own const block by
// TestAllStatesMatchesTheConstBlock, so a new state cannot reach here
// unannounced.
func TestTriggerClassForIsExhaustive(t *testing.T) {
	all := machine.AllStates()
	for _, state := range all {
		if _, ok := wantSubscriptionClasses[state]; !ok {
			t.Fatalf("machine.State %q (%d) has no entry in wantSubscriptionClasses: "+
				"classify it in TriggerClassFor (completed / errored / neither) and add it "+
				"to the table", state, int(state))
		}
	}
	// The table must not outlive the enum either: an entry for a state that no
	// longer exists means the table is being maintained against a stale picture.
	if len(wantSubscriptionClasses) != len(all) {
		t.Fatalf("wantSubscriptionClasses has %d entries but machine.AllStates has %d: "+
			"the table has drifted from the enum", len(wantSubscriptionClasses), len(all))
	}
}

func TestClassMatchesTrigger(t *testing.T) {
	cases := []struct {
		class   Class
		trigger models.BroadcastTrigger
		want    bool
	}{
		{ClassCompleted, models.BroadcastTriggerCompleted, true},
		{ClassCompleted, models.BroadcastTriggerErrored, false},
		{ClassCompleted, models.BroadcastTriggerSettled, true},
		{ClassErrored, models.BroadcastTriggerCompleted, false},
		{ClassErrored, models.BroadcastTriggerErrored, true},
		{ClassErrored, models.BroadcastTriggerSettled, true},
	}
	for _, tc := range cases {
		if got := tc.class.Matches(tc.trigger); got != tc.want {
			t.Fatalf("Class(%q).Matches(%q) = %v, want %v", tc.class, tc.trigger, got, tc.want)
		}
	}
}

func TestSubscriptionMatchingTriggerFiresOneBroadcast(t *testing.T) {
	sub := activeSub("sub-1", "sess-1", models.BroadcastTriggerCompleted)
	store := newFakeSubscriptionStore(sub)
	sender := &fakeSender{}
	ev := newTestEvaluator(store, sender)

	if err := ev.OnSessionState(context.Background(), "sess-1", machine.Merged); err != nil {
		t.Fatalf("OnSessionState: %v", err)
	}
	if got := sender.count(); got != 1 {
		t.Fatalf("sends = %d, want 1", got)
	}
	sent := sender.sent[0]
	if sent.ID == "" {
		t.Fatal("broadcast id is empty: it must be generated before MarkFired so the CAS can record it")
	}
	if sent.Message != secretBody {
		t.Fatalf("message not carried to the sender: %q", sent.Message)
	}
	stored := store.get(t, "sub-1")
	if stored.State != models.BroadcastSubscriptionStateFired {
		t.Fatalf("subscription state = %q, want fired", stored.State)
	}
	if stored.FiredBroadcastID == nil || *stored.FiredBroadcastID != sent.ID {
		t.Fatalf("fired_broadcast_id = %v, want %q", stored.FiredBroadcastID, sent.ID)
	}
}

func TestSubscriptionNonMatchingTriggerFiresNothing(t *testing.T) {
	store := newFakeSubscriptionStore(activeSub("sub-1", "sess-1", models.BroadcastTriggerErrored))
	sender := &fakeSender{}
	ev := newTestEvaluator(store, sender)

	if err := ev.OnSessionState(context.Background(), "sess-1", machine.Merged); err != nil {
		t.Fatalf("OnSessionState: %v", err)
	}
	if got := sender.count(); got != 0 {
		t.Fatalf("sends = %d, want 0", got)
	}
	if got := store.get(t, "sub-1").State; got != models.BroadcastSubscriptionStateActive {
		t.Fatalf("subscription state = %q, want active", got)
	}
}

func TestSubscriptionNonTriggerStateFiresNothing(t *testing.T) {
	store := newFakeSubscriptionStore(activeSub("sub-1", "sess-1", models.BroadcastTriggerSettled))
	sender := &fakeSender{}
	ev := newTestEvaluator(store, sender)

	if err := ev.OnSessionState(context.Background(), "sess-1", machine.ImplementingPlan); err != nil {
		t.Fatalf("OnSessionState: %v", err)
	}
	if got := sender.count(); got != 0 {
		t.Fatalf("sends = %d, want 0", got)
	}
}

func TestSubscriptionSettledMatchesBothClasses(t *testing.T) {
	for _, state := range []machine.State{machine.Merged, machine.Blocked} {
		store := newFakeSubscriptionStore(activeSub("sub-1", "sess-1", models.BroadcastTriggerSettled))
		sender := &fakeSender{}
		ev := newTestEvaluator(store, sender)
		if err := ev.OnSessionState(context.Background(), "sess-1", state); err != nil {
			t.Fatalf("OnSessionState(%s): %v", state, err)
		}
		if got := sender.count(); got != 1 {
			t.Fatalf("state %s: sends = %d, want 1", state, got)
		}
	}
}

func TestSubscriptionExpiredNeverFires(t *testing.T) {
	sub := activeSub("sub-1", "sess-1", models.BroadcastTriggerCompleted)
	// Expiry already passed as of the evaluator's clock. The row is still
	// literally `active` (the sweep is lazy), so the evaluator must apply the
	// predicate itself.
	sub.ExpiresAt = testTime().Add(-time.Minute)
	store := newFakeSubscriptionStore(sub)
	sender := &fakeSender{}
	ev := newTestEvaluator(store, sender)

	if err := ev.OnSessionState(context.Background(), "sess-1", machine.Merged); err != nil {
		t.Fatalf("OnSessionState: %v", err)
	}
	if got := sender.count(); got != 0 {
		t.Fatalf("sends = %d, want 0", got)
	}
	if got := store.get(t, "sub-1").State; got != models.BroadcastSubscriptionStateActive {
		t.Fatalf("expired subscription was marked %q; it must not be fired", got)
	}
}

// TestSubscriptionFiresOnce is the exactly-once proof: two concurrent
// transitions race through the CAS and exactly one broadcast goes out. Run it
// with -race -count=2.
//
// The barrier makes it a proof of the ORDERING rather than a lucky schedule.
// Both callers are held until each has read the same pre-CAS snapshot showing
// the subscription active, so the ONLY thing that can stop the second from
// sending is losing the CAS. Reverse fire's order — send, then MarkFired — and
// this test fails on every run instead of only when the goroutines happen to
// interleave. See fakeSubscriptionStore.listBarrier.
func TestSubscriptionFiresOnce(t *testing.T) {
	const callers = 2
	store := newFakeSubscriptionStore(activeSub("sub-1", "sess-1", models.BroadcastTriggerSettled))
	var barrier sync.WaitGroup
	barrier.Add(callers)
	store.listBarrier = &barrier
	sender := &fakeSender{}
	ev := newTestEvaluator(store, sender)

	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, callers)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = ev.OnSessionState(context.Background(), "sess-1", machine.Merged)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: OnSessionState: %v", i, err)
		}
	}
	if got := sender.count(); got != 1 {
		t.Fatalf("sends = %d, want exactly 1 (the MarkFired CAS must precede the send)", got)
	}
	if got := store.get(t, "sub-1").State; got != models.BroadcastSubscriptionStateFired {
		t.Fatalf("subscription state = %q, want fired", got)
	}
}

func TestSubscriptionSendFailureLeavesItFiredAndLeaksNoBody(t *testing.T) {
	store := newFakeSubscriptionStore(activeSub("sub-1", "sess-1", models.BroadcastTriggerCompleted))
	sender := &fakeSender{err: errors.New("deliver: chat unreachable")}
	var logs strings.Builder
	ev := NewSubscriptionEvaluator(store, sender, testTime, zerolog.New(&logs))

	err := ev.OnSessionState(context.Background(), "sess-1", machine.Merged)
	if err == nil {
		t.Fatal("OnSessionState returned nil; a send failure must be reported to the caller")
	}
	if got := store.get(t, "sub-1").State; got != models.BroadcastSubscriptionStateFired {
		t.Fatalf("subscription state = %q, want fired (a send failure must never roll back to active)", got)
	}
	if strings.Contains(err.Error(), secretBody) {
		t.Fatal("the returned error contains the subscription message body")
	}
	if logs.Len() == 0 {
		t.Fatal("the send failure was not logged")
	}
	if strings.Contains(logs.String(), secretBody) {
		t.Fatal("a log line contains the subscription message body")
	}
	if !strings.Contains(logs.String(), "sub-1") {
		t.Fatalf("the failure log does not name the subscription: %s", logs.String())
	}
}

// TestSubscriptionSuccessPathLogsNoBody covers the half of the secret-body rule
// the failure test cannot reach. Every other log assertion drives an ERROR, so
// the success-path Info() line — the one that runs on every fire the daemon ever
// performs — had no captured sink over it at all, and a body interpolated there
// would leak on every successful notification while the whole suite stayed
// green.
func TestSubscriptionSuccessPathLogsNoBody(t *testing.T) {
	store := newFakeSubscriptionStore(activeSub("sub-1", "sess-1", models.BroadcastTriggerCompleted))
	sender := &fakeSender{}
	var logs strings.Builder
	ev := NewSubscriptionEvaluator(store, sender, testTime, zerolog.New(&logs))

	if err := ev.OnSessionState(context.Background(), "sess-1", machine.Merged); err != nil {
		t.Fatalf("OnSessionState: %v", err)
	}
	// Non-vacuity in both directions: the fire really happened AND it really
	// logged, so an empty-log false pass is impossible.
	if got := sender.count(); got != 1 {
		t.Fatalf("sends = %d, want 1", got)
	}
	if logs.Len() == 0 {
		t.Fatal("a successful fire logged nothing; the leak assertion below would be vacuous")
	}
	if !strings.Contains(logs.String(), "sub-1") {
		t.Fatalf("the success log does not name the subscription: %s", logs.String())
	}
	if strings.Contains(logs.String(), secretBody) {
		t.Fatalf("the success log line contains the subscription message body: %s", logs.String())
	}
}

func TestSubscriptionEvaluatorKeepsGoingAfterOneFailure(t *testing.T) {
	store := newFakeSubscriptionStore(
		activeSub("sub-1", "sess-1", models.BroadcastTriggerCompleted),
		activeSub("sub-2", "sess-1", models.BroadcastTriggerSettled),
	)
	sender := &fakeSender{err: errors.New("boom")}
	ev := newTestEvaluator(store, sender)

	err := ev.OnSessionState(context.Background(), "sess-1", machine.Merged)
	if err == nil {
		t.Fatal("want a joined error covering both failures")
	}
	for _, id := range []string{"sub-1", "sub-2"} {
		if got := store.get(t, id).State; got != models.BroadcastSubscriptionStateFired {
			t.Fatalf("%s state = %q, want fired: processing must continue past a failure", id, got)
		}
	}
}

func TestReconcileAllExpiresAndFiresAlreadySettledSessions(t *testing.T) {
	overdue := activeSub("sub-overdue", "sess-gone", models.BroadcastTriggerSettled)
	overdue.ExpiresAt = testTime().Add(-time.Minute)
	live := activeSub("sub-live", "sess-merged", models.BroadcastTriggerCompleted)
	store := newFakeSubscriptionStore(overdue, live)
	sender := &fakeSender{}
	ev := newTestEvaluator(store, sender)
	ev.WithSessionStates(&fakeSessionStates{sessions: map[string]*models.Session{
		"sess-merged": {ID: "sess-merged", State: machine.Merged},
		"sess-gone":   {ID: "sess-gone", State: machine.ImplementingPlan},
	}})

	if err := ev.ReconcileAll(context.Background()); err != nil {
		t.Fatalf("ReconcileAll: %v", err)
	}
	if got := store.get(t, "sub-overdue").State; got != models.BroadcastSubscriptionStateExpired {
		t.Fatalf("overdue subscription state = %q, want expired", got)
	}
	if got := store.get(t, "sub-live").State; got != models.BroadcastSubscriptionStateFired {
		t.Fatalf("live subscription state = %q, want fired", got)
	}
	if got := sender.count(); got != 1 {
		t.Fatalf("sends = %d, want 1", got)
	}
}

func TestReconcileAllSkipsMissingSessions(t *testing.T) {
	store := newFakeSubscriptionStore(activeSub("sub-1", "sess-vanished", models.BroadcastTriggerSettled))
	sender := &fakeSender{}
	ev := newTestEvaluator(store, sender)
	ev.WithSessionStates(&fakeSessionStates{sessions: map[string]*models.Session{}})

	if err := ev.ReconcileAll(context.Background()); err != nil {
		t.Fatalf("ReconcileAll: %v", err)
	}
	if got := sender.count(); got != 0 {
		t.Fatalf("sends = %d, want 0", got)
	}
}

func TestReconcileAllWithoutSessionReaderReportsWiringGap(t *testing.T) {
	store := newFakeSubscriptionStore(activeSub("sub-1", "sess-1", models.BroadcastTriggerSettled))
	ev := newTestEvaluator(store, &fakeSender{})

	err := ev.ReconcileAll(context.Background())
	if err == nil {
		t.Fatal("ReconcileAll with no session reader must report the wiring gap, not go quiet")
	}
	if store.expireCalls != 1 {
		t.Fatalf("expire calls = %d, want 1: expiry must still run", store.expireCalls)
	}
}
