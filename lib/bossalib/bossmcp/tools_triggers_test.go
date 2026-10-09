package bossmcp

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

const triggerSecret = "whsec-ONE-TIME-secret-7731"

// fakeTriggerBackend is a fakeBackend that ALSO implements TriggerBackend, so a
// plain fakeBackend is the "backend without triggers" case. Each method has its
// own hook, so a tool wired to a sibling method fails its own subtest.
type fakeTriggerBackend struct {
	fakeBackend

	getTriggerCatalog      func(ctx context.Context) (*pb.TriggerCatalog, error)
	listTriggers           func(ctx context.Context, req *pb.ListTriggersRequest) (*pb.ListTriggersResponse, error)
	getTrigger             func(ctx context.Context, id string) (*pb.Trigger, error)
	listTriggerInvocations func(ctx context.Context, req *pb.ListTriggerInvocationsRequest) (*pb.ListTriggerInvocationsResponse, error)
	createTrigger          func(ctx context.Context, req *pb.CreateTriggerRequest) (*pb.CreateTriggerResponse, error)
	updateTrigger          func(ctx context.Context, req *pb.UpdateTriggerRequest) (*pb.Trigger, error)
	rotateTriggerSecret    func(ctx context.Context, id string) (*pb.RotateTriggerSecretResponse, error)
	testTrigger            func(ctx context.Context, req *pb.TestTriggerRequest) (*pb.TriggerInvocation, error)
	deleteTrigger          func(ctx context.Context, id string) error
}

var _ TriggerBackend = (*fakeTriggerBackend)(nil)

func (f *fakeTriggerBackend) GetTriggerCatalog(ctx context.Context) (*pb.TriggerCatalog, error) {
	if f.getTriggerCatalog == nil {
		return nil, errNotImpl
	}
	return f.getTriggerCatalog(ctx)
}

func (f *fakeTriggerBackend) ListTriggers(ctx context.Context, req *pb.ListTriggersRequest) (*pb.ListTriggersResponse, error) {
	if f.listTriggers == nil {
		return nil, errNotImpl
	}
	return f.listTriggers(ctx, req)
}

func (f *fakeTriggerBackend) GetTrigger(ctx context.Context, id string) (*pb.Trigger, error) {
	if f.getTrigger == nil {
		return nil, errNotImpl
	}
	return f.getTrigger(ctx, id)
}

func (f *fakeTriggerBackend) ListTriggerInvocations(ctx context.Context, req *pb.ListTriggerInvocationsRequest) (*pb.ListTriggerInvocationsResponse, error) {
	if f.listTriggerInvocations == nil {
		return nil, errNotImpl
	}
	return f.listTriggerInvocations(ctx, req)
}

func (f *fakeTriggerBackend) CreateTrigger(ctx context.Context, req *pb.CreateTriggerRequest) (*pb.CreateTriggerResponse, error) {
	if f.createTrigger == nil {
		return nil, errNotImpl
	}
	return f.createTrigger(ctx, req)
}

func (f *fakeTriggerBackend) UpdateTrigger(ctx context.Context, req *pb.UpdateTriggerRequest) (*pb.Trigger, error) {
	if f.updateTrigger == nil {
		return nil, errNotImpl
	}
	return f.updateTrigger(ctx, req)
}

func (f *fakeTriggerBackend) RotateTriggerSecret(ctx context.Context, id string) (*pb.RotateTriggerSecretResponse, error) {
	if f.rotateTriggerSecret == nil {
		return nil, errNotImpl
	}
	return f.rotateTriggerSecret(ctx, id)
}

func (f *fakeTriggerBackend) TestTrigger(ctx context.Context, req *pb.TestTriggerRequest) (*pb.TriggerInvocation, error) {
	if f.testTrigger == nil {
		return nil, errNotImpl
	}
	return f.testTrigger(ctx, req)
}

func (f *fakeTriggerBackend) DeleteTrigger(ctx context.Context, id string) error {
	if f.deleteTrigger == nil {
		return errNotImpl
	}
	return f.deleteTrigger(ctx, id)
}

// registeredTools lists the tools a server built over backend and opts
// advertises, keyed by name.
func registeredTools(t *testing.T, backend Backend, opts Options) map[string]*mcp.Tool {
	t.Helper()
	cs := newConnectedClient(t, backend, opts)
	res, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	out := make(map[string]*mcp.Tool, len(res.Tools))
	for _, tool := range res.Tools {
		out[tool.Name] = tool
	}
	return out
}

func sortedKeys(m map[string]*mcp.Tool) []string {
	out := make([]string, 0, len(m))
	for name := range m {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func sortedUnion(lists ...[]string) []string {
	var out []string
	for _, l := range lists {
		out = append(out, l...)
	}
	sort.Strings(out)
	return out
}

// triggerToolNames is the trigger family of the hosted tier, and
// triggerReadOnlyToolNames its read subset. They are spelled out here, not
// derived from HostedToolNames(), so this test pins the trigger family alone
// and needs no edit when another hosted family is added.
var (
	triggerToolNames         = []string{"get_trigger_catalog", "list_triggers", "get_trigger", "save_trigger", "test_trigger", "delete_trigger"}
	triggerReadOnlyToolNames = []string{"get_trigger_catalog", "list_triggers", "get_trigger"}
)

// TestHostedTriggerTools pins the trigger family's registration rule: the six
// tools register only with IncludeHostedTools AND a TriggerBackend, ReadOnly
// keeps only the three read tools, Only still filters them, and the static
// inventories keep them out of ToolNames(). The whole tier — every family
// together, its inventory and its order — is pinned by
// TestHostedSessionWebhookTools.
func TestHostedTriggerTools(t *testing.T) {
	hosted := HostedToolNames()
	for _, name := range triggerToolNames {
		if !slices.Contains(hosted, name) {
			t.Errorf("HostedToolNames() = %v, missing trigger tool %q", hosted, name)
		}
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
		{"trigger backend without the option", &fakeTriggerBackend{}, Options{}, defaultSurface},
		{"option without a trigger backend", &fakeBackend{}, Options{IncludeHostedTools: true}, defaultSurface},
		{"option and trigger backend", &fakeTriggerBackend{}, Options{IncludeHostedTools: true}, sortedUnion(ToolNames(), triggerToolNames)},
		{"read-only keeps the hosted read tools", &fakeTriggerBackend{}, Options{IncludeHostedTools: true, ReadOnly: true}, sortedUnion(ReadOnlyToolNames(), triggerReadOnlyToolNames)},
		{"only filters hosted tools too", &fakeTriggerBackend{}, Options{IncludeHostedTools: true, Only: map[string]bool{"list_triggers": true, "list_sessions": true}}, []string{"list_sessions", "list_triggers"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sortedKeys(registeredTools(t, tc.backend, tc.opts)); !slices.Equal(got, tc.want) {
				t.Errorf("registered = %v\nwant %v", got, tc.want)
			}
		})
	}

	tools := registeredTools(t, &fakeTriggerBackend{}, Options{IncludeHostedTools: true})
	for _, name := range triggerReadOnlyToolNames {
		if a := tools[name].Annotations; a == nil || !a.ReadOnlyHint {
			t.Errorf("%s: want ReadOnlyHint", name)
		}
	}
	if a := tools["delete_trigger"].Annotations; a == nil || a.DestructiveHint == nil || !*a.DestructiveHint {
		t.Errorf("delete_trigger: want DestructiveHint true")
	}
}

// callTrigger invokes a hosted tool over a server built with the hosted tier.
func callTrigger(t *testing.T, backend *fakeTriggerBackend, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	cs := newConnectedClient(t, backend, Options{IncludeHostedTools: true})
	return callNoteTool(t, cs, name, args)
}

func callTriggerOK(t *testing.T, backend *fakeTriggerBackend, name string, args map[string]any) map[string]any {
	t.Helper()
	res := callTrigger(t, backend, name, args)
	if res.IsError {
		t.Fatalf("%s returned an error result: %s", name, textOf(t, res))
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &out); err != nil {
		t.Fatalf("%s result is not a JSON object: %v\n%s", name, err, textOf(t, res))
	}
	return out
}

func callTriggerErr(t *testing.T, backend *fakeTriggerBackend, name string, args map[string]any) string {
	t.Helper()
	res := callTrigger(t, backend, name, args)
	if !res.IsError {
		t.Fatalf("%s succeeded, want an error result: %s", name, textOf(t, res))
	}
	return textOf(t, res)
}

// TestTriggerToolsForwarding proves each hosted tool reaches its OWN backend
// method with its arguments mapped, that save_trigger routes to create, update
// or rotate, that delete_trigger refuses without confirm, and that the one-time
// secret appears only in create and rotate results.
func TestTriggerToolsForwarding(t *testing.T) {
	t.Run("get_trigger_catalog", func(t *testing.T) {
		b := &fakeTriggerBackend{getTriggerCatalog: func(context.Context) (*pb.TriggerCatalog, error) {
			return &pb.TriggerCatalog{Types: []*pb.TriggerTypeSpec{{Name: "http"}}}, nil
		}}
		out := callTriggerOK(t, b, "get_trigger_catalog", map[string]any{})
		types, _ := out["types"].([]any)
		if len(types) != 1 || types[0].(map[string]any)["name"] != "http" {
			t.Errorf("catalog = %v, want one http type", out)
		}
	})

	t.Run("list_triggers", func(t *testing.T) {
		var reqs []*pb.ListTriggersRequest
		b := &fakeTriggerBackend{listTriggers: func(_ context.Context, req *pb.ListTriggersRequest) (*pb.ListTriggersResponse, error) {
			reqs = append(reqs, req)
			if req.GetOrganizationId() == "org-1" {
				return &pb.ListTriggersResponse{Triggers: []*pb.Trigger{{Id: "trg-1", ConcurrencyPolicy: pb.TriggerConcurrencyPolicy_TRIGGER_CONCURRENCY_POLICY_CANCEL_IN_PROGRESS}}}, nil
			}
			return &pb.ListTriggersResponse{}, nil
		}}
		out := callTriggerOK(t, b, "list_triggers", map[string]any{"organization_id": "org-1"})
		triggers, _ := out["triggers"].([]any)
		if len(triggers) != 1 {
			t.Fatalf("triggers = %v, want one", out)
		}
		// Enums render as their value names, the spelling save_trigger accepts.
		if got := triggers[0].(map[string]any)["concurrency_policy"]; got != "TRIGGER_CONCURRENCY_POLICY_CANCEL_IN_PROGRESS" {
			t.Errorf("concurrency_policy = %v, want the enum value name", got)
		}
		out = callTriggerOK(t, b, "list_triggers", map[string]any{})
		if reqs[1].OrganizationId != nil {
			t.Errorf("omitted organization_id was sent as %q", reqs[1].GetOrganizationId())
		}
		if triggers, ok := out["triggers"].([]any); !ok || len(triggers) != 0 {
			t.Errorf("empty list = %v, want triggers: []", out)
		}
	})

	t.Run("get_trigger includes recent invocations", func(t *testing.T) {
		var gotID string
		var invReq *pb.ListTriggerInvocationsRequest
		b := &fakeTriggerBackend{
			getTrigger: func(_ context.Context, id string) (*pb.Trigger, error) {
				gotID = id
				return &pb.Trigger{Id: id}, nil
			},
			listTriggerInvocations: func(_ context.Context, req *pb.ListTriggerInvocationsRequest) (*pb.ListTriggerInvocationsResponse, error) {
				invReq = req
				return &pb.ListTriggerInvocationsResponse{Invocations: []*pb.TriggerInvocation{{Id: "inv-1", DecisionReason: "cooldown"}}}, nil
			},
		}
		out := callTriggerOK(t, b, "get_trigger", map[string]any{"id": "trg-9"})
		if gotID != "trg-9" || invReq.GetTriggerId() != "trg-9" || invReq.GetLimit() != 20 {
			t.Errorf("get id = %q, invocations req = %v; want trg-9 and limit 20", gotID, invReq)
		}
		recent, _ := out["recent_invocations"].([]any)
		if len(recent) != 1 || recent[0].(map[string]any)["decision_reason"] != "cooldown" {
			t.Errorf("recent_invocations = %v", out["recent_invocations"])
		}
		if out["trigger"].(map[string]any)["id"] != "trg-9" {
			t.Errorf("trigger = %v", out["trigger"])
		}

		b.listTriggerInvocations = func(context.Context, *pb.ListTriggerInvocationsRequest) (*pb.ListTriggerInvocationsResponse, error) {
			return &pb.ListTriggerInvocationsResponse{}, nil
		}
		res := callTrigger(t, b, "get_trigger", map[string]any{"id": "trg-9"})
		if text := textOf(t, res); !strings.Contains(text, `"recent_invocations": []`) {
			t.Errorf("no invocations = %s, want recent_invocations: []", text)
		}
	})

	t.Run("save_trigger without id creates and returns the secret", func(t *testing.T) {
		var got *pb.CreateTriggerRequest
		b := &fakeTriggerBackend{createTrigger: func(_ context.Context, req *pb.CreateTriggerRequest) (*pb.CreateTriggerResponse, error) {
			got = req
			return &pb.CreateTriggerResponse{Trigger: &pb.Trigger{Id: "trg-new"}, Secret: triggerSecret}, nil
		}}
		out := callTriggerOK(t, b, "save_trigger", map[string]any{
			"organization_id":    "org-1",
			"name":               "deploys",
			"trigger_type":       "http",
			"http":               map[string]any{"allowed_methods": []any{"POST", "PUT"}, "dedup_window_seconds": 60},
			"repo_origin_url":    "https://github.com/acme/widgets",
			"launch":             map[string]any{"prompt_template": "ship {{body.ref}}", "skill_name": "boss-build", "model": "opus"},
			"placement":          map[string]any{"mode": "first_available"},
			"concurrency_policy": "CANCEL_IN_PROGRESS",
			"cooldown_seconds":   30,
			"filters":            []any{map[string]any{"field": "body.ref", "operator": "PREFIX", "values": []any{"refs/heads/"}}},
			"payload_fields":     []any{"body.ref"},
		})
		if got.GetOrganizationId() != "org-1" || got.GetName() != "deploys" || got.GetTriggerType() != "http" ||
			got.GetRepoOriginUrl() != "https://github.com/acme/widgets" || got.GetCooldownSeconds() != 30 {
			t.Errorf("create request scalars = %v", got)
		}
		if !got.GetIsEnabled() {
			t.Error("omitted is_enabled must create an enabled trigger")
		}
		if h := got.GetHttp(); h == nil || !slices.Equal(h.GetAllowedMethods(), []string{"POST", "PUT"}) || h.GetDedupWindowSeconds() != 60 {
			t.Errorf("http config = %v", got.GetHttp())
		}
		if l := got.GetLaunch(); l.GetPromptTemplate() != "ship {{body.ref}}" || l.GetSkillName() != "boss-build" || l.GetModel() != "opus" || l.Effort != nil {
			t.Errorf("launch = %v", got.GetLaunch())
		}
		if got.GetPlacement().GetMode() != pb.TriggerPlacementMode_TRIGGER_PLACEMENT_MODE_FIRST_AVAILABLE {
			t.Errorf("placement = %v", got.GetPlacement())
		}
		if got.GetConcurrencyPolicy() != pb.TriggerConcurrencyPolicy_TRIGGER_CONCURRENCY_POLICY_CANCEL_IN_PROGRESS {
			t.Errorf("concurrency_policy = %v", got.GetConcurrencyPolicy())
		}
		if f := got.GetFilters(); len(f) != 1 || f[0].GetOperator() != pb.TriggerFilterOperator_TRIGGER_FILTER_OPERATOR_PREFIX || f[0].GetField() != "body.ref" {
			t.Errorf("filters = %v", got.GetFilters())
		}
		if !slices.Equal(got.GetPayloadFields(), []string{"body.ref"}) {
			t.Errorf("payload_fields = %v", got.GetPayloadFields())
		}
		if out["secret"] != triggerSecret || out["secret_note"] != "store it now; it is not shown again" {
			t.Errorf("create result = %v, want the secret and its note", out)
		}

		callTriggerOK(t, b, "save_trigger", map[string]any{"organization_id": "org-1", "is_enabled": false, "github": map[string]any{"event_types": []any{"push"}}})
		if got.GetIsEnabled() || got.GetGithub() == nil {
			t.Errorf("explicit is_enabled:false / github config lost: %v", got)
		}
	})

	t.Run("save_trigger create without a secret omits it", func(t *testing.T) {
		b := &fakeTriggerBackend{createTrigger: func(context.Context, *pb.CreateTriggerRequest) (*pb.CreateTriggerResponse, error) {
			return &pb.CreateTriggerResponse{Trigger: &pb.Trigger{Id: "trg-gh"}}, nil
		}}
		out := callTriggerOK(t, b, "save_trigger", map[string]any{"organization_id": "org-1", "trigger_type": "github"})
		if _, ok := out["secret"]; ok {
			t.Errorf("result = %v, want no secret key when none was issued", out)
		}
		if _, ok := out["secret_note"]; ok {
			t.Errorf("result = %v, want no secret_note when none was issued", out)
		}
	})

	t.Run("save_trigger with id updates only the given fields", func(t *testing.T) {
		var got *pb.UpdateTriggerRequest
		b := &fakeTriggerBackend{
			updateTrigger: func(_ context.Context, req *pb.UpdateTriggerRequest) (*pb.Trigger, error) {
				got = req
				return &pb.Trigger{Id: req.GetId()}, nil
			},
			createTrigger: func(context.Context, *pb.CreateTriggerRequest) (*pb.CreateTriggerResponse, error) {
				t.Fatal("update must not call CreateTrigger")
				return nil, nil
			},
		}
		out := callTriggerOK(t, b, "save_trigger", map[string]any{
			"id":                          "trg-1",
			"is_enabled":                  false,
			"should_clear_filters":        true,
			"should_clear_payload_fields": true,
		})
		if got.GetId() != "trg-1" || got.IsEnabled == nil || *got.IsEnabled {
			t.Errorf("update request = %v, want id trg-1 and is_enabled false", got)
		}
		if got.Name != nil || got.RepoOriginUrl != nil || got.ConcurrencyPolicy != nil || got.CooldownSeconds != nil ||
			got.Launch != nil || got.Placement != nil || got.TypeConfig != nil || len(got.Filters) != 0 || len(got.PayloadFields) != 0 {
			t.Errorf("omitted fields were sent: %v", got)
		}
		if !got.GetShouldClearFilters() || !got.GetShouldClearPayloadFields() {
			t.Errorf("clear flags = %v", got)
		}
		if _, ok := out["secret"]; ok {
			t.Errorf("update result carries a secret: %v", out)
		}

		callTriggerOK(t, b, "save_trigger", map[string]any{"id": "trg-1", "name": "renamed", "concurrency_policy": "TRIGGER_CONCURRENCY_POLICY_ALLOW_PARALLEL", "cooldown_seconds": 0})
		if got.GetName() != "renamed" || got.GetConcurrencyPolicy() != pb.TriggerConcurrencyPolicy_TRIGGER_CONCURRENCY_POLICY_ALLOW_PARALLEL ||
			got.CooldownSeconds == nil || got.GetCooldownSeconds() != 0 || got.IsEnabled != nil {
			t.Errorf("second update = %v", got)
		}
	})

	t.Run("save_trigger rotate_secret rotates and returns the secret", func(t *testing.T) {
		var gotID string
		b := &fakeTriggerBackend{rotateTriggerSecret: func(_ context.Context, id string) (*pb.RotateTriggerSecretResponse, error) {
			gotID = id
			return &pb.RotateTriggerSecretResponse{Trigger: &pb.Trigger{Id: id}, Secret: triggerSecret}, nil
		}}
		out := callTriggerOK(t, b, "save_trigger", map[string]any{"id": "trg-1", "rotate_secret": true})
		if gotID != "trg-1" || out["secret"] != triggerSecret || out["secret_note"] == nil {
			t.Errorf("rotate id = %q, result = %v", gotID, out)
		}
	})

	t.Run("save_trigger refuses ambiguous input before any RPC", func(t *testing.T) {
		b := &fakeTriggerBackend{} // every hook unset: reaching the backend reports errNotImpl
		for _, tc := range []struct {
			name string
			args map[string]any
			want string
		}{
			{"rotate without id", map[string]any{"rotate_secret": true}, "needs the id"},
			{"rotate with changes", map[string]any{"id": "trg-1", "rotate_secret": true, "name": "x"}, "two calls"},
			{"unknown enum", map[string]any{"organization_id": "o", "concurrency_policy": "SOMETIMES"}, "SKIP_IF_RUNNING, ALLOW_PARALLEL, CANCEL_IN_PROGRESS"},
			{"retired queue latest", map[string]any{"organization_id": "o", "concurrency_policy": "QUEUE_LATEST"}, "is not one of SKIP_IF_RUNNING, ALLOW_PARALLEL, CANCEL_IN_PROGRESS"},
			{"unspecified enum", map[string]any{"id": "trg-1", "placement": map[string]any{"mode": "UNSPECIFIED"}}, "SPECIFIC_DAEMON, FIRST_AVAILABLE"},
			{"bad filter operator", map[string]any{"id": "trg-1", "filters": []any{map[string]any{"field": "f", "operator": "LIKE", "values": []any{}}}}, "filters[0].operator"},
			{"immutable field on update", map[string]any{"id": "trg-1", "trigger_type": "github"}, "cannot change"},
			{"clear flag on create", map[string]any{"organization_id": "o", "should_clear_filters": true}, "only to an update"},
			{"both type configs", map[string]any{"organization_id": "o", "http": map[string]any{}, "github": map[string]any{"event_types": []any{}}}, "at most one"},
		} {
			if text := callTriggerErr(t, b, "save_trigger", tc.args); !strings.Contains(text, tc.want) {
				t.Errorf("%s: error %q, want it to mention %q", tc.name, text, tc.want)
			}
		}
	})

	t.Run("server validation errors pass through verbatim", func(t *testing.T) {
		b := &fakeTriggerBackend{createTrigger: func(context.Context, *pb.CreateTriggerRequest) (*pb.CreateTriggerResponse, error) {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("launch.prompt_template is required"))
		}}
		if text := callTriggerErr(t, b, "save_trigger", map[string]any{"organization_id": "o"}); text != "invalid_argument: launch.prompt_template is required" {
			t.Errorf("error = %q, want the server's message verbatim", text)
		}
	})

	t.Run("test_trigger", func(t *testing.T) {
		var got *pb.TestTriggerRequest
		b := &fakeTriggerBackend{testTrigger: func(_ context.Context, req *pb.TestTriggerRequest) (*pb.TriggerInvocation, error) {
			got = req
			return &pb.TriggerInvocation{Id: "inv-t", Source: "test", Status: pb.TriggerInvocationStatus_TRIGGER_INVOCATION_STATUS_FILTERED}, nil
		}}
		out := callTriggerOK(t, b, "test_trigger", map[string]any{"trigger_id": "trg-1", "sample_payload_json": `{"ref":"main"}`, "sample_event_type": "push"})
		if got.GetTriggerId() != "trg-1" || got.GetSamplePayloadJson() != `{"ref":"main"}` || got.GetSampleEventType() != "push" || got.GetShouldLaunch() {
			t.Errorf("test request = %v, want should_launch false by default", got)
		}
		if inv := out["invocation"].(map[string]any); inv["status"] != "TRIGGER_INVOCATION_STATUS_FILTERED" {
			t.Errorf("invocation = %v", inv)
		}
		callTriggerOK(t, b, "test_trigger", map[string]any{"trigger_id": "trg-1", "sample_payload_json": "{}", "should_launch": true})
		if !got.GetShouldLaunch() {
			t.Error("should_launch:true was not forwarded")
		}
	})

	t.Run("delete_trigger requires confirm", func(t *testing.T) {
		var deleted []string
		b := &fakeTriggerBackend{deleteTrigger: func(_ context.Context, id string) error {
			deleted = append(deleted, id)
			return nil
		}}
		if text := callTriggerErr(t, b, "delete_trigger", map[string]any{"id": "trg-1"}); !strings.Contains(text, `requires {"confirm": true}`) {
			t.Errorf("unconfirmed delete error = %q", text)
		}
		if len(deleted) != 0 {
			t.Fatalf("unconfirmed delete reached the backend: %v", deleted)
		}
		out := callTriggerOK(t, b, "delete_trigger", map[string]any{"id": "trg-1", "confirm": true})
		if !slices.Equal(deleted, []string{"trg-1"}) || out["deleted_trigger"] != "trg-1" {
			t.Errorf("deleted = %v, result = %v", deleted, out)
		}
	})

	t.Run("the secret never appears outside create and rotate", func(t *testing.T) {
		// Even if a backend wrongly returned a secret-bearing field elsewhere,
		// the read and update tools have no secret key to put it in: pin that
		// their results never contain the secret string.
		b := &fakeTriggerBackend{
			listTriggers: func(context.Context, *pb.ListTriggersRequest) (*pb.ListTriggersResponse, error) {
				return &pb.ListTriggersResponse{Triggers: []*pb.Trigger{{Id: "trg-1"}}}, nil
			},
			getTrigger: func(_ context.Context, id string) (*pb.Trigger, error) { return &pb.Trigger{Id: id}, nil },
			listTriggerInvocations: func(context.Context, *pb.ListTriggerInvocationsRequest) (*pb.ListTriggerInvocationsResponse, error) {
				return &pb.ListTriggerInvocationsResponse{}, nil
			},
			updateTrigger: func(_ context.Context, req *pb.UpdateTriggerRequest) (*pb.Trigger, error) {
				return &pb.Trigger{Id: req.GetId()}, nil
			},
		}
		for _, call := range []struct {
			name string
			args map[string]any
		}{
			{"list_triggers", map[string]any{}},
			{"get_trigger", map[string]any{"id": "trg-1"}},
			{"save_trigger", map[string]any{"id": "trg-1", "name": "n"}},
		} {
			text := textOf(t, callTrigger(t, b, call.name, call.args))
			if strings.Contains(text, "secret") {
				t.Errorf("%s result mentions a secret: %s", call.name, text)
			}
		}
	})
}

// TestSaveTriggerArgsHasUpdateFieldsCoversEveryField pins hasUpdateFields
// against the SaveTriggerArgs struct by reflection. hasUpdateFields is a
// hand-written OR over the fields, and the rotate guard in save_trigger relies
// on it: a field added to the struct but not to that OR would be silently
// dropped when combined with rotate_secret instead of being refused.
func TestSaveTriggerArgsHasUpdateFieldsCoversEveryField(t *testing.T) {
	t.Parallel()

	if (SaveTriggerArgs{ID: "trg_1", RotateSecret: true}).hasUpdateFields() {
		t.Fatal("hasUpdateFields() = true with only id and rotate_secret set")
	}

	typ := reflect.TypeOf(SaveTriggerArgs{})
	for i := range typ.NumField() {
		field := typ.Field(i)
		if field.Name == "ID" || field.Name == "RotateSecret" {
			continue
		}
		t.Run(field.Name, func(t *testing.T) {
			t.Parallel()
			var args SaveTriggerArgs
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
				t.Errorf("hasUpdateFields() = false with only %s set: add it to hasUpdateFields so rotate_secret refuses it", field.Name)
			}
		})
	}
}
