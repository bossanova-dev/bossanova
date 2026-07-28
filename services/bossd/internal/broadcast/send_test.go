package broadcast

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	bcast "github.com/recurser/bossalib/broadcast"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
)

// fakeSendStore records the broadcast + delivery writes the send path makes and
// can be scripted to fail any of them.
type fakeSendStore struct {
	mu         sync.Mutex
	created    []models.Broadcast
	deliveries map[string][]models.BroadcastDelivery
	deleted    []string
	settled    []string

	createErr     error
	deliveriesErr error
	settleErr     error
	deleteErr     error
}

func newFakeSendStore() *fakeSendStore {
	return &fakeSendStore{deliveries: map[string][]models.BroadcastDelivery{}}
}

func (s *fakeSendStore) Create(_ context.Context, b *models.Broadcast) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.createErr != nil {
		return s.createErr
	}
	if b.ID == "" {
		b.ID = "minted-by-the-store"
	}
	s.created = append(s.created, *b)
	return nil
}

func (s *fakeSendStore) CreateDeliveries(_ context.Context, broadcastID string, targets []models.BroadcastDelivery) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deliveriesErr != nil {
		return s.deliveriesErr
	}
	s.deliveries[broadcastID] = append(s.deliveries[broadcastID], targets...)
	return nil
}

func (s *fakeSendStore) CompleteIfSettled(_ context.Context, broadcastID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settleErr != nil {
		return s.settleErr
	}
	s.settled = append(s.settled, broadcastID)
	return nil
}

func (s *fakeSendStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.deleted = append(s.deleted, id)
	return nil
}

func (s *fakeSendStore) createdIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.created))
	for _, b := range s.created {
		out = append(out, b.ID)
	}
	return out
}

// fakeAudience is a scripted audienceResolver.
type fakeAudience struct {
	targets []bcast.Target
	err     error

	gotOrigin        string
	gotIncludeOrigin bool
}

func (f *fakeAudience) Resolve(_ context.Context, _ bcast.Selector, originChatID string, includeOrigin bool) ([]bcast.Target, error) {
	f.gotOrigin = originChatID
	f.gotIncludeOrigin = includeOrigin
	if f.err != nil {
		return nil, f.err
	}
	return f.targets, nil
}

func newTestSender(t *testing.T, store *fakeSendStore, audience *fakeAudience) *Sender {
	t.Helper()
	return NewSender(store, audience, zerolog.Nop())
}

func allSelector(t *testing.T) bcast.Selector {
	t.Helper()
	sel, err := bcast.Parse("repo:repo-1")
	if err != nil {
		t.Fatalf("parse selector: %v", err)
	}
	return sel
}

// TestSenderPersistsTheBroadcastUnderThePresetID is the load-bearing property
// for subscriptions: the evaluator mints the broadcast id BEFORE the CAS and
// records it on the subscription row, so the send must persist under exactly
// that id or fired_broadcast_id points at nothing.
func TestSenderPersistsTheBroadcastUnderThePresetID(t *testing.T) {
	store := newFakeSendStore()
	audience := &fakeAudience{targets: []bcast.Target{{ChatID: "chat-1"}, {ChatID: "chat-2"}}}
	sender := newTestSender(t, store, audience)

	err := sender.SendBroadcast(context.Background(), SubscriptionBroadcast{
		ID:             "preset-broadcast-id",
		SubscriptionID: "sub-1",
		Selector:       allSelector(t),
		Message:        secretBody,
		OriginChatID:   "chat-origin",
		ExpiresAt:      time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("send broadcast: %v", err)
	}

	if got := store.createdIDs(); len(got) != 1 || got[0] != "preset-broadcast-id" {
		t.Fatalf("created broadcast ids = %v, want [preset-broadcast-id]", got)
	}
	if got := len(store.deliveries["preset-broadcast-id"]); got != 2 {
		t.Fatalf("deliveries = %d, want 2", got)
	}
	if store.created[0].Message != secretBody {
		t.Error("the body must reach the store; it is the delivery worker's only source")
	}
	if store.created[0].State != models.BroadcastStatePending {
		t.Errorf("created state = %v, want pending (CreateDeliveries owns pending -> resolved)", store.created[0].State)
	}
}

// TestSenderIncludesTheOriginInAFiredAudience is the headline use case: a
// coordinator registers "tell me when this child finishes" with a selector that
// names its own chat, and must actually be told.
//
// Self-exclusion is the right default for an explicit send, where the origin is
// the SENDER and must not wake itself into a loop. On a fired subscription the
// origin is the REQUESTER, so excluding it would resolve the audience to zero
// targets and mark the subscription fired without notifying the one chat that
// asked — a silent no-fire with no error anywhere. The selector alone decides
// the audience; origin_chat_id stays provenance.
func TestSenderIncludesTheOriginInAFiredAudience(t *testing.T) {
	store := newFakeSendStore()
	audience := &fakeAudience{targets: []bcast.Target{{ChatID: "chat-origin"}}}
	sender := newTestSender(t, store, audience)

	if err := sender.SendBroadcast(context.Background(), SubscriptionBroadcast{
		ID:           "b1",
		Selector:     allSelector(t),
		Message:      secretBody,
		OriginChatID: "chat-origin",
		ExpiresAt:    time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("send broadcast: %v", err)
	}
	if audience.gotOrigin != "chat-origin" {
		t.Errorf("resolver origin = %q, want chat-origin", audience.gotOrigin)
	}
	if !audience.gotIncludeOrigin {
		t.Fatal("a fired subscription must include the origin: it is the chat that asked to be told")
	}
	// And the requester really is in the frozen audience, not merely permitted.
	got := store.deliveries["b1"]
	if len(got) != 1 || got[0].TargetChatID != "chat-origin" {
		t.Fatalf("deliveries = %+v, want one addressed to chat-origin", got)
	}
}

// TestSenderRefusesOverTheFanOutCapWithoutWritingAnything: the cap is a refusal,
// never a truncation, and a refused send must leave no row behind.
func TestSenderRefusesOverTheFanOutCapWithoutWritingAnything(t *testing.T) {
	store := newFakeSendStore()
	audience := &fakeAudience{err: &TooManyTargetsError{Count: MaxTargets + 1, Max: MaxTargets}}
	sender := newTestSender(t, store, audience)

	_, err := sender.Send(context.Background(), SendRequest{
		Selector:  allSelector(t),
		Message:   secretBody,
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if !errors.Is(err, ErrTooManyTargets) {
		t.Fatalf("err = %v, want ErrTooManyTargets", err)
	}
	if len(store.createdIDs()) != 0 {
		t.Error("an over-cap send must not create a broadcast")
	}
}

// TestSenderCleansUpWhenTheAudienceFailsToMaterialise: Create commits in its own
// transaction, so a CreateDeliveries failure would otherwise strand a pending
// broadcast holding a secret body for the whole expiry window.
func TestSenderCleansUpWhenTheAudienceFailsToMaterialise(t *testing.T) {
	store := newFakeSendStore()
	store.deliveriesErr = errors.New("disk full")
	audience := &fakeAudience{targets: []bcast.Target{{ChatID: "chat-1"}}}
	sender := newTestSender(t, store, audience)

	_, err := sender.Send(context.Background(), SendRequest{
		ID:        "b1",
		Selector:  allSelector(t),
		Message:   secretBody,
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err == nil {
		t.Fatal("expected an error when the audience fails to materialise")
	}
	if len(store.deleted) != 1 || store.deleted[0] != "b1" {
		t.Fatalf("deleted = %v, want [b1]", store.deleted)
	}
}

// TestSenderDoesNotCleanUpSomebodyElsesResolvedBroadcast is the exception that
// keeps the cleanup above from being destructive.
//
// ErrBroadcastAlreadyResolved means the row under this id is no longer the
// pending one we created but another caller's RESOLVED broadcast, with its
// audience already materialised. Delete is a hard delete that cascades to
// broadcast_deliveries, so cleaning up here would destroy a send that SUCCEEDED
// — the winner was told ok, and its rows would vanish underneath it with only
// our own error to show for it. Reachable once two concurrent sends can share a
// pre-minted id, which the cross-daemon ingress makes routine.
func TestSenderDoesNotCleanUpSomebodyElsesResolvedBroadcast(t *testing.T) {
	store := newFakeSendStore()
	store.deliveriesErr = fmt.Errorf("%w: broadcast b1 is resolved, not pending", db.ErrBroadcastAlreadyResolved)
	audience := &fakeAudience{targets: []bcast.Target{{ChatID: "chat-1"}}}
	sender := newTestSender(t, store, audience)

	_, err := sender.Send(context.Background(), SendRequest{
		ID:        "b1",
		Selector:  allSelector(t),
		Message:   secretBody,
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err == nil {
		t.Fatal("expected an error when the id is already resolved")
	}
	if !errors.Is(err, db.ErrBroadcastAlreadyResolved) {
		t.Fatalf("err = %v, want it to wrap ErrBroadcastAlreadyResolved so callers can tell the cases apart", err)
	}
	if len(store.deleted) != 0 {
		t.Fatalf("deleted = %v, want none: the resolved row belongs to the caller who won the race", store.deleted)
	}
}

// TestSenderCleanupSurvivesACancelledContext mirrors the handler's
// WithoutCancel guarantee: a cancelled caller is one of the likelier ways the
// delivery write fails at all, and the cleanup must still run.
func TestSenderCleanupSurvivesACancelledContext(t *testing.T) {
	store := newFakeSendStore()
	store.deliveriesErr = context.Canceled
	audience := &fakeAudience{targets: []bcast.Target{{ChatID: "chat-1"}}}
	sender := newTestSender(t, store, audience)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sender.Send(ctx, SendRequest{
		ID:        "b1",
		Selector:  allSelector(t),
		Message:   secretBody,
		ExpiresAt: time.Now().Add(time.Hour),
	}); err == nil {
		t.Fatal("expected an error")
	}
	if len(store.deleted) != 1 {
		t.Fatalf("deleted = %v, want the cleanup to have run under a cancelled context", store.deleted)
	}
}

// TestSenderSettlesAnEmptyAudience: zero targets is a successful send, and the
// broadcast reaches its terminal state synchronously rather than lingering.
func TestSenderSettlesAnEmptyAudience(t *testing.T) {
	store := newFakeSendStore()
	audience := &fakeAudience{}
	sender := newTestSender(t, store, audience)

	res, err := sender.Send(context.Background(), SendRequest{
		ID:        "b1",
		Selector:  allSelector(t),
		Message:   secretBody,
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(store.settled) != 1 || store.settled[0] != "b1" {
		t.Fatalf("settled = %v, want [b1]", store.settled)
	}
	if res.Broadcast.State != models.BroadcastStateCompleted {
		t.Errorf("state = %v, want completed", res.Broadcast.State)
	}
	if len(res.Deliveries) != 0 {
		t.Errorf("deliveries = %d, want 0", len(res.Deliveries))
	}
}

// TestSenderReportsResolvedWhenTheAudienceIsNotEmpty.
func TestSenderReportsResolvedWhenTheAudienceIsNotEmpty(t *testing.T) {
	store := newFakeSendStore()
	audience := &fakeAudience{targets: []bcast.Target{{ChatID: "chat-1"}}}
	sender := newTestSender(t, store, audience)

	res, err := sender.Send(context.Background(), SendRequest{
		ID:        "b1",
		Selector:  allSelector(t),
		Message:   secretBody,
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if res.Broadcast.State != models.BroadcastStateResolved {
		t.Errorf("state = %v, want resolved", res.Broadcast.State)
	}
	if len(store.settled) != 0 {
		t.Errorf("settled = %v, want none (the worker owns resolved -> completed)", store.settled)
	}
}

// TestSenderErrorsNeverCarryTheMessageBody. The body is a secret, and an error
// string is a log line waiting to happen.
func TestSenderErrorsNeverCarryTheMessageBody(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*fakeSendStore, *fakeAudience)
	}{
		{"resolve fails", func(_ *fakeSendStore, a *fakeAudience) { a.err = errors.New("boom") }},
		{"create fails", func(s *fakeSendStore, _ *fakeAudience) { s.createErr = errors.New("boom") }},
		{"deliveries fail", func(s *fakeSendStore, _ *fakeAudience) { s.deliveriesErr = errors.New("boom") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeSendStore()
			audience := &fakeAudience{targets: []bcast.Target{{ChatID: "chat-1"}}}
			tc.setup(store, audience)
			sender := newTestSender(t, store, audience)

			_, err := sender.Send(context.Background(), SendRequest{
				ID:        "b1",
				Selector:  allSelector(t),
				Message:   secretBody,
				ExpiresAt: time.Now().Add(time.Hour),
			})
			if err == nil {
				t.Fatal("expected an error")
			}
			if strings.Contains(err.Error(), secretBody) {
				t.Fatalf("error leaked the message body: %v", err)
			}
		})
	}
}

// TestSenderSatisfiesTheEvaluatorsSenderContract pins the structural claim that
// makes subscriptions "a thin layer over the existing delivery path": the same
// type the RPC handler sends through is what the evaluator fires into.
func TestSenderSatisfiesTheEvaluatorsSenderContract(t *testing.T) {
	var _ broadcastSender = (*Sender)(nil)
}

// TestSenderLogsNeverCarryTheMessageBody covers the Sender's two log lines.
// Every other send test hands it zerolog.Nop(), so both Warn sites were
// completely unobserved and a body interpolated into either would have leaked on
// a real failure while the suite stayed green. Both cases force the log to
// actually emit, so neither assertion can pass on an empty buffer.
func TestSenderLogsNeverCarryTheMessageBody(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*fakeSendStore, *fakeAudience)
	}{
		// The failed-cleanup Warn: deliveries fail, and so does the delete that
		// would have removed the stranded broadcast.
		{"cleanup of an unmaterialised audience fails", func(s *fakeSendStore, _ *fakeAudience) {
			s.deliveriesErr = errors.New("boom")
			s.deleteErr = errors.New("delete boom")
		}},
		// The empty-audience settle Warn: nobody matched, and the settle fails.
		{"settling an empty audience fails", func(s *fakeSendStore, a *fakeAudience) {
			a.targets = nil
			s.settleErr = errors.New("settle boom")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeSendStore()
			audience := &fakeAudience{targets: []bcast.Target{{ChatID: "chat-1"}}}
			tc.setup(store, audience)
			var logs strings.Builder
			sender := NewSender(store, audience, zerolog.New(&logs))

			_, _ = sender.Send(context.Background(), SendRequest{
				ID:        "b1",
				Selector:  allSelector(t),
				Message:   secretBody,
				ExpiresAt: time.Now().Add(time.Hour),
			})

			if logs.Len() == 0 {
				t.Fatal("nothing was logged; the leak assertion below would be vacuous")
			}
			if !strings.Contains(logs.String(), "b1") {
				t.Fatalf("the log does not name the broadcast: %s", logs.String())
			}
			if strings.Contains(logs.String(), secretBody) {
				t.Fatalf("a Sender log line contains the message body: %s", logs.String())
			}
		})
	}
}
