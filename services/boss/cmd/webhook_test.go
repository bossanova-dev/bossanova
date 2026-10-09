package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

const (
	testWebhookSecret        = "whk_generated_secret_value_1234"
	testWebhookRotatedSecret = "whk_rotated_secret_value_5678"
)

// fakeWebhookServer is a fake orchestrator. Every session-webhook RPC records
// its request in its OWN field, so a command wired to a sibling RPC fails the
// assertion for the RPC it should have called. calls counts every request that
// reached the server, which is what "nothing is sent" is measured against.
type fakeWebhookServer struct {
	bossanovav1connect.UnimplementedOrchestratorServiceHandler

	calls atomic.Int32

	webhook    *pb.SessionWebhook
	webhooks   []*pb.SessionWebhook
	events     []*pb.SessionWebhookEventType
	delivery   *pb.SessionWebhookDelivery
	attempt    *pb.SessionWebhookDeliveryAttempt
	deliveries []*pb.SessionWebhookDelivery
	nextToken  string
	createErr  error

	eventsReq     *pb.ListSessionWebhookEventTypesRequest
	listReq       *pb.ListSessionWebhooksRequest
	createReq     *pb.CreateSessionWebhookRequest
	updateReq     *pb.UpdateSessionWebhookRequest
	rotateReq     *pb.RotateSessionWebhookSecretRequest
	deleteReq     *pb.DeleteSessionWebhookRequest
	testReq       *pb.SendSessionWebhookTestEventRequest
	deliveriesReq *pb.ListSessionWebhookDeliveriesRequest
	deliveryReq   *pb.GetSessionWebhookDeliveryRequest
}

func (f *fakeWebhookServer) ListSessionWebhookEventTypes(_ context.Context, req *connect.Request[pb.ListSessionWebhookEventTypesRequest]) (*connect.Response[pb.ListSessionWebhookEventTypesResponse], error) {
	f.calls.Add(1)
	f.eventsReq = req.Msg
	return connect.NewResponse(&pb.ListSessionWebhookEventTypesResponse{EventTypes: f.events}), nil
}

func (f *fakeWebhookServer) ListSessionWebhooks(_ context.Context, req *connect.Request[pb.ListSessionWebhooksRequest]) (*connect.Response[pb.ListSessionWebhooksResponse], error) {
	f.calls.Add(1)
	f.listReq = req.Msg
	return connect.NewResponse(&pb.ListSessionWebhooksResponse{Webhooks: f.webhooks}), nil
}

func (f *fakeWebhookServer) CreateSessionWebhook(_ context.Context, req *connect.Request[pb.CreateSessionWebhookRequest]) (*connect.Response[pb.CreateSessionWebhookResponse], error) {
	f.calls.Add(1)
	f.createReq = req.Msg
	if f.createErr != nil {
		return nil, f.createErr
	}
	secret := testWebhookSecret
	if req.Msg.Secret != nil {
		secret = req.Msg.GetSecret()
	}
	return connect.NewResponse(&pb.CreateSessionWebhookResponse{Webhook: f.webhook, Secret: secret}), nil
}

func (f *fakeWebhookServer) UpdateSessionWebhook(_ context.Context, req *connect.Request[pb.UpdateSessionWebhookRequest]) (*connect.Response[pb.UpdateSessionWebhookResponse], error) {
	f.calls.Add(1)
	f.updateReq = req.Msg
	return connect.NewResponse(&pb.UpdateSessionWebhookResponse{Webhook: f.webhook}), nil
}

func (f *fakeWebhookServer) RotateSessionWebhookSecret(_ context.Context, req *connect.Request[pb.RotateSessionWebhookSecretRequest]) (*connect.Response[pb.RotateSessionWebhookSecretResponse], error) {
	f.calls.Add(1)
	f.rotateReq = req.Msg
	secret := testWebhookRotatedSecret
	if req.Msg.Secret != nil {
		secret = req.Msg.GetSecret()
	}
	return connect.NewResponse(&pb.RotateSessionWebhookSecretResponse{Webhook: f.webhook, Secret: secret}), nil
}

func (f *fakeWebhookServer) DeleteSessionWebhook(_ context.Context, req *connect.Request[pb.DeleteSessionWebhookRequest]) (*connect.Response[pb.DeleteSessionWebhookResponse], error) {
	f.calls.Add(1)
	f.deleteReq = req.Msg
	return connect.NewResponse(&pb.DeleteSessionWebhookResponse{}), nil
}

func (f *fakeWebhookServer) SendSessionWebhookTestEvent(_ context.Context, req *connect.Request[pb.SendSessionWebhookTestEventRequest]) (*connect.Response[pb.SendSessionWebhookTestEventResponse], error) {
	f.calls.Add(1)
	f.testReq = req.Msg
	return connect.NewResponse(&pb.SendSessionWebhookTestEventResponse{Delivery: f.delivery, Attempt: f.attempt}), nil
}

func (f *fakeWebhookServer) ListSessionWebhookDeliveries(_ context.Context, req *connect.Request[pb.ListSessionWebhookDeliveriesRequest]) (*connect.Response[pb.ListSessionWebhookDeliveriesResponse], error) {
	f.calls.Add(1)
	f.deliveriesReq = req.Msg
	return connect.NewResponse(&pb.ListSessionWebhookDeliveriesResponse{Deliveries: f.deliveries, NextPageToken: f.nextToken}), nil
}

func (f *fakeWebhookServer) GetSessionWebhookDelivery(_ context.Context, req *connect.Request[pb.GetSessionWebhookDeliveryRequest]) (*connect.Response[pb.GetSessionWebhookDeliveryResponse], error) {
	f.calls.Add(1)
	f.deliveryReq = req.Msg
	var attempts []*pb.SessionWebhookDeliveryAttempt
	if f.attempt != nil {
		attempts = []*pb.SessionWebhookDeliveryAttempt{f.attempt}
	}
	return connect.NewResponse(&pb.GetSessionWebhookDeliveryResponse{
		Delivery:       f.delivery,
		Attempts:       attempts,
		RequestBody:    `{"type":"session.passing"}`,
		RequestHeaders: []*pb.SessionWebhookRequestHeader{{Name: "X-Bossanova-Event-Id", Value: "evt-1"}},
	}), nil
}

func newFakeWebhookURL(t *testing.T, fake *fakeWebhookServer) string {
	t.Helper()
	path, handler := bossanovav1connect.NewOrchestratorServiceHandler(fake)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func sampleWebhook() *pb.SessionWebhook {
	ts := timestamppb.New(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	return &pb.SessionWebhook{
		Id: "wh-1", OrganizationId: "org-1", Url: "https://hooks.example.test/boss", Description: "ops channel",
		EventTypes: []string{"session.failing", "session.passing"}, IsEnabled: true, CreatedByUserId: "user-1",
		CreatedAt: ts, UpdatedAt: ts,
	}
}

func sampleWebhookDelivery() *pb.SessionWebhookDelivery {
	ts := timestamppb.New(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	code := int32(200)
	return &pb.SessionWebhookDelivery{
		Id: "del-1", WebhookId: "wh-1", EventId: "evt-1", EventType: "session.passing", IsTest: true,
		Status: "succeeded", AttemptCount: 1, LastResponseStatusCode: &code, CreatedAt: ts, CompletedAt: ts,
	}
}

func sampleWebhookAttempt() *pb.SessionWebhookDeliveryAttempt {
	code := int32(200)
	return &pb.SessionWebhookDeliveryAttempt{
		AttemptNumber: 1, StartedAt: timestamppb.New(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)),
		DurationMs: 87, ResponseStatusCode: &code, ResponseBodyExcerpt: "ok", IsSuccess: true,
	}
}

func newSampleWebhookFake() *fakeWebhookServer {
	return &fakeWebhookServer{
		webhook:  sampleWebhook(),
		webhooks: []*pb.SessionWebhook{sampleWebhook()},
		events: []*pb.SessionWebhookEventType{{
			Type: "session.passing", DisplayLabel: "Passing", Description: "Checks went green", SamplePayloadJson: `{"is_test":true}`,
		}},
		delivery:   sampleWebhookDelivery(),
		attempt:    sampleWebhookAttempt(),
		deliveries: []*pb.SessionWebhookDelivery{sampleWebhookDelivery()},
	}
}

// runWebhookCLI runs `boss webhook <args>` against fake through the real
// RemoteClient and the shipped flag definitions, returning stdout.
func runWebhookCLI(t *testing.T, fake *fakeWebhookServer, stdin string, args ...string) (string, error) {
	t.Helper()
	url := newFakeWebhookURL(t, fake)
	prev := newSessionWebhookClient
	newSessionWebhookClient = func(*cobra.Command) (sessionWebhookClient, error) { return client.NewRemote(url, "tok"), nil }
	t.Cleanup(func() { newSessionWebhookClient = prev })

	cmd := webhookCmd()
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

var webhookJSONKeys = []string{
	"created_at", "created_by_user_id", "description", "enabled", "event_types", "id", "organization_id",
	"secret_rotated_at", "updated_at", "url",
}

var webhookDeliveryJSONKeys = []string{
	"attempt_count", "completed_at", "created_at", "event_id", "event_type", "id", "is_test", "last_error",
	"last_response_status_code", "next_attempt_at", "status", "webhook_id",
}

var webhookAttemptJSONKeys = []string{
	"attempt_number", "duration_ms", "error_message", "is_success", "response_body_excerpt",
	"response_status_code", "started_at",
}

func decodeJSONArray(t *testing.T, raw json.RawMessage) []map[string]json.RawMessage {
	t.Helper()
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return items
}

// TestWebhookCommands proves each `boss webhook` subcommand calls its own RPC
// with the fields its flags describe, passes --org through unchanged (empty =
// the active organization), and keeps a stable --json key set.
func TestWebhookCommands(t *testing.T) {
	t.Run("subcommands", func(t *testing.T) {
		var names []string
		for _, sub := range webhookCmd().Commands() {
			names = append(names, sub.Name())
			for _, flag := range []string{jsonFlagName, "org"} {
				if sub.Flags().Lookup(flag) == nil {
					t.Errorf("boss webhook %s has no --%s flag", sub.Name(), flag)
				}
			}
		}
		sort.Strings(names)
		want := []string{"add", "deliveries", "delivery", "edit", "events", "ls", "rm", "rotate-secret", "test"}
		if !slices.Equal(names, want) {
			t.Fatalf("webhook subcommands = %v, want %v", names, want)
		}
	})

	t.Run("org passthrough", func(t *testing.T) {
		fake := newSampleWebhookFake()
		if _, err := runWebhookCLI(t, fake, "", "ls"); err != nil {
			t.Fatalf("ls: %v", err)
		}
		if got := fake.listReq.GetOrganizationId(); got != "" {
			t.Errorf("ls without --org sent organization_id %q, want empty (active organization)", got)
		}
		if _, err := runWebhookCLI(t, fake, "", "ls", "--org", "org-9"); err != nil {
			t.Fatalf("ls --org: %v", err)
		}
		if got := fake.listReq.GetOrganizationId(); got != "org-9" {
			t.Errorf("ls --org sent organization_id %q, want org-9", got)
		}
		if n := fake.calls.Load(); n != 2 {
			t.Errorf("%d request(s), want 2 (no organization lookup)", n)
		}
	})

	t.Run("events", func(t *testing.T) {
		fake := newSampleWebhookFake()
		out, err := runWebhookCLI(t, fake, "", "events", "--org", "org-2")
		if err != nil {
			t.Fatalf("events: %v", err)
		}
		if fake.eventsReq.GetOrganizationId() != "org-2" || !strings.Contains(out, "session.passing") || !strings.Contains(out, "Checks went green") {
			t.Errorf("events req=%v out=%q", fake.eventsReq, out)
		}
		out, err = runWebhookCLI(t, fake, "", "events", "--json")
		if err != nil {
			t.Fatalf("events --json: %v", err)
		}
		types := decodeJSONArray(t, decodeJSONObject(t, out)["event_types"])
		if got, want := sortedJSONKeys(types[0]), []string{"description", "display_label", "sample_payload_json", "type"}; !slices.Equal(got, want) {
			t.Errorf("event type keys = %v, want %v", got, want)
		}
	})

	t.Run("ls", func(t *testing.T) {
		fake := newSampleWebhookFake()
		out, err := runWebhookCLI(t, fake, "", "ls")
		if err != nil {
			t.Fatalf("ls: %v", err)
		}
		for _, want := range []string{"wh-1", "https://hooks.example.test/boss", "session.failing,session.passing", "ops channel"} {
			if !strings.Contains(out, want) {
				t.Errorf("ls output missing %q:\n%s", want, out)
			}
		}
		out, err = runWebhookCLI(t, fake, "", "ls", "--json")
		if err != nil {
			t.Fatalf("ls --json: %v", err)
		}
		hooks := decodeJSONArray(t, decodeJSONObject(t, out)["webhooks"])
		if len(hooks) != 1 || !slices.Equal(sortedJSONKeys(hooks[0]), webhookJSONKeys) {
			t.Errorf("ls --json webhooks = %v, want keys %v", hooks, webhookJSONKeys)
		}
		if string(hooks[0]["secret_rotated_at"]) != `""` {
			t.Errorf("unset secret_rotated_at = %s, want empty string", hooks[0]["secret_rotated_at"])
		}

		fake.webhooks = nil
		out, err = runWebhookCLI(t, fake, "", "ls", "--json")
		if err != nil {
			t.Fatalf("empty ls --json: %v", err)
		}
		if strings.TrimSpace(out) != "{\n  \"webhooks\": []\n}" {
			t.Errorf("empty ls --json = %q, want an empty array", out)
		}
	})

	t.Run("add", func(t *testing.T) {
		fake := newSampleWebhookFake()
		_, err := runWebhookCLI(t, fake, "", "add", "https://hooks.example.test/boss",
			"--event", "session.passing", "--event", "session.failing", "--description", "ops", "--disabled", "--org", "org-1")
		if err != nil {
			t.Fatalf("add: %v", err)
		}
		req := fake.createReq
		if req.GetUrl() != "https://hooks.example.test/boss" || req.GetDescription() != "ops" || req.GetIsEnabled() ||
			req.GetOrganizationId() != "org-1" || !slices.Equal(req.GetEventTypes(), []string{"session.passing", "session.failing"}) {
			t.Errorf("create request = %v", req)
		}
		if req.Secret != nil {
			t.Errorf("add without --secret-file sent a secret")
		}
		if _, err := runWebhookCLI(t, fake, "", "add", "https://x.test", "--event", "session.passing"); err != nil {
			t.Fatalf("add enabled: %v", err)
		}
		if !fake.createReq.GetIsEnabled() {
			t.Errorf("add without --disabled created the webhook disabled")
		}
	})

	t.Run("edit sends only given flags", func(t *testing.T) {
		fake := newSampleWebhookFake()
		if _, err := runWebhookCLI(t, fake, "", "edit", "wh-1", "--description", ""); err != nil {
			t.Fatalf("edit: %v", err)
		}
		req := fake.updateReq
		if req.GetId() != "wh-1" || req.Description == nil || req.GetDescription() != "" || req.Url != nil ||
			req.IsEnabled != nil || req.GetShouldReplaceEventTypes() || len(req.GetEventTypes()) != 0 {
			t.Errorf("description-only edit request = %v", req)
		}

		if _, err := runWebhookCLI(t, fake, "", "edit", "wh-1", "--event", "session.passing", "--url", "https://y.test", "--disable", "--org", "org-3"); err != nil {
			t.Fatalf("edit: %v", err)
		}
		req = fake.updateReq
		if !req.GetShouldReplaceEventTypes() || !slices.Equal(req.GetEventTypes(), []string{"session.passing"}) ||
			req.GetUrl() != "https://y.test" || req.IsEnabled == nil || req.GetIsEnabled() || req.Description != nil ||
			req.GetOrganizationId() != "org-3" {
			t.Errorf("multi-field edit request = %v", req)
		}

		if _, err := runWebhookCLI(t, fake, "", "edit", "wh-1", "--enable"); err != nil {
			t.Fatalf("edit --enable: %v", err)
		}
		if fake.updateReq.IsEnabled == nil || !fake.updateReq.GetIsEnabled() {
			t.Errorf("--enable request = %v", fake.updateReq)
		}

		out, err := runWebhookCLI(t, fake, "", "edit", "wh-1", "--enable", "--json")
		if err != nil {
			t.Fatalf("edit --json: %v", err)
		}
		if got := sortedJSONKeys(decodeJSONObject(t, out)); !slices.Equal(got, webhookJSONKeys) {
			t.Errorf("edit --json keys = %v", got)
		}
	})

	t.Run("rotate-secret", func(t *testing.T) {
		fake := newSampleWebhookFake()
		if _, err := runWebhookCLI(t, fake, "", "rotate-secret", "wh-1", "--org", "org-1"); err != nil {
			t.Fatalf("rotate-secret: %v", err)
		}
		if fake.rotateReq.GetId() != "wh-1" || fake.rotateReq.GetOrganizationId() != "org-1" || fake.rotateReq.Secret != nil {
			t.Errorf("rotate request = %v", fake.rotateReq)
		}
	})

	t.Run("rm", func(t *testing.T) {
		fake := newSampleWebhookFake()
		out, err := runWebhookCLI(t, fake, "", "rm", "wh-1", "--yes", "--json")
		if err != nil {
			t.Fatalf("rm: %v", err)
		}
		if fake.deleteReq.GetId() != "wh-1" || strings.TrimSpace(out) != "{\n  \"deleted_webhook\": \"wh-1\"\n}" {
			t.Errorf("rm req=%v out=%q", fake.deleteReq, out)
		}
	})

	t.Run("test", func(t *testing.T) {
		fake := newSampleWebhookFake()
		out, err := runWebhookCLI(t, fake, "", "test", "wh-1", "session.passing")
		if err != nil {
			t.Fatalf("test: %v", err)
		}
		if fake.testReq.GetWebhookId() != "wh-1" || fake.testReq.GetEventType() != "session.passing" || fake.testReq.PayloadJson != nil {
			t.Errorf("test request = %v", fake.testReq)
		}
		for _, want := range []string{"succeeded", "HTTP 200", "87ms"} {
			if !strings.Contains(out, want) {
				t.Errorf("test output missing %q:\n%s", want, out)
			}
		}

		if _, err := runWebhookCLI(t, fake, `{"custom":true}`, "test", "wh-1", "session.passing", "--payload-file", "-"); err != nil {
			t.Fatalf("test --payload-file -: %v", err)
		}
		if fake.testReq.GetPayloadJson() != `{"custom":true}` {
			t.Errorf("payload = %q", fake.testReq.GetPayloadJson())
		}

		out, err = runWebhookCLI(t, fake, "", "test", "wh-1", "session.passing", "--json")
		if err != nil {
			t.Fatalf("test --json: %v", err)
		}
		obj := decodeJSONObject(t, out)
		if got := sortedJSONKeys(obj); !slices.Equal(got, []string{"attempt", "delivery"}) {
			t.Fatalf("test --json keys = %v", got)
		}
		var attempt map[string]json.RawMessage
		if err := json.Unmarshal(obj["attempt"], &attempt); err != nil || !slices.Equal(sortedJSONKeys(attempt), webhookAttemptJSONKeys) {
			t.Errorf("attempt = %s (%v)", obj["attempt"], err)
		}

		fake.attempt = nil
		fake.delivery.Status = "cancelled"
		fake.delivery.LastResponseStatusCode = nil
		out, err = runWebhookCLI(t, fake, "", "test", "wh-1", "session.passing", "--json")
		if err != nil {
			t.Fatalf("cancelled test --json: %v", err)
		}
		obj = decodeJSONObject(t, out)
		if string(obj["attempt"]) != "null" {
			t.Errorf("cancelled attempt = %s, want null", obj["attempt"])
		}
		var delivery map[string]json.RawMessage
		if err := json.Unmarshal(obj["delivery"], &delivery); err != nil || string(delivery["last_response_status_code"]) != "null" {
			t.Errorf("absent last_response_status_code = %s, want null", delivery["last_response_status_code"])
		}
	})

	t.Run("deliveries", func(t *testing.T) {
		fake := newSampleWebhookFake()
		fake.nextToken = "tok-2"
		out, err := runWebhookCLI(t, fake, "", "deliveries", "wh-1", "--status", "failed", "--event", "session.passing",
			"--test", "--page-size", "10", "--page-token", "tok-1")
		if err != nil {
			t.Fatalf("deliveries: %v", err)
		}
		req := fake.deliveriesReq
		if req.GetWebhookId() != "wh-1" || req.GetStatus() != "failed" || req.GetEventType() != "session.passing" ||
			req.IsTest == nil || !req.GetIsTest() || req.GetPageSize() != 10 || req.GetPageToken() != "tok-1" {
			t.Errorf("deliveries request = %v", req)
		}
		if !strings.Contains(out, "del-1") || !strings.Contains(out, "--page-token tok-2") {
			t.Errorf("deliveries output = %q", out)
		}

		if _, err := runWebhookCLI(t, fake, "", "deliveries", "wh-1", "--live"); err != nil {
			t.Fatalf("deliveries --live: %v", err)
		}
		req = fake.deliveriesReq
		if req.IsTest == nil || req.GetIsTest() || req.Status != nil || req.EventType != nil {
			t.Errorf("--live request = %v", req)
		}
		if _, err := runWebhookCLI(t, fake, "", "deliveries", "wh-1"); err != nil {
			t.Fatalf("deliveries: %v", err)
		}
		if fake.deliveriesReq.IsTest != nil {
			t.Errorf("deliveries without --test/--live sent is_test")
		}

		out, err = runWebhookCLI(t, fake, "", "deliveries", "wh-1", "--json")
		if err != nil {
			t.Fatalf("deliveries --json: %v", err)
		}
		obj := decodeJSONObject(t, out)
		if got := sortedJSONKeys(obj); !slices.Equal(got, []string{"deliveries", "next_page_token"}) {
			t.Fatalf("deliveries --json keys = %v", got)
		}
		items := decodeJSONArray(t, obj["deliveries"])
		if len(items) != 1 || !slices.Equal(sortedJSONKeys(items[0]), webhookDeliveryJSONKeys) {
			t.Errorf("delivery keys = %v, want %v", items, webhookDeliveryJSONKeys)
		}
		if string(items[0]["last_response_status_code"]) != "200" {
			t.Errorf("last_response_status_code = %s, want 200", items[0]["last_response_status_code"])
		}
	})

	t.Run("deliveries json empty page is an array", func(t *testing.T) {
		fake := newSampleWebhookFake()
		fake.deliveries = nil
		out, err := runWebhookCLI(t, fake, "", "deliveries", "wh-1", "--json")
		if err != nil {
			t.Fatalf("deliveries --json: %v", err)
		}
		if got := string(decodeJSONObject(t, out)["deliveries"]); got != "[]" {
			t.Errorf("empty deliveries = %s, want []", got)
		}
	})

	t.Run("delivery", func(t *testing.T) {
		fake := newSampleWebhookFake()
		out, err := runWebhookCLI(t, fake, "", "delivery", "del-1", "--org", "org-1")
		if err != nil {
			t.Fatalf("delivery: %v", err)
		}
		if fake.deliveryReq.GetDeliveryId() != "del-1" || fake.deliveryReq.GetOrganizationId() != "org-1" {
			t.Errorf("delivery request = %v", fake.deliveryReq)
		}
		for _, want := range []string{"del-1", "Attempt 1", "HTTP 200", "X-Bossanova-Event-Id: evt-1", `{"type":"session.passing"}`} {
			if !strings.Contains(out, want) {
				t.Errorf("delivery output missing %q:\n%s", want, out)
			}
		}
		out, err = runWebhookCLI(t, fake, "", "delivery", "del-1", "--json")
		if err != nil {
			t.Fatalf("delivery --json: %v", err)
		}
		obj := decodeJSONObject(t, out)
		if got, want := sortedJSONKeys(obj), []string{"attempts", "delivery", "request_body", "request_headers"}; !slices.Equal(got, want) {
			t.Errorf("delivery --json keys = %v, want %v", got, want)
		}
	})

	t.Run("api error passes through as a json envelope", func(t *testing.T) {
		fake := newSampleWebhookFake()
		fake.createErr = connect.NewError(connect.CodePermissionDenied, errors.New("owner role required"))
		out, err := runWebhookCLI(t, fake, "", "add", "https://x.test", "--event", "session.passing", "--json")
		if err == nil {
			t.Fatal("add succeeded against a refusing API")
		}
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("err code = %v, want PermissionDenied", connect.CodeOf(err))
		}
		obj := decodeJSONObject(t, out)
		if got := sortedJSONKeys(obj); !slices.Equal(got, []string{"error"}) {
			t.Fatalf("envelope keys = %v", got)
		}
		if !strings.Contains(string(obj["error"]), "PERMISSION_DENIED") || !strings.Contains(string(obj["error"]), "owner role required") {
			t.Errorf("envelope = %s", out)
		}
	})
}

// TestWebhookSecretPrintedOnce proves the signing secret appears exactly once
// on add and rotate-secret (human and --json), and never anywhere else.
func TestWebhookSecretPrintedOnce(t *testing.T) {
	fake := newSampleWebhookFake()
	for _, tc := range []struct {
		args   []string
		secret string
	}{
		{[]string{"add", "https://x.test", "--event", "session.passing"}, testWebhookSecret},
		{[]string{"rotate-secret", "wh-1"}, testWebhookRotatedSecret},
	} {
		out, err := runWebhookCLI(t, fake, "", tc.args...)
		if err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if n := strings.Count(out, tc.secret); n != 1 {
			t.Errorf("%v printed the secret %d times, want once:\n%s", tc.args, n, out)
		}
		if !strings.Contains(out, webhookSecretHeader+"\n"+tc.secret+"\n") {
			t.Errorf("%v output does not put the secret on its own line after the header:\n%s", tc.args, out)
		}

		out, err = runWebhookCLI(t, fake, "", append(slices.Clone(tc.args), "--json")...)
		if err != nil {
			t.Fatalf("%v --json: %v", tc.args, err)
		}
		obj := decodeJSONObject(t, out)
		if got := sortedJSONKeys(obj); !slices.Equal(got, []string{"secret", "webhook"}) {
			t.Errorf("%v --json keys = %v", tc.args, got)
		}
		if string(obj["secret"]) != `"`+tc.secret+`"` || strings.Count(out, tc.secret) != 1 {
			t.Errorf("%v --json = %s, want the secret exactly once under \"secret\"", tc.args, out)
		}
	}

	// No other command, human or --json, carries a secret.
	for _, args := range [][]string{
		{"events", "--json"}, {"ls"}, {"ls", "--json"}, {"edit", "wh-1", "--enable"}, {"edit", "wh-1", "--enable", "--json"},
		{"test", "wh-1", "session.passing", "--json"}, {"deliveries", "wh-1", "--json"}, {"delivery", "del-1", "--json"},
		{"rm", "wh-1", "--yes", "--json"},
	} {
		out, err := runWebhookCLI(t, fake, "", args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if strings.Contains(out, testWebhookSecret) || strings.Contains(out, testWebhookRotatedSecret) || strings.Contains(out, webhookSecretHeader) {
			t.Errorf("%v output carries a secret: %q", args, out)
		}
		if strings.Contains(out, `"secret"`) {
			t.Errorf("%v --json carries a \"secret\" key: %q", args, out)
		}
	}
}

// TestWebhookSecretFile proves a caller-chosen secret comes only from
// --secret-file, with '-' reading stdin and one trailing newline trimmed.
func TestWebhookSecretFile(t *testing.T) {
	const chosen = "my-chosen-secret-value-0123"

	fake := newSampleWebhookFake()
	if _, err := runWebhookCLI(t, fake, chosen+"\n\n", "add", "https://x.test", "--event", "session.passing", "--secret-file", "-"); err != nil {
		t.Fatalf("add --secret-file -: %v", err)
	}
	if fake.createReq.Secret == nil || fake.createReq.GetSecret() != chosen+"\n" {
		t.Errorf("stdin secret = %q, want exactly one trailing newline trimmed", fake.createReq.GetSecret())
	}

	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(chosen+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runWebhookCLI(t, fake, "", "rotate-secret", "wh-1", "--secret-file", path); err != nil {
		t.Fatalf("rotate-secret --secret-file: %v", err)
	}
	if fake.rotateReq.GetSecret() != chosen {
		t.Errorf("file secret = %q, want %q", fake.rotateReq.GetSecret(), chosen)
	}

	// There is no flag that takes the secret on the command line.
	for _, sub := range webhookCmd().Commands() {
		if sub.Flags().Lookup("secret") != nil {
			t.Errorf("boss webhook %s has a --secret flag; secrets come only from --secret-file", sub.Name())
		}
	}

	// An empty file is refused before any request, and the error never quotes it.
	before := fake.calls.Load()
	if _, err := runWebhookCLI(t, fake, "\n", "add", "https://x.test", "--event", "session.passing", "--secret-file", "-"); err == nil {
		t.Fatal("add accepted an empty --secret-file")
	}
	if fake.calls.Load() != before {
		t.Errorf("an empty --secret-file reached the orchestrator")
	}
}

// TestWebhookGuards proves the usage refusals happen before any RPC.
func TestWebhookGuards(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		wantCode string
		mustSay  string
	}{
		{"rm without --yes", []string{"rm", "wh-1", "--json"}, codeConfirmationRequired, "--yes"},
		{"rm without --yes human", []string{"rm", "wh-1"}, "", "--yes"},
		{"edit without a field", []string{"edit", "wh-1", "--json"}, codeInvalidArgument, "nothing to change"},
		{"edit with only --org", []string{"edit", "wh-1", "--org", "org-1"}, "", "nothing to change"},
		{"edit --enable --disable", []string{"edit", "wh-1", "--enable", "--disable", "--json"}, codeInvalidArgument, "mutually exclusive"},
		{"deliveries --test --live", []string{"deliveries", "wh-1", "--test", "--live", "--json"}, codeInvalidArgument, "mutually exclusive"},
		{"add without --event", []string{"add", "https://x.test", "--json"}, codeInvalidArgument, "--event"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newSampleWebhookFake()
			out, err := runWebhookCLI(t, fake, "", tc.args...)
			if err == nil {
				t.Fatalf("%v succeeded", tc.args)
			}
			if !strings.Contains(err.Error(), tc.mustSay) {
				t.Errorf("err = %q, want it to mention %q", err.Error(), tc.mustSay)
			}
			if n := fake.calls.Load(); n != 0 {
				t.Errorf("%d request(s) reached the orchestrator", n)
			}
			if tc.wantCode != "" {
				if !strings.Contains(string(decodeJSONObject(t, out)["error"]), tc.wantCode) {
					t.Errorf("envelope = %q, want code %s", out, tc.wantCode)
				}
			}
		})
	}
}

// TestWebhookRequiresLogin proves a caller with no cloud login gets an error
// naming `boss login` and that no request reaches the orchestrator.
func TestWebhookRequiresLogin(t *testing.T) {
	fake := newSampleWebhookFake()
	url := newFakeWebhookURL(t, fake)
	mgr := auth.NewManager(emptyTokenStore{}, auth.Config{})

	if _, err := sessionWebhookRemote(context.Background(), mgr, url); err == nil || !strings.Contains(err.Error(), "run 'boss login' first") {
		t.Fatalf("sessionWebhookRemote err = %v, want the boss login hint", err)
	}

	prev := newSessionWebhookClient
	newSessionWebhookClient = func(cmd *cobra.Command) (sessionWebhookClient, error) {
		return sessionWebhookRemote(cmd.Context(), mgr, url)
	}
	t.Cleanup(func() { newSessionWebhookClient = prev })
	cmd := webhookCmd()
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"ls", "--json"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Fatal("ls succeeded without a login")
	}
	if !strings.Contains(out.String(), "run 'boss login' first") {
		t.Errorf("ls --json envelope = %q, want the login hint", out.String())
	}
	if n := fake.calls.Load(); n != 0 {
		t.Errorf("%d request(s) reached the orchestrator without a login", n)
	}
}

// TestWebhookBoolFlagValues pins that an explicit boolean value on --enable,
// --disable, --test and --live is honoured rather than the flag's mere presence.
func TestWebhookBoolFlagValues(t *testing.T) {
	cases := []struct {
		flag    string
		enabled bool
	}{
		{"--enable", true}, {"--enable=true", true}, {"--enable=false", false},
		{"--disable", false}, {"--disable=true", false}, {"--disable=false", true},
	}
	for _, tc := range cases {
		fake := newSampleWebhookFake()
		if _, err := runWebhookCLI(t, fake, "", "edit", "wh-1", tc.flag); err != nil {
			t.Fatalf("edit %s: %v", tc.flag, err)
		}
		if fake.updateReq.IsEnabled == nil || fake.updateReq.GetIsEnabled() != tc.enabled {
			t.Errorf("edit %s: is_enabled = %v, want %v", tc.flag, fake.updateReq.IsEnabled, tc.enabled)
		}
	}

	deliveryCases := []struct {
		flag   string
		isTest bool
	}{
		{"--test", true}, {"--test=false", false}, {"--live", false}, {"--live=false", true},
	}
	for _, tc := range deliveryCases {
		fake := newSampleWebhookFake()
		if _, err := runWebhookCLI(t, fake, "", "deliveries", "wh-1", tc.flag); err != nil {
			t.Fatalf("deliveries %s: %v", tc.flag, err)
		}
		if fake.deliveriesReq.IsTest == nil || fake.deliveriesReq.GetIsTest() != tc.isTest {
			t.Errorf("deliveries %s: is_test = %v, want %v", tc.flag, fake.deliveriesReq.IsTest, tc.isTest)
		}
	}
}

type failingWebhookWriter struct{}

func (failingWebhookWriter) Write([]byte) (int, error) { return 0, errors.New("stdout closed") }

// TestWebhookSecretWriteFailure pins that a failed stdout write of the one-time
// secret surfaces as an error instead of a silent exit 0.
func TestWebhookSecretWriteFailure(t *testing.T) {
	for _, args := range [][]string{{"add", "https://hooks.example.test/boss", "--event", "session.passing"}, {"rotate-secret", "wh-1"}} {
		fake := newSampleWebhookFake()
		url := newFakeWebhookURL(t, fake)
		prev := newSessionWebhookClient
		newSessionWebhookClient = func(*cobra.Command) (sessionWebhookClient, error) { return client.NewRemote(url, "tok"), nil }
		cmd := webhookCmd()
		cmd.SetOut(failingWebhookWriter{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetIn(strings.NewReader(""))
		cmd.SetArgs(args)
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		err := cmd.ExecuteContext(context.Background())
		newSessionWebhookClient = prev
		if err == nil || !strings.Contains(err.Error(), "write webhook secret") {
			t.Errorf("%v: err = %v, want a write webhook secret error", args, err)
		}
	}
}
