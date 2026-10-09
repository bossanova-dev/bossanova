package bossmcp

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

const sessionWebhookSecret = "whsec-SESSION-WEBHOOK-once-4410"

// sessionWebhookToolNames is the session webhook family of the hosted tier and
// sessionWebhookReadOnlyToolNames its read subset, spelled out so this test
// pins the family on its own.
var (
	sessionWebhookToolNames = []string{
		"list_session_webhook_event_types", "list_session_webhooks",
		"list_session_webhook_deliveries", "get_session_webhook_delivery",
		"save_session_webhook", "test_session_webhook", "delete_session_webhook",
	}
	sessionWebhookReadOnlyToolNames = []string{
		"list_session_webhook_event_types", "list_session_webhooks",
		"list_session_webhook_deliveries", "get_session_webhook_delivery",
	}
)

// sessionWebhookHooks implements SessionWebhookBackend with one hook per
// method, so a tool wired to a sibling method fails its own subtest. It holds
// no fakeBackend, so it can be embedded beside fakeTriggerBackend without an
// ambiguous Backend method set.
type sessionWebhookHooks struct {
	listEventTypes func(ctx context.Context, req *pb.ListSessionWebhookEventTypesRequest) (*pb.ListSessionWebhookEventTypesResponse, error)
	listWebhooks   func(ctx context.Context, req *pb.ListSessionWebhooksRequest) (*pb.ListSessionWebhooksResponse, error)
	listDeliveries func(ctx context.Context, req *pb.ListSessionWebhookDeliveriesRequest) (*pb.ListSessionWebhookDeliveriesResponse, error)
	getDelivery    func(ctx context.Context, req *pb.GetSessionWebhookDeliveryRequest) (*pb.GetSessionWebhookDeliveryResponse, error)
	create         func(ctx context.Context, req *pb.CreateSessionWebhookRequest) (*pb.CreateSessionWebhookResponse, error)
	update         func(ctx context.Context, req *pb.UpdateSessionWebhookRequest) (*pb.SessionWebhook, error)
	rotate         func(ctx context.Context, req *pb.RotateSessionWebhookSecretRequest) (*pb.RotateSessionWebhookSecretResponse, error)
	sendTest       func(ctx context.Context, req *pb.SendSessionWebhookTestEventRequest) (*pb.SendSessionWebhookTestEventResponse, error)
	del            func(ctx context.Context, req *pb.DeleteSessionWebhookRequest) error
}

func (h *sessionWebhookHooks) ListSessionWebhookEventTypes(ctx context.Context, req *pb.ListSessionWebhookEventTypesRequest) (*pb.ListSessionWebhookEventTypesResponse, error) {
	if h.listEventTypes == nil {
		return nil, errNotImpl
	}
	return h.listEventTypes(ctx, req)
}

func (h *sessionWebhookHooks) ListSessionWebhooks(ctx context.Context, req *pb.ListSessionWebhooksRequest) (*pb.ListSessionWebhooksResponse, error) {
	if h.listWebhooks == nil {
		return nil, errNotImpl
	}
	return h.listWebhooks(ctx, req)
}

func (h *sessionWebhookHooks) ListSessionWebhookDeliveries(ctx context.Context, req *pb.ListSessionWebhookDeliveriesRequest) (*pb.ListSessionWebhookDeliveriesResponse, error) {
	if h.listDeliveries == nil {
		return nil, errNotImpl
	}
	return h.listDeliveries(ctx, req)
}

func (h *sessionWebhookHooks) GetSessionWebhookDelivery(ctx context.Context, req *pb.GetSessionWebhookDeliveryRequest) (*pb.GetSessionWebhookDeliveryResponse, error) {
	if h.getDelivery == nil {
		return nil, errNotImpl
	}
	return h.getDelivery(ctx, req)
}

func (h *sessionWebhookHooks) CreateSessionWebhook(ctx context.Context, req *pb.CreateSessionWebhookRequest) (*pb.CreateSessionWebhookResponse, error) {
	if h.create == nil {
		return nil, errNotImpl
	}
	return h.create(ctx, req)
}

func (h *sessionWebhookHooks) UpdateSessionWebhook(ctx context.Context, req *pb.UpdateSessionWebhookRequest) (*pb.SessionWebhook, error) {
	if h.update == nil {
		return nil, errNotImpl
	}
	return h.update(ctx, req)
}

func (h *sessionWebhookHooks) RotateSessionWebhookSecret(ctx context.Context, req *pb.RotateSessionWebhookSecretRequest) (*pb.RotateSessionWebhookSecretResponse, error) {
	if h.rotate == nil {
		return nil, errNotImpl
	}
	return h.rotate(ctx, req)
}

func (h *sessionWebhookHooks) SendSessionWebhookTestEvent(ctx context.Context, req *pb.SendSessionWebhookTestEventRequest) (*pb.SendSessionWebhookTestEventResponse, error) {
	if h.sendTest == nil {
		return nil, errNotImpl
	}
	return h.sendTest(ctx, req)
}

func (h *sessionWebhookHooks) DeleteSessionWebhook(ctx context.Context, req *pb.DeleteSessionWebhookRequest) error {
	if h.del == nil {
		return errNotImpl
	}
	return h.del(ctx, req)
}

// fakeSessionWebhookBackend is a Backend that implements SessionWebhookBackend
// but NOT TriggerBackend: the "webhook family alone" case.
type fakeSessionWebhookBackend struct {
	fakeBackend
	sessionWebhookHooks
}

// fakeHostedBackend implements both hosted families.
type fakeHostedBackend struct {
	fakeTriggerBackend
	sessionWebhookHooks
}

var (
	_ SessionWebhookBackend = (*fakeSessionWebhookBackend)(nil)
	_ SessionWebhookBackend = (*fakeHostedBackend)(nil)
	_ TriggerBackend        = (*fakeHostedBackend)(nil)
)

// TestHostedSessionWebhookTools pins the session webhook family's registration
// rule and the whole hosted tier: each family registers on its own interface,
// independently of the other; neither registers without IncludeHostedTools;
// ReadOnly keeps only the hosted read tools; Only filters both families; and
// HostedToolNames() is exactly the two families, in ToolDefinitions order.
func TestHostedSessionWebhookTools(t *testing.T) {
	hosted := HostedToolNames()
	if got, want := sortedUnion(hosted), sortedUnion(triggerToolNames, sessionWebhookToolNames); !slices.Equal(got, want) {
		t.Fatalf("HostedToolNames() = %v, want the trigger and session webhook families %v", got, want)
	}
	if got, want := sortedUnion(hostedReadOnlyToolNames), sortedUnion(triggerReadOnlyToolNames, sessionWebhookReadOnlyToolNames); !slices.Equal(got, want) {
		t.Fatalf("hostedReadOnlyToolNames = %v, want %v", got, want)
	}
	for _, name := range sessionWebhookToolNames {
		if slices.Contains(ToolNames(), name) {
			t.Errorf("ToolNames() includes hosted-only %q", name)
		}
	}

	defaultSurface := sortedUnion(ToolNames())
	cases := []struct {
		name    string
		backend Backend
		opts    Options
		want    []string
	}{
		{"webhook backend without the option", &fakeSessionWebhookBackend{}, Options{}, defaultSurface},
		{"both families without the option", &fakeHostedBackend{}, Options{}, defaultSurface},
		{"webhook family alone", &fakeSessionWebhookBackend{}, Options{IncludeHostedTools: true}, sortedUnion(ToolNames(), sessionWebhookToolNames)},
		{"trigger family alone", &fakeTriggerBackend{}, Options{IncludeHostedTools: true}, sortedUnion(ToolNames(), triggerToolNames)},
		{"both families", &fakeHostedBackend{}, Options{IncludeHostedTools: true}, sortedUnion(ToolNames(), hosted)},
		{"read-only keeps the hosted read tools", &fakeHostedBackend{}, Options{IncludeHostedTools: true, ReadOnly: true}, sortedUnion(ReadOnlyToolNames(), hostedReadOnlyToolNames)},
		{"read-only webhook family alone", &fakeSessionWebhookBackend{}, Options{IncludeHostedTools: true, ReadOnly: true}, sortedUnion(ReadOnlyToolNames(), sessionWebhookReadOnlyToolNames)},
		{
			"only filters both families", &fakeHostedBackend{},
			Options{IncludeHostedTools: true, Only: map[string]bool{"list_triggers": true, "list_session_webhooks": true, "delete_session_webhook": true}},
			[]string{"delete_session_webhook", "list_session_webhooks", "list_triggers"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sortedKeys(registeredTools(t, tc.backend, tc.opts)); !slices.Equal(got, tc.want) {
				t.Errorf("registered = %v\nwant %v", got, tc.want)
			}
		})
	}

	tools := registeredTools(t, &fakeSessionWebhookBackend{}, Options{IncludeHostedTools: true})
	for _, name := range sessionWebhookReadOnlyToolNames {
		if a := tools[name].Annotations; a == nil || !a.ReadOnlyHint {
			t.Errorf("%s: want ReadOnlyHint", name)
		}
	}
	for _, name := range []string{"save_session_webhook", "test_session_webhook"} {
		if a := tools[name].Annotations; a == nil || a.ReadOnlyHint || (a.DestructiveHint != nil && *a.DestructiveHint) {
			t.Errorf("%s: want a mutating, non-destructive annotation, got %+v", name, a)
		}
	}
	if a := tools["delete_session_webhook"].Annotations; a == nil || a.DestructiveHint == nil || !*a.DestructiveHint {
		t.Errorf("delete_session_webhook: want DestructiveHint true")
	}

	defs, err := ToolDefinitions(context.Background(), Options{IncludeHostedTools: true})
	if err != nil {
		t.Fatalf("ToolDefinitions: %v", err)
	}
	wantOrder := append(ToolNames(), hosted...)
	gotOrder := make([]string, len(defs))
	for i, d := range defs {
		gotOrder[i] = d.Name
	}
	if !slices.Equal(gotOrder, wantOrder) {
		t.Errorf("ToolDefinitions(IncludeHostedTools) order = %v\nwant %v", gotOrder, wantOrder)
	}
}

func callSessionWebhook(t *testing.T, backend *fakeSessionWebhookBackend, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	cs := newConnectedClient(t, backend, Options{IncludeHostedTools: true})
	return callNoteTool(t, cs, name, args)
}

func callSessionWebhookOK(t *testing.T, backend *fakeSessionWebhookBackend, name string, args map[string]any) map[string]any {
	t.Helper()
	res := callSessionWebhook(t, backend, name, args)
	if res.IsError {
		t.Fatalf("%s returned an error result: %s", name, textOf(t, res))
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &out); err != nil {
		t.Fatalf("%s result is not a JSON object: %v\n%s", name, err, textOf(t, res))
	}
	return out
}

func callSessionWebhookErr(t *testing.T, backend *fakeSessionWebhookBackend, name string, args map[string]any) string {
	t.Helper()
	res := callSessionWebhook(t, backend, name, args)
	if !res.IsError {
		t.Fatalf("%s succeeded, want an error result: %s", name, textOf(t, res))
	}
	return textOf(t, res)
}

// TestSessionWebhookToolsForwarding proves each session webhook tool reaches
// its OWN backend method with every argument mapped to its request field, that
// save_session_webhook routes to create, update or rotate and refuses ambiguous
// input before any RPC, that delete_session_webhook refuses without confirm,
// that the one-time secret appears only in create and rotate results, and that
// API errors come back unchanged.
func TestSessionWebhookToolsForwarding(t *testing.T) {
	t.Run("list_session_webhook_event_types", func(t *testing.T) {
		var reqs []*pb.ListSessionWebhookEventTypesRequest
		b := &fakeSessionWebhookBackend{}
		b.listEventTypes = func(_ context.Context, req *pb.ListSessionWebhookEventTypesRequest) (*pb.ListSessionWebhookEventTypesResponse, error) {
			reqs = append(reqs, req)
			return &pb.ListSessionWebhookEventTypesResponse{EventTypes: []*pb.SessionWebhookEventType{{Type: "session.passing", DisplayLabel: "Passing"}}}, nil
		}
		out := callSessionWebhookOK(t, b, "list_session_webhook_event_types", map[string]any{"organization_id": "org-1"})
		types, _ := out["event_types"].([]any)
		if len(types) != 1 || types[0].(map[string]any)["type"] != "session.passing" || types[0].(map[string]any)["display_label"] != "Passing" {
			t.Errorf("event_types = %v", out)
		}
		callSessionWebhookOK(t, b, "list_session_webhook_event_types", map[string]any{})
		if reqs[0].GetOrganizationId() != "org-1" || reqs[1].GetOrganizationId() != "" {
			t.Errorf("organization ids = %q, %q; want org-1 then empty (the active org)", reqs[0].GetOrganizationId(), reqs[1].GetOrganizationId())
		}
	})

	t.Run("list_session_webhooks", func(t *testing.T) {
		var got *pb.ListSessionWebhooksRequest
		b := &fakeSessionWebhookBackend{}
		b.listWebhooks = func(_ context.Context, req *pb.ListSessionWebhooksRequest) (*pb.ListSessionWebhooksResponse, error) {
			got = req
			if req.GetOrganizationId() == "org-1" {
				return &pb.ListSessionWebhooksResponse{Webhooks: []*pb.SessionWebhook{{Id: "swh-1", IsEnabled: false}}}, nil
			}
			return &pb.ListSessionWebhooksResponse{}, nil
		}
		out := callSessionWebhookOK(t, b, "list_session_webhooks", map[string]any{"organization_id": "org-1"})
		if got.GetOrganizationId() != "org-1" {
			t.Errorf("request = %v", got)
		}
		webhooks, _ := out["webhooks"].([]any)
		if len(webhooks) != 1 {
			t.Fatalf("webhooks = %v, want one", out)
		}
		// EmitDefaultValues keeps is_enabled:false visible.
		if v, ok := webhooks[0].(map[string]any)["is_enabled"]; !ok || v != false {
			t.Errorf("webhook = %v, want is_enabled: false rendered", webhooks[0])
		}
		out = callSessionWebhookOK(t, b, "list_session_webhooks", map[string]any{})
		if webhooks, ok := out["webhooks"].([]any); !ok || len(webhooks) != 0 {
			t.Errorf("empty list = %v, want webhooks: []", out)
		}
	})

	t.Run("list_session_webhook_deliveries maps every filter", func(t *testing.T) {
		var got *pb.ListSessionWebhookDeliveriesRequest
		b := &fakeSessionWebhookBackend{}
		b.listDeliveries = func(_ context.Context, req *pb.ListSessionWebhookDeliveriesRequest) (*pb.ListSessionWebhookDeliveriesResponse, error) {
			got = req
			if req.GetPageToken() == "" {
				return &pb.ListSessionWebhookDeliveriesResponse{
					Deliveries:    []*pb.SessionWebhookDelivery{{Id: "dlv-1", Status: "failed"}},
					NextPageToken: "tok-2",
				}, nil
			}
			return &pb.ListSessionWebhookDeliveriesResponse{}, nil
		}
		out := callSessionWebhookOK(t, b, "list_session_webhook_deliveries", map[string]any{
			"organization_id": "org-1",
			"webhook_id":      "swh-1",
			"status":          "failed",
			"event_type":      "session.passing",
			"is_test":         false,
			"page_size":       10,
		})
		if got.GetOrganizationId() != "org-1" || got.GetWebhookId() != "swh-1" || got.GetStatus() != "failed" ||
			got.GetEventType() != "session.passing" || got.IsTest == nil || got.GetIsTest() || got.GetPageSize() != 10 || got.GetPageToken() != "" {
			t.Errorf("deliveries request = %v", got)
		}
		deliveries, _ := out["deliveries"].([]any)
		if len(deliveries) != 1 || deliveries[0].(map[string]any)["id"] != "dlv-1" || out["next_page_token"] != "tok-2" {
			t.Errorf("result = %v", out)
		}

		out = callSessionWebhookOK(t, b, "list_session_webhook_deliveries", map[string]any{"webhook_id": "swh-1", "page_token": "tok-2"})
		if got.Status != nil || got.EventType != nil || got.IsTest != nil || got.GetPageToken() != "tok-2" {
			t.Errorf("omitted filters were sent: %v", got)
		}
		if deliveries, ok := out["deliveries"].([]any); !ok || len(deliveries) != 0 || out["next_page_token"] != "" {
			t.Errorf("last page = %v, want deliveries: [] and an empty next_page_token", out)
		}
	})

	t.Run("get_session_webhook_delivery", func(t *testing.T) {
		var got *pb.GetSessionWebhookDeliveryRequest
		b := &fakeSessionWebhookBackend{}
		b.getDelivery = func(_ context.Context, req *pb.GetSessionWebhookDeliveryRequest) (*pb.GetSessionWebhookDeliveryResponse, error) {
			got = req
			return &pb.GetSessionWebhookDeliveryResponse{
				Delivery:       &pb.SessionWebhookDelivery{Id: req.GetDeliveryId()},
				Attempts:       []*pb.SessionWebhookDeliveryAttempt{{AttemptNumber: 1, IsSuccess: true}},
				RequestBody:    `{"type":"session.passing"}`,
				RequestHeaders: []*pb.SessionWebhookRequestHeader{{Name: "X-Bossanova-Event-Id", Value: "evt-1"}},
			}, nil
		}
		out := callSessionWebhookOK(t, b, "get_session_webhook_delivery", map[string]any{"organization_id": "org-1", "delivery_id": "dlv-9"})
		if got.GetOrganizationId() != "org-1" || got.GetDeliveryId() != "dlv-9" {
			t.Errorf("request = %v", got)
		}
		if out["delivery"].(map[string]any)["id"] != "dlv-9" || out["request_body"] != `{"type":"session.passing"}` {
			t.Errorf("result = %v", out)
		}
		if attempts, _ := out["attempts"].([]any); len(attempts) != 1 {
			t.Errorf("attempts = %v", out["attempts"])
		}
		if headers, _ := out["request_headers"].([]any); len(headers) != 1 || headers[0].(map[string]any)["name"] != "X-Bossanova-Event-Id" {
			t.Errorf("request_headers = %v", out["request_headers"])
		}
	})

	t.Run("save_session_webhook without id creates and returns the secret", func(t *testing.T) {
		var got *pb.CreateSessionWebhookRequest
		b := &fakeSessionWebhookBackend{}
		b.create = func(_ context.Context, req *pb.CreateSessionWebhookRequest) (*pb.CreateSessionWebhookResponse, error) {
			got = req
			return &pb.CreateSessionWebhookResponse{Webhook: &pb.SessionWebhook{Id: "swh-new", Url: req.GetUrl()}, Secret: sessionWebhookSecret}, nil
		}
		out := callSessionWebhookOK(t, b, "save_session_webhook", map[string]any{
			"organization_id": "org-1",
			"url":             "https://hooks.example.com/boss",
			"description":     "deploy bot",
			"event_types":     []any{"session.passing", "session.failing"},
		})
		if got.GetOrganizationId() != "org-1" || got.GetUrl() != "https://hooks.example.com/boss" || got.GetDescription() != "deploy bot" ||
			!slices.Equal(got.GetEventTypes(), []string{"session.passing", "session.failing"}) {
			t.Errorf("create request = %v", got)
		}
		if !got.GetIsEnabled() {
			t.Error("omitted is_enabled must create an enabled webhook")
		}
		if got.Secret != nil {
			t.Errorf("create request carries a caller-supplied secret %q; the tool must let the server generate it", got.GetSecret())
		}
		if out["secret"] != sessionWebhookSecret || out["secret_note"] != "store it now; it is not shown again" {
			t.Errorf("create result = %v, want the secret and its note", out)
		}
		if out["webhook"].(map[string]any)["id"] != "swh-new" {
			t.Errorf("webhook = %v", out["webhook"])
		}

		callSessionWebhookOK(t, b, "save_session_webhook", map[string]any{"url": "https://h.example.com", "event_types": []any{"session.passing"}, "is_enabled": false})
		if got.GetIsEnabled() || got.GetOrganizationId() != "" {
			t.Errorf("explicit is_enabled:false lost or org invented: %v", got)
		}
	})

	t.Run("save_session_webhook with id updates only the given fields", func(t *testing.T) {
		var got *pb.UpdateSessionWebhookRequest
		b := &fakeSessionWebhookBackend{}
		b.update = func(_ context.Context, req *pb.UpdateSessionWebhookRequest) (*pb.SessionWebhook, error) {
			got = req
			return &pb.SessionWebhook{Id: req.GetId()}, nil
		}
		out := callSessionWebhookOK(t, b, "save_session_webhook", map[string]any{"id": "swh-1", "organization_id": "org-1", "is_enabled": false})
		if got.GetId() != "swh-1" || got.GetOrganizationId() != "org-1" || got.IsEnabled == nil || got.GetIsEnabled() {
			t.Errorf("update request = %v, want id, org and is_enabled false", got)
		}
		if got.Url != nil || got.Description != nil || len(got.EventTypes) != 0 || got.GetShouldReplaceEventTypes() {
			t.Errorf("omitted fields were sent: %v", got)
		}
		if _, ok := out["secret"]; ok {
			t.Errorf("update result carries a secret: %v", out)
		}

		callSessionWebhookOK(t, b, "save_session_webhook", map[string]any{
			"id":          "swh-1",
			"url":         "https://new.example.com",
			"description": "",
			"event_types": []any{"session.merged"},
		})
		if got.GetUrl() != "https://new.example.com" || got.Description == nil || got.GetDescription() != "" ||
			!slices.Equal(got.GetEventTypes(), []string{"session.merged"}) || !got.GetShouldReplaceEventTypes() || got.IsEnabled != nil {
			t.Errorf("second update = %v, want url, cleared description, replaced event types", got)
		}
	})

	t.Run("save_session_webhook rotate_secret rotates and returns the secret", func(t *testing.T) {
		var got *pb.RotateSessionWebhookSecretRequest
		b := &fakeSessionWebhookBackend{}
		b.rotate = func(_ context.Context, req *pb.RotateSessionWebhookSecretRequest) (*pb.RotateSessionWebhookSecretResponse, error) {
			got = req
			return &pb.RotateSessionWebhookSecretResponse{Webhook: &pb.SessionWebhook{Id: req.GetId()}, Secret: sessionWebhookSecret}, nil
		}
		out := callSessionWebhookOK(t, b, "save_session_webhook", map[string]any{"id": "swh-1", "organization_id": "org-1", "rotate_secret": true})
		if got.GetId() != "swh-1" || got.GetOrganizationId() != "org-1" || got.Secret != nil {
			t.Errorf("rotate request = %v, want id and org, no caller secret", got)
		}
		if out["secret"] != sessionWebhookSecret || out["secret_note"] == nil {
			t.Errorf("rotate result = %v", out)
		}
	})

	t.Run("save_session_webhook refuses ambiguous input before any RPC", func(t *testing.T) {
		b := &fakeSessionWebhookBackend{} // every hook unset: reaching the backend reports errNotImpl
		for _, tc := range []struct {
			name string
			args map[string]any
			want string
		}{
			{"rotate without id", map[string]any{"rotate_secret": true}, "needs the id"},
			{"rotate with url", map[string]any{"id": "swh-1", "rotate_secret": true, "url": "https://x.example.com"}, "two calls"},
			{"rotate with is_enabled", map[string]any{"id": "swh-1", "rotate_secret": true, "is_enabled": true}, "two calls"},
			{"id with nothing to change", map[string]any{"id": "swh-1"}, "nothing to change"},
			{"id with empty event_types only", map[string]any{"id": "swh-1", "organization_id": "org-1", "event_types": []any{}}, "nothing to change"},
			{"create without url", map[string]any{"event_types": []any{"session.passing"}}, "url is required"},
			{"create without event_types", map[string]any{"url": "https://x.example.com"}, "event_types is required"},
		} {
			if text := callSessionWebhookErr(t, b, "save_session_webhook", tc.args); !strings.Contains(text, tc.want) || strings.Contains(text, errNotImpl.Error()) {
				t.Errorf("%s: error %q, want it to mention %q without reaching the backend", tc.name, text, tc.want)
			}
		}
	})

	t.Run("API errors pass through verbatim", func(t *testing.T) {
		b := &fakeSessionWebhookBackend{}
		b.create = func(context.Context, *pb.CreateSessionWebhookRequest) (*pb.CreateSessionWebhookResponse, error) {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("url must use https"))
		}
		b.listWebhooks = func(context.Context, *pb.ListSessionWebhooksRequest) (*pb.ListSessionWebhooksResponse, error) {
			return nil, connect.NewError(connect.CodePermissionDenied, errors.New("organization owner role required"))
		}
		b.del = func(context.Context, *pb.DeleteSessionWebhookRequest) error {
			return connect.NewError(connect.CodeNotFound, errors.New("session webhook not found"))
		}
		if text := callSessionWebhookErr(t, b, "save_session_webhook", map[string]any{"url": "http://x", "event_types": []any{"session.passing"}}); text != "invalid_argument: url must use https" {
			t.Errorf("create error = %q, want the API's message verbatim", text)
		}
		if text := callSessionWebhookErr(t, b, "list_session_webhooks", map[string]any{}); text != "permission_denied: organization owner role required" {
			t.Errorf("list error = %q, want the API's message verbatim", text)
		}
		if text := callSessionWebhookErr(t, b, "delete_session_webhook", map[string]any{"id": "swh-x", "confirm": true}); text != "not_found: session webhook not found" {
			t.Errorf("delete error = %q, want the API's message verbatim", text)
		}
	})

	t.Run("test_session_webhook", func(t *testing.T) {
		var got *pb.SendSessionWebhookTestEventRequest
		b := &fakeSessionWebhookBackend{}
		b.sendTest = func(_ context.Context, req *pb.SendSessionWebhookTestEventRequest) (*pb.SendSessionWebhookTestEventResponse, error) {
			got = req
			if req.PayloadJson == nil {
				return &pb.SendSessionWebhookTestEventResponse{Delivery: &pb.SessionWebhookDelivery{Id: "dlv-c", Status: "cancelled"}}, nil
			}
			return &pb.SendSessionWebhookTestEventResponse{
				Delivery: &pb.SessionWebhookDelivery{Id: "dlv-t", IsTest: true, Status: "succeeded"},
				Attempt:  &pb.SessionWebhookDeliveryAttempt{AttemptNumber: 1, IsSuccess: true},
			}, nil
		}
		out := callSessionWebhookOK(t, b, "test_session_webhook", map[string]any{
			"organization_id": "org-1", "webhook_id": "swh-1", "event_type": "session.passing", "payload_json": `{"a":1}`,
		})
		if got.GetOrganizationId() != "org-1" || got.GetWebhookId() != "swh-1" || got.GetEventType() != "session.passing" || got.GetPayloadJson() != `{"a":1}` {
			t.Errorf("test request = %v", got)
		}
		if out["delivery"].(map[string]any)["status"] != "succeeded" || out["attempt"].(map[string]any)["attempt_number"] != float64(1) {
			t.Errorf("result = %v", out)
		}

		res := callSessionWebhook(t, b, "test_session_webhook", map[string]any{"webhook_id": "swh-1", "event_type": "session.passing"})
		if got.PayloadJson != nil {
			t.Errorf("omitted payload_json was sent as %q", got.GetPayloadJson())
		}
		if text := textOf(t, res); !strings.Contains(text, `"attempt": null`) {
			t.Errorf("cancelled result = %s, want attempt: null", text)
		}
	})

	t.Run("delete_session_webhook requires confirm", func(t *testing.T) {
		var deleted []*pb.DeleteSessionWebhookRequest
		b := &fakeSessionWebhookBackend{}
		b.del = func(_ context.Context, req *pb.DeleteSessionWebhookRequest) error {
			deleted = append(deleted, req)
			return nil
		}
		if text := callSessionWebhookErr(t, b, "delete_session_webhook", map[string]any{"id": "swh-1"}); !strings.Contains(text, `requires {"confirm": true}`) {
			t.Errorf("unconfirmed delete error = %q", text)
		}
		if len(deleted) != 0 {
			t.Fatalf("unconfirmed delete reached the backend: %v", deleted)
		}
		out := callSessionWebhookOK(t, b, "delete_session_webhook", map[string]any{"id": "swh-1", "organization_id": "org-1", "confirm": true})
		if len(deleted) != 1 || deleted[0].GetId() != "swh-1" || deleted[0].GetOrganizationId() != "org-1" || out["deleted_session_webhook"] != "swh-1" {
			t.Errorf("deleted = %v, result = %v", deleted, out)
		}
	})

	t.Run("the secret never appears outside create and rotate", func(t *testing.T) {
		b := &fakeSessionWebhookBackend{}
		b.listWebhooks = func(context.Context, *pb.ListSessionWebhooksRequest) (*pb.ListSessionWebhooksResponse, error) {
			return &pb.ListSessionWebhooksResponse{Webhooks: []*pb.SessionWebhook{{Id: "swh-1"}}}, nil
		}
		b.update = func(_ context.Context, req *pb.UpdateSessionWebhookRequest) (*pb.SessionWebhook, error) {
			return &pb.SessionWebhook{Id: req.GetId()}, nil
		}
		b.getDelivery = func(context.Context, *pb.GetSessionWebhookDeliveryRequest) (*pb.GetSessionWebhookDeliveryResponse, error) {
			return &pb.GetSessionWebhookDeliveryResponse{Delivery: &pb.SessionWebhookDelivery{Id: "dlv-1"}}, nil
		}
		for _, call := range []struct {
			name string
			args map[string]any
		}{
			{"list_session_webhooks", map[string]any{}},
			{"get_session_webhook_delivery", map[string]any{"delivery_id": "dlv-1"}},
			{"save_session_webhook", map[string]any{"id": "swh-1", "description": "d"}},
		} {
			text := textOf(t, callSessionWebhook(t, b, call.name, call.args))
			if strings.Contains(text, `"secret"`) || strings.Contains(text, "secret_note") || strings.Contains(text, sessionWebhookSecret) {
				t.Errorf("%s result carries a secret: %s", call.name, text)
			}
		}
	})
}

// TestSaveSessionWebhookArgsHasUpdateFieldsCoversEveryField pins
// hasUpdateFields against the SaveSessionWebhookArgs struct by reflection. The
// rotate guard and the empty-update guard both rely on it: a field added to
// the struct but not to that OR would be silently dropped beside rotate_secret,
// or sent alone as an empty update.
func TestSaveSessionWebhookArgsHasUpdateFieldsCoversEveryField(t *testing.T) {
	t.Parallel()

	routing := map[string]bool{"ID": true, "RotateSecret": true, "OrganizationID": true}
	if (SaveSessionWebhookArgs{ID: "swh-1", RotateSecret: true, OrganizationID: "org-1"}).hasUpdateFields() {
		t.Fatal("hasUpdateFields() = true with only id, rotate_secret and organization_id set")
	}

	typ := reflect.TypeOf(SaveSessionWebhookArgs{})
	for i := range typ.NumField() {
		field := typ.Field(i)
		if routing[field.Name] {
			continue
		}
		t.Run(field.Name, func(t *testing.T) {
			t.Parallel()
			var args SaveSessionWebhookArgs
			value := reflect.ValueOf(&args).Elem().Field(i)
			switch value.Kind() {
			case reflect.String:
				value.SetString("x")
			case reflect.Bool:
				value.SetBool(true)
			case reflect.Pointer:
				value.Set(reflect.New(value.Type().Elem()))
			case reflect.Slice:
				value.Set(reflect.MakeSlice(value.Type(), 1, 1))
			default:
				t.Fatalf("field %s has kind %s: teach this test how to set it", field.Name, value.Kind())
			}
			if !args.hasUpdateFields() {
				t.Errorf("hasUpdateFields() = false with only %s set: add it to hasUpdateFields", field.Name)
			}
		})
	}
}
