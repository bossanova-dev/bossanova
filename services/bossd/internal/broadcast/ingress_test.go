package broadcast

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	bcast "github.com/recurser/bossalib/broadcast"
	"github.com/recurser/bossalib/models"
)

// fakeIngressStore is a scripted IngressStore. An unknown id reports
// sql.ErrNoRows exactly as SQLiteBroadcastStore.Get does, so the idempotency
// probe is exercised against the real "absent" signal.
type fakeIngressStore struct {
	broadcasts map[string]*models.Broadcast
	err        error
	deleteErr  error

	getCalls    []string
	deleteCalls []string
}

func newFakeIngressStore() *fakeIngressStore {
	return &fakeIngressStore{broadcasts: map[string]*models.Broadcast{}}
}

func (s *fakeIngressStore) Get(_ context.Context, id string) (*models.Broadcast, error) {
	s.getCalls = append(s.getCalls, id)
	if s.err != nil {
		return nil, s.err
	}
	b, ok := s.broadcasts[id]
	if !ok {
		return nil, sql.ErrNoRows
	}
	return b, nil
}

// DeleteUnmaterialised mirrors the store's state-guarded delete: it removes the
// row ONLY while it is still pending, and reports whether it removed one. The
// guard is the point — modelling it faithfully is what lets a test drive the
// "a sibling materialised it between our Get and our delete" branch.
func (s *fakeIngressStore) DeleteUnmaterialised(_ context.Context, id string) (bool, error) {
	s.deleteCalls = append(s.deleteCalls, id)
	if s.deleteErr != nil {
		return false, s.deleteErr
	}
	b, ok := s.broadcasts[id]
	if !ok || b.State != models.BroadcastStatePending {
		return false, nil
	}
	delete(s.broadcasts, id)
	return true, nil
}

// *db.SQLiteBroadcastStore is the production store; pinning the fake against the
// interface keeps the two in step.
var _ IngressStore = (*fakeIngressStore)(nil)

// recordingIngressSender is a scripted send seam that records every request the
// ingress materialises through it.
type recordingIngressSender struct {
	calls []SendRequest
	err   error
}

func (s *recordingIngressSender) Send(_ context.Context, req SendRequest) (*SendResult, error) {
	s.calls = append(s.calls, req)
	if s.err != nil {
		return nil, s.err
	}
	return &SendResult{Broadcast: &models.Broadcast{ID: req.ID}}, nil
}

// *Sender is the production send seam; pinning it here means a signature change
// on Sender.Send breaks this test rather than silently forking a second path.
var _ ingressSender = (*Sender)(nil)

// inboundFixture is a valid inbound command from another daemon.
func inboundFixture(t *testing.T) InboundBroadcast {
	t.Helper()
	return InboundBroadcast{
		ID:             "origin-broadcast-1",
		Selector:       mustParse(t, "repo:repo-1"),
		OriginDaemonID: "d-remote",
		OriginChatID:   "chat-on-the-other-daemon",
		Message:        secretBody,
		ExpiresAt:      time.Now().Add(time.Hour),
	}
}

// TestIngressReceiveDropsSelfOriginatedCommand is the loop guard, and it runs
// FIRST: a command echoed back to its own origin daemon must be dropped before
// the store or the resolver is touched at all. A drop is a success — bosso must
// not retry it.
func TestIngressReceiveDropsSelfOriginatedCommand(t *testing.T) {
	store := newFakeIngressStore()
	sender := &recordingIngressSender{}
	ingress := NewIngress(store, sender, "d-local", nil, zerolog.Nop())

	in := inboundFixture(t)
	in.OriginDaemonID = "d-local"

	if err := ingress.Receive(context.Background(), in); err != nil {
		t.Fatalf("Receive: %v, want nil (a drop is a success, not a retryable error)", err)
	}
	if len(store.getCalls) != 0 {
		t.Errorf("store.Get calls = %v, want none: the loop guard runs before anything else", store.getCalls)
	}
	if len(sender.calls) != 0 {
		t.Errorf("send calls = %d, want 0: a self-originated command creates no rows", len(sender.calls))
	}
}

// TestIngressReceiveDropsSelfOriginatedCommandDespiteWhitespace: the ids are
// compared trimmed, matching how every other daemon-id comparison in this
// package normalises (see ReachesBeyondDaemon).
func TestIngressReceiveDropsSelfOriginatedCommandDespiteWhitespace(t *testing.T) {
	store := newFakeIngressStore()
	sender := &recordingIngressSender{}
	ingress := NewIngress(store, sender, "  d-local\t", nil, zerolog.Nop())

	in := inboundFixture(t)
	in.OriginDaemonID = "d-local "

	if err := ingress.Receive(context.Background(), in); err != nil {
		t.Fatalf("Receive: %v, want nil", err)
	}
	if len(sender.calls) != 0 {
		t.Fatalf("send calls = %d, want 0", len(sender.calls))
	}
}

// TestIngressReceiveWithAnUnknownLocalIDStillMaterialises pins the
// daemonID == "" decision: a blank LOCAL id must not make every inbound command
// look self-originated and silently swallow the fleet's broadcasts.
func TestIngressReceiveWithAnUnknownLocalIDStillMaterialises(t *testing.T) {
	store := newFakeIngressStore()
	sender := &recordingIngressSender{}
	ingress := NewIngress(store, sender, "", nil, zerolog.Nop())

	in := inboundFixture(t)
	in.OriginDaemonID = ""

	if err := ingress.Receive(context.Background(), in); err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if len(sender.calls) != 1 {
		t.Fatalf("send calls = %d, want 1: an unknown local id must not swallow inbound broadcasts", len(sender.calls))
	}
}

// TestIngressReceiveRejectsAnInvalidCommand: an invalid command is an error
// naming the field, never the body.
func TestIngressReceiveRejectsAnInvalidCommand(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*InboundBroadcast)
		wantSub string
	}{
		{"missing broadcast id", func(in *InboundBroadcast) { in.ID = "  " }, "broadcast_id"},
		{"blank message", func(in *InboundBroadcast) { in.Message = "   " }, "message"},
		{"invalid selector", func(in *InboundBroadcast) {
			in.Selector = bcast.Selector{Clauses: []bcast.Clause{{ChatIDs: []string{""}}}}
		}, "selector"},
		// The adapter normalises an absent/epoch wire timestamp to the zero time,
		// but NewIngress and BroadcastReceiver are exported: the rule that an
		// absent expiry is a rejection rather than a default has to hold at the
		// domain boundary that owns it, or a second caller persists a broadcast
		// every delivery is instantly overdue for.
		{"missing expiry", func(in *InboundBroadcast) { in.ExpiresAt = time.Time{} }, "expires_at"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeIngressStore()
			sender := &recordingIngressSender{}
			ingress := NewIngress(store, sender, "d-local", nil, zerolog.Nop())

			in := inboundFixture(t)
			tc.mutate(&in)

			err := ingress.Receive(context.Background(), in)
			if err == nil {
				t.Fatal("expected an error for an invalid inbound command")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %v, want it to name %q", err, tc.wantSub)
			}
			if strings.Contains(err.Error(), secretBody) {
				t.Fatalf("the error leaked the message body: %v", err)
			}
			// Typed as PERMANENT. The stream is at-least-once, so an error the
			// router reads as transient will be redelivered forever; a malformed
			// command fails identically every time and must say so.
			if !errors.Is(err, ErrInvalidInbound) {
				t.Errorf("err = %v, want it to wrap ErrInvalidInbound so the router can stop retrying", err)
			}
			if len(sender.calls) != 0 {
				t.Errorf("send calls = %d, want 0 for an invalid command", len(sender.calls))
			}
		})
	}
}

// TestIngressReceiveRedeliveredBroadcastIsNoOp: bosso delivers at least once, so
// the same broadcast_id can arrive twice. The second arrival must create NO
// additional rows.
func TestIngressReceiveRedeliveredBroadcastIsNoOp(t *testing.T) {
	store := newFakeIngressStore()
	in := inboundFixture(t)
	store.broadcasts[in.ID] = &models.Broadcast{ID: in.ID, State: models.BroadcastStateResolved}
	sender := &recordingIngressSender{}
	ingress := NewIngress(store, sender, "d-local", nil, zerolog.Nop())

	if err := ingress.Receive(context.Background(), in); err != nil {
		t.Fatalf("Receive: %v, want nil for a known broadcast id", err)
	}
	if len(sender.calls) != 0 {
		t.Fatalf("send calls = %d, want 0: a redelivery must not resolve a second audience", len(sender.calls))
	}
}

// TestIngressReceiveRecoversAStrandedPendingBroadcast is the other half of the
// idempotency rule: EXISTENCE is not the test, STATE is.
//
// Create and CreateDeliveries are separate transactions, so a row can be
// stranded at pending with no audience when CreateDeliveries fails and the
// Sender's best-effort cleanup Delete fails too. Nothing drives such a row
// forward — the worker only lists resolved broadcasts — so if bare existence
// counted as "already handled", the at-least-once redelivery that exists to
// recover this would report success forever while the broadcast was never
// delivered. The redelivery must clear the stranded row and re-resolve.
func TestIngressReceiveRecoversAStrandedPendingBroadcast(t *testing.T) {
	store := newFakeIngressStore()
	in := inboundFixture(t)
	store.broadcasts[in.ID] = &models.Broadcast{ID: in.ID, State: models.BroadcastStatePending}
	sender := &recordingIngressSender{}
	ingress := NewIngress(store, sender, "d-local", nil, zerolog.Nop())

	if err := ingress.Receive(context.Background(), in); err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if len(store.deleteCalls) != 1 || store.deleteCalls[0] != in.ID {
		t.Fatalf("delete calls = %v, want exactly [%s]: the stranded row must be cleared", store.deleteCalls, in.ID)
	}
	if len(sender.calls) != 1 {
		t.Fatalf("send calls = %d, want 1: the redelivery must re-resolve the audience", len(sender.calls))
	}
	if sender.calls[0].ID != in.ID {
		t.Errorf("send id = %q, want the ORIGIN's id %q", sender.calls[0].ID, in.ID)
	}
}

// TestIngressReceiveLeavesAConcurrentlyMaterialisingBroadcastAlone is the other
// side of the stranded-row rule, and the dangerous one.
//
// "pending" is also the TRANSIENT state of a send in flight right now, between
// Create and CreateDeliveries. Dispatch is async over an at-least-once stream,
// so two Receive calls for one id really can overlap — and because Delete
// cascades to broadcast_deliveries, clearing a sibling's live row would make its
// CreateDeliveries fail and take the winner's materialised audience with it.
// Both callers would be told ok and nothing would be delivered. A YOUNG pending
// row must therefore be left strictly alone.
//
// Left alone is not the same as acknowledged, and this pins both halves. The
// sibling is holding a PENDING row, so it has committed no audience yet and may
// still fail and clean up; the two overlapping calls are redeliveries of ONE
// command, so an ok from this one can retire it and leave the failure with
// nothing to retry. Deferring therefore returns the transient ErrIngressInFlight
// — untyped at the transport boundary and so retryable — rather than nil.
func TestIngressReceiveLeavesAConcurrentlyMaterialisingBroadcastAlone(t *testing.T) {
	now := time.Now().UTC()
	store := newFakeIngressStore()
	in := inboundFixture(t)
	store.broadcasts[in.ID] = &models.Broadcast{
		ID:        in.ID,
		State:     models.BroadcastStatePending,
		UpdatedAt: now.Add(-time.Second), // a sibling, milliseconds into its send
	}
	sender := &recordingIngressSender{}
	ingress := NewIngress(store, sender, "d-local", func() time.Time { return now }, zerolog.Nop())

	err := ingress.Receive(context.Background(), in)
	if !errors.Is(err, ErrIngressInFlight) {
		t.Fatalf("Receive: %v, want ErrIngressInFlight: acknowledging an uncommitted sibling can retire the command", err)
	}
	// The refusal must stay retryable: the deterministic sentinels are what the
	// adapter types as invalid-argument, and this is emphatically not one of them.
	if errors.Is(err, ErrInvalidInbound) || errors.Is(err, ErrTooManyTargets) {
		t.Fatalf("Receive returned a PERMANENT sentinel for a transient overlap: %v", err)
	}
	if strings.Contains(err.Error(), secretBody) {
		t.Fatalf("the error leaked the message body: %v", err)
	}
	if len(store.deleteCalls) != 0 {
		t.Fatalf("delete calls = %v, want none: deleting a live row cascades away the sibling's deliveries", store.deleteCalls)
	}
	if len(sender.calls) != 0 {
		t.Fatalf("send calls = %d, want 0: the sibling is already materialising this id", len(sender.calls))
	}
}

// TestIngressReceiveClearsOnlyAnAgedStrandedBroadcast pins the boundary itself,
// so a future change to StrandedAfter cannot silently collapse the two cases
// above into one.
func TestIngressReceiveClearsOnlyAnAgedStrandedBroadcast(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name      string
		age       time.Duration
		wantClear bool
	}{
		{name: "just inside the window is a live sibling", age: StrandedAfter - time.Millisecond, wantClear: false},
		{name: "past the window is abandoned", age: StrandedAfter + time.Millisecond, wantClear: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeIngressStore()
			in := inboundFixture(t)
			store.broadcasts[in.ID] = &models.Broadcast{
				ID:        in.ID,
				State:     models.BroadcastStatePending,
				UpdatedAt: now.Add(-tc.age),
			}
			sender := &recordingIngressSender{}
			ingress := NewIngress(store, sender, "d-local", func() time.Time { return now }, zerolog.Nop())

			// The boundary decides the OUTCOME too, not just the delete: inside the
			// window the redelivery is refused so it comes back, past it the row is
			// cleared and re-resolved and the command is genuinely handled.
			err := ingress.Receive(context.Background(), in)
			if tc.wantClear && err != nil {
				t.Fatalf("Receive: %v, want nil once the stranded row is cleared and re-resolved", err)
			}
			if !tc.wantClear && !errors.Is(err, ErrIngressInFlight) {
				t.Fatalf("Receive: %v, want ErrIngressInFlight while a sibling is still in flight", err)
			}
			if cleared := len(store.deleteCalls) == 1; cleared != tc.wantClear {
				t.Fatalf("cleared = %v, want %v (delete calls %v)", cleared, tc.wantClear, store.deleteCalls)
			}
			if resent := len(sender.calls) == 1; resent != tc.wantClear {
				t.Fatalf("re-resolved = %v, want %v", resent, tc.wantClear)
			}
		})
	}
}

// TestIngressReceiveFailsWhenClearingAStrandedBroadcastFails: proceeding past a
// failed clear would only hit the primary-key conflict on Create with a less
// obvious error, so the clear failure is what the caller is told about.
func TestIngressReceiveFailsWhenClearingAStrandedBroadcastFails(t *testing.T) {
	store := newFakeIngressStore()
	in := inboundFixture(t)
	store.broadcasts[in.ID] = &models.Broadcast{ID: in.ID, State: models.BroadcastStatePending}
	store.deleteErr = errors.New("database is locked")
	sender := &recordingIngressSender{}
	ingress := NewIngress(store, sender, "d-local", nil, zerolog.Nop())

	err := ingress.Receive(context.Background(), in)
	if err == nil {
		t.Fatal("expected an error when the stranded row could not be cleared")
	}
	if len(sender.calls) != 0 {
		t.Errorf("send calls = %d, want 0 when the stranded row is still there", len(sender.calls))
	}
	if strings.Contains(err.Error(), secretBody) {
		t.Fatalf("the error leaked the message body: %v", err)
	}
}

// TestIngressReceiveFailsWhenTheIdempotencyProbeErrors: only sql.ErrNoRows means
// "absent". Any other store error must fail the receive rather than silently
// re-materialising a duplicate audience under the same id.
func TestIngressReceiveFailsWhenTheIdempotencyProbeErrors(t *testing.T) {
	store := newFakeIngressStore()
	store.err = errors.New("database is locked")
	sender := &recordingIngressSender{}
	ingress := NewIngress(store, sender, "d-local", nil, zerolog.Nop())

	err := ingress.Receive(context.Background(), inboundFixture(t))
	if err == nil {
		t.Fatal("expected an error when the idempotency probe fails")
	}
	if len(sender.calls) != 0 {
		t.Errorf("send calls = %d, want 0 when the probe could not be made", len(sender.calls))
	}
	if strings.Contains(err.Error(), secretBody) {
		t.Fatalf("the error leaked the message body: %v", err)
	}
}

// ingressFixtureChats returns the local candidate set used by the
// through-the-real-resolver tests:
//
//   - chat-live   — matches repo:repo-1 and is reachable  => a target
//   - chat-broken — matches repo:repo-1 but has a StartError => NOT a target
//   - chat-other  — reachable but in repo-2                 => NOT a target
func ingressFixtureChats() ([]*models.AgentChat, map[string]*models.Session) {
	broken := chat("chat-broken", "sess-1", "claude", "acct-1", "")
	broken.StartError = strPtr("pane never came up")
	chats := []*models.AgentChat{
		chat("chat-live", "sess-1", "claude", "acct-1", ""),
		broken,
		chat("chat-other", "sess-2", "claude", "acct-1", ""),
	}
	sessions := map[string]*models.Session{
		"sess-1": {ID: "sess-1", RepoID: "repo-1"},
		"sess-2": {ID: "sess-2", RepoID: "repo-2"},
	}
	return chats, sessions
}

// newRealIngress wires an Ingress over the REAL Resolver and the REAL Sender, so
// the local-only resolution, the StartError filter and the fan-out cap are
// genuinely exercised rather than scripted away.
func newRealIngress(chats []*models.AgentChat, sessions map[string]*models.Session, logger zerolog.Logger) (*Ingress, *fakeSendStore) {
	sendStore := newFakeSendStore()
	resolver := NewResolver(&fakeChats{chats: chats}, &fakeSessions{sessions: sessions}, logger)
	sender := NewSender(sendStore, resolver, logger)
	return NewIngress(newFakeIngressStore(), sender, "d-local", nil, logger), sendStore
}

// TestIngressReceiveMaterialisesLocallyMatchingTargets is the headline: an
// inbound command becomes exactly the LOCAL delivery rows the selector matches,
// through the same resolver every local send uses.
func TestIngressReceiveMaterialisesLocallyMatchingTargets(t *testing.T) {
	chats, sessions := ingressFixtureChats()
	ingress, sendStore := newRealIngress(chats, sessions, zerolog.Nop())

	in := inboundFixture(t)
	if err := ingress.Receive(context.Background(), in); err != nil {
		t.Fatalf("Receive: %v", err)
	}

	got := sendStore.deliveries[in.ID]
	if len(got) != 1 {
		t.Fatalf("deliveries = %+v, want exactly one (chat-live)", got)
	}
	if got[0].TargetChatID != "chat-live" {
		t.Errorf("target = %q, want chat-live", got[0].TargetChatID)
	}
	for _, d := range got {
		if d.TargetChatID == "chat-broken" {
			t.Error("a chat with a StartError can never come up and must not be a target")
		}
		if d.TargetChatID == "chat-other" {
			t.Error("a chat the selector does not match must not be a target")
		}
	}
}

// TestIngressReceiveHonoursTheFanOutCap: a runaway inbound selector is loud, not
// silently truncated, and nothing is persisted.
func TestIngressReceiveHonoursTheFanOutCap(t *testing.T) {
	chats := make([]*models.AgentChat, 0, MaxTargets+1)
	for i := range MaxTargets + 1 {
		chats = append(chats, chat(fmt.Sprintf("chat-%d", i), "sess-1", "claude", "acct-1", ""))
	}
	sessions := map[string]*models.Session{"sess-1": {ID: "sess-1", RepoID: "repo-1"}}
	ingress, sendStore := newRealIngress(chats, sessions, zerolog.Nop())

	err := ingress.Receive(context.Background(), inboundFixture(t))
	if !errors.Is(err, ErrTooManyTargets) {
		t.Fatalf("err = %v, want it to satisfy errors.Is(err, ErrTooManyTargets)", err)
	}
	if len(sendStore.createdIDs()) != 0 {
		t.Errorf("created = %v, want nothing persisted for an over-cap inbound broadcast", sendStore.createdIDs())
	}
}

// TestIngressReceivePersistsUnderTheOriginBroadcastID: every daemon that
// delivers a broadcast records it under the ORIGIN's id, which is what makes a
// redelivery recognisable at all.
func TestIngressReceivePersistsUnderTheOriginBroadcastID(t *testing.T) {
	chats, sessions := ingressFixtureChats()
	ingress, sendStore := newRealIngress(chats, sessions, zerolog.Nop())

	in := inboundFixture(t)
	if err := ingress.Receive(context.Background(), in); err != nil {
		t.Fatalf("Receive: %v", err)
	}
	got := sendStore.createdIDs()
	if len(got) != 1 || got[0] != in.ID {
		t.Fatalf("created broadcast ids = %v, want [%s] (the origin's id, not a locally minted one)", got, in.ID)
	}
}

// TestIngressReceiveIncludesTheOriginChat: origin_chat_id names a chat on
// ANOTHER daemon, so the resolver's self-exclusion cannot apply. Passing
// IncludeOrigin=false would be a no-op at best and, were a local chat to share
// the id, a silent drop of the one target that matched.
func TestIngressReceiveIncludesTheOriginChat(t *testing.T) {
	store := newFakeIngressStore()
	sender := &recordingIngressSender{}
	ingress := NewIngress(store, sender, "d-local", nil, zerolog.Nop())

	in := inboundFixture(t)
	if err := ingress.Receive(context.Background(), in); err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if len(sender.calls) != 1 {
		t.Fatalf("send calls = %d, want 1", len(sender.calls))
	}
	req := sender.calls[0]
	if !req.IncludeOrigin {
		t.Error("IncludeOrigin must be true: the origin chat lives on another daemon")
	}
	if req.OriginChatID != in.OriginChatID {
		t.Errorf("OriginChatID = %q, want %q (provenance is carried verbatim)", req.OriginChatID, in.OriginChatID)
	}
	if !req.ExpiresAt.Equal(in.ExpiresAt) {
		t.Errorf("ExpiresAt = %v, want the origin's absolute expiry %v", req.ExpiresAt, in.ExpiresAt)
	}
	if req.Message != in.Message {
		t.Error("the body must reach the send seam; it is the delivery worker's only source")
	}
	if req.Selector.String() != in.Selector.String() {
		t.Errorf("Selector = %q, want %q", req.Selector.String(), in.Selector.String())
	}
}

// TestIngressReceiveNeverPublishesEgress is THE anti-storm invariant: an inbound
// broadcast must never be re-published upstream, or two daemons that each
// resolve targets for the other's selector would trade the same message forever.
//
// The assertions are STRUCTURAL, so the guarantee holds for every input rather
// than for the one command this test happens to drive: reflection proves no
// field reachable from Ingress can reach an EgressPublisher, and no parameter of
// NewIngress can accept one. A spy publisher would be strictly weaker — an
// object the ingress was never handed obviously cannot be called — so there is
// none here; the drive-through below exists only to keep the reflection
// assertions from holding vacuously over an Ingress that never ran.
func TestIngressReceiveNeverPublishesEgress(t *testing.T) {
	publisherIface := reflect.TypeOf((*EgressPublisher)(nil)).Elem()
	var assertNoPublisher func(t *testing.T, typ reflect.Type, path string, depth int)
	assertNoPublisher = func(t *testing.T, typ reflect.Type, path string, depth int) {
		t.Helper()
		if depth > 4 {
			return
		}
		if typ.Implements(publisherIface) || reflect.PointerTo(typ).Implements(publisherIface) {
			t.Fatalf("%s (%s) can reach an EgressPublisher; the ingress must hold none", path, typ)
		}
		for typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct {
			return
		}
		for i := range typ.NumField() {
			f := typ.Field(i)
			assertNoPublisher(t, f.Type, path+"."+f.Name, depth+1)
		}
	}
	assertNoPublisher(t, reflect.TypeOf(Ingress{}), "Ingress", 0)

	// ...and NewIngress's own signature cannot be widened to accept one without
	// breaking this: the constructor is the only way the production wiring builds
	// an Ingress, so pinning its parameter types closes the "hand it a publisher
	// through the door" route the struct walk above closes at the field level.
	ctor := reflect.TypeOf(NewIngress)
	for i := range ctor.NumIn() {
		if in := ctor.In(i); in.Implements(publisherIface) || reflect.PointerTo(in).Implements(publisherIface) {
			t.Fatalf("NewIngress parameter %d (%s) accepts an EgressPublisher; the ingress must take none", i, in)
		}
	}

	// ...and a fully valid command really does run to completion, materialising
	// rows, with no publisher anywhere in the picture. Without this the two
	// assertions above would hold vacuously for an Ingress that never ran.
	chats, sessions := ingressFixtureChats()
	ingress, sendStore := newRealIngress(chats, sessions, zerolog.Nop())
	in := inboundFixture(t)
	if err := ingress.Receive(context.Background(), in); err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if len(sendStore.deliveries[in.ID]) == 0 {
		t.Fatal("the command did not materialise; the assertions above would be vacuous")
	}
}

// TestIngressReceiveNeverLogsTheMessageBody covers EVERY log line the ingress
// emits — the receipt, the loop-guard drop, the redelivery no-op, the deferral
// to a concurrent sibling, the stranded-row clear — plus the error strings of a
// failure path. The body is a secret; a log line is where it would leak, and the
// criterion is per-line, so a new log statement belongs in this table.
//
// Every store fixture below carries the body on the row it seeds, so a log line
// that reached for the persisted broadcast rather than the inbound one would be
// caught too.
func TestIngressReceiveNeverLogsTheMessageBody(t *testing.T) {
	cases := []struct {
		name  string
		build func(t *testing.T, logger zerolog.Logger) (*Ingress, InboundBroadcast)
		// wantError and wantNoLog are INDEPENDENT: erroring and logging are not
		// opposites here. Deferring to a concurrent sibling does both — it logs the
		// no-op and returns the transient ErrIngressInFlight — so gating the log
		// assertions on the error would silently drop that line's leak coverage.
		wantError bool
		wantNoLog bool
	}{
		{
			name: "the success path logs a receipt",
			build: func(t *testing.T, logger zerolog.Logger) (*Ingress, InboundBroadcast) {
				t.Helper()
				chats, sessions := ingressFixtureChats()
				ingress, _ := newRealIngress(chats, sessions, logger)
				return ingress, inboundFixture(t)
			},
		},
		{
			name: "the loop guard logs a drop",
			build: func(t *testing.T, logger zerolog.Logger) (*Ingress, InboundBroadcast) {
				t.Helper()
				in := inboundFixture(t)
				in.OriginDaemonID = "d-local"
				return NewIngress(newFakeIngressStore(), &recordingIngressSender{}, "d-local", nil, logger), in
			},
		},
		{
			name: "a redelivery logs a no-op",
			build: func(t *testing.T, logger zerolog.Logger) (*Ingress, InboundBroadcast) {
				t.Helper()
				in := inboundFixture(t)
				store := newFakeIngressStore()
				store.broadcasts[in.ID] = &models.Broadcast{ID: in.ID, Message: secretBody}
				return NewIngress(store, &recordingIngressSender{}, "d-local", nil, logger), in
			},
		},
		{
			name: "deferring to a concurrent sibling logs a no-op",
			build: func(t *testing.T, logger zerolog.Logger) (*Ingress, InboundBroadcast) {
				t.Helper()
				now := time.Now().UTC()
				in := inboundFixture(t)
				store := newFakeIngressStore()
				store.broadcasts[in.ID] = &models.Broadcast{
					ID:        in.ID,
					Message:   secretBody,
					State:     models.BroadcastStatePending,
					UpdatedAt: now.Add(-time.Second),
				}
				return NewIngress(store, &recordingIngressSender{}, "d-local", func() time.Time { return now }, logger), in
			},
			// Deferring refuses the redelivery so it comes back; the sibling has
			// committed no audience yet, so an ok here could retire the command.
			wantError: true,
		},
		{
			name: "clearing a stranded row logs a warning",
			build: func(t *testing.T, logger zerolog.Logger) (*Ingress, InboundBroadcast) {
				t.Helper()
				now := time.Now().UTC()
				in := inboundFixture(t)
				store := newFakeIngressStore()
				store.broadcasts[in.ID] = &models.Broadcast{
					ID:        in.ID,
					Message:   secretBody,
					State:     models.BroadcastStatePending,
					UpdatedAt: now.Add(-2 * StrandedAfter),
				}
				return NewIngress(store, &recordingIngressSender{}, "d-local", func() time.Time { return now }, logger), in
			},
		},
		{
			name: "a failed send neither logs nor returns the body",
			build: func(t *testing.T, logger zerolog.Logger) (*Ingress, InboundBroadcast) {
				t.Helper()
				sender := &recordingIngressSender{err: errors.New("disk full")}
				return NewIngress(newFakeIngressStore(), sender, "d-local", nil, logger), inboundFixture(t)
			},
			wantError: true,
			wantNoLog: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs strings.Builder
			ingress, in := tc.build(t, zerolog.New(&logs))

			err := ingress.Receive(context.Background(), in)
			if tc.wantError && err == nil {
				t.Fatal("expected an error")
			}
			if !tc.wantError && err != nil {
				t.Fatalf("Receive: %v", err)
			}
			if err != nil && strings.Contains(err.Error(), secretBody) {
				t.Fatalf("the error leaked the message body: %v", err)
			}
			if !tc.wantNoLog {
				if logs.Len() == 0 {
					t.Fatal("nothing was logged; the leak assertion below would be vacuous")
				}
				if !strings.Contains(logs.String(), in.ID) {
					t.Fatalf("the log does not name the broadcast: %s", logs.String())
				}
			}
			if strings.Contains(logs.String(), secretBody) {
				t.Fatalf("an ingress log line contains the message body: %s", logs.String())
			}
		})
	}
}

// TestIngressReceiveSkipsAStrandedRowMaterialisedUnderIt covers the branch the
// state-guarded delete exists for.
//
// The age test is a heuristic; the DELETE's own `AND state = 'pending'` clause is
// what makes the clear safe. When a sibling commits its audience between our Get
// and our delete, DeleteUnmaterialised matches nothing and reports false — and
// that is the redelivery no-op by another name, NOT a failure. Re-resolving there
// would fan out a second audience under an id that already has one.
func TestIngressReceiveSkipsAStrandedRowMaterialisedUnderIt(t *testing.T) {
	now := time.Now().UTC()
	store := newFakeIngressStore()
	in := inboundFixture(t)
	// Aged enough to pass the heuristic...
	store.broadcasts[in.ID] = &models.Broadcast{
		ID:        in.ID,
		State:     models.BroadcastStatePending,
		UpdatedAt: now.Add(-2 * StrandedAfter),
	}
	sender := &recordingIngressSender{}
	ingress := NewIngress(store, sender, "d-local", func() time.Time { return now }, zerolog.Nop())

	// ...but a sibling resolves it in the gap before the delete lands. The fake's
	// DeleteUnmaterialised models the store's state guard, so it matches nothing.
	store.broadcasts[in.ID].State = models.BroadcastStateResolved

	if err := ingress.Receive(context.Background(), in); err != nil {
		t.Fatalf("Receive: %v, want nil: somebody else materialised it, which is the no-op", err)
	}
	if len(sender.calls) != 0 {
		t.Fatalf("send calls = %d, want 0: re-resolving would fan out a second audience under one id", len(sender.calls))
	}
	if store.broadcasts[in.ID] == nil {
		t.Fatal("the sibling's resolved broadcast was deleted; the state guard did not hold")
	}
}

// TestIngressReceiveIsSafeUnderConcurrentRedelivery drives the real thing:
// StreamClient.runAsyncCommand spawns a goroutine per command, and the stream is
// at-least-once, so N redeliveries of one broadcast id genuinely run Receive
// concurrently. Every other concurrency test in this file simulates an
// interleaving from a single goroutine; this one does not.
//
// The invariant: exactly ONE of them materialises an audience and the row
// survives. A loser may report the store's primary-key conflict — that is the
// benign lost race, and the redelivery behind it finds the row and no-ops — but
// nothing else may fail, and nothing may destroy the winner's work. (Under -race
// this also covers the shared-state question the seams raise.)
func TestIngressReceiveIsSafeUnderConcurrentRedelivery(t *testing.T) {
	const receivers = 8
	store := newConcurrentIngressStore()
	sender := &countingIngressSender{store: store}
	in := inboundFixture(t)

	var wg sync.WaitGroup
	errs := make([]error, receivers)
	for i := range receivers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ingress := NewIngress(store, sender, "d-local", nil, zerolog.Nop())
			errs[i] = ingress.Receive(context.Background(), in)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil && !errors.Is(err, errDuplicateBroadcastID) {
			t.Errorf("receiver %d: %v, want nil or the benign primary-key conflict", i, err)
		}
		if err != nil && strings.Contains(err.Error(), secretBody) {
			t.Errorf("receiver %d leaked the message body: %v", i, err)
		}
	}
	if got := sender.count(); got != 1 {
		t.Fatalf("audiences materialised = %d, want exactly 1 across %d concurrent redeliveries", got, receivers)
	}
	if !store.has(in.ID) {
		t.Fatal("the broadcast row did not survive: concurrent receives destroyed each other's work")
	}
}

// concurrentIngressStore is a mutex-guarded IngressStore that models the two
// properties the real SQLite store provides and that the concurrency test turns
// on: Get is a consistent point read, and DeleteUnmaterialised removes a row
// only while it is pending, atomically, reporting whether it did.
type concurrentIngressStore struct {
	mu         sync.Mutex
	broadcasts map[string]*models.Broadcast
}

func newConcurrentIngressStore() *concurrentIngressStore {
	return &concurrentIngressStore{broadcasts: map[string]*models.Broadcast{}}
}

func (s *concurrentIngressStore) Get(_ context.Context, id string) (*models.Broadcast, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.broadcasts[id]
	if !ok {
		return nil, sql.ErrNoRows
	}
	clone := *b
	return &clone, nil
}

func (s *concurrentIngressStore) DeleteUnmaterialised(_ context.Context, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.broadcasts[id]
	if !ok || b.State != models.BroadcastStatePending {
		return false, nil
	}
	delete(s.broadcasts, id)
	return true, nil
}

// create models the store's primary key: the first writer wins, the rest lose.
func (s *concurrentIngressStore) create(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.broadcasts[id]; exists {
		return false
	}
	s.broadcasts[id] = &models.Broadcast{ID: id, State: models.BroadcastStateResolved, UpdatedAt: time.Now().UTC()}
	return true
}

func (s *concurrentIngressStore) has(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.broadcasts[id] != nil
}

// countingIngressSender counts the audiences actually materialised, refusing a
// second one under the same id exactly as the store's primary key would.
type countingIngressSender struct {
	store *concurrentIngressStore
	mu    sync.Mutex
	n     int
}

func (s *countingIngressSender) Send(_ context.Context, req SendRequest) (*SendResult, error) {
	if !s.store.create(req.ID) {
		return nil, fmt.Errorf("create broadcast: %w", errDuplicateBroadcastID)
	}
	s.mu.Lock()
	s.n++
	s.mu.Unlock()
	return &SendResult{Broadcast: &models.Broadcast{ID: req.ID}}, nil
}

func (s *countingIngressSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

var errDuplicateBroadcastID = errors.New("UNIQUE constraint failed: broadcasts.id")
