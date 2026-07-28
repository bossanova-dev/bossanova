package server

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// subscriptionBodySentinel is a token that appears nowhere else in the
// codebase, so asserting its absence from a serialised response asserts the
// real write-only-body rule rather than a field-name spot check.
const subscriptionBodySentinel = "SUBSCRIPTION-BODY-SENTINEL-4c71"

// newSubscriptionServer builds a Server backed by a real migrated in-memory
// database — the subscription store, and the session store the owner-session
// check reads — plus one repo and one session to own subscriptions. It returns
// the id of that session.
func newSubscriptionServer(t *testing.T) (*Server, *sql.DB, string) {
	t.Helper()
	sqlDB := setupServerTestDB(t)
	ctx := context.Background()

	repos := db.NewRepoStore(sqlDB)
	sessions := db.NewSessionStore(sqlDB)
	repo, err := repos.Create(ctx, db.CreateRepoParams{
		DisplayName:       "r",
		LocalPath:         "/tmp/r",
		OriginURL:         "https://github.com/acme/r.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/wt",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	sess, err := sessions.Create(ctx, db.CreateSessionParams{
		RepoID:     repo.ID,
		Title:      "owner",
		BranchName: "feature",
		BaseBranch: "main",
		AgentName:  "claude",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	srv := &Server{
		sessions:               sessions,
		broadcastSubscriptions: db.NewBroadcastSubscriptionStore(sqlDB),
		logger:                 zerolog.Nop(),
	}
	return srv, sqlDB, sess.ID
}

func validCreateSubscriptionRequest(t *testing.T, ownerSessionID string) *pb.CreateBroadcastSubscriptionRequest {
	t.Helper()
	return &pb.CreateBroadcastSubscriptionRequest{
		OwnerSessionId: ownerSessionID,
		TriggerEvent:   models.BroadcastTriggerCompleted.String(),
		Selector:       repoSelector(t, "repo-1"),
		Message:        "the child finished",
	}
}

func TestCreateBroadcastSubscription_SelectorValidation(t *testing.T) {
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
			srv, _, owner := newSubscriptionServer(t)
			req := validCreateSubscriptionRequest(t, owner)
			req.Selector = tc.sel
			_, err := srv.CreateBroadcastSubscription(context.Background(), connect.NewRequest(req))
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := connectCode(t, err); got != connect.CodeInvalidArgument {
				t.Errorf("code = %v, want InvalidArgument", got)
			}
		})
	}
}

func TestCreateBroadcastSubscription_MessageValidation(t *testing.T) {
	for _, body := range []string{"", "   ", "\n\t "} {
		srv, _, owner := newSubscriptionServer(t)
		req := validCreateSubscriptionRequest(t, owner)
		req.Message = body
		_, err := srv.CreateBroadcastSubscription(context.Background(), connect.NewRequest(req))
		if err == nil {
			t.Fatalf("message %q: expected an error", body)
		}
		if got := connectCode(t, err); got != connect.CodeInvalidArgument {
			t.Errorf("message %q: code = %v, want InvalidArgument", body, got)
		}
	}
}

// TestCreateBroadcastSubscription_TriggerValidation pins that an unrecognised
// trigger is refused rather than registering a standing rule that could never
// match any outcome.
func TestCreateBroadcastSubscription_TriggerValidation(t *testing.T) {
	for _, trigger := range []string{"", "  ", "finished", "Completed", "merged"} {
		srv, _, owner := newSubscriptionServer(t)
		req := validCreateSubscriptionRequest(t, owner)
		req.TriggerEvent = trigger
		_, err := srv.CreateBroadcastSubscription(context.Background(), connect.NewRequest(req))
		if err == nil {
			t.Fatalf("trigger %q: expected an error", trigger)
		}
		if got := connectCode(t, err); got != connect.CodeInvalidArgument {
			t.Errorf("trigger %q: code = %v, want InvalidArgument", trigger, got)
		}
	}
}

func TestCreateBroadcastSubscription_EveryTriggerIsAccepted(t *testing.T) {
	for _, trigger := range models.BroadcastTriggers() {
		srv, _, owner := newSubscriptionServer(t)
		req := validCreateSubscriptionRequest(t, owner)
		req.TriggerEvent = trigger.String()
		resp, err := srv.CreateBroadcastSubscription(context.Background(), connect.NewRequest(req))
		if err != nil {
			t.Fatalf("trigger %q: CreateBroadcastSubscription: %v", trigger, err)
		}
		if got := resp.Msg.GetSubscription().GetTriggerEvent(); got != trigger.String() {
			t.Errorf("trigger = %q, want %q", got, trigger)
		}
	}
}

func TestCreateBroadcastSubscription_OwnerSessionValidation(t *testing.T) {
	srv, _, owner := newSubscriptionServer(t)
	ctx := context.Background()

	// Blank owner is an argument error...
	for _, blank := range []string{"", "   "} {
		req := validCreateSubscriptionRequest(t, owner)
		req.OwnerSessionId = blank
		_, err := srv.CreateBroadcastSubscription(ctx, connect.NewRequest(req))
		if err == nil {
			t.Fatalf("owner %q: expected an error", blank)
		}
		if got := connectCode(t, err); got != connect.CodeInvalidArgument {
			t.Errorf("owner %q: code = %v, want InvalidArgument", blank, got)
		}
	}

	// ...and an owner that names no session is NotFound: a subscription on a
	// session that does not exist could never fire.
	req := validCreateSubscriptionRequest(t, "no-such-session")
	_, err := srv.CreateBroadcastSubscription(ctx, connect.NewRequest(req))
	if err == nil {
		t.Fatal("expected an error for an unknown owner session")
	}
	if got := connectCode(t, err); got != connect.CodeNotFound {
		t.Errorf("unknown owner code = %v, want NotFound", got)
	}
}

func TestCreateBroadcastSubscription_ExpiresInValidation(t *testing.T) {
	for _, bad := range []string{"soon", "-1h", "0s", "31d", "5w"} {
		srv, _, owner := newSubscriptionServer(t)
		req := validCreateSubscriptionRequest(t, owner)
		req.ExpiresIn = &bad
		_, err := srv.CreateBroadcastSubscription(context.Background(), connect.NewRequest(req))
		if err == nil {
			t.Fatalf("expires_in %q: expected an error", bad)
		}
		if got := connectCode(t, err); got != connect.CodeInvalidArgument {
			t.Errorf("expires_in %q: code = %v, want InvalidArgument", bad, got)
		}
	}
}

func TestCreateBroadcastSubscription_ExpiresInDefaultsTo24h(t *testing.T) {
	srv, _, owner := newSubscriptionServer(t)
	before := time.Now().UTC()

	resp, err := srv.CreateBroadcastSubscription(context.Background(),
		connect.NewRequest(validCreateSubscriptionRequest(t, owner)))
	if err != nil {
		t.Fatalf("CreateBroadcastSubscription: %v", err)
	}
	got := resp.Msg.GetSubscription().GetExpiresAt().AsTime()
	want := before.Add(24 * time.Hour)
	if delta := got.Sub(want); delta < -time.Minute || delta > time.Minute {
		t.Errorf("default expiry = %v, want ~%v", got, want)
	}
}

func TestCreateBroadcastSubscription_ExpiresInHonouredWhenSet(t *testing.T) {
	srv, _, owner := newSubscriptionServer(t)
	before := time.Now().UTC()
	in := "7d"
	req := validCreateSubscriptionRequest(t, owner)
	req.ExpiresIn = &in

	resp, err := srv.CreateBroadcastSubscription(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("CreateBroadcastSubscription: %v", err)
	}
	got := resp.Msg.GetSubscription().GetExpiresAt().AsTime()
	want := before.Add(7 * 24 * time.Hour)
	if delta := got.Sub(want); delta < -time.Minute || delta > time.Minute {
		t.Errorf("expiry = %v, want ~%v", got, want)
	}
}

// TestBroadcastSubscription_ResponsesNeverCarryTheBody is the acceptance
// criterion for the write-only body: the registered message is planted as a
// distinctive token and must appear in NEITHER the create response NOR any list
// response, checked over the whole serialised message (binary and protojson) so
// a new field carrying it anywhere would fail. The final assertion proves the
// body really was persisted, so the absence checks are not vacuous.
func TestBroadcastSubscription_ResponsesNeverCarryTheBody(t *testing.T) {
	srv, sqlDB, owner := newSubscriptionServer(t)
	ctx := context.Background()

	req := validCreateSubscriptionRequest(t, owner)
	req.Message = "tell the coordinator: " + subscriptionBodySentinel
	req.OriginChatId = "chat-origin"

	resp, err := srv.CreateBroadcastSubscription(ctx, connect.NewRequest(req))
	if err != nil {
		t.Fatalf("CreateBroadcastSubscription: %v", err)
	}
	assertNoSubscriptionBody(t, "CreateBroadcastSubscriptionResponse", resp.Msg)

	listResp, err := srv.ListBroadcastSubscriptions(ctx,
		connect.NewRequest(&pb.ListBroadcastSubscriptionsRequest{}))
	if err != nil {
		t.Fatalf("ListBroadcastSubscriptions: %v", err)
	}
	if got := len(listResp.Msg.GetSubscriptions()); got != 1 {
		t.Fatalf("listing = %d, want 1", got)
	}
	assertNoSubscriptionBody(t, "ListBroadcastSubscriptionsResponse", listResp.Msg)

	// Filtered listings are a separate read surface; they must be body-free too.
	ownerFilter := owner
	byOwner, err := srv.ListBroadcastSubscriptions(ctx,
		connect.NewRequest(&pb.ListBroadcastSubscriptionsRequest{OwnerSessionId: &ownerFilter}))
	if err != nil {
		t.Fatalf("ListBroadcastSubscriptions(owner): %v", err)
	}
	assertNoSubscriptionBody(t, "ListBroadcastSubscriptionsResponse(owner)", byOwner.Msg)

	// The body IS stored — firing needs it — so the absence assertions above are
	// meaningful rather than vacuous.
	stored, err := db.NewBroadcastSubscriptionStore(sqlDB).Get(ctx, resp.Msg.GetSubscription().GetId())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !strings.Contains(stored.Message, subscriptionBodySentinel) {
		t.Fatal("the message body was not persisted; the leak assertions would be vacuous")
	}
}

// assertNoSubscriptionBody fails when the sentinel body appears anywhere in msg,
// checking both the binary encoding (catches any string field) and protojson
// (catches a field whose binary bytes might be split across the wire framing).
func assertNoSubscriptionBody(t *testing.T, label string, msg proto.Message) {
	t.Helper()
	raw, err := proto.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal %s: %v", label, err)
	}
	if strings.Contains(string(raw), subscriptionBodySentinel) {
		t.Errorf("%s carries the message body", label)
	}
	js, err := protojson.Marshal(msg)
	if err != nil {
		t.Fatalf("protojson marshal %s: %v", label, err)
	}
	if strings.Contains(string(js), subscriptionBodySentinel) {
		t.Errorf("%s carries the message body in its JSON form", label)
	}
}

func TestBroadcastSubscription_CreateListDeleteRoundTrip(t *testing.T) {
	srv, _, owner := newSubscriptionServer(t)
	ctx := context.Background()

	req := validCreateSubscriptionRequest(t, owner)
	req.TriggerEvent = models.BroadcastTriggerSettled.String()
	req.OriginChatId = "chat-origin"
	created, err := srv.CreateBroadcastSubscription(ctx, connect.NewRequest(req))
	if err != nil {
		t.Fatalf("CreateBroadcastSubscription: %v", err)
	}
	sub := created.Msg.GetSubscription()
	if sub.GetId() == "" {
		t.Fatal("expected a subscription id")
	}
	if sub.GetState() != models.BroadcastSubscriptionStateActive.String() {
		t.Errorf("state = %q, want active", sub.GetState())
	}
	if sub.GetOwnerSessionId() != owner {
		t.Errorf("owner = %q, want %q", sub.GetOwnerSessionId(), owner)
	}
	if sub.GetOriginChatId() != "chat-origin" {
		t.Errorf("origin = %q, want chat-origin", sub.GetOriginChatId())
	}
	if sub.GetSelector() == nil || len(sub.GetSelector().GetClauses()) != 1 {
		t.Errorf("selector = %v, want the registered one clause", sub.GetSelector())
	}
	if sub.GetFiredBroadcastId() != "" {
		t.Errorf("fired_broadcast_id = %q, want empty before firing", sub.GetFiredBroadcastId())
	}

	listed, err := srv.ListBroadcastSubscriptions(ctx,
		connect.NewRequest(&pb.ListBroadcastSubscriptionsRequest{}))
	if err != nil {
		t.Fatalf("ListBroadcastSubscriptions: %v", err)
	}
	if got := len(listed.Msg.GetSubscriptions()); got != 1 {
		t.Fatalf("listing = %d, want 1", got)
	}
	got := listed.Msg.GetSubscriptions()[0]
	if got.GetId() != sub.GetId() {
		t.Errorf("listed id = %q, want %q", got.GetId(), sub.GetId())
	}
	if got.GetTriggerEvent() != models.BroadcastTriggerSettled.String() {
		t.Errorf("listed trigger = %q, want settled", got.GetTriggerEvent())
	}
	if got.GetState() != models.BroadcastSubscriptionStateActive.String() {
		t.Errorf("listed state = %q, want active", got.GetState())
	}

	// Delete retires the rule: it is a state transition to canceled, so the row
	// survives as history but no longer matches an active listing.
	if _, err := srv.DeleteBroadcastSubscription(ctx,
		connect.NewRequest(&pb.DeleteBroadcastSubscriptionRequest{Id: sub.GetId()})); err != nil {
		t.Fatalf("DeleteBroadcastSubscription: %v", err)
	}
	active := models.BroadcastSubscriptionStateActive.String()
	stillActive, err := srv.ListBroadcastSubscriptions(ctx,
		connect.NewRequest(&pb.ListBroadcastSubscriptionsRequest{State: &active}))
	if err != nil {
		t.Fatalf("ListBroadcastSubscriptions(active): %v", err)
	}
	if n := len(stillActive.Msg.GetSubscriptions()); n != 0 {
		t.Errorf("active listing = %d, want 0 after delete", n)
	}
	after, err := srv.ListBroadcastSubscriptions(ctx,
		connect.NewRequest(&pb.ListBroadcastSubscriptionsRequest{}))
	if err != nil {
		t.Fatalf("ListBroadcastSubscriptions(after delete): %v", err)
	}
	if n := len(after.Msg.GetSubscriptions()); n != 1 {
		t.Fatalf("listing after delete = %d, want the canceled row", n)
	}
	if s := after.Msg.GetSubscriptions()[0].GetState(); s != models.BroadcastSubscriptionStateCanceled.String() {
		t.Errorf("state after delete = %q, want canceled", s)
	}

	// A second delete is a benign no-op, exactly as DeleteBroadcast's repeated
	// cancel is — and so is deleting an id that never existed.
	if _, err := srv.DeleteBroadcastSubscription(ctx,
		connect.NewRequest(&pb.DeleteBroadcastSubscriptionRequest{Id: sub.GetId()})); err != nil {
		t.Errorf("second delete: %v, want nil", err)
	}
	if _, err := srv.DeleteBroadcastSubscription(ctx,
		connect.NewRequest(&pb.DeleteBroadcastSubscriptionRequest{Id: "no-such-id"})); err != nil {
		t.Errorf("delete of an absent id: %v, want nil", err)
	}
	if _, err := srv.DeleteBroadcastSubscription(ctx,
		connect.NewRequest(&pb.DeleteBroadcastSubscriptionRequest{Id: "  "})); err == nil {
		t.Error("expected an error for a blank id")
	} else if code := connectCode(t, err); code != connect.CodeInvalidArgument {
		t.Errorf("blank id code = %v, want InvalidArgument", code)
	}
}

func TestListBroadcastSubscriptions_Filters(t *testing.T) {
	srv, sqlDB, owner := newSubscriptionServer(t)
	ctx := context.Background()

	// A second owning session so the owner filter has something to exclude.
	sessions := db.NewSessionStore(sqlDB)
	first, err := sessions.Get(ctx, owner)
	if err != nil {
		t.Fatalf("get owner session: %v", err)
	}
	other, err := sessions.Create(ctx, db.CreateSessionParams{
		RepoID:     first.RepoID,
		Title:      "other",
		BranchName: "other",
		BaseBranch: "main",
		AgentName:  "claude",
	})
	if err != nil {
		t.Fatalf("create second session: %v", err)
	}

	a := validCreateSubscriptionRequest(t, owner)
	a.OriginChatId = "chat-a"
	if _, err := srv.CreateBroadcastSubscription(ctx, connect.NewRequest(a)); err != nil {
		t.Fatalf("create a: %v", err)
	}
	b := validCreateSubscriptionRequest(t, other.ID)
	b.TriggerEvent = models.BroadcastTriggerErrored.String()
	if _, err := srv.CreateBroadcastSubscription(ctx, connect.NewRequest(b)); err != nil {
		t.Fatalf("create b: %v", err)
	}

	all, err := srv.ListBroadcastSubscriptions(ctx, connect.NewRequest(&pb.ListBroadcastSubscriptionsRequest{}))
	if err != nil {
		t.Fatalf("ListBroadcastSubscriptions: %v", err)
	}
	if got := len(all.Msg.GetSubscriptions()); got != 2 {
		t.Fatalf("unfiltered listing = %d, want 2", got)
	}

	ownerFilter := owner
	byOwner, err := srv.ListBroadcastSubscriptions(ctx,
		connect.NewRequest(&pb.ListBroadcastSubscriptionsRequest{OwnerSessionId: &ownerFilter}))
	if err != nil {
		t.Fatalf("ListBroadcastSubscriptions(owner): %v", err)
	}
	if got := len(byOwner.Msg.GetSubscriptions()); got != 1 {
		t.Errorf("owner filter = %d, want 1", got)
	}

	originFilter := "chat-a"
	byOrigin, err := srv.ListBroadcastSubscriptions(ctx,
		connect.NewRequest(&pb.ListBroadcastSubscriptionsRequest{OriginChatId: &originFilter}))
	if err != nil {
		t.Fatalf("ListBroadcastSubscriptions(origin): %v", err)
	}
	if got := len(byOrigin.Msg.GetSubscriptions()); got != 1 {
		t.Errorf("origin filter = %d, want 1", got)
	}

	trigger := models.BroadcastTriggerErrored.String()
	byTrigger, err := srv.ListBroadcastSubscriptions(ctx,
		connect.NewRequest(&pb.ListBroadcastSubscriptionsRequest{TriggerEvent: &trigger}))
	if err != nil {
		t.Fatalf("ListBroadcastSubscriptions(trigger): %v", err)
	}
	if got := len(byTrigger.Msg.GetSubscriptions()); got != 1 {
		t.Errorf("trigger filter = %d, want 1", got)
	}

	limited, err := srv.ListBroadcastSubscriptions(ctx,
		connect.NewRequest(&pb.ListBroadcastSubscriptionsRequest{Limit: 1}))
	if err != nil {
		t.Fatalf("ListBroadcastSubscriptions(limit): %v", err)
	}
	if got := len(limited.Msg.GetSubscriptions()); got != 1 {
		t.Errorf("limit = %d, want 1", got)
	}
}

// TestListBroadcastSubscriptions_UnknownFilterValuesAreRejected keeps a typo in
// a filter from reading as "none match": an unknown state or trigger is an
// argument error, mirroring ListBroadcasts.
func TestListBroadcastSubscriptions_UnknownFilterValuesAreRejected(t *testing.T) {
	srv, _, _ := newSubscriptionServer(t)
	ctx := context.Background()

	bogus := "acitve"
	if _, err := srv.ListBroadcastSubscriptions(ctx,
		connect.NewRequest(&pb.ListBroadcastSubscriptionsRequest{State: &bogus})); err == nil {
		t.Error("expected an error for an unknown state filter")
	} else if got := connectCode(t, err); got != connect.CodeInvalidArgument {
		t.Errorf("state filter code = %v, want InvalidArgument", got)
	}
	if _, err := srv.ListBroadcastSubscriptions(ctx,
		connect.NewRequest(&pb.ListBroadcastSubscriptionsRequest{TriggerEvent: &bogus})); err == nil {
		t.Error("expected an error for an unknown trigger filter")
	} else if got := connectCode(t, err); got != connect.CodeInvalidArgument {
		t.Errorf("trigger filter code = %v, want InvalidArgument", got)
	}
}

// TestBroadcastSubscriptionHandlers_UnconfiguredStore keeps a daemon without
// the subscription wiring from panicking: every handler reports Unavailable.
func TestBroadcastSubscriptionHandlers_UnconfiguredStore(t *testing.T) {
	srv := &Server{logger: zerolog.Nop()}
	ctx := context.Background()

	if _, err := srv.CreateBroadcastSubscription(ctx,
		connect.NewRequest(validCreateSubscriptionRequest(t, "sess-1"))); err == nil {
		t.Error("CreateBroadcastSubscription: expected an error")
	} else if got := connectCode(t, err); got != connect.CodeUnavailable {
		t.Errorf("CreateBroadcastSubscription code = %v, want Unavailable", got)
	}
	if _, err := srv.ListBroadcastSubscriptions(ctx,
		connect.NewRequest(&pb.ListBroadcastSubscriptionsRequest{})); err == nil {
		t.Error("ListBroadcastSubscriptions: expected an error")
	} else if got := connectCode(t, err); got != connect.CodeUnavailable {
		t.Errorf("ListBroadcastSubscriptions code = %v, want Unavailable", got)
	}
	if _, err := srv.DeleteBroadcastSubscription(ctx,
		connect.NewRequest(&pb.DeleteBroadcastSubscriptionRequest{Id: "x"})); err == nil {
		t.Error("DeleteBroadcastSubscription: expected an error")
	} else if got := connectCode(t, err); got != connect.CodeUnavailable {
		t.Errorf("DeleteBroadcastSubscription code = %v, want Unavailable", got)
	}
}
