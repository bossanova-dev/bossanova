package server

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	bcast "github.com/recurser/bossalib/broadcast"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	bcastsvc "github.com/recurser/bossd/internal/broadcast"
	"github.com/recurser/bossd/internal/db"
	"github.com/rs/zerolog"
)

// localDaemonID is the persisted id the Server under test believes is its own.
const localDaemonID = "daemon-local"

// recordingEgressPublisher captures every published event and can fail on
// demand, so a test can assert both "exactly one" and "a publish failure
// changes nothing about the local send".
type recordingEgressPublisher struct {
	mu     sync.Mutex
	events []bcastsvc.EgressEvent
	err    error
}

func (p *recordingEgressPublisher) PublishBroadcastEgress(_ context.Context, ev bcastsvc.EgressEvent) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, ev)
	return p.err
}

func (p *recordingEgressPublisher) published() []bcastsvc.EgressEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]bcastsvc.EgressEvent(nil), p.events...)
}

// newEgressBroadcastServer builds the broadcast Server of send_broadcast_test.go
// with the egress wiring attached: a known persisted daemon id and a scripted
// publisher.
func newEgressBroadcastServer(t *testing.T, pub bcastsvc.EgressPublisher) (*Server, *sql.DB) {
	t.Helper()
	srv, sqlDB := newBroadcastServer(t, twoChatFixture())
	srv.daemonID = localDaemonID
	srv.broadcastEgress = pub
	return srv, sqlDB
}

func daemonSelector(t *testing.T, expr string) *pb.BroadcastSelector {
	t.Helper()
	sel, err := bcast.Parse(expr)
	if err != nil {
		t.Fatalf("parse selector %q: %v", expr, err)
	}
	return bcast.SelectorToProto(sel)
}

// TestSendBroadcast_SelectorNamingOnlyThisDaemonPublishesNothing: a selector
// that is already fully qualified to this daemon is a local send. Routing it
// would hand every other daemon a broadcast that resolves to nobody.
func TestSendBroadcast_SelectorNamingOnlyThisDaemonPublishesNothing(t *testing.T) {
	pub := &recordingEgressPublisher{}
	srv, _ := newEgressBroadcastServer(t, pub)

	req := validSendBroadcastRequest(t)
	req.Selector = daemonSelector(t, "daemon:"+localDaemonID)

	if _, err := srv.SendBroadcast(context.Background(), connect.NewRequest(req)); err != nil {
		t.Fatalf("SendBroadcast: %v", err)
	}
	if got := len(pub.published()); got != 0 {
		t.Errorf("published %d egress events, want 0 for a purely local selector", got)
	}
}

// TestSendBroadcast_UnqualifiedSelectorPublishesNothing pins the second
// boundary: an unqualified selector must not silently fan out across the fleet.
func TestSendBroadcast_UnqualifiedSelectorPublishesNothing(t *testing.T) {
	pub := &recordingEgressPublisher{}
	srv, _ := newEgressBroadcastServer(t, pub)

	if _, err := srv.SendBroadcast(context.Background(), connect.NewRequest(validSendBroadcastRequest(t))); err != nil {
		t.Fatalf("SendBroadcast: %v", err)
	}
	if got := len(pub.published()); got != 0 {
		t.Errorf("published %d egress events, want 0 without a daemon dimension or the flag", got)
	}
}

// TestSendBroadcast_SelectorNamingAnotherDaemonPublishesExactlyOneEvent is the
// core egress criterion: one event, carrying the CONFIGURED persisted daemon id
// and the PERSISTED broadcast id (the same id every receiving daemon stores
// under).
func TestSendBroadcast_SelectorNamingAnotherDaemonPublishesExactlyOneEvent(t *testing.T) {
	pub := &recordingEgressPublisher{}
	srv, _ := newEgressBroadcastServer(t, pub)

	req := validSendBroadcastRequest(t)
	req.Selector = daemonSelector(t, "daemon:daemon-other")
	req.OriginChatId = "  chat-b  "

	resp, err := srv.SendBroadcast(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("SendBroadcast: %v", err)
	}
	events := pub.published()
	if len(events) != 1 {
		t.Fatalf("published %d egress events, want exactly 1", len(events))
	}
	ev := events[0]
	if want := resp.Msg.GetBroadcast().GetId(); ev.BroadcastID != want {
		t.Errorf("BroadcastID = %q, want the persisted broadcast id %q", ev.BroadcastID, want)
	}
	if ev.OriginDaemonID != localDaemonID {
		t.Errorf("OriginDaemonID = %q, want the configured persisted id %q", ev.OriginDaemonID, localDaemonID)
	}
	if ev.OriginChatID != "chat-b" {
		t.Errorf("OriginChatID = %q, want the trimmed origin chat id", ev.OriginChatID)
	}
	if ev.Message != req.GetMessage() {
		t.Errorf("Message = %q, want the sent body", ev.Message)
	}
	if got := ev.Selector.String(); got != "daemon:daemon-other" {
		t.Errorf("Selector = %q, want the validated selector", got)
	}
	// The same instant the send used. Compared with a sub-millisecond tolerance
	// rather than Equal: the response's value has been through SQLite, which
	// stores the timestamp at millisecond precision, while the event carries the
	// in-memory value the send computed.
	want := resp.Msg.GetBroadcast().GetExpiresAt().AsTime()
	if delta := ev.ExpiresAt.Sub(want); delta < -time.Millisecond || delta > time.Millisecond {
		t.Errorf("ExpiresAt = %v, want the send's expiry %v", ev.ExpiresAt, want)
	}
}

// TestSendBroadcast_CrossDaemonFlagPublishesExactlyOneEvent: the flag is how a
// caller asks for fan-out when the selector says nothing about daemons.
func TestSendBroadcast_CrossDaemonFlagPublishesExactlyOneEvent(t *testing.T) {
	pub := &recordingEgressPublisher{}
	srv, _ := newEgressBroadcastServer(t, pub)

	req := validSendBroadcastRequest(t) // repo:repo-1 — no daemon dimension
	req.CrossDaemon = true

	resp, err := srv.SendBroadcast(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("SendBroadcast: %v", err)
	}
	events := pub.published()
	if len(events) != 1 {
		t.Fatalf("published %d egress events, want exactly 1", len(events))
	}
	if events[0].BroadcastID != resp.Msg.GetBroadcast().GetId() {
		t.Errorf("BroadcastID = %q, want %q", events[0].BroadcastID, resp.Msg.GetBroadcast().GetId())
	}
	if events[0].OriginDaemonID != localDaemonID {
		t.Errorf("OriginDaemonID = %q, want %q", events[0].OriginDaemonID, localDaemonID)
	}
	// The local audience is unaffected by asking for cross-daemon routing.
	if got := len(resp.Msg.GetDeliveries()); got != 1 {
		t.Errorf("local deliveries = %d, want 1", got)
	}
}

// TestSendBroadcast_PublishFailureLeavesLocalDeliveriesIntact is the invariant
// that the send has ALREADY happened by the time we publish: a failure is
// logged and dropped, never returned, because failing the RPC would invite a
// retry that resolves a second audience and delivers twice.
func TestSendBroadcast_PublishFailureLeavesLocalDeliveriesIntact(t *testing.T) {
	pub := &recordingEgressPublisher{err: errors.New("upstream stream is not connected")}
	srv, sqlDB := newEgressBroadcastServer(t, pub)
	ctx := context.Background()

	req := validSendBroadcastRequest(t)
	req.CrossDaemon = true

	resp, err := srv.SendBroadcast(ctx, connect.NewRequest(req))
	if err != nil {
		t.Fatalf("SendBroadcast returned %v, want success despite the publish failure", err)
	}
	if got := len(resp.Msg.GetDeliveries()); got != 1 {
		t.Fatalf("deliveries = %d, want 1", got)
	}
	if got := resp.Msg.GetDeliveries()[0].GetTargetChatId(); got != "chat-a" {
		t.Errorf("target = %q, want chat-a", got)
	}
	// The rows are really committed, not just reported.
	rows, err := db.NewBroadcastStore(sqlDB).ListDeliveries(ctx, resp.Msg.GetBroadcast().GetId())
	if err != nil {
		t.Fatalf("ListDeliveries: %v", err)
	}
	if len(rows) != 1 || rows[0].TargetChatID != "chat-a" {
		t.Errorf("persisted deliveries = %+v, want exactly chat-a", rows)
	}
}

// TestSendBroadcast_NilPublisherStillDeliversLocally is the "local delivery
// succeeds with the upstream stream disconnected" criterion: a daemon with no
// upstream wiring has no publisher at all, and a cross-daemon send must still
// be a successful local one.
func TestSendBroadcast_NilPublisherStillDeliversLocally(t *testing.T) {
	srv, sqlDB := newBroadcastServer(t, twoChatFixture())
	srv.daemonID = localDaemonID // configured id, but no publisher
	ctx := context.Background()

	// Two clauses (+, not ,): one names a foreign daemon so the send reaches
	// beyond, the other matches a local chat so there is a local delivery to
	// assert survived.
	req := validSendBroadcastRequest(t)
	req.Selector = daemonSelector(t, "daemon:daemon-other+repo:repo-1")

	resp, err := srv.SendBroadcast(ctx, connect.NewRequest(req))
	if err != nil {
		t.Fatalf("SendBroadcast returned %v, want success with no publisher wired", err)
	}
	if resp.Msg.GetBroadcast().GetId() == "" {
		t.Fatal("expected a persisted broadcast id")
	}
	rows, err := db.NewBroadcastStore(sqlDB).ListDeliveries(ctx, resp.Msg.GetBroadcast().GetId())
	if err != nil {
		t.Fatalf("ListDeliveries: %v", err)
	}
	if len(rows) != 1 || rows[0].TargetChatID != "chat-a" {
		t.Errorf("persisted deliveries = %+v, want exactly chat-a", rows)
	}
}

// TestSendBroadcast_EgressLogsNeverCarryTheMessageBody is the log-hygiene
// criterion. The body is a secret; a publish failure is exactly the moment a
// careless `Err(err)` or a `%v` of the event would spill it into the daemon log.
func TestSendBroadcast_EgressLogsNeverCarryTheMessageBody(t *testing.T) {
	const token = "zz-secret-token-bos558"

	var logs bytes.Buffer
	pub := &recordingEgressPublisher{err: errors.New("publish failed: " + token)}
	srv, _ := newEgressBroadcastServer(t, pub)
	// Capture at the most permissive level, so a debug-level leak counts too.
	srv.logger = zerolog.New(&logs).Level(zerolog.TraceLevel)

	req := validSendBroadcastRequest(t)
	req.Message = "rebase now: " + token
	req.CrossDaemon = true

	resp, err := srv.SendBroadcast(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("SendBroadcast: %v", err)
	}
	if strings.Contains(logs.String(), token) {
		t.Errorf("the daemon log carries the broadcast body: %s", logs.String())
	}
	// The publish failure IS observable — a silent drop would make the assertion
	// above vacuous — and it names the broadcast id.
	if !strings.Contains(logs.String(), resp.Msg.GetBroadcast().GetId()) {
		t.Errorf("the publish failure was not logged with its broadcast id: %s", logs.String())
	}
	if strings.Contains(resp.Msg.String(), token) {
		t.Error("SendBroadcastResponse carries the message body")
	}
}

// TestSendBroadcast_EgressSuccessLogsIdsOnly holds the same rule on the happy
// path: a successful publish is logged, and with ids and counts only.
func TestSendBroadcast_EgressSuccessLogsIdsOnly(t *testing.T) {
	const token = "zz-secret-token-bos558-ok"

	var logs bytes.Buffer
	pub := &recordingEgressPublisher{}
	srv, _ := newEgressBroadcastServer(t, pub)
	srv.logger = zerolog.New(&logs).Level(zerolog.TraceLevel)

	req := validSendBroadcastRequest(t)
	req.Message = token
	req.CrossDaemon = true

	resp, err := srv.SendBroadcast(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("SendBroadcast: %v", err)
	}
	out := logs.String()
	if strings.Contains(out, token) {
		t.Errorf("the daemon log carries the broadcast body: %s", out)
	}
	if !strings.Contains(out, resp.Msg.GetBroadcast().GetId()) {
		t.Errorf("the successful publish was not logged with its broadcast id: %s", out)
	}
	if !strings.Contains(out, localDaemonID) {
		t.Errorf("the successful publish was not logged with its origin daemon id: %s", out)
	}
}
