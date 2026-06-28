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

var allowedEvents = map[Event]struct{}{
	EventCLICommandInvoked:         {},
	EventDaemonStarted:             {},
	EventSessionCreated:            {},
	EventChatCreated:               {},
	EventChatAttached:              {},
	EventAuthChanged:               {},
	EventRepairStarted:             {},
	EventRepairCompleted:           {},
	EventBugReportSubmitted:        {},
	EventCloudAccessDenied:         {},
	EventCloudCheckoutStarted:      {},
	EventCloudCheckoutReturned:     {},
	EventSignupUserCreated:         {},
	EventBillingAccountProvisioned: {},
}

func IsAllowed(event Event) bool {
	_, ok := allowedEvents[event]
	return ok
}
