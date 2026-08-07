package telemetry

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const (
	ProductionProjectToken = "phc_p4W5gaEDYv2Nke88eAL8FFVBtCrieXaK9EZQtUz4kpzm"
	StagingProjectToken    = "phc_BuZNLVTZjpeqaLMErvf2DDeHqnSyQ7yHg6mQiA5u5ifQ"
	ProductionPostHogHost  = "https://k.bossanova.dev"
	StagingPostHogHost     = "https://k-staging.bossanova.dev"
	DefaultHost            = ProductionPostHogHost
)

type Event string

const (
	EventCLICommandInvoked         Event = "cli_command_invoked"
	EventDaemonStarted             Event = "daemon_started"
	EventSessionCreated            Event = "session_created"
	EventChatCreated               Event = "chat_created"
	EventChatAttached              Event = "chat_attached"
	EventAuthChanged               Event = "auth_changed"
	EventRepairStarted             Event = "repair_started"
	EventRepairCompleted           Event = "repair_completed"
	EventBugReportSubmitted        Event = "bug_report_submitted"
	EventCloudAccessDenied         Event = "cloud_access_denied"
	EventCloudCheckoutStarted      Event = "cloud_checkout_started"
	EventCloudCheckoutReturned     Event = "cloud_checkout_returned"
	EventSignupUserCreated         Event = "signup_user_created"
	EventBillingAccountProvisioned Event = "billing_account_provisioned"
	EventCloudActionInvoked        Event = "cloud_action_invoked"
	EventAccountRotated            Event = "account_rotated"
	EventCronJobFired              Event = "cron_job_fired"
	EventPRCallbackDelivered       Event = "pr_callback_delivered"
	EventBroadcastDelivered        Event = "broadcast_delivered"
	EventSessionFinalized          Event = "session_finalized"
	EventFeatureViewed             Event = "feature_viewed"
	EventFeatureInteraction        Event = "feature_interaction"
	EventTUIAction                 Event = "tui_action"
)

// FunnelDistinctID is the canonical signup/subscription funnel distinct id:
// user:<workos-user-id> when known, else email:<lowercased+trimmed>, else "anonymous".
func FunnelDistinctID(workOSUserID, email string) string {
	if workOSUserID != "" {
		return "user:" + workOSUserID
	}
	if e := strings.ToLower(strings.TrimSpace(email)); e != "" {
		return "email:" + e
	}
	return "anonymous"
}

func LocalDistinctID(value string) string {
	return prefixedHashID("local", value)
}

func DaemonDistinctID(value string) string {
	return prefixedHashID("daemon", value)
}

func UserDistinctID(email string) string {
	normalized := strings.ToLower(strings.TrimSpace(email))
	if normalized == "" {
		return ""
	}
	return prefixedHashID("user", normalized)
}

func prefixedHashID(prefix, value string) string {
	if strings.TrimSpace(value) == "" {
		return prefix + "-unknown"
	}
	sum := sha256.Sum256([]byte(value))
	hash := hex.EncodeToString(sum[:])
	if len(hash) < 16 {
		return prefix + "-unknown"
	}
	return prefix + "-" + hash[:16]
}

// EventSpec describes one event's public telemetry contract. Registry is the
// single source of truth for event names and the scalar properties each event
// may send.
type EventSpec struct {
	Surface     string
	Description string
	Properties  map[string]struct{}
}

// CommonProperties are safe across events. Event-specific properties belong in
// the corresponding Registry entry.
var CommonProperties = propertySet("source")

// IdentifyProperties are safe to send with an Identify call. Identify payloads
// are not events, so their schema is kept separate from the event Registry.
var IdentifyProperties = propertySet(
	"action",
	"authenticated",
	"can_create_checkout",
	"checkout_action",
	"checkout_started",
	"cloud_access_state",
	"command",
	"context_has_error",
	"denial_reason",
	"entry_point",
	"ok",
	"product_area",
	"report_id",
	"resume",
	"source",
	"status",
	"step",
	"workos_org_id",
)

// Registry is intentionally exported so tests and tooling can verify the
// taxonomy. Callers must not mutate it.
var Registry = map[Event]EventSpec{
	EventCLICommandInvoked:         {Surface: "cli", Description: "A boss command completed", Properties: propertySet("command", "status")},
	EventDaemonStarted:             {Surface: "daemon", Description: "A bossd daemon became ready", Properties: propertySet()},
	EventSessionCreated:            {Surface: "tui", Description: "A session was created", Properties: propertySet()},
	EventChatCreated:               {Surface: "tui", Description: "A chat was created", Properties: propertySet()},
	EventChatAttached:              {Surface: "tui", Description: "A chat was attached", Properties: propertySet("action", "resume")},
	EventAuthChanged:               {Surface: "cli", Description: "CLI authentication changed", Properties: propertySet("action")},
	EventRepairStarted:             {Surface: "cli", Description: "A repair began", Properties: propertySet()},
	EventRepairCompleted:           {Surface: "cli", Description: "A repair completed", Properties: propertySet("status")},
	EventBugReportSubmitted:        {Surface: "cloud", Description: "A bug report was submitted", Properties: propertySet("report_id", "authenticated")},
	EventCloudAccessDenied:         {Surface: "cloud", Description: "Cloud access was denied", Properties: billingProperties()},
	EventCloudCheckoutStarted:      {Surface: "cloud", Description: "Cloud checkout started", Properties: billingProperties()},
	EventCloudCheckoutReturned:     {Surface: "cloud", Description: "Cloud checkout return was processed", Properties: billingProperties()},
	EventSignupUserCreated:         {Surface: "cloud", Description: "A signup created a user", Properties: propertySet("step")},
	EventBillingAccountProvisioned: {Surface: "cloud", Description: "A billing account was provisioned", Properties: propertySet("product_area", "step", "workos_org_id")},
	EventCloudActionInvoked:        {Surface: "cloud", Description: "A mutating cloud RPC completed", Properties: propertySet("command", "status", "product_area", "error_code")},
	EventAccountRotated:            {Surface: "daemon", Description: "An account rotation selected a replacement", Properties: propertySet("rotation_reason", "provider", "status")},
	EventCronJobFired:              {Surface: "daemon", Description: "A cron job fire reached an outcome", Properties: propertySet("status", "skip_reason", "zero_output")},
	EventPRCallbackDelivered:       {Surface: "daemon", Description: "A PR callback reached a terminal delivery outcome", Properties: propertySet("trigger", "status", "attempt_count")},
	EventBroadcastDelivered:        {Surface: "daemon", Description: "A broadcast delivery reached a terminal outcome", Properties: propertySet("status", "attempt_count")},
	EventSessionFinalized:          {Surface: "daemon", Description: "A session finalize reached an outcome", Properties: propertySet("outcome", "agent", "unattended")},
	EventFeatureViewed:             {Surface: "web", Description: "A product surface was viewed", Properties: propertySet("feature")},
	EventFeatureInteraction:        {Surface: "web", Description: "A client-only product interaction occurred", Properties: propertySet("feature", "action")},
	EventTUIAction:                 {Surface: "tui", Description: "A TUI feature action reached an outcome", Properties: propertySet("feature", "action", "status")},
}

func propertySet(properties ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(properties))
	for _, property := range properties {
		set[property] = struct{}{}
	}
	return set
}

func billingProperties() map[string]struct{} {
	return propertySet(
		"product_area",
		"cloud_access_state",
		"entry_point",
		"can_create_checkout",
		"checkout_started",
		"checkout_action",
		"denial_reason",
		"workos_org_id",
	)
}

func IsAllowed(event Event) bool {
	_, ok := Registry[event]
	return ok
}

func IsAllowedProperty(event Event, property string) bool {
	if _, ok := CommonProperties[property]; ok {
		return true
	}
	spec, ok := Registry[event]
	if !ok {
		return false
	}
	_, ok = spec.Properties[property]
	return ok
}

func IsAllowedIdentifyProperty(property string) bool {
	_, ok := IdentifyProperties[property]
	return ok
}
