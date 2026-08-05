package server

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/config"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sqlutil"
	libtelemetry "github.com/recurser/bossalib/telemetry"
	"github.com/recurser/bossd/internal/db"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type callbackExpiryTelemetryRecorder struct{ properties []map[string]any }

func (r *callbackExpiryTelemetryRecorder) Capture(_ context.Context, _ libtelemetry.Event, _ string, properties map[string]any) {
	r.properties = append(r.properties, properties)
}
func (*callbackExpiryTelemetryRecorder) Identify(context.Context, string, map[string]any) {}
func (*callbackExpiryTelemetryRecorder) Alias(context.Context, string, string)            {}
func (*callbackExpiryTelemetryRecorder) Close()                                           {}

func enableGithubCallbackTelemetry(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("BOSS_SETTINGS_PATH", filepath.Join(t.TempDir(), "settings.json"))
	settings := config.DefaultSettings()
	settings.EventTracingEnabled = true
	if err := config.Save(settings); err != nil {
		t.Fatalf("enable telemetry: %v", err)
	}
}

// newGithubCallbackServerWithDB builds a Server backed by a real migrated
// in-memory callback store so the RPC handlers exercise the actual SQL path,
// and returns the underlying DB for tests that need to reach past the store's
// validation (e.g. backdating expires_at to simulate the passage of time).
func newGithubCallbackServerWithDB(t *testing.T) (*Server, *sql.DB) {
	t.Helper()
	sqlDB := setupServerTestDB(t)
	srv := &Server{
		githubCallbacks: db.NewGithubCallbackStore(sqlDB),
		logger:          zerolog.Nop(),
	}
	return srv, sqlDB
}

// newGithubCallbackServer builds a Server backed by a real migrated in-memory
// callback store so the RPC handlers exercise the actual SQL path.
func newGithubCallbackServer(t *testing.T) *Server {
	t.Helper()
	srv, _ := newGithubCallbackServerWithDB(t)
	return srv
}

func validCreateGithubCallbackRequest() *pb.CreateGithubCallbackRequest {
	return &pb.CreateGithubCallbackRequest{
		TargetChatId: "chat-1",
		RepoOwner:    "Owner",
		RepoName:     "Repo",
		PrNumber:     42,
		Trigger:      string(models.GithubCallbackTriggerMerged),
		Message:      "secret prompt body",
	}
}

func TestCreateGithubCallback_HappyPathAppliesDefaults(t *testing.T) {
	srv := newGithubCallbackServer(t)
	ctx := context.Background()

	before := time.Now().UTC()
	resp, err := srv.CreateGithubCallback(ctx, connect.NewRequest(validCreateGithubCallbackRequest()))
	if err != nil {
		t.Fatalf("CreateGithubCallback: %v", err)
	}
	cb := resp.Msg.GithubCallback
	if cb == nil {
		t.Fatal("response callback is nil")
	}
	if cb.Id == "" {
		t.Error("expected a non-empty id")
	}
	if cb.State != string(models.GithubCallbackStateActive) {
		t.Errorf("state = %q, want active", cb.State)
	}
	// Repo owner/name are normalized to lowercase by the store.
	if cb.RepoOwner != "owner" || cb.RepoName != "repo" {
		t.Errorf("repo = %s/%s, want owner/repo", cb.RepoOwner, cb.RepoName)
	}
	if cb.Message != "secret prompt body" {
		t.Errorf("message round-trip = %q", cb.Message)
	}
	if cb.Trigger != string(models.GithubCallbackTriggerMerged) {
		t.Errorf("trigger = %q, want merged", cb.Trigger)
	}
	if cb.ExpiresAt == nil {
		t.Fatal("expected a default expiry")
	}
	// Default expiry is ~24h out.
	gotExpiry := cb.ExpiresAt.AsTime()
	wantExpiry := before.Add(db.GithubCallbackDefaultExpiry)
	if delta := gotExpiry.Sub(wantExpiry); delta < -time.Minute || delta > time.Minute {
		t.Errorf("default expiry = %v, want ~%v", gotExpiry, wantExpiry)
	}
	if cb.AttemptCount != 0 {
		t.Errorf("attempt_count = %d, want 0", cb.AttemptCount)
	}
}

func TestCreateGithubCallback_ExplicitExpiryRoundTrips(t *testing.T) {
	srv := newGithubCallbackServer(t)
	ctx := context.Background()

	exp := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Second)
	req := validCreateGithubCallbackRequest()
	req.ExpiresAt = timestamppb.New(exp)
	group := "grp-1"
	req.GroupId = &group

	resp, err := srv.CreateGithubCallback(ctx, connect.NewRequest(req))
	if err != nil {
		t.Fatalf("CreateGithubCallback: %v", err)
	}
	cb := resp.Msg.GithubCallback
	if got := cb.ExpiresAt.AsTime(); !got.Equal(exp) {
		t.Errorf("expiry = %v, want %v", got, exp)
	}
	if cb.GroupId != group {
		t.Errorf("group_id = %q, want %q", cb.GroupId, group)
	}
}

func TestCreateGithubCallback_Validation(t *testing.T) {
	future := timestamppb.New(time.Now().UTC().Add(24 * time.Hour))
	cases := []struct {
		name   string
		mutate func(*pb.CreateGithubCallbackRequest)
	}{
		{"unknown trigger", func(r *pb.CreateGithubCallbackRequest) { r.Trigger = "exploded" }},
		{"missing target chat", func(r *pb.CreateGithubCallbackRequest) { r.TargetChatId = "" }},
		{"missing repo owner", func(r *pb.CreateGithubCallbackRequest) { r.RepoOwner = "" }},
		{"non-positive pr", func(r *pb.CreateGithubCallbackRequest) { r.PrNumber = 0 }},
		{"empty message", func(r *pb.CreateGithubCallbackRequest) { r.Message = "" }},
		{"past expiry", func(r *pb.CreateGithubCallbackRequest) {
			r.ExpiresAt = timestamppb.New(time.Now().UTC().Add(-time.Hour))
		}},
		{"expiry beyond 30 days", func(r *pb.CreateGithubCallbackRequest) {
			r.ExpiresAt = timestamppb.New(time.Now().UTC().Add(31 * 24 * time.Hour))
		}},
		{"future expiry is accepted (control)", func(r *pb.CreateGithubCallbackRequest) { r.ExpiresAt = future }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newGithubCallbackServer(t)
			req := validCreateGithubCallbackRequest()
			tc.mutate(req)
			_, err := srv.CreateGithubCallback(context.Background(), connect.NewRequest(req))
			if tc.name == "future expiry is accepted (control)" {
				if err != nil {
					t.Fatalf("control case should succeed, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error for %s", tc.name)
			}
			if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
				t.Fatalf("code = %v, want InvalidArgument (%s)", got, tc.name)
			}
		})
	}
}

func TestListGithubCallbacks_OrderingAndFilters(t *testing.T) {
	srv := newGithubCallbackServer(t)
	ctx := context.Background()

	// Three callbacks: two for chat-a on repo owner/repo, one for chat-b.
	mk := func(chat, owner, name string, pr int32, trigger models.GithubCallbackTrigger, msg string) string {
		r := &pb.CreateGithubCallbackRequest{
			TargetChatId: chat, RepoOwner: owner, RepoName: name,
			PrNumber: pr, Trigger: string(trigger), Message: msg,
		}
		resp, err := srv.CreateGithubCallback(ctx, connect.NewRequest(r))
		if err != nil {
			t.Fatalf("create %s: %v", msg, err)
		}
		return resp.Msg.GithubCallback.Id
	}
	_ = mk("chat-a", "o", "r", 1, models.GithubCallbackTriggerMerged, "first")
	id2 := mk("chat-a", "o", "r", 2, models.GithubCallbackTriggerClosed, "second")
	_ = mk("chat-b", "o2", "r2", 3, models.GithubCallbackTriggerChecksPassed, "third")

	// Unfiltered: all three, in the store's documented (created_at, id) order.
	all, err := srv.ListGithubCallbacks(ctx, connect.NewRequest(&pb.ListGithubCallbacksRequest{}))
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	got := all.Msg.GithubCallbacks
	if len(got) != 3 {
		t.Fatalf("list all = %d, want 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		prev, cur := got[i-1], got[i]
		pt, ct := prev.CreatedAt.AsTime(), cur.CreatedAt.AsTime()
		if ct.Before(pt) || (ct.Equal(pt) && cur.Id < prev.Id) {
			t.Errorf("ordering violated at %d: (%v,%s) then (%v,%s)", i, pt, prev.Id, ct, cur.Id)
		}
	}
	// Secret message round-trips through the list path (find by id, order-agnostic).
	msgByID := map[string]string{}
	for _, cb := range got {
		msgByID[cb.Id] = cb.Message
	}
	if msgByID[id2] != "second" {
		t.Errorf("message for id2 = %q, want second", msgByID[id2])
	}

	// Filter by chat.
	chat := "chat-a"
	byChat, err := srv.ListGithubCallbacks(ctx, connect.NewRequest(&pb.ListGithubCallbacksRequest{TargetChatId: &chat}))
	if err != nil {
		t.Fatalf("list by chat: %v", err)
	}
	if len(byChat.Msg.GithubCallbacks) != 2 {
		t.Fatalf("list by chat = %d, want 2", len(byChat.Msg.GithubCallbacks))
	}

	// Filter by trigger.
	trig := string(models.GithubCallbackTriggerClosed)
	byTrigger, err := srv.ListGithubCallbacks(ctx, connect.NewRequest(&pb.ListGithubCallbacksRequest{Trigger: &trig}))
	if err != nil {
		t.Fatalf("list by trigger: %v", err)
	}
	if len(byTrigger.Msg.GithubCallbacks) != 1 || byTrigger.Msg.GithubCallbacks[0].Id != id2 {
		t.Fatalf("list by trigger = %+v, want only id2", byTrigger.Msg.GithubCallbacks)
	}

	// Filter by state=active matches all; state=delivered matches none.
	active := string(models.GithubCallbackStateActive)
	byActive, err := srv.ListGithubCallbacks(ctx, connect.NewRequest(&pb.ListGithubCallbacksRequest{State: &active}))
	if err != nil {
		t.Fatalf("list by state: %v", err)
	}
	if len(byActive.Msg.GithubCallbacks) != 3 {
		t.Errorf("list by active = %d, want 3", len(byActive.Msg.GithubCallbacks))
	}
	delivered := string(models.GithubCallbackStateDelivered)
	byDelivered, err := srv.ListGithubCallbacks(ctx, connect.NewRequest(&pb.ListGithubCallbacksRequest{State: &delivered}))
	if err != nil {
		t.Fatalf("list by delivered: %v", err)
	}
	if len(byDelivered.Msg.GithubCallbacks) != 0 {
		t.Errorf("list by delivered = %d, want 0", len(byDelivered.Msg.GithubCallbacks))
	}
}

func TestListGithubCallbacks_ExpiresOverdueBeforeReading(t *testing.T) {
	enableGithubCallbackTelemetry(t)
	srv, sqlDB := newGithubCallbackServerWithDB(t)
	recorder := &callbackExpiryTelemetryRecorder{}
	srv.telemetry = recorder
	ctx := context.Background()

	// Create through the RPC (24h default expiry, active state).
	resp, err := srv.CreateGithubCallback(ctx, connect.NewRequest(validCreateGithubCallbackRequest()))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id := resp.Msg.GithubCallback.Id

	// Backdate expires_at into the past. Create rejects a past expiry, so rewrite
	// the column directly to simulate the callback outliving its deadline.
	past := sqlutil.FormatTime(time.Now().UTC().Add(-time.Hour))
	if _, err := sqlDB.ExecContext(ctx,
		"UPDATE github_callbacks SET expires_at = ? WHERE id = ?", past, id); err != nil {
		t.Fatalf("backdate expiry: %v", err)
	}

	// Listing sweeps overdue rows to expired before reading, so the row surfaces
	// as expired rather than the stale active state.
	all, err := srv.ListGithubCallbacks(ctx, connect.NewRequest(&pb.ListGithubCallbacksRequest{}))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all.Msg.GithubCallbacks) != 1 {
		t.Fatalf("list = %d, want 1", len(all.Msg.GithubCallbacks))
	}
	if got := all.Msg.GithubCallbacks[0].State; got != string(models.GithubCallbackStateExpired) {
		t.Errorf("state = %q, want expired", got)
	}
	if len(recorder.properties) != 1 {
		t.Fatalf("expiry captures = %d, want 1", len(recorder.properties))
	}
	if got := recorder.properties[0]["status"]; got != "abandoned" {
		t.Errorf("status = %v, want abandoned", got)
	}
	if got := recorder.properties[0]["attempt_count"]; got != 0 {
		t.Errorf("attempt_count = %v, want 0", got)
	}

	// A state=active filter no longer matches the now-expired row.
	active := string(models.GithubCallbackStateActive)
	byActive, err := srv.ListGithubCallbacks(ctx, connect.NewRequest(&pb.ListGithubCallbacksRequest{State: &active}))
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	if len(byActive.Msg.GithubCallbacks) != 0 {
		t.Errorf("active list = %d, want 0", len(byActive.Msg.GithubCallbacks))
	}

	// A state=expired filter matches it (sweep runs before the filter applies).
	expired := string(models.GithubCallbackStateExpired)
	byExpired, err := srv.ListGithubCallbacks(ctx, connect.NewRequest(&pb.ListGithubCallbacksRequest{State: &expired}))
	if err != nil {
		t.Fatalf("list expired: %v", err)
	}
	if len(byExpired.Msg.GithubCallbacks) != 1 {
		t.Errorf("expired list = %d, want 1", len(byExpired.Msg.GithubCallbacks))
	}
	if len(recorder.properties) != 1 {
		t.Errorf("repeated list recaptured expiry: %d", len(recorder.properties))
	}
}

func TestDeleteGithubCallback_Idempotent(t *testing.T) {
	srv := newGithubCallbackServer(t)
	ctx := context.Background()

	resp, err := srv.CreateGithubCallback(ctx, connect.NewRequest(validCreateGithubCallbackRequest()))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id := resp.Msg.GithubCallback.Id

	if _, err := srv.DeleteGithubCallback(ctx, connect.NewRequest(&pb.DeleteGithubCallbackRequest{Id: id})); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	// Gone from the list.
	all, err := srv.ListGithubCallbacks(ctx, connect.NewRequest(&pb.ListGithubCallbacksRequest{}))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all.Msg.GithubCallbacks) != 0 {
		t.Fatalf("after delete list = %d, want 0", len(all.Msg.GithubCallbacks))
	}
	// Second delete is a no-op success (idempotent), not NotFound.
	if _, err := srv.DeleteGithubCallback(ctx, connect.NewRequest(&pb.DeleteGithubCallbackRequest{Id: id})); err != nil {
		t.Fatalf("second delete should be OK, got %v", err)
	}

	// Blank id is rejected.
	if _, err := srv.DeleteGithubCallback(ctx, connect.NewRequest(&pb.DeleteGithubCallbackRequest{Id: "  "})); err == nil ||
		connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("blank id delete = %v, want InvalidArgument", err)
	}
}

func TestGithubCallbackToProto_MapsEveryField(t *testing.T) {
	group := "g"
	owner := "wkr"
	lastErr := "delivery failed"
	lastEvent := "pull_request.closed"
	lease := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	next := time.Now().UTC().Add(2 * time.Minute).Truncate(time.Second)
	trig := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	deliv := time.Now().UTC().Truncate(time.Second)
	created := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)

	m := &models.GithubCallback{
		ID:              "cb-1",
		GroupID:         &group,
		TargetChatID:    "chat-1",
		RepoOwner:       "owner",
		RepoName:        "repo",
		PRNumber:        7,
		Trigger:         models.GithubCallbackTriggerChecksFailed,
		State:           models.GithubCallbackStateTriggered,
		Message:         "body",
		LeaseOwner:      &owner,
		LeaseDeadlineAt: &lease,
		AttemptCount:    3,
		NextAttemptAt:   &next,
		TriggeredAt:     &trig,
		DeliveredAt:     &deliv,
		LastError:       &lastErr,
		LastEvent:       &lastEvent,
		ExpiresAt:       created.Add(24 * time.Hour),
		CreatedAt:       created,
		UpdatedAt:       created.Add(time.Minute),
	}

	p := githubCallbackToProto(m)
	if p.Id != "cb-1" || p.GroupId != "g" || p.TargetChatId != "chat-1" {
		t.Errorf("identity fields wrong: %+v", p)
	}
	if p.RepoOwner != "owner" || p.RepoName != "repo" || p.PrNumber != 7 {
		t.Errorf("repo fields wrong: %+v", p)
	}
	if p.Trigger != string(models.GithubCallbackTriggerChecksFailed) {
		t.Errorf("trigger = %q", p.Trigger)
	}
	if p.State != string(models.GithubCallbackStateTriggered) {
		t.Errorf("state = %q", p.State)
	}
	if p.Message != "body" || p.LeaseOwner != "wkr" || p.AttemptCount != 3 {
		t.Errorf("scalar fields wrong: %+v", p)
	}
	if p.LastError != "delivery failed" || p.LastEvent != "pull_request.closed" {
		t.Errorf("diagnostic fields wrong: %+v", p)
	}
	if !p.LeaseDeadlineAt.AsTime().Equal(lease) || !p.NextAttemptAt.AsTime().Equal(next) {
		t.Errorf("lease/next timestamps wrong")
	}
	if !p.TriggeredAt.AsTime().Equal(trig) || !p.DeliveredAt.AsTime().Equal(deliv) {
		t.Errorf("triggered/delivered timestamps wrong")
	}
	if !p.ExpiresAt.AsTime().Equal(m.ExpiresAt) || !p.CreatedAt.AsTime().Equal(created) {
		t.Errorf("expires/created timestamps wrong")
	}

	// Nil nullable fields map to unset proto fields.
	bare := &models.GithubCallback{
		ID:           "cb-2",
		TargetChatID: "chat",
		RepoOwner:    "o",
		RepoName:     "r",
		PRNumber:     1,
		Trigger:      models.GithubCallbackTriggerMerged,
		State:        models.GithubCallbackStateActive,
		Message:      "m",
		ExpiresAt:    created,
		CreatedAt:    created,
		UpdatedAt:    created,
	}
	pb2 := githubCallbackToProto(bare)
	if pb2.GroupId != "" || pb2.LeaseOwner != "" || pb2.LastError != "" || pb2.LastEvent != "" {
		t.Errorf("expected empty nullable strings, got %+v", pb2)
	}
	if pb2.LeaseDeadlineAt != nil || pb2.NextAttemptAt != nil || pb2.TriggeredAt != nil || pb2.DeliveredAt != nil {
		t.Errorf("expected nil nullable timestamps, got %+v", pb2)
	}
}
