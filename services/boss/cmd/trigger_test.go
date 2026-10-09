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
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

const (
	testTriggerSecret   = "whsec_super_secret_value"
	testRotatedSecret   = "whsec_rotated_secret_value"
	testTriggerCloudURL = "https://cloud.example.test"
)

// fakeTriggerServer is a fake orchestrator. Every trigger RPC records its
// request in its OWN field, so a command wired to a sibling RPC fails the
// assertion for the RPC it should have called. calls counts every request that
// reached the server, which is what "nothing is sent" is measured against.
type fakeTriggerServer struct {
	bossanovav1connect.UnimplementedOrchestratorServiceHandler

	calls atomic.Int32

	orgs       []*pb.Organization
	trigger    *pb.Trigger
	triggers   []*pb.Trigger
	catalog    *pb.TriggerCatalog
	invocation *pb.TriggerInvocation
	history    []*pb.TriggerInvocation
	createErr  error

	catalogHit bool
	listReq    *pb.ListTriggersRequest
	getReq     *pb.GetTriggerRequest
	createReq  *pb.CreateTriggerRequest
	updateReq  *pb.UpdateTriggerRequest
	deleteReq  *pb.DeleteTriggerRequest
	rotateReq  *pb.RotateTriggerSecretRequest
	testReq    *pb.TestTriggerRequest
	historyReq *pb.ListTriggerInvocationsRequest
}

func (f *fakeTriggerServer) ListOrganizations(context.Context, *connect.Request[pb.ListOrganizationsRequest]) (*connect.Response[pb.ListOrganizationsResponse], error) {
	f.calls.Add(1)
	return connect.NewResponse(&pb.ListOrganizationsResponse{Organizations: f.orgs}), nil
}

func (f *fakeTriggerServer) GetTriggerCatalog(context.Context, *connect.Request[pb.GetTriggerCatalogRequest]) (*connect.Response[pb.GetTriggerCatalogResponse], error) {
	f.calls.Add(1)
	f.catalogHit = true
	return connect.NewResponse(&pb.GetTriggerCatalogResponse{Catalog: f.catalog}), nil
}

func (f *fakeTriggerServer) ListTriggers(_ context.Context, req *connect.Request[pb.ListTriggersRequest]) (*connect.Response[pb.ListTriggersResponse], error) {
	f.calls.Add(1)
	f.listReq = req.Msg
	return connect.NewResponse(&pb.ListTriggersResponse{Triggers: f.triggers}), nil
}

func (f *fakeTriggerServer) GetTrigger(_ context.Context, req *connect.Request[pb.GetTriggerRequest]) (*connect.Response[pb.GetTriggerResponse], error) {
	f.calls.Add(1)
	f.getReq = req.Msg
	return connect.NewResponse(&pb.GetTriggerResponse{Trigger: f.trigger}), nil
}

func (f *fakeTriggerServer) CreateTrigger(_ context.Context, req *connect.Request[pb.CreateTriggerRequest]) (*connect.Response[pb.CreateTriggerResponse], error) {
	f.calls.Add(1)
	f.createReq = req.Msg
	if f.createErr != nil {
		return nil, f.createErr
	}
	resp := &pb.CreateTriggerResponse{Trigger: f.trigger}
	if req.Msg.GetTriggerType() == "http" {
		resp.Secret = testTriggerSecret
	}
	return connect.NewResponse(resp), nil
}

func (f *fakeTriggerServer) UpdateTrigger(_ context.Context, req *connect.Request[pb.UpdateTriggerRequest]) (*connect.Response[pb.UpdateTriggerResponse], error) {
	f.calls.Add(1)
	f.updateReq = req.Msg
	return connect.NewResponse(&pb.UpdateTriggerResponse{Trigger: f.trigger}), nil
}

func (f *fakeTriggerServer) DeleteTrigger(_ context.Context, req *connect.Request[pb.DeleteTriggerRequest]) (*connect.Response[pb.DeleteTriggerResponse], error) {
	f.calls.Add(1)
	f.deleteReq = req.Msg
	return connect.NewResponse(&pb.DeleteTriggerResponse{}), nil
}

func (f *fakeTriggerServer) RotateTriggerSecret(_ context.Context, req *connect.Request[pb.RotateTriggerSecretRequest]) (*connect.Response[pb.RotateTriggerSecretResponse], error) {
	f.calls.Add(1)
	f.rotateReq = req.Msg
	return connect.NewResponse(&pb.RotateTriggerSecretResponse{Trigger: f.trigger, Secret: testRotatedSecret}), nil
}

func (f *fakeTriggerServer) TestTrigger(_ context.Context, req *connect.Request[pb.TestTriggerRequest]) (*connect.Response[pb.TestTriggerResponse], error) {
	f.calls.Add(1)
	f.testReq = req.Msg
	return connect.NewResponse(&pb.TestTriggerResponse{Invocation: f.invocation}), nil
}

func (f *fakeTriggerServer) ListTriggerInvocations(_ context.Context, req *connect.Request[pb.ListTriggerInvocationsRequest]) (*connect.Response[pb.ListTriggerInvocationsResponse], error) {
	f.calls.Add(1)
	f.historyReq = req.Msg
	return connect.NewResponse(&pb.ListTriggerInvocationsResponse{Invocations: f.history}), nil
}

// newFakeTriggerServer serves fake over HTTP and returns its base URL.
func newFakeTriggerServer(t *testing.T, fake *fakeTriggerServer) string {
	t.Helper()
	path, handler := bossanovav1connect.NewOrchestratorServiceHandler(fake)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// sampleTrigger is a fully populated HTTP trigger, so JSON field-set checks see
// every key populated.
func sampleTrigger() *pb.Trigger {
	ts := timestamppb.New(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	model := "opus"
	return &pb.Trigger{
		Id: "tr-1", OrganizationId: "org-1", CreatorUserId: "user-1", Name: "deploy hook",
		IsEnabled: true, TriggerType: "http", ConfigVersion: 1,
		TypeConfig: &pb.Trigger_Http{Http: &pb.HttpTriggerConfig{
			AllowedMethods: []string{"POST"}, IdempotencyHeader: "X-Key", DedupWindowSeconds: 300,
		}},
		RepoOriginUrl: "https://github.com/acme/widget",
		Launch: &pb.TriggerLaunchSettings{
			PromptTemplate: "Investigate the deploy", SkillName: "boss-build", AgentName: "claude",
			Model: &model, BaseBranch: "main",
		},
		Placement:         &pb.TriggerPlacement{Mode: pb.TriggerPlacementMode_TRIGGER_PLACEMENT_MODE_FIRST_AVAILABLE},
		ConcurrencyPolicy: pb.TriggerConcurrencyPolicy_TRIGGER_CONCURRENCY_POLICY_SKIP_IF_RUNNING,
		CooldownSeconds:   300,
		Filters: []*pb.TriggerFilter{{
			Field: "body.ref", Operator: pb.TriggerFilterOperator_TRIGGER_FILTER_OPERATOR_EQUALS, Values: []string{"refs/heads/main"},
		}},
		PayloadFields: []string{"body.ref"},
		EndpointPath:  "/triggers/http/pub-1",
		LastInvocation: &pb.TriggerInvocationSummary{
			Status: pb.TriggerInvocationStatus_TRIGGER_INVOCATION_STATUS_LAUNCHED, ReceivedAt: ts, SessionId: "sess-1",
		},
		CreatedAt: ts, UpdatedAt: ts,
	}
}

func sampleInvocation() *pb.TriggerInvocation {
	ts := timestamppb.New(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	return &pb.TriggerInvocation{
		Id: "inv-1", TriggerId: "tr-1", Source: "test", EventType: "POST",
		Status: pb.TriggerInvocationStatus_TRIGGER_INVOCATION_STATUS_FILTERED, DecisionReason: "filter_mismatch",
		ErrorDetail: "body.ref did not match", PayloadExcerpt: `{"body.ref":"refs/heads/dev"}`,
		DaemonId: "d-1", SessionId: "", AttemptCount: 0, ReceivedAt: ts, DecidedAt: ts,
	}
}

func newSampleFake() *fakeTriggerServer {
	return &fakeTriggerServer{
		orgs:       []*pb.Organization{{Id: "org-1", Name: "Acme"}},
		trigger:    sampleTrigger(),
		triggers:   []*pb.Trigger{sampleTrigger()},
		invocation: sampleInvocation(),
		history:    []*pb.TriggerInvocation{sampleInvocation()},
		catalog: &pb.TriggerCatalog{Types: []*pb.TriggerTypeSpec{{
			Name: "github", DisplayName: "GitHub", ConfigVersion: 1,
			EventTypes: []*pb.TriggerEventTypeSpec{{Id: "pull_request.opened", DisplayName: "PR opened", Description: "A pull request was opened"}},
			FilterFields: []*pb.TriggerFilterFieldSpec{{
				Field: "body.", Description: "Any payload field", IsPrefix: true,
				Operators: []pb.TriggerFilterOperator{pb.TriggerFilterOperator_TRIGGER_FILTER_OPERATOR_EQUALS, pb.TriggerFilterOperator_TRIGGER_FILTER_OPERATOR_PREFIX},
			}},
		}}},
	}
}

// runTriggerCLI runs `boss trigger <args>` against fake through the real
// RemoteClient and the shipped flag definitions, returning stdout.
func runTriggerCLI(t *testing.T, fake *fakeTriggerServer, stdin string, args ...string) (string, error) {
	t.Helper()
	url := newFakeTriggerServer(t, fake)
	t.Setenv("BOSS_CLOUD_URL", testTriggerCloudURL)
	prev := newTriggerClient
	newTriggerClient = func(*cobra.Command) (triggerClient, error) { return client.NewRemote(url, "tok"), nil }
	t.Cleanup(func() { newTriggerClient = prev })

	cmd := triggerCmd()
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

func decodeJSONObject(t *testing.T, out string) map[string]json.RawMessage {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		t.Fatalf("output %q is not one JSON object: %v", out, err)
	}
	return obj
}

// triggerJSONKeys is the documented key set of one trigger in every --json
// output. Pinning it makes a renamed or leaked key (above all, a secret) a
// test failure.
var triggerJSONKeys = []string{
	"concurrency_policy", "config_version", "cooldown_seconds", "created_at", "creator_user_id",
	"enabled", "endpoint_path", "endpoint_url", "filters", "github", "http", "id", "last_invocation",
	"launch", "name", "organization_id", "payload_fields", "placement", "repo_origin_url", "type",
	"updated_at",
}

var invocationJSONKeys = []string{
	"attempt_count", "daemon_id", "decided_at", "decision_reason", "error_detail", "event_type", "id",
	"launched_at", "payload_excerpt", "received_at", "session_id", "source", "status", "trigger_id",
}

// TestTriggerCommands proves each `boss trigger` subcommand calls its own RPC
// with the fields its flags describe, and that --json output keeps a stable
// key set.
func TestTriggerCommands(t *testing.T) {
	t.Run("subcommands", func(t *testing.T) {
		var names []string
		for _, sub := range triggerCmd().Commands() {
			names = append(names, sub.Name())
			if sub.Flags().Lookup(jsonFlagName) == nil {
				t.Errorf("boss trigger %s has no --json flag", sub.Name())
			}
		}
		sort.Strings(names)
		want := []string{"add", "catalog", "disable", "enable", "history", "ls", "rm", "rotate-secret", "show", "test", "update"}
		if !slices.Equal(names, want) {
			t.Fatalf("trigger subcommands = %v, want %v", names, want)
		}
	})

	t.Run("catalog", func(t *testing.T) {
		fake := newSampleFake()
		out, err := runTriggerCLI(t, fake, "", "catalog")
		if err != nil {
			t.Fatalf("catalog: %v", err)
		}
		if !fake.catalogHit || !strings.Contains(out, "pull_request.opened") || !strings.Contains(out, "body.") {
			t.Errorf("catalog output = %q", out)
		}
		out, err = runTriggerCLI(t, fake, "", "catalog", "--json")
		if err != nil {
			t.Fatalf("catalog --json: %v", err)
		}
		obj := decodeJSONObject(t, out)
		if got := sortedJSONKeys(obj); !slices.Equal(got, []string{"types"}) {
			t.Fatalf("catalog keys = %v", got)
		}
		var types []map[string]json.RawMessage
		if err := json.Unmarshal(obj["types"], &types); err != nil || len(types) != 1 {
			t.Fatalf("decode types: %v", err)
		}
		if got, want := sortedJSONKeys(types[0]), []string{"config_version", "display_name", "event_types", "filter_fields", "name"}; !slices.Equal(got, want) {
			t.Errorf("type keys = %v, want %v", got, want)
		}
		if !strings.Contains(string(types[0]["filter_fields"]), `"prefix"`) {
			t.Errorf("filter_fields = %s, want operator spellings", types[0]["filter_fields"])
		}
	})

	t.Run("ls", func(t *testing.T) {
		fake := newSampleFake()
		out, err := runTriggerCLI(t, fake, "", "ls", "--org", "org-9")
		if err != nil {
			t.Fatalf("ls: %v", err)
		}
		if fake.listReq.GetOrganizationId() != "org-9" {
			t.Errorf("ls request = %v, want org-9", fake.listReq)
		}
		for _, want := range []string{"tr-1", "deploy hook", "http", "first-available", "launched"} {
			if !strings.Contains(out, want) {
				t.Errorf("ls output missing %q:\n%s", want, out)
			}
		}

		out, err = runTriggerCLI(t, fake, "", "ls", "--json")
		if err != nil {
			t.Fatalf("ls --json: %v", err)
		}
		if fake.listReq.OrganizationId != nil {
			t.Errorf("ls without --org sent organization_id %q", fake.listReq.GetOrganizationId())
		}
		obj := decodeJSONObject(t, out)
		var triggers []map[string]json.RawMessage
		if err := json.Unmarshal(obj["triggers"], &triggers); err != nil || len(triggers) != 1 {
			t.Fatalf("decode triggers: %v", err)
		}
		if got := sortedJSONKeys(triggers[0]); !slices.Equal(got, triggerJSONKeys) {
			t.Errorf("trigger keys = %v, want %v", got, triggerJSONKeys)
		}

		fake.triggers = nil
		out, err = runTriggerCLI(t, fake, "", "ls", "--json")
		if err != nil {
			t.Fatalf("empty ls --json: %v", err)
		}
		if strings.TrimSpace(out) != "{\n  \"triggers\": []\n}" {
			t.Errorf("empty ls --json = %q, want an empty array", out)
		}
	})

	t.Run("show", func(t *testing.T) {
		fake := newSampleFake()
		out, err := runTriggerCLI(t, fake, "", "show", "tr-1")
		if err != nil {
			t.Fatalf("show: %v", err)
		}
		if fake.getReq.GetId() != "tr-1" || fake.historyReq.GetTriggerId() != "tr-1" || fake.historyReq.GetLimit() != 20 {
			t.Errorf("show requests: get=%v history=%v", fake.getReq, fake.historyReq)
		}
		for _, want := range []string{testTriggerCloudURL + "/triggers/http/pub-1", "body.ref = refs/heads/main", "filter_mismatch", "Investigate the deploy"} {
			if !strings.Contains(out, want) {
				t.Errorf("show output missing %q:\n%s", want, out)
			}
		}

		out, err = runTriggerCLI(t, fake, "", "show", "tr-1", "--json")
		if err != nil {
			t.Fatalf("show --json: %v", err)
		}
		obj := decodeJSONObject(t, out)
		if got := sortedJSONKeys(obj); !slices.Equal(got, []string{"invocations", "trigger"}) {
			t.Fatalf("show keys = %v", got)
		}
		var trig map[string]json.RawMessage
		if err := json.Unmarshal(obj["trigger"], &trig); err != nil {
			t.Fatal(err)
		}
		if got := sortedJSONKeys(trig); !slices.Equal(got, triggerJSONKeys) {
			t.Errorf("trigger keys = %v, want %v", got, triggerJSONKeys)
		}
		if string(trig["endpoint_url"]) != `"`+testTriggerCloudURL+`/triggers/http/pub-1"` {
			t.Errorf("endpoint_url = %s", trig["endpoint_url"])
		}
		var invs []map[string]json.RawMessage
		if err := json.Unmarshal(obj["invocations"], &invs); err != nil || len(invs) != 1 {
			t.Fatalf("decode invocations: %v", err)
		}
		if got := sortedJSONKeys(invs[0]); !slices.Equal(got, invocationJSONKeys) {
			t.Errorf("invocation keys = %v, want %v", got, invocationJSONKeys)
		}
	})

	t.Run("add http", func(t *testing.T) {
		fake := newSampleFake()
		_, err := runTriggerCLI(t, fake, "", "add", "--type", "http", "--name", "deploy hook",
			"--repo-url", "https://github.com/acme/widget", "--prompt", "Investigate",
			"--skill", "boss-build", "--agent", "codex", "--model", "gpt", "--effort", "high", "--base-branch", "main",
			"--first-available", "--concurrency", "cancel-in-progress", "--cooldown", "5m",
			"--filter", "body.ref = refs/heads/main", "--filter", "header.X-Env in prod,staging",
			"--payload-field", "body.ref", "--methods", "POST,PUT", "--idempotency-header", "X-Key", "--dedup-window", "10m")
		if err != nil {
			t.Fatalf("add: %v", err)
		}
		req := fake.createReq
		model, effort := "gpt", "high"
		want := &pb.CreateTriggerRequest{
			OrganizationId: "org-1", Name: "deploy hook", IsEnabled: true, TriggerType: "http",
			TypeConfig: &pb.CreateTriggerRequest_Http{Http: &pb.HttpTriggerConfig{
				AllowedMethods: []string{"POST", "PUT"}, IdempotencyHeader: "X-Key", DedupWindowSeconds: 600,
			}},
			RepoOriginUrl: "https://github.com/acme/widget",
			Launch: &pb.TriggerLaunchSettings{
				PromptTemplate: "Investigate", SkillName: "boss-build", AgentName: "codex",
				Model: &model, Effort: &effort, BaseBranch: "main",
			},
			Placement:         &pb.TriggerPlacement{Mode: pb.TriggerPlacementMode_TRIGGER_PLACEMENT_MODE_FIRST_AVAILABLE},
			ConcurrencyPolicy: pb.TriggerConcurrencyPolicy_TRIGGER_CONCURRENCY_POLICY_CANCEL_IN_PROGRESS,
			CooldownSeconds:   300,
			Filters: []*pb.TriggerFilter{
				{Field: "body.ref", Operator: pb.TriggerFilterOperator_TRIGGER_FILTER_OPERATOR_EQUALS, Values: []string{"refs/heads/main"}},
				{Field: "header.X-Env", Operator: pb.TriggerFilterOperator_TRIGGER_FILTER_OPERATOR_IN, Values: []string{"prod", "staging"}},
			},
			PayloadFields: []string{"body.ref"},
		}
		if !proto.Equal(req, want) {
			t.Errorf("create request =\n%v\nwant\n%v", req, want)
		}
	})

	t.Run("add github", func(t *testing.T) {
		fake := newSampleFake()
		dir := t.TempDir()
		promptPath := filepath.Join(dir, "prompt.md")
		if err := os.WriteFile(promptPath, []byte("Review the PR"), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := runTriggerCLI(t, fake, "", "add", "--type", "github", "--name", "pr opened", "--org", "org-7",
			"--repo-url", "https://github.com/acme/widget", "--prompt-file", promptPath,
			"--daemon", "d-9", "--event", "pull_request.opened", "--event", "pull_request.reopened", "--disabled", "--json")
		if err != nil {
			t.Fatalf("add github: %v", err)
		}
		req := fake.createReq
		if req.GetOrganizationId() != "org-7" || req.GetTriggerType() != "github" || req.GetIsEnabled() ||
			req.GetLaunch().GetPromptTemplate() != "Review the PR" ||
			!slices.Equal(req.GetGithub().GetEventTypes(), []string{"pull_request.opened", "pull_request.reopened"}) ||
			req.GetPlacement().GetMode() != pb.TriggerPlacementMode_TRIGGER_PLACEMENT_MODE_SPECIFIC_DAEMON ||
			req.GetPlacement().GetDaemonId() != "d-9" || req.GetHttp() != nil {
			t.Errorf("github create request = %v", req)
		}
		if got := sortedJSONKeys(decodeJSONObject(t, out)); !slices.Equal(got, []string{"secret", "trigger"}) {
			t.Errorf("add --json keys = %v", got)
		}
	})

	t.Run("add validation sends nothing", func(t *testing.T) {
		cases := map[string][]string{
			"no placement":     {"add", "--type", "http", "--name", "n", "--repo-url", "u", "--prompt", "p"},
			"both placements":  {"add", "--type", "http", "--name", "n", "--repo-url", "u", "--prompt", "p", "--daemon", "d", "--first-available"},
			"github no event":  {"add", "--type", "github", "--name", "n", "--repo-url", "u", "--prompt", "p", "--first-available"},
			"http with event":  {"add", "--type", "http", "--name", "n", "--repo-url", "u", "--prompt", "p", "--first-available", "--event", "push"},
			"github w/ method": {"add", "--type", "github", "--name", "n", "--repo-url", "u", "--prompt", "p", "--first-available", "--event", "push", "--methods", "PUT"},
			"unknown type":     {"add", "--type", "smtp", "--name", "n", "--repo-url", "u", "--prompt", "p", "--first-available"},
			"bad concurrency":  {"add", "--type", "http", "--name", "n", "--repo-url", "u", "--prompt", "p", "--first-available", "--concurrency", "maybe"},
			"retired queue":    {"add", "--type", "http", "--name", "n", "--repo-url", "u", "--prompt", "p", "--first-available", "--concurrency", "queue"},
			"bad filter":       {"add", "--type", "http", "--name", "n", "--repo-url", "u", "--prompt", "p", "--first-available", "--filter", "body.ref ~ x"},
			"no prompt":        {"add", "--type", "http", "--name", "n", "--repo-url", "u", "--first-available"},
		}
		for name, args := range cases {
			fake := newSampleFake()
			if _, err := runTriggerCLI(t, fake, "", args...); err == nil {
				t.Errorf("%s: add succeeded, want a validation error", name)
			}
			if fake.createReq != nil {
				t.Errorf("%s: CreateTrigger was called", name)
			}
		}
	})

	t.Run("update sends only given flags", func(t *testing.T) {
		fake := newSampleFake()
		_, err := runTriggerCLI(t, fake, "", "update", "tr-1", "--name", "renamed", "--cooldown", "1m", "--clear-filters", "--concurrency", "allow")
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		name, cooldown := "renamed", int32(60)
		allow := pb.TriggerConcurrencyPolicy_TRIGGER_CONCURRENCY_POLICY_ALLOW_PARALLEL
		want := &pb.UpdateTriggerRequest{Id: "tr-1", Name: &name, CooldownSeconds: &cooldown, ConcurrencyPolicy: &allow, ShouldClearFilters: true}
		if !proto.Equal(fake.updateReq, want) {
			t.Errorf("update request =\n%v\nwant\n%v", fake.updateReq, want)
		}
		if fake.getReq != nil {
			t.Errorf("update read the trigger although no merged field changed")
		}
	})

	t.Run("update merges partial launch and http config", func(t *testing.T) {
		fake := newSampleFake()
		_, err := runTriggerCLI(t, fake, "", "update", "tr-1", "--prompt", "New prompt", "--dedup-window", "1m",
			"--payload-field", "body.sha", "--filter", "body.ref prefix refs/heads/release/")
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		req := fake.updateReq
		if fake.getReq.GetId() != "tr-1" {
			t.Errorf("update did not read the current trigger before merging")
		}
		// The prompt changes; every other launch setting is carried over.
		if req.GetLaunch().GetPromptTemplate() != "New prompt" || req.GetLaunch().GetSkillName() != "boss-build" || req.GetLaunch().GetModel() != "opus" {
			t.Errorf("launch = %v, want the new prompt over the stored settings", req.GetLaunch())
		}
		if req.GetHttp().GetDedupWindowSeconds() != 60 || req.GetHttp().GetIdempotencyHeader() != "X-Key" {
			t.Errorf("http = %v, want the new window over the stored config", req.GetHttp())
		}
		if !slices.Equal(req.GetPayloadFields(), []string{"body.sha"}) || req.GetShouldClearPayloadFields() {
			t.Errorf("payload fields = %v clear=%v", req.GetPayloadFields(), req.GetShouldClearPayloadFields())
		}
		if len(req.GetFilters()) != 1 || req.GetFilters()[0].GetOperator() != pb.TriggerFilterOperator_TRIGGER_FILTER_OPERATOR_PREFIX {
			t.Errorf("filters = %v", req.GetFilters())
		}
		if req.Name != nil || req.CooldownSeconds != nil || req.Placement != nil || req.ConcurrencyPolicy != nil {
			t.Errorf("update sent fields that were not given: %v", req)
		}
	})

	t.Run("update refusals", func(t *testing.T) {
		for name, args := range map[string][]string{
			"nothing":              {"update", "tr-1"},
			"filter and clear":     {"update", "tr-1", "--filter", "a = b", "--clear-filters"},
			"payload and clear":    {"update", "tr-1", "--payload-field", "a", "--clear-payload-fields"},
			"event on http":        {"update", "tr-1", "--event", "push"},
			"placement both flags": {"update", "tr-1", "--daemon", "d", "--first-available"},
		} {
			fake := newSampleFake()
			if _, err := runTriggerCLI(t, fake, "", args...); err == nil {
				t.Errorf("%s: update succeeded, want an error", name)
			}
			if fake.updateReq != nil {
				t.Errorf("%s: UpdateTrigger was called", name)
			}
		}
	})

	t.Run("enable and disable", func(t *testing.T) {
		fake := newSampleFake()
		if _, err := runTriggerCLI(t, fake, "", "disable", "tr-1"); err != nil {
			t.Fatalf("disable: %v", err)
		}
		if fake.updateReq.GetId() != "tr-1" || fake.updateReq.IsEnabled == nil || fake.updateReq.GetIsEnabled() {
			t.Errorf("disable request = %v", fake.updateReq)
		}
		out, err := runTriggerCLI(t, fake, "", "enable", "tr-1", "--json")
		if err != nil {
			t.Fatalf("enable: %v", err)
		}
		if !fake.updateReq.GetIsEnabled() || fake.updateReq.Name != nil {
			t.Errorf("enable request = %v", fake.updateReq)
		}
		if got := sortedJSONKeys(decodeJSONObject(t, out)); !slices.Equal(got, triggerJSONKeys) {
			t.Errorf("enable --json keys = %v", got)
		}
	})

	t.Run("rotate-secret", func(t *testing.T) {
		fake := newSampleFake()
		out, err := runTriggerCLI(t, fake, "", "rotate-secret", "tr-1", "--json")
		if err != nil {
			t.Fatalf("rotate-secret: %v", err)
		}
		if fake.rotateReq.GetId() != "tr-1" {
			t.Errorf("rotate request = %v", fake.rotateReq)
		}
		if got := sortedJSONKeys(decodeJSONObject(t, out)); !slices.Equal(got, []string{"secret", "trigger"}) {
			t.Errorf("rotate --json keys = %v", got)
		}
	})

	t.Run("test", func(t *testing.T) {
		fake := newSampleFake()
		payload := filepath.Join(t.TempDir(), "payload.json")
		if err := os.WriteFile(payload, []byte(`{"ref":"refs/heads/dev"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := runTriggerCLI(t, fake, "", "test", "tr-1", "--payload-file", payload, "--event", "push")
		if err != nil {
			t.Fatalf("test: %v", err)
		}
		want := &pb.TestTriggerRequest{TriggerId: "tr-1", SamplePayloadJson: `{"ref":"refs/heads/dev"}`, SampleEventType: "push"}
		if !proto.Equal(fake.testReq, want) {
			t.Errorf("test request = %v, want a dry run %v", fake.testReq, want)
		}
		if !strings.Contains(out, "filtered") || !strings.Contains(out, "filter_mismatch") {
			t.Errorf("test output = %q", out)
		}

		out, err = runTriggerCLI(t, fake, `{"a":1}`, "test", "tr-1", "--payload-file", "-", "--launch", "--json")
		if err != nil {
			t.Fatalf("test --launch: %v", err)
		}
		if fake.testReq.GetSamplePayloadJson() != `{"a":1}` || !fake.testReq.GetShouldLaunch() {
			t.Errorf("test --launch request = %v", fake.testReq)
		}
		if got := sortedJSONKeys(decodeJSONObject(t, out)); !slices.Equal(got, invocationJSONKeys) {
			t.Errorf("test --json keys = %v", got)
		}

		if _, err := runTriggerCLI(t, fake, "", "test", "tr-1"); err != nil {
			t.Fatalf("test without payload: %v", err)
		}
		if fake.testReq.GetSamplePayloadJson() != "{}" {
			t.Errorf("default payload = %q, want {}", fake.testReq.GetSamplePayloadJson())
		}
	})

	t.Run("history", func(t *testing.T) {
		fake := newSampleFake()
		out, err := runTriggerCLI(t, fake, "", "history", "tr-1", "--limit", "5", "--json")
		if err != nil {
			t.Fatalf("history: %v", err)
		}
		if fake.historyReq.GetTriggerId() != "tr-1" || fake.historyReq.GetLimit() != 5 {
			t.Errorf("history request = %v", fake.historyReq)
		}
		obj := decodeJSONObject(t, out)
		if got := sortedJSONKeys(obj); !slices.Equal(got, []string{"invocations"}) {
			t.Errorf("history keys = %v", got)
		}
	})

	t.Run("rm", func(t *testing.T) {
		fake := newSampleFake()
		out, err := runTriggerCLI(t, fake, "", "rm", "tr-1", "--yes", "--json")
		if err != nil {
			t.Fatalf("rm --yes: %v", err)
		}
		if fake.deleteReq.GetId() != "tr-1" {
			t.Errorf("delete request = %v", fake.deleteReq)
		}
		if strings.TrimSpace(out) != "{\n  \"deleted_trigger\": \"tr-1\"\n}" {
			t.Errorf("rm --json = %q", out)
		}

		// Without --yes and without a terminal the command refuses rather than
		// deleting unconfirmed.
		fake = newSampleFake()
		prev := triggerStdinIsTerminal
		triggerStdinIsTerminal = func() bool { return false }
		t.Cleanup(func() { triggerStdinIsTerminal = prev })
		if _, err := runTriggerCLI(t, fake, "", "rm", "tr-1"); err == nil || errorCodeFor(err) != codeConfirmationRequired {
			t.Errorf("rm without --yes off a TTY: err = %v, want CONFIRMATION_REQUIRED", err)
		}
		if fake.deleteReq != nil {
			t.Error("rm without confirmation deleted the trigger")
		}

		// On a terminal, anything but y cancels; y deletes.
		triggerStdinIsTerminal = func() bool { return true }
		if _, err := runTriggerCLI(t, fake, "n\n", "rm", "tr-1"); err != nil || fake.deleteReq != nil {
			t.Errorf("declined rm: err=%v deleted=%v", err, fake.deleteReq)
		}
		if _, err := runTriggerCLI(t, fake, "y\n", "rm", "tr-1"); err != nil || fake.deleteReq.GetId() != "tr-1" {
			t.Errorf("confirmed rm: err=%v deleted=%v", err, fake.deleteReq)
		}
	})

	t.Run("api error becomes a json envelope", func(t *testing.T) {
		fake := newSampleFake()
		fake.createErr = connect.NewError(connect.CodePermissionDenied, errors.New("cloud access required"))
		out, err := runTriggerCLI(t, fake, "", "add", "--type", "github", "--name", "n", "--repo-url", "u",
			"--prompt", "p", "--first-available", "--event", "push", "--json")
		if err == nil {
			t.Fatal("add succeeded against a refusing API")
		}
		obj := decodeJSONObject(t, out)
		if !strings.Contains(string(obj["error"]), "PERMISSION_DENIED") {
			t.Errorf("envelope = %s", out)
		}
	})
}

// TestParseTriggerFilter proves every operator spelling maps to its enum and a
// malformed token is rejected with a message naming it.
func TestParseTriggerFilter(t *testing.T) {
	op := func(o pb.TriggerFilterOperator) pb.TriggerFilterOperator { return o }
	cases := []struct {
		in     string
		field  string
		op     pb.TriggerFilterOperator
		values []string
	}{
		{"body.ref = refs/heads/main", "body.ref", op(pb.TriggerFilterOperator_TRIGGER_FILTER_OPERATOR_EQUALS), []string{"refs/heads/main"}},
		{"body.ref=refs/heads/main", "body.ref", op(pb.TriggerFilterOperator_TRIGGER_FILTER_OPERATOR_EQUALS), []string{"refs/heads/main"}},
		{"body.ref == main", "body.ref", op(pb.TriggerFilterOperator_TRIGGER_FILTER_OPERATOR_EQUALS), []string{"main"}},
		{"body.ref != main", "body.ref", op(pb.TriggerFilterOperator_TRIGGER_FILTER_OPERATOR_NOT_EQUALS), []string{"main"}},
		{"body.ref!=main", "body.ref", op(pb.TriggerFilterOperator_TRIGGER_FILTER_OPERATOR_NOT_EQUALS), []string{"main"}},
		{"event_type in pull_request.opened, pull_request.closed", "event_type", op(pb.TriggerFilterOperator_TRIGGER_FILTER_OPERATOR_IN), []string{"pull_request.opened", "pull_request.closed"}},
		{"header.X-Env not-in dev,test", "header.X-Env", op(pb.TriggerFilterOperator_TRIGGER_FILTER_OPERATOR_NOT_IN), []string{"dev", "test"}},
		{"header.X-Env NOT_IN dev", "header.X-Env", op(pb.TriggerFilterOperator_TRIGGER_FILTER_OPERATOR_NOT_IN), []string{"dev"}},
		{"body.title contains WIP, draft", "body.title", op(pb.TriggerFilterOperator_TRIGGER_FILTER_OPERATOR_CONTAINS), []string{"WIP, draft"}},
		{"body.ref prefix refs/tags/", "body.ref", op(pb.TriggerFilterOperator_TRIGGER_FILTER_OPERATOR_PREFIX), []string{"refs/tags/"}},
		{"  body.msg = hello world  ", "body.msg", op(pb.TriggerFilterOperator_TRIGGER_FILTER_OPERATOR_EQUALS), []string{"hello world"}},
	}
	for _, tc := range cases {
		got, err := parseTriggerFilter(tc.in)
		if err != nil {
			t.Errorf("parseTriggerFilter(%q): %v", tc.in, err)
			continue
		}
		if got.GetField() != tc.field || got.GetOperator() != tc.op || !slices.Equal(got.GetValues(), tc.values) {
			t.Errorf("parseTriggerFilter(%q) = %v, want field=%q op=%v values=%q", tc.in, got, tc.field, tc.op, tc.values)
		}
	}

	bad := []struct{ in, mustName string }{
		{"", "empty"},
		{"body.ref", "body.ref"},
		{"body.ref ~ x", `"~"`},
		{"body.ref matches x", `"matches"`},
		{"= x", "= x"},
		{"body.ref in", "body.ref in"},
		{"body.ref in ,,", "body.ref in ,,"},
		{"body.ref =", "body.ref ="},
	}
	for _, tc := range bad {
		_, err := parseTriggerFilter(tc.in)
		if err == nil {
			t.Errorf("parseTriggerFilter(%q) succeeded, want an error", tc.in)
			continue
		}
		if !strings.Contains(err.Error(), tc.mustName) {
			t.Errorf("parseTriggerFilter(%q) error %q does not name %q", tc.in, err.Error(), tc.mustName)
		}
	}

	// Every operator the enum defines round-trips through its display spelling.
	for value := range pb.TriggerFilterOperator_name {
		o := pb.TriggerFilterOperator(value)
		if o == pb.TriggerFilterOperator_TRIGGER_FILTER_OPERATOR_UNSPECIFIED {
			continue
		}
		f, err := parseTriggerFilter("f " + triggerOperatorSpelling(o) + " v")
		if err != nil || f.GetOperator() != o {
			t.Errorf("operator %v does not round-trip through %q: %v", o, triggerOperatorSpelling(o), err)
		}
	}
}

// TestTriggerRequiresLogin proves a caller with no cloud login gets an error
// naming `boss login` and that no request reaches the orchestrator.
func TestTriggerRequiresLogin(t *testing.T) {
	fake := newSampleFake()
	url := newFakeTriggerServer(t, fake)
	mgr := auth.NewManager(emptyTokenStore{}, auth.Config{})

	_, err := triggerRemote(context.Background(), mgr, url)
	if err == nil {
		t.Fatal("triggerRemote succeeded without a login")
	}
	if !strings.Contains(err.Error(), "run 'boss login' first") {
		t.Errorf("err = %q, want it to say run 'boss login' first", err.Error())
	}
	if n := fake.calls.Load(); n != 0 {
		t.Errorf("%d request(s) reached the orchestrator without a login", n)
	}

	// The same refusal reaches a --json caller as an envelope, and no RPC runs.
	prev := newTriggerClient
	newTriggerClient = func(cmd *cobra.Command) (triggerClient, error) { return triggerRemote(cmd.Context(), mgr, url) }
	t.Cleanup(func() { newTriggerClient = prev })
	cmd := triggerCmd()
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

// TestTriggerSecretPrintedOnce proves the signing secret appears only on HTTP
// create and on rotate, and never on show, ls, or their --json forms.
func TestTriggerSecretPrintedOnce(t *testing.T) {
	addHTTP := []string{"add", "--type", "http", "--name", "n", "--repo-url", "u", "--prompt", "p", "--first-available"}

	fake := newSampleFake()
	out, err := runTriggerCLI(t, fake, "", addHTTP...)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if !strings.Contains(out, testTriggerSecret) || !strings.Contains(out, "store it now; it will not be shown again") {
		t.Errorf("http add output = %q, want the secret once with the warning", out)
	}
	if strings.Count(out, testTriggerSecret) != 1 {
		t.Errorf("secret printed %d times, want once", strings.Count(out, testTriggerSecret))
	}

	out, err = runTriggerCLI(t, fake, "", append(slices.Clone(addHTTP), "--json")...)
	if err != nil {
		t.Fatalf("add --json: %v", err)
	}
	if strings.Count(out, testTriggerSecret) != 1 {
		t.Errorf("http add --json = %q, want the secret exactly once", out)
	}

	out, err = runTriggerCLI(t, fake, "", "add", "--type", "github", "--name", "n", "--repo-url", "u", "--prompt", "p", "--first-available", "--event", "push")
	if err != nil {
		t.Fatalf("github add: %v", err)
	}
	if strings.Contains(out, "store it now") {
		t.Errorf("github add output mentions a secret: %q", out)
	}

	out, err = runTriggerCLI(t, fake, "", "rotate-secret", "tr-1")
	if err != nil {
		t.Fatalf("rotate-secret: %v", err)
	}
	if strings.Count(out, testRotatedSecret) != 1 || !strings.Contains(out, "store it now; it will not be shown again") {
		t.Errorf("rotate output = %q, want the new secret once with the warning", out)
	}

	out, err = runTriggerCLI(t, fake, "", "rotate-secret", "tr-1", "--json")
	if err != nil {
		t.Fatalf("rotate-secret --json: %v", err)
	}
	if strings.Count(out, testRotatedSecret) != 1 {
		t.Errorf("rotate --json = %q, want the new secret exactly once", out)
	}

	for _, args := range [][]string{{"show", "tr-1"}, {"show", "tr-1", "--json"}, {"ls"}, {"ls", "--json"}, {"enable", "tr-1", "--json"}, {"history", "tr-1", "--json"}} {
		out, err := runTriggerCLI(t, fake, "", args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if strings.Contains(out, testTriggerSecret) || strings.Contains(out, testRotatedSecret) || strings.Contains(strings.ToLower(out), "secret") {
			t.Errorf("%v output carries a secret: %q", args, out)
		}
	}
}

// TestTriggerConcurrencyNames: every accepted --concurrency spelling parses to
// its policy, and the policy prints back as the short name.
func TestTriggerConcurrencyNames(t *testing.T) {
	cases := []struct {
		inputs []string
		policy pb.TriggerConcurrencyPolicy
		name   string
	}{
		{[]string{"skip", "skip-if-running", "SKIP"}, pb.TriggerConcurrencyPolicy_TRIGGER_CONCURRENCY_POLICY_SKIP_IF_RUNNING, "skip"},
		{[]string{"cancel", "cancel-in-progress"}, pb.TriggerConcurrencyPolicy_TRIGGER_CONCURRENCY_POLICY_CANCEL_IN_PROGRESS, "cancel"},
		{[]string{"allow", "allow-parallel"}, pb.TriggerConcurrencyPolicy_TRIGGER_CONCURRENCY_POLICY_ALLOW_PARALLEL, "allow"},
	}
	for _, c := range cases {
		for _, in := range c.inputs {
			got, err := parseTriggerConcurrency(in)
			if err != nil || got != c.policy {
				t.Errorf("parse %q = %v, %v; want %v", in, got, err, c.policy)
			}
		}
		if got := triggerConcurrencyName(c.policy); got != c.name {
			t.Errorf("name of %v = %q, want %q", c.policy, got, c.name)
		}
	}
	for _, retired := range []string{"queue", "queue-latest"} {
		if _, err := parseTriggerConcurrency(retired); err == nil {
			t.Errorf("parse %q succeeded, want the retired policy refused", retired)
		}
	}
	if got := triggerConcurrencyName(pb.TriggerConcurrencyPolicy_TRIGGER_CONCURRENCY_POLICY_UNSPECIFIED); got != "skip" {
		t.Errorf("name of UNSPECIFIED = %q, want skip", got)
	}
}
