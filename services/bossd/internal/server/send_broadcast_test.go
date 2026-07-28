package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	bcast "github.com/recurser/bossalib/broadcast"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	bcastsvc "github.com/recurser/bossd/internal/broadcast"
	"github.com/recurser/bossd/internal/db"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/proto"
)

// bodySentinel is a token that appears nowhere else in the codebase, so a test
// asserting its absence from a marshalled response is asserting the real
// secret-body rule rather than a field-name spot check.
const bodySentinel = "BROADCAST-BODY-SENTINEL-9f3a"

// broadcastTestChats is a mutable chatLister so a test can add a chat AFTER a
// broadcast was sent and prove the audience was frozen at send time.
type broadcastTestChats struct {
	mu    sync.Mutex
	chats []*models.AgentChat
	err   error
}

func (f *broadcastTestChats) ListRoutableChats(context.Context) ([]*models.AgentChat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return append([]*models.AgentChat(nil), f.chats...), nil
}

func (f *broadcastTestChats) add(c *models.AgentChat) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chats = append(f.chats, c)
}

// broadcastTestSessions is a sessionGetter over an in-memory map; an unknown id
// reports sql.ErrNoRows exactly as the SQLite store does.
type broadcastTestSessions struct {
	sessions map[string]*models.Session
}

func (f *broadcastTestSessions) Get(_ context.Context, id string) (*models.Session, error) {
	s, ok := f.sessions[id]
	if !ok {
		return nil, sql.ErrNoRows
	}
	return s, nil
}

func broadcastTestChat(chatID, sessionID, agentName string) *models.AgentChat {
	return &models.AgentChat{
		AgentSessionID: chatID,
		SessionID:      sessionID,
		AgentName:      agentName,
	}
}

// newBroadcastServer builds a Server backed by a real migrated in-memory
// broadcast store (so the handlers exercise the actual SQL path) and a resolver
// over the supplied chat fixtures.
func newBroadcastServer(t *testing.T, chats *broadcastTestChats) (*Server, *sql.DB) {
	t.Helper()
	sqlDB := setupServerTestDB(t)
	sessions := &broadcastTestSessions{sessions: map[string]*models.Session{
		"sess-1": {ID: "sess-1", RepoID: "repo-1"},
		"sess-2": {ID: "sess-2", RepoID: "repo-2"},
	}}
	srv := &Server{
		broadcasts:        db.NewBroadcastStore(sqlDB),
		broadcastResolver: bcastsvc.NewResolver(chats, sessions, zerolog.Nop()),
		logger:            zerolog.Nop(),
	}
	return srv, sqlDB
}

// twoChatFixture spreads two chats over two sessions/repos so a repo selector
// addresses exactly one of them.
func twoChatFixture() *broadcastTestChats {
	return &broadcastTestChats{chats: []*models.AgentChat{
		broadcastTestChat("chat-a", "sess-1", "claude"),
		broadcastTestChat("chat-b", "sess-2", "codex"),
	}}
}

func repoSelector(t *testing.T, repo string) *pb.BroadcastSelector {
	t.Helper()
	sel, err := bcast.Parse("repo:" + repo)
	if err != nil {
		t.Fatalf("parse selector: %v", err)
	}
	return bcast.SelectorToProto(sel)
}

func validSendBroadcastRequest(t *testing.T) *pb.SendBroadcastRequest {
	t.Helper()
	return &pb.SendBroadcastRequest{
		Selector: repoSelector(t, "repo-1"),
		Message:  "please rebase",
	}
}

// connectCode reports the connect code of err, failing the test when err is a
// plain error (which would otherwise report as CodeUnknown and pass nothing).
func connectCode(t *testing.T, err error) connect.Code {
	t.Helper()
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("error %v is not a *connect.Error", err)
	}
	return cerr.Code()
}

func TestSendBroadcast_SelectorValidation(t *testing.T) {
	cases := []struct {
		name string
		sel  *pb.BroadcastSelector
	}{
		{"nil selector", nil},
		{"selector with no clauses", &pb.BroadcastSelector{}},
		{"clause constraining nothing", &pb.BroadcastSelector{Clauses: []*pb.BroadcastSelectorClause{{}}}},
		{"clause with a blank value", &pb.BroadcastSelector{Clauses: []*pb.BroadcastSelectorClause{{RepoIds: []string{""}}}}},
		{"clause with a separator in a value", &pb.BroadcastSelector{Clauses: []*pb.BroadcastSelectorClause{{RepoIds: []string{"a+b"}}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newBroadcastServer(t, twoChatFixture())
			req := validSendBroadcastRequest(t)
			req.Selector = tc.sel
			_, err := srv.SendBroadcast(context.Background(), connect.NewRequest(req))
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := connectCode(t, err); got != connect.CodeInvalidArgument {
				t.Errorf("code = %v, want InvalidArgument", got)
			}
		})
	}
}

func TestSendBroadcast_MessageValidation(t *testing.T) {
	for _, body := range []string{"", "   ", "\n\t "} {
		srv, _ := newBroadcastServer(t, twoChatFixture())
		req := validSendBroadcastRequest(t)
		req.Message = body
		_, err := srv.SendBroadcast(context.Background(), connect.NewRequest(req))
		if err == nil {
			t.Fatalf("message %q: expected an error", body)
		}
		if got := connectCode(t, err); got != connect.CodeInvalidArgument {
			t.Errorf("message %q: code = %v, want InvalidArgument", body, got)
		}
	}
}

func TestSendBroadcast_ExpiresInValidation(t *testing.T) {
	for _, bad := range []string{"soon", "-1h", "0s", "31d", "5w"} {
		srv, _ := newBroadcastServer(t, twoChatFixture())
		req := validSendBroadcastRequest(t)
		req.ExpiresIn = &bad
		_, err := srv.SendBroadcast(context.Background(), connect.NewRequest(req))
		if err == nil {
			t.Fatalf("expires_in %q: expected an error", bad)
		}
		if got := connectCode(t, err); got != connect.CodeInvalidArgument {
			t.Errorf("expires_in %q: code = %v, want InvalidArgument", bad, got)
		}
	}
}

func TestSendBroadcast_ExpiresInDefaultsTo24h(t *testing.T) {
	srv, _ := newBroadcastServer(t, twoChatFixture())
	before := time.Now().UTC()

	resp, err := srv.SendBroadcast(context.Background(), connect.NewRequest(validSendBroadcastRequest(t)))
	if err != nil {
		t.Fatalf("SendBroadcast: %v", err)
	}
	got := resp.Msg.GetBroadcast().GetExpiresAt().AsTime()
	want := before.Add(24 * time.Hour)
	if delta := got.Sub(want); delta < -time.Minute || delta > time.Minute {
		t.Errorf("default expiry = %v, want ~%v", got, want)
	}
}

func TestSendBroadcast_ExpiresInHonouredWhenSet(t *testing.T) {
	srv, _ := newBroadcastServer(t, twoChatFixture())
	before := time.Now().UTC()
	in := "7d"
	req := validSendBroadcastRequest(t)
	req.ExpiresIn = &in

	resp, err := srv.SendBroadcast(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("SendBroadcast: %v", err)
	}
	got := resp.Msg.GetBroadcast().GetExpiresAt().AsTime()
	want := before.Add(7 * 24 * time.Hour)
	if delta := got.Sub(want); delta < -time.Minute || delta > time.Minute {
		t.Errorf("expiry = %v, want ~%v", got, want)
	}
}

func TestSendBroadcast_HappyPathPersistsFrozenAudience(t *testing.T) {
	chats := twoChatFixture()
	srv, sqlDB := newBroadcastServer(t, chats)
	ctx := context.Background()

	resp, err := srv.SendBroadcast(ctx, connect.NewRequest(validSendBroadcastRequest(t)))
	if err != nil {
		t.Fatalf("SendBroadcast: %v", err)
	}
	b := resp.Msg.GetBroadcast()
	if b.GetId() == "" {
		t.Fatal("expected a broadcast id")
	}
	if b.GetState() != models.BroadcastStateResolved.String() {
		t.Errorf("state = %q, want resolved", b.GetState())
	}
	if got := len(resp.Msg.GetDeliveries()); got != 1 {
		t.Fatalf("deliveries = %d, want 1", got)
	}
	d := resp.Msg.GetDeliveries()[0]
	if d.GetTargetChatId() != "chat-a" {
		t.Errorf("target = %q, want chat-a", d.GetTargetChatId())
	}
	if d.GetBroadcastId() != b.GetId() {
		t.Errorf("delivery broadcast id = %q, want %q", d.GetBroadcastId(), b.GetId())
	}
	if d.GetState() != models.BroadcastDeliveryStatePending.String() {
		t.Errorf("delivery state = %q, want pending", d.GetState())
	}

	// A chat created AFTER the send is not retroactively in the audience: a
	// broadcast is a message to the chats that matched when it was sent.
	chats.add(broadcastTestChat("chat-late", "sess-1", "claude"))

	store := db.NewBroadcastStore(sqlDB)
	rows, err := store.ListDeliveries(ctx, b.GetId())
	if err != nil {
		t.Fatalf("ListDeliveries: %v", err)
	}
	if len(rows) != 1 || rows[0].TargetChatID != "chat-a" {
		t.Fatalf("persisted deliveries = %+v, want exactly chat-a", rows)
	}
}

func TestSendBroadcast_OriginChatIsExcludedByDefault(t *testing.T) {
	chats := &broadcastTestChats{chats: []*models.AgentChat{
		broadcastTestChat("chat-a", "sess-1", "claude"),
		broadcastTestChat("chat-origin", "sess-1", "claude"),
	}}
	srv, _ := newBroadcastServer(t, chats)

	req := validSendBroadcastRequest(t)
	req.OriginChatId = "chat-origin"
	resp, err := srv.SendBroadcast(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("SendBroadcast: %v", err)
	}
	if got := len(resp.Msg.GetDeliveries()); got != 1 {
		t.Fatalf("deliveries = %d, want 1 (origin excluded)", got)
	}
	if got := resp.Msg.GetDeliveries()[0].GetTargetChatId(); got != "chat-a" {
		t.Errorf("target = %q, want chat-a", got)
	}
	if got := resp.Msg.GetBroadcast().GetOriginChatId(); got != "chat-origin" {
		t.Errorf("origin = %q, want chat-origin", got)
	}

	req.IncludeOrigin = true
	resp, err = srv.SendBroadcast(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("SendBroadcast(include_origin): %v", err)
	}
	if got := len(resp.Msg.GetDeliveries()); got != 2 {
		t.Errorf("deliveries = %d, want 2 with include_origin", got)
	}
}

// TestSendBroadcast_ZeroTargetsIsSuccess pins the proto contract: a selector
// matching nobody is a successful send with zero deliveries, never an error.
func TestSendBroadcast_ZeroTargetsIsSuccess(t *testing.T) {
	srv, sqlDB := newBroadcastServer(t, twoChatFixture())
	ctx := context.Background()

	req := validSendBroadcastRequest(t)
	req.Selector = repoSelector(t, "repo-nobody")

	resp, err := srv.SendBroadcast(ctx, connect.NewRequest(req))
	if err != nil {
		t.Fatalf("SendBroadcast: %v", err)
	}
	if got := len(resp.Msg.GetDeliveries()); got != 0 {
		t.Errorf("deliveries = %d, want 0", got)
	}
	b := resp.Msg.GetBroadcast()
	if b.GetId() == "" {
		t.Fatal("expected a persisted broadcast id")
	}
	// An empty audience settles immediately rather than sitting resolved with
	// nothing to complete it.
	if b.GetState() != models.BroadcastStateCompleted.String() {
		t.Errorf("state = %q, want completed", b.GetState())
	}
	stored, err := db.NewBroadcastStore(sqlDB).Get(ctx, b.GetId())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.TargetCount != 0 {
		t.Errorf("target_count = %d, want 0", stored.TargetCount)
	}
	if stored.State != models.BroadcastStateCompleted {
		t.Errorf("persisted state = %q, want completed", stored.State)
	}
}

// TestSendBroadcast_ResponseNeverCarriesTheMessageBody is the hard acceptance
// criterion: the body is a secret and must not appear anywhere on a read
// surface. The whole marshalled response is searched, not just Broadcast.message.
func TestSendBroadcast_ResponseNeverCarriesTheMessageBody(t *testing.T) {
	srv, sqlDB := newBroadcastServer(t, twoChatFixture())
	ctx := context.Background()

	req := validSendBroadcastRequest(t)
	req.Message = "rebase now: " + bodySentinel

	resp, err := srv.SendBroadcast(ctx, connect.NewRequest(req))
	if err != nil {
		t.Fatalf("SendBroadcast: %v", err)
	}
	raw, err := proto.Marshal(resp.Msg)
	if err != nil {
		t.Fatalf("marshal send response: %v", err)
	}
	if strings.Contains(string(raw), bodySentinel) {
		t.Error("SendBroadcastResponse carries the message body")
	}
	if got := resp.Msg.GetBroadcast().GetMessage(); got != "" {
		t.Errorf("Broadcast.message = %q, want empty", got)
	}

	listResp, err := srv.ListBroadcasts(ctx, connect.NewRequest(&pb.ListBroadcastsRequest{}))
	if err != nil {
		t.Fatalf("ListBroadcasts: %v", err)
	}
	listRaw, err := proto.Marshal(listResp.Msg)
	if err != nil {
		t.Fatalf("marshal list response: %v", err)
	}
	if strings.Contains(string(listRaw), bodySentinel) {
		t.Error("ListBroadcastsResponse carries the message body")
	}

	// The body must not have leaked into a delivery diagnostic either.
	rows, err := sqlDB.QueryContext(ctx, "SELECT COALESCE(last_error, '') FROM broadcast_deliveries")
	if err != nil {
		t.Fatalf("query last_error: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var lastError string
		if err := rows.Scan(&lastError); err != nil {
			t.Fatalf("scan last_error: %v", err)
		}
		if strings.Contains(lastError, bodySentinel) {
			t.Error("broadcast_deliveries.last_error carries the message body")
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate last_error: %v", err)
	}

	// The body IS stored (the delivery worker is the only consumer), so the
	// absence assertions above are meaningful rather than vacuous.
	stored, err := db.NewBroadcastStore(sqlDB).Get(ctx, resp.Msg.GetBroadcast().GetId())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !strings.Contains(stored.Message, bodySentinel) {
		t.Fatal("the message body was not persisted; the leak assertions would be vacuous")
	}
}

// TestSendBroadcast_OverTheFanOutCapIsRefused pins refusal, not truncation.
func TestSendBroadcast_OverTheFanOutCapIsRefused(t *testing.T) {
	over := bcastsvc.MaxTargets + 1
	chats := &broadcastTestChats{}
	for i := 0; i < over; i++ {
		chats.add(broadcastTestChat(fmt.Sprintf("chat-%03d", i), "sess-1", "claude"))
	}
	srv, sqlDB := newBroadcastServer(t, chats)
	ctx := context.Background()

	_, err := srv.SendBroadcast(ctx, connect.NewRequest(validSendBroadcastRequest(t)))
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := connectCode(t, err); got != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", got)
	}
	msg := err.Error()
	if !strings.Contains(msg, fmt.Sprint(over)) {
		t.Errorf("error %q does not name the resolved count %d", msg, over)
	}
	if !strings.Contains(msg, fmt.Sprint(bcastsvc.MaxTargets)) {
		t.Errorf("error %q does not name the cap %d", msg, bcastsvc.MaxTargets)
	}

	// Refusal is total: nothing was persisted, so no partial audience exists.
	stored, err := db.NewBroadcastStore(sqlDB).List(ctx, db.ListBroadcastsFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(stored) != 0 {
		t.Errorf("persisted %d broadcasts, want 0 on refusal", len(stored))
	}
}

func TestListBroadcasts_Filters(t *testing.T) {
	srv, _ := newBroadcastServer(t, twoChatFixture())
	ctx := context.Background()

	req := validSendBroadcastRequest(t)
	req.OriginChatId = "chat-b"
	first, err := srv.SendBroadcast(ctx, connect.NewRequest(req))
	if err != nil {
		t.Fatalf("SendBroadcast: %v", err)
	}
	second := validSendBroadcastRequest(t)
	second.Selector = repoSelector(t, "repo-2")
	if _, err := srv.SendBroadcast(ctx, connect.NewRequest(second)); err != nil {
		t.Fatalf("SendBroadcast(second): %v", err)
	}

	all, err := srv.ListBroadcasts(ctx, connect.NewRequest(&pb.ListBroadcastsRequest{}))
	if err != nil {
		t.Fatalf("ListBroadcasts: %v", err)
	}
	if got := len(all.Msg.GetBroadcasts()); got != 2 {
		t.Fatalf("unfiltered listing = %d, want 2", got)
	}

	origin := "chat-b"
	byOrigin, err := srv.ListBroadcasts(ctx, connect.NewRequest(&pb.ListBroadcastsRequest{OriginChatId: &origin}))
	if err != nil {
		t.Fatalf("ListBroadcasts(origin): %v", err)
	}
	if got := len(byOrigin.Msg.GetBroadcasts()); got != 1 {
		t.Fatalf("origin filter = %d, want 1", got)
	}
	if got := byOrigin.Msg.GetBroadcasts()[0].GetId(); got != first.Msg.GetBroadcast().GetId() {
		t.Errorf("origin filter returned %q, want %q", got, first.Msg.GetBroadcast().GetId())
	}

	target := "chat-a"
	byTarget, err := srv.ListBroadcasts(ctx, connect.NewRequest(&pb.ListBroadcastsRequest{TargetChatId: &target}))
	if err != nil {
		t.Fatalf("ListBroadcasts(target): %v", err)
	}
	if got := len(byTarget.Msg.GetBroadcasts()); got != 1 {
		t.Errorf("target filter = %d, want 1", got)
	}

	state := models.BroadcastStateResolved.String()
	byState, err := srv.ListBroadcasts(ctx, connect.NewRequest(&pb.ListBroadcastsRequest{State: &state}))
	if err != nil {
		t.Fatalf("ListBroadcasts(state): %v", err)
	}
	if got := len(byState.Msg.GetBroadcasts()); got != 2 {
		t.Errorf("state filter = %d, want 2", got)
	}

	limited, err := srv.ListBroadcasts(ctx, connect.NewRequest(&pb.ListBroadcastsRequest{Limit: 1}))
	if err != nil {
		t.Fatalf("ListBroadcasts(limit): %v", err)
	}
	if got := len(limited.Msg.GetBroadcasts()); got != 1 {
		t.Errorf("limit = %d, want 1", got)
	}
}

// TestListBroadcasts_UnknownStateIsRejected: an unparseable state must be an
// argument error, not a silently empty result that reads like "none match".
func TestListBroadcasts_UnknownStateIsRejected(t *testing.T) {
	srv, _ := newBroadcastServer(t, twoChatFixture())
	bogus := "sent"
	_, err := srv.ListBroadcasts(context.Background(), connect.NewRequest(&pb.ListBroadcastsRequest{State: &bogus}))
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := connectCode(t, err); got != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", got)
	}
}

func TestDeleteBroadcast(t *testing.T) {
	srv, sqlDB := newBroadcastServer(t, twoChatFixture())
	ctx := context.Background()

	resp, err := srv.SendBroadcast(ctx, connect.NewRequest(validSendBroadcastRequest(t)))
	if err != nil {
		t.Fatalf("SendBroadcast: %v", err)
	}
	id := resp.Msg.GetBroadcast().GetId()

	if _, err := srv.DeleteBroadcast(ctx, connect.NewRequest(&pb.DeleteBroadcastRequest{Id: id})); err != nil {
		t.Fatalf("DeleteBroadcast: %v", err)
	}
	if _, err := db.NewBroadcastStore(sqlDB).Get(ctx, id); err == nil {
		t.Error("broadcast still present after delete")
	}
	// Deliveries go with it via ON DELETE CASCADE.
	remaining, err := db.NewBroadcastStore(sqlDB).ListDeliveries(ctx, id)
	if err != nil {
		t.Fatalf("ListDeliveries: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("deliveries = %d, want 0 after delete", len(remaining))
	}

	// Idempotent: deleting an absent id succeeds, per the RPC's documented
	// contract and the GithubCallback precedent, so repeated cancel is safe.
	if _, err := srv.DeleteBroadcast(ctx, connect.NewRequest(&pb.DeleteBroadcastRequest{Id: "no-such-id"})); err != nil {
		t.Errorf("delete of an absent id: %v, want nil", err)
	}

	if _, err := srv.DeleteBroadcast(ctx, connect.NewRequest(&pb.DeleteBroadcastRequest{Id: "  "})); err == nil {
		t.Error("expected an error for a blank id")
	} else if got := connectCode(t, err); got != connect.CodeInvalidArgument {
		t.Errorf("blank id code = %v, want InvalidArgument", got)
	}
}

// TestBroadcastHandlers_UnconfiguredStore keeps a daemon without the broadcast
// wiring from panicking: every handler reports Unavailable instead.
func TestBroadcastHandlers_UnconfiguredStore(t *testing.T) {
	srv := &Server{logger: zerolog.Nop()}
	ctx := context.Background()

	if _, err := srv.SendBroadcast(ctx, connect.NewRequest(validSendBroadcastRequest(t))); err == nil {
		t.Error("SendBroadcast: expected an error")
	} else if got := connectCode(t, err); got != connect.CodeUnavailable {
		t.Errorf("SendBroadcast code = %v, want Unavailable", got)
	}
	if _, err := srv.ListBroadcasts(ctx, connect.NewRequest(&pb.ListBroadcastsRequest{})); err == nil {
		t.Error("ListBroadcasts: expected an error")
	} else if got := connectCode(t, err); got != connect.CodeUnavailable {
		t.Errorf("ListBroadcasts code = %v, want Unavailable", got)
	}
	if _, err := srv.DeleteBroadcast(ctx, connect.NewRequest(&pb.DeleteBroadcastRequest{Id: "x"})); err == nil {
		t.Error("DeleteBroadcast: expected an error")
	} else if got := connectCode(t, err); got != connect.CodeUnavailable {
		t.Errorf("DeleteBroadcast code = %v, want Unavailable", got)
	}
}

// failingDeliveriesStore wraps a real store and fails CreateDeliveries, so the
// post-Create cleanup path can be exercised directly rather than through a
// contrived fixture. It records whether the cleanup Delete ran.
type failingDeliveriesStore struct {
	broadcastStore
	err error
	// cancel, when set, is invoked as CreateDeliveries fails, so the request
	// context is cancelled at exactly the point the handler reaches the cleanup
	// — not before Create, which database/sql refuses outright.
	cancel  context.CancelFunc
	deleted []string
}

func (f *failingDeliveriesStore) CreateDeliveries(context.Context, string, []models.BroadcastDelivery) error {
	if f.cancel != nil {
		f.cancel()
	}
	return f.err
}

func (f *failingDeliveriesStore) Delete(ctx context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return f.broadcastStore.Delete(ctx, id)
}

// TestSendBroadcast_FailedAudienceLeavesNoOrphan verifies a CreateDeliveries
// failure leaves nothing behind. Create commits in its own transaction, so
// without cleanup the send would strand a pending broadcast that no worker
// drives -- holding its secret body at rest until the lazy sweep retired a
// never-sent message as `expired`.
func TestSendBroadcast_FailedAudienceLeavesNoOrphan(t *testing.T) {
	srv, sqlDB := newBroadcastServer(t, twoChatFixture())
	failing := &failingDeliveriesStore{broadcastStore: srv.broadcasts, err: errors.New("database is locked")}
	srv.broadcasts = failing

	_, err := srv.SendBroadcast(context.Background(), connect.NewRequest(validSendBroadcastRequest(t)))
	if err == nil {
		t.Fatal("SendBroadcast succeeded, want the audience insert to be reported")
	}
	if connectCode(t, err) != connect.CodeInternal {
		t.Errorf("code = %v, want Internal", connectCode(t, err))
	}
	if len(failing.deleted) != 1 {
		t.Errorf("cleanup Delete called %d times, want 1", len(failing.deleted))
	}

	var n int
	if qerr := sqlDB.QueryRow("SELECT COUNT(*) FROM broadcasts").Scan(&n); qerr != nil {
		t.Fatalf("count broadcasts: %v", qerr)
	}
	if n != 0 {
		t.Errorf("broadcasts rows = %d, want 0 (a failed send must leave no orphan)", n)
	}
}

// TestSendBroadcast_CleanupRunsOnACancelledRequest verifies the cleanup is
// detached from the request context: a cancelled context is one of the likelier
// ways CreateDeliveries fails at all, and the orphan must still be removed.
func TestSendBroadcast_CleanupRunsOnACancelledRequest(t *testing.T) {
	srv, sqlDB := newBroadcastServer(t, twoChatFixture())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancel as CreateDeliveries fails: cancelling up front would make the
	// earlier Create fail instead (database/sql checks ctx before taking a
	// connection), so the cleanup path would never be reached and the test would
	// pass vacuously.
	failing := &failingDeliveriesStore{broadcastStore: srv.broadcasts, err: context.Canceled, cancel: cancel}
	srv.broadcasts = failing

	if _, err := srv.SendBroadcast(ctx, connect.NewRequest(validSendBroadcastRequest(t))); err == nil {
		t.Fatal("SendBroadcast succeeded, want an error")
	}
	if len(failing.deleted) != 1 {
		t.Fatalf("cleanup Delete called %d times, want 1 (it must survive the cancellation)", len(failing.deleted))
	}

	var n int
	if qerr := sqlDB.QueryRow("SELECT COUNT(*) FROM broadcasts").Scan(&n); qerr != nil {
		t.Fatalf("count broadcasts: %v", qerr)
	}
	if n != 0 {
		t.Errorf("broadcasts rows = %d, want 0 (cleanup must survive a cancelled request)", n)
	}
}

// TestSendBroadcast_BlankChatIDIsSkippedNotFatal verifies one malformed chat row
// costs a single unreachable target rather than failing every broadcast: a blank
// target chat id makes CreateDeliveries reject the WHOLE batch.
func TestSendBroadcast_BlankChatIDIsSkippedNotFatal(t *testing.T) {
	chats := &broadcastTestChats{chats: []*models.AgentChat{
		broadcastTestChat("", "sess-1", "claude"),
		broadcastTestChat("chat-a", "sess-1", "claude"),
	}}
	srv, _ := newBroadcastServer(t, chats)

	res, err := srv.SendBroadcast(context.Background(), connect.NewRequest(validSendBroadcastRequest(t)))
	if err != nil {
		t.Fatalf("SendBroadcast: %v", err)
	}
	deliveries := res.Msg.GetDeliveries()
	if len(deliveries) != 1 {
		t.Fatalf("deliveries = %d, want 1 (the blank-id chat is skipped)", len(deliveries))
	}
	if got := deliveries[0].GetTargetChatId(); got != "chat-a" {
		t.Errorf("target = %q, want chat-a", got)
	}
}
