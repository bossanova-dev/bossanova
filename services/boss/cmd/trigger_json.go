package main

import (
	"strings"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// --- JSON schemas ---------------------------------------------------------
//
// These are the stable `boss trigger … --json` contracts. Field names are part
// of the machine contract; renames are breaking. Timestamps are RFC3339 UTC,
// empty when unset. No schema carries a secret except the create/rotate
// envelopes, which carry it in their own top-level "secret" key.

type triggerHTTPJSON struct {
	AllowedMethods     []string `json:"allowed_methods"`
	IdempotencyHeader  string   `json:"idempotency_header"`
	DedupWindowSeconds int32    `json:"dedup_window_seconds"`
}

type triggerGithubJSON struct {
	EventTypes []string `json:"event_types"`
}

type triggerLaunchJSON struct {
	PromptTemplate string `json:"prompt_template"`
	SkillName      string `json:"skill_name"`
	AgentName      string `json:"agent_name"`
	Model          string `json:"model"`
	Effort         string `json:"effort"`
	BaseBranch     string `json:"base_branch"`
}

type triggerPlacementJSON struct {
	Mode     string `json:"mode"`
	DaemonID string `json:"daemon_id"`
}

type triggerFilterJSON struct {
	Field    string   `json:"field"`
	Operator string   `json:"operator"`
	Values   []string `json:"values"`
}

type triggerLastInvocationJSON struct {
	Status         string `json:"status"`
	DecisionReason string `json:"decision_reason"`
	ReceivedAt     string `json:"received_at"`
	SessionID      string `json:"session_id"`
}

type triggerJSON struct {
	ID                string                     `json:"id"`
	OrganizationID    string                     `json:"organization_id"`
	CreatorUserID     string                     `json:"creator_user_id"`
	Name              string                     `json:"name"`
	Enabled           bool                       `json:"enabled"`
	Type              string                     `json:"type"`
	ConfigVersion     int32                      `json:"config_version"`
	HTTP              *triggerHTTPJSON           `json:"http"`
	Github            *triggerGithubJSON         `json:"github"`
	RepoOriginURL     string                     `json:"repo_origin_url"`
	Launch            triggerLaunchJSON          `json:"launch"`
	Placement         triggerPlacementJSON       `json:"placement"`
	ConcurrencyPolicy string                     `json:"concurrency_policy"`
	CooldownSeconds   int32                      `json:"cooldown_seconds"`
	Filters           []triggerFilterJSON        `json:"filters"`
	PayloadFields     []string                   `json:"payload_fields"`
	EndpointPath      string                     `json:"endpoint_path"`
	EndpointURL       string                     `json:"endpoint_url"`
	LastInvocation    *triggerLastInvocationJSON `json:"last_invocation"`
	CreatedAt         string                     `json:"created_at"`
	UpdatedAt         string                     `json:"updated_at"`
}

type triggerInvocationJSON struct {
	ID             string `json:"id"`
	TriggerID      string `json:"trigger_id"`
	Source         string `json:"source"`
	EventType      string `json:"event_type"`
	Status         string `json:"status"`
	DecisionReason string `json:"decision_reason"`
	ErrorDetail    string `json:"error_detail"`
	PayloadExcerpt string `json:"payload_excerpt"`
	DaemonID       string `json:"daemon_id"`
	SessionID      string `json:"session_id"`
	AttemptCount   int32  `json:"attempt_count"`
	ReceivedAt     string `json:"received_at"`
	DecidedAt      string `json:"decided_at"`
	LaunchedAt     string `json:"launched_at"`
}

// triggerSecretJSON is the create / rotate-secret envelope. secret is "" for a
// trigger type with no secret (GitHub).
type triggerSecretJSON struct {
	Trigger triggerJSON `json:"trigger"`
	Secret  string      `json:"secret"`
}

type triggerShowJSON struct {
	Trigger     triggerJSON             `json:"trigger"`
	Invocations []triggerInvocationJSON `json:"invocations"`
}

type triggerCatalogJSON struct {
	Types []triggerTypeJSON `json:"types"`
}

type triggerTypeJSON struct {
	Name          string                   `json:"name"`
	DisplayName   string                   `json:"display_name"`
	ConfigVersion int32                    `json:"config_version"`
	EventTypes    []triggerEventTypeJSON   `json:"event_types"`
	FilterFields  []triggerFilterFieldJSON `json:"filter_fields"`
}

type triggerEventTypeJSON struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
}

type triggerFilterFieldJSON struct {
	Field       string   `json:"field"`
	Description string   `json:"description"`
	Operators   []string `json:"operators"`
	IsPrefix    bool     `json:"is_prefix"`
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// triggerEndpointURL joins the cloud base URL and an HTTP trigger's endpoint
// path; "" when the trigger has no endpoint.
func triggerEndpointURL(base, path string) string {
	if path == "" {
		return ""
	}
	return strings.TrimRight(base, "/") + path
}

// triggerToJSON maps a proto Trigger field by field, so a new proto field (and
// above all a secret) cannot leak into the contract by default.
func triggerToJSON(t *pb.Trigger, baseURL string) triggerJSON {
	out := triggerJSON{
		ID:             t.GetId(),
		OrganizationID: t.GetOrganizationId(),
		CreatorUserID:  t.GetCreatorUserId(),
		Name:           t.GetName(),
		Enabled:        t.GetIsEnabled(),
		Type:           t.GetTriggerType(),
		ConfigVersion:  t.GetConfigVersion(),
		RepoOriginURL:  t.GetRepoOriginUrl(),
		Launch: triggerLaunchJSON{
			PromptTemplate: t.GetLaunch().GetPromptTemplate(),
			SkillName:      t.GetLaunch().GetSkillName(),
			AgentName:      t.GetLaunch().GetAgentName(),
			Model:          t.GetLaunch().GetModel(),
			Effort:         t.GetLaunch().GetEffort(),
			BaseBranch:     t.GetLaunch().GetBaseBranch(),
		},
		Placement: triggerPlacementJSON{
			Mode:     triggerPlacementMode(t.GetPlacement().GetMode()),
			DaemonID: t.GetPlacement().GetDaemonId(),
		},
		ConcurrencyPolicy: triggerConcurrencyName(t.GetConcurrencyPolicy()),
		CooldownSeconds:   t.GetCooldownSeconds(),
		Filters:           make([]triggerFilterJSON, 0, len(t.GetFilters())),
		PayloadFields:     nonNilStrings(t.GetPayloadFields()),
		EndpointPath:      t.GetEndpointPath(),
		EndpointURL:       triggerEndpointURL(baseURL, t.GetEndpointPath()),
		CreatedAt:         rfc3339OrEmpty(t.GetCreatedAt()),
		UpdatedAt:         rfc3339OrEmpty(t.GetUpdatedAt()),
	}
	if h := t.GetHttp(); h != nil {
		out.HTTP = &triggerHTTPJSON{
			AllowedMethods:     nonNilStrings(h.GetAllowedMethods()),
			IdempotencyHeader:  h.GetIdempotencyHeader(),
			DedupWindowSeconds: h.GetDedupWindowSeconds(),
		}
	}
	if g := t.GetGithub(); g != nil {
		out.Github = &triggerGithubJSON{EventTypes: nonNilStrings(g.GetEventTypes())}
	}
	for _, f := range t.GetFilters() {
		out.Filters = append(out.Filters, triggerFilterJSON{
			Field: f.GetField(), Operator: triggerOperatorSpelling(f.GetOperator()), Values: nonNilStrings(f.GetValues()),
		})
	}
	if last := t.GetLastInvocation(); last != nil {
		out.LastInvocation = &triggerLastInvocationJSON{
			Status:         triggerInvocationStatus(last.GetStatus()),
			DecisionReason: last.GetDecisionReason(),
			ReceivedAt:     rfc3339OrEmpty(last.GetReceivedAt()),
			SessionID:      last.GetSessionId(),
		}
	}
	return out
}

func triggerInvocationToJSON(inv *pb.TriggerInvocation) triggerInvocationJSON {
	return triggerInvocationJSON{
		ID:             inv.GetId(),
		TriggerID:      inv.GetTriggerId(),
		Source:         inv.GetSource(),
		EventType:      inv.GetEventType(),
		Status:         triggerInvocationStatus(inv.GetStatus()),
		DecisionReason: inv.GetDecisionReason(),
		ErrorDetail:    inv.GetErrorDetail(),
		PayloadExcerpt: inv.GetPayloadExcerpt(),
		DaemonID:       inv.GetDaemonId(),
		SessionID:      inv.GetSessionId(),
		AttemptCount:   inv.GetAttemptCount(),
		ReceivedAt:     rfc3339OrEmpty(inv.GetReceivedAt()),
		DecidedAt:      rfc3339OrEmpty(inv.GetDecidedAt()),
		LaunchedAt:     rfc3339OrEmpty(inv.GetLaunchedAt()),
	}
}

func triggerInvocationsToJSON(invs []*pb.TriggerInvocation) []triggerInvocationJSON {
	out := make([]triggerInvocationJSON, len(invs))
	for i, inv := range invs {
		out[i] = triggerInvocationToJSON(inv)
	}
	return out
}

func triggerCatalogToJSON(c *pb.TriggerCatalog) triggerCatalogJSON {
	out := triggerCatalogJSON{Types: make([]triggerTypeJSON, 0, len(c.GetTypes()))}
	for _, t := range c.GetTypes() {
		tj := triggerTypeJSON{
			Name: t.GetName(), DisplayName: t.GetDisplayName(), ConfigVersion: t.GetConfigVersion(),
			EventTypes:   make([]triggerEventTypeJSON, 0, len(t.GetEventTypes())),
			FilterFields: make([]triggerFilterFieldJSON, 0, len(t.GetFilterFields())),
		}
		for _, e := range t.GetEventTypes() {
			tj.EventTypes = append(tj.EventTypes, triggerEventTypeJSON{ID: e.GetId(), DisplayName: e.GetDisplayName(), Description: e.GetDescription()})
		}
		for _, f := range t.GetFilterFields() {
			ops := make([]string, 0, len(f.GetOperators()))
			for _, op := range f.GetOperators() {
				ops = append(ops, triggerOperatorSpelling(op))
			}
			tj.FilterFields = append(tj.FilterFields, triggerFilterFieldJSON{
				Field: f.GetField(), Description: f.GetDescription(), Operators: ops, IsPrefix: f.GetIsPrefix(),
			})
		}
		out.Types = append(out.Types, tj)
	}
	return out
}
