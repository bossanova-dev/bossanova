package bossmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// Inbound-trigger tools (BOS-1422): the hosted-only tool tier. Triggers are
// bosso-owned, so only a backend that reaches the API as the caller — the
// hosted gateway — implements TriggerBackend, and these tools register only
// under Options{IncludeHostedTools}.
//
// This file is deliberately NOT in scripts/mcp-tool-registry.mjs
// TOOL_SOURCE_FILES. That registry is the LOCAL full-catalog inventory the docs
// count gates check (`bin/mcp` exposes N tools); the local server never lists
// these tools, so counting them there would make every local count claim false.
// HostedToolNames() is their inventory instead, and TestHostedTriggerTools pins
// it against what actually registers.
//
// Results are rendered with protojson (proto field names, enum value names),
// not encoding/json over the generated structs, because the inputs take enum
// value names: an agent reads back exactly the spelling it writes, and the
// type_config oneof renders as its "http"/"github" key.

// triggerRecentInvocationLimit is how many invocations get_trigger includes.
const triggerRecentInvocationLimit = 20

// hostedTriggerBackend returns the backend's TriggerBackend when the hosted
// tier is enabled and the backend implements it, else nil.
func hostedTriggerBackend(backend Backend, opts Options) TriggerBackend {
	if !opts.IncludeHostedTools {
		return nil
	}
	triggers, ok := backend.(TriggerBackend)
	if !ok {
		return nil
	}
	return triggers
}

// parseTriggerEnum maps an enum argument to its proto value. It accepts the
// proto value name in any case, with or without its type prefix
// ("CANCEL_IN_PROGRESS" or "TRIGGER_CONCURRENCY_POLICY_CANCEL_IN_PROGRESS"). An unknown
// name, or the UNSPECIFIED zero value, is a tool error naming the valid values
// rather than a silent 0, which the server would read as a different choice.
func parseTriggerEnum(arg, value, prefix string, values map[string]int32) (int32, error) {
	name := strings.ToUpper(strings.TrimSpace(value))
	if !strings.HasPrefix(name, prefix) {
		name = prefix + name
	}
	if v, ok := values[name]; ok && v != 0 {
		return v, nil
	}
	type named struct {
		name string
		v    int32
	}
	var valid []named
	for n, v := range values {
		if v != 0 {
			valid = append(valid, named{strings.TrimPrefix(n, prefix), v})
		}
	}
	sort.Slice(valid, func(i, j int) bool { return valid[i].v < valid[j].v })
	names := make([]string, len(valid))
	for i, n := range valid {
		names[i] = n.name
	}
	return 0, fmt.Errorf("%s %q is not one of %s", arg, value, strings.Join(names, ", "))
}

// ListTriggersArgs is the typed argument struct for list_triggers.
type ListTriggersArgs struct {
	OrganizationID string `json:"organization_id,omitempty" jsonschema:"only this organization; omit for every organization you belong to"`
}

// TriggerIDArgs is the typed argument struct for get_trigger.
type TriggerIDArgs struct {
	ID string `json:"id" jsonschema:"the trigger id"`
}

// DeleteTriggerArgs is the typed argument struct for delete_trigger.
type DeleteTriggerArgs struct {
	ID      string `json:"id" jsonschema:"the trigger id"`
	Confirm bool   `json:"confirm,omitempty" jsonschema:"must be true to actually delete the trigger"`
}

// TestTriggerArgs is the typed argument struct for test_trigger.
type TestTriggerArgs struct {
	TriggerID         string `json:"trigger_id" jsonschema:"the trigger id"`
	SamplePayloadJSON string `json:"sample_payload_json" jsonschema:"sample payload as a JSON object (HTTP body or GitHub event payload)"`
	SampleEventType   string `json:"sample_event_type,omitempty" jsonschema:"catalog event id; required for github triggers"`
	ShouldLaunch      bool   `json:"should_launch,omitempty" jsonschema:"launch a real session if the sample passes (default false: record the decision only)"`
}

// TriggerHTTPConfigArgs mirrors pb.HttpTriggerConfig.
type TriggerHTTPConfigArgs struct {
	AllowedMethods     []string `json:"allowed_methods,omitempty" jsonschema:"accepted methods; empty means POST only"`
	IdempotencyHeader  string   `json:"idempotency_header,omitempty" jsonschema:"empty means Idempotency-Key"`
	DedupWindowSeconds int32    `json:"dedup_window_seconds,omitempty" jsonschema:"0 means 300"`
}

// TriggerGithubConfigArgs mirrors pb.GithubTriggerConfig.
type TriggerGithubConfigArgs struct {
	EventTypes []string `json:"event_types" jsonschema:"catalog event ids, e.g. pull_request.opened"`
}

// TriggerLaunchArgs mirrors pb.TriggerLaunchSettings.
type TriggerLaunchArgs struct {
	PromptTemplate string `json:"prompt_template" jsonschema:"prompt sent to the agent; may reference payload_fields"`
	SkillName      string `json:"skill_name,omitempty" jsonschema:"skill to invoke, without the leading /"`
	AgentName      string `json:"agent_name,omitempty" jsonschema:"agent runner; empty means claude"`
	Model          string `json:"model,omitempty"`
	Effort         string `json:"effort,omitempty"`
	BaseBranch     string `json:"base_branch,omitempty" jsonschema:"empty means the repo default"`
}

// TriggerPlacementArgs mirrors pb.TriggerPlacement.
type TriggerPlacementArgs struct {
	Mode     string `json:"mode" jsonschema:"SPECIFIC_DAEMON or FIRST_AVAILABLE"`
	DaemonID string `json:"daemon_id,omitempty" jsonschema:"required for SPECIFIC_DAEMON"`
}

// TriggerFilterArgs mirrors pb.TriggerFilter.
type TriggerFilterArgs struct {
	Field    string   `json:"field" jsonschema:"catalog filter field, e.g. body.ref"`
	Operator string   `json:"operator" jsonschema:"EQUALS, NOT_EQUALS, IN, NOT_IN, CONTAINS or PREFIX"`
	Values   []string `json:"values"`
}

// SaveTriggerArgs is the typed argument struct for save_trigger. Without id it
// creates; with id it updates, sending only the fields present (pointers and
// non-empty lists), so an omitted field is left unchanged; with id and
// rotate_secret alone it rotates the HTTP signing secret.
type SaveTriggerArgs struct {
	ID                       string                   `json:"id,omitempty" jsonschema:"trigger id; omit to create"`
	RotateSecret             bool                     `json:"rotate_secret,omitempty" jsonschema:"rotate the HTTP signing secret"`
	OrganizationID           string                   `json:"organization_id,omitempty" jsonschema:"create only (required there)"`
	TriggerType              string                   `json:"trigger_type,omitempty" jsonschema:"create only: catalog type, e.g. http or github"`
	Name                     *string                  `json:"name,omitempty"`
	IsEnabled                *bool                    `json:"is_enabled,omitempty" jsonschema:"create defaults to true"`
	HTTP                     *TriggerHTTPConfigArgs   `json:"http,omitempty" jsonschema:"config for an http trigger"`
	Github                   *TriggerGithubConfigArgs `json:"github,omitempty" jsonschema:"config for a github trigger"`
	RepoOriginURL            *string                  `json:"repo_origin_url,omitempty" jsonschema:"origin URL of the session's repo"`
	Launch                   *TriggerLaunchArgs       `json:"launch,omitempty" jsonschema:"what to launch; replaces the whole block on update"`
	Placement                *TriggerPlacementArgs    `json:"placement,omitempty" jsonschema:"which daemon launches; replaces the whole block on update"`
	ConcurrencyPolicy        *string                  `json:"concurrency_policy,omitempty" jsonschema:"SKIP_IF_RUNNING, CANCEL_IN_PROGRESS or ALLOW_PARALLEL"`
	CooldownSeconds          *int32                   `json:"cooldown_seconds,omitempty" jsonschema:"minimum gap between launches; 0 disables"`
	Filters                  []TriggerFilterArgs      `json:"filters,omitempty" jsonschema:"all must match; non-empty replaces"`
	ShouldClearFilters       bool                     `json:"should_clear_filters,omitempty" jsonschema:"update only: remove every filter"`
	PayloadFields            []string                 `json:"payload_fields,omitempty" jsonschema:"payload paths copied into the prompt context; non-empty replaces"`
	ShouldClearPayloadFields bool                     `json:"should_clear_payload_fields,omitempty" jsonschema:"update only: remove every payload field"`
}

// hasUpdateFields reports whether any field other than id and rotate_secret is
// set, which is what makes rotate_secret ambiguous.
func (a SaveTriggerArgs) hasUpdateFields() bool {
	return a.OrganizationID != "" || a.TriggerType != "" || a.Name != nil || a.IsEnabled != nil ||
		a.HTTP != nil || a.Github != nil || a.RepoOriginURL != nil || a.Launch != nil ||
		a.Placement != nil || a.ConcurrencyPolicy != nil || a.CooldownSeconds != nil ||
		len(a.Filters) > 0 || a.ShouldClearFilters || len(a.PayloadFields) > 0 || a.ShouldClearPayloadFields
}

func (a SaveTriggerArgs) httpConfig() *pb.HttpTriggerConfig {
	return &pb.HttpTriggerConfig{
		AllowedMethods:     a.HTTP.AllowedMethods,
		IdempotencyHeader:  a.HTTP.IdempotencyHeader,
		DedupWindowSeconds: a.HTTP.DedupWindowSeconds,
	}
}

func (a SaveTriggerArgs) githubConfig() *pb.GithubTriggerConfig {
	return &pb.GithubTriggerConfig{EventTypes: a.Github.EventTypes}
}

func (a SaveTriggerArgs) launch() *pb.TriggerLaunchSettings {
	if a.Launch == nil {
		return nil
	}
	return &pb.TriggerLaunchSettings{
		PromptTemplate: a.Launch.PromptTemplate,
		SkillName:      a.Launch.SkillName,
		AgentName:      a.Launch.AgentName,
		Model:          optionalString(a.Launch.Model),
		Effort:         optionalString(a.Launch.Effort),
		BaseBranch:     a.Launch.BaseBranch,
	}
}

func (a SaveTriggerArgs) placement() (*pb.TriggerPlacement, error) {
	if a.Placement == nil {
		return nil, nil
	}
	mode, err := parseTriggerEnum("placement.mode", a.Placement.Mode, "TRIGGER_PLACEMENT_MODE_", pb.TriggerPlacementMode_value)
	if err != nil {
		return nil, err
	}
	return &pb.TriggerPlacement{Mode: pb.TriggerPlacementMode(mode), DaemonId: a.Placement.DaemonID}, nil
}

// triggerConcurrencyPolicies are the policies save_trigger accepts: every
// value but the retired QUEUE_LATEST.
var triggerConcurrencyPolicies = map[string]int32{
	pb.TriggerConcurrencyPolicy_TRIGGER_CONCURRENCY_POLICY_SKIP_IF_RUNNING.String():    int32(pb.TriggerConcurrencyPolicy_TRIGGER_CONCURRENCY_POLICY_SKIP_IF_RUNNING),
	pb.TriggerConcurrencyPolicy_TRIGGER_CONCURRENCY_POLICY_CANCEL_IN_PROGRESS.String(): int32(pb.TriggerConcurrencyPolicy_TRIGGER_CONCURRENCY_POLICY_CANCEL_IN_PROGRESS),
	pb.TriggerConcurrencyPolicy_TRIGGER_CONCURRENCY_POLICY_ALLOW_PARALLEL.String():     int32(pb.TriggerConcurrencyPolicy_TRIGGER_CONCURRENCY_POLICY_ALLOW_PARALLEL),
}

func (a SaveTriggerArgs) concurrencyPolicy() (*pb.TriggerConcurrencyPolicy, error) {
	if a.ConcurrencyPolicy == nil {
		return nil, nil
	}
	v, err := parseTriggerEnum("concurrency_policy", *a.ConcurrencyPolicy, "TRIGGER_CONCURRENCY_POLICY_", triggerConcurrencyPolicies)
	if err != nil {
		return nil, err
	}
	policy := pb.TriggerConcurrencyPolicy(v)
	return &policy, nil
}

func (a SaveTriggerArgs) filters() ([]*pb.TriggerFilter, error) {
	if len(a.Filters) == 0 {
		return nil, nil
	}
	out := make([]*pb.TriggerFilter, len(a.Filters))
	for i, f := range a.Filters {
		op, err := parseTriggerEnum(fmt.Sprintf("filters[%d].operator", i), f.Operator, "TRIGGER_FILTER_OPERATOR_", pb.TriggerFilterOperator_value)
		if err != nil {
			return nil, err
		}
		out[i] = &pb.TriggerFilter{Field: f.Field, Operator: pb.TriggerFilterOperator(op), Values: f.Values}
	}
	return out, nil
}

// createRequest builds CreateTriggerRequest. Absent optional fields take the
// proto zero value, except is_enabled, which defaults to true: a trigger
// created to be used should run without a second call.
func (a SaveTriggerArgs) createRequest() (*pb.CreateTriggerRequest, error) {
	if a.ShouldClearFilters || a.ShouldClearPayloadFields {
		return nil, errors.New("should_clear_filters and should_clear_payload_fields apply only to an update (pass id)")
	}
	placement, err := a.placement()
	if err != nil {
		return nil, err
	}
	policy, err := a.concurrencyPolicy()
	if err != nil {
		return nil, err
	}
	filters, err := a.filters()
	if err != nil {
		return nil, err
	}
	req := &pb.CreateTriggerRequest{
		OrganizationId: a.OrganizationID,
		IsEnabled:      a.IsEnabled == nil || *a.IsEnabled,
		TriggerType:    a.TriggerType,
		Launch:         a.launch(),
		Placement:      placement,
		Filters:        filters,
		PayloadFields:  a.PayloadFields,
	}
	if a.Name != nil {
		req.Name = *a.Name
	}
	if a.RepoOriginURL != nil {
		req.RepoOriginUrl = *a.RepoOriginURL
	}
	if policy != nil {
		req.ConcurrencyPolicy = *policy
	}
	if a.CooldownSeconds != nil {
		req.CooldownSeconds = *a.CooldownSeconds
	}
	switch {
	case a.HTTP != nil && a.Github != nil:
		return nil, errors.New("pass at most one of http and github")
	case a.HTTP != nil:
		req.TypeConfig = &pb.CreateTriggerRequest_Http{Http: a.httpConfig()}
	case a.Github != nil:
		req.TypeConfig = &pb.CreateTriggerRequest_Github{Github: a.githubConfig()}
	}
	return req, nil
}

// updateRequest builds UpdateTriggerRequest carrying only the fields present.
func (a SaveTriggerArgs) updateRequest() (*pb.UpdateTriggerRequest, error) {
	if a.OrganizationID != "" || a.TriggerType != "" {
		return nil, errors.New("organization_id and trigger_type are set at create and cannot change; omit them when passing id")
	}
	placement, err := a.placement()
	if err != nil {
		return nil, err
	}
	policy, err := a.concurrencyPolicy()
	if err != nil {
		return nil, err
	}
	filters, err := a.filters()
	if err != nil {
		return nil, err
	}
	req := &pb.UpdateTriggerRequest{
		Id:                       a.ID,
		Name:                     a.Name,
		IsEnabled:                a.IsEnabled,
		RepoOriginUrl:            a.RepoOriginURL,
		Launch:                   a.launch(),
		Placement:                placement,
		ConcurrencyPolicy:        policy,
		CooldownSeconds:          a.CooldownSeconds,
		Filters:                  filters,
		ShouldClearFilters:       a.ShouldClearFilters,
		PayloadFields:            a.PayloadFields,
		ShouldClearPayloadFields: a.ShouldClearPayloadFields,
	}
	switch {
	case a.HTTP != nil && a.Github != nil:
		return nil, errors.New("pass at most one of http and github")
	case a.HTTP != nil:
		req.TypeConfig = &pb.UpdateTriggerRequest_Http{Http: a.httpConfig()}
	case a.Github != nil:
		req.TypeConfig = &pb.UpdateTriggerRequest_Github{Github: a.githubConfig()}
	}
	return req, nil
}

// savedTrigger is the save_trigger result. Secret and SecretNote appear only
// on create and rotate, and only when the server issued a secret.
type savedTrigger struct {
	Trigger    json.RawMessage `json:"trigger"`
	Secret     string          `json:"secret,omitempty"`
	SecretNote string          `json:"secret_note,omitempty"`
}

func savedTriggerResult(trigger *pb.Trigger, secret string) (*mcp.CallToolResult, any, error) {
	b, err := hostedMessageJSON(trigger)
	out := savedTrigger{Trigger: b}
	if secret != "" {
		out.Secret = secret
		out.SecretNote = hostedSecretNote
	}
	return hostedResult(out, err)
}

func registerTriggerReadTools(server *mcp.Server, backend TriggerBackend, opts Options) {
	addTool(server, opts, &mcp.Tool{
		Name:        "get_trigger_catalog",
		Description: "List the trigger types, event types and filter fields save_trigger accepts.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ NoArgs) (*mcp.CallToolResult, any, error) {
		catalog, err := backend.GetTriggerCatalog(ctx)
		if err != nil {
			return errorResult(err), nil, nil
		}
		return hostedResult(hostedMessageJSON(catalog))
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "list_triggers",
		Description: "List the inbound triggers you created; secrets are never returned.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args ListTriggersArgs) (*mcp.CallToolResult, any, error) {
		resp, err := backend.ListTriggers(ctx, &pb.ListTriggersRequest{OrganizationId: optionalString(args.OrganizationID)})
		if err != nil {
			return errorResult(err), nil, nil
		}
		triggers, err := hostedMessagesJSON(resp.GetTriggers())
		return hostedResult(map[string][]json.RawMessage{"triggers": triggers}, err)
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "get_trigger",
		Description: "Get a trigger and its 20 most recent invocations, newest first: each says why it did or did not launch.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args TriggerIDArgs) (*mcp.CallToolResult, any, error) {
		trigger, err := backend.GetTrigger(ctx, args.ID)
		if err != nil {
			return errorResult(err), nil, nil
		}
		invocations, err := backend.ListTriggerInvocations(ctx, &pb.ListTriggerInvocationsRequest{
			TriggerId: args.ID,
			Limit:     triggerRecentInvocationLimit,
		})
		if err != nil {
			return errorResult(err), nil, nil
		}
		triggerOut, err := hostedMessageJSON(trigger)
		if err != nil {
			return nil, nil, err
		}
		recent, err := hostedMessagesJSON(invocations.GetInvocations())
		return hostedResult(struct {
			Trigger           json.RawMessage   `json:"trigger"`
			RecentInvocations []json.RawMessage `json:"recent_invocations"`
		}{triggerOut, recent}, err)
	})
}

func registerTriggerWriteTools(server *mcp.Server, backend TriggerBackend, opts Options) {
	addTool(server, opts, &mcp.Tool{
		Name: "save_trigger",
		Description: "Create an inbound trigger (no id), update one (id; only given fields change), or rotate its HTTP " +
			"secret (id + rotate_secret alone). Create and rotate return the one-time secret. Enums take proto value names.",
		Annotations: &mcp.ToolAnnotations{},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args SaveTriggerArgs) (*mcp.CallToolResult, any, error) {
		switch {
		case args.RotateSecret && args.ID == "":
			return errorResult(errors.New("rotate_secret needs the id of the trigger to rotate")), nil, nil
		case args.RotateSecret && args.hasUpdateFields():
			return errorResult(errors.New("rotate_secret cannot be combined with other changes: rotate and update in two calls")), nil, nil
		case args.RotateSecret:
			resp, err := backend.RotateTriggerSecret(ctx, args.ID)
			if err != nil {
				return errorResult(err), nil, nil
			}
			return savedTriggerResult(resp.GetTrigger(), resp.GetSecret())
		case args.ID == "":
			req, err := args.createRequest()
			if err != nil {
				return errorResult(err), nil, nil
			}
			resp, err := backend.CreateTrigger(ctx, req)
			if err != nil {
				return errorResult(err), nil, nil
			}
			return savedTriggerResult(resp.GetTrigger(), resp.GetSecret())
		default:
			req, err := args.updateRequest()
			if err != nil {
				return errorResult(err), nil, nil
			}
			trigger, err := backend.UpdateTrigger(ctx, req)
			if err != nil {
				return errorResult(err), nil, nil
			}
			return savedTriggerResult(trigger, "")
		}
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "test_trigger",
		Description: "Run a sample payload through a trigger's filters and policies and record the invocation.",
		Annotations: &mcp.ToolAnnotations{},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args TestTriggerArgs) (*mcp.CallToolResult, any, error) {
		invocation, err := backend.TestTrigger(ctx, &pb.TestTriggerRequest{
			TriggerId:         args.TriggerID,
			SamplePayloadJson: args.SamplePayloadJSON,
			SampleEventType:   args.SampleEventType,
			ShouldLaunch:      args.ShouldLaunch,
		})
		if err != nil {
			return errorResult(err), nil, nil
		}
		b, err := hostedMessageJSON(invocation)
		return hostedResult(map[string]json.RawMessage{"invocation": b}, err)
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "delete_trigger",
		Description: "Permanently delete a trigger with its secret and invocation history. Destructive — requires confirm:true.",
		Annotations: destructiveAnnotations(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args DeleteTriggerArgs) (*mcp.CallToolResult, any, error) {
		if r := requireConfirm(args.Confirm, "delete_trigger"); r != nil {
			return r, nil, nil
		}
		if err := backend.DeleteTrigger(ctx, args.ID); err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(map[string]string{"deleted_trigger": args.ID})
		return r, nil, err
	})
}
