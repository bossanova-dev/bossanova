package models

import "time"

// GitHubCallbackDefaultExpiry is applied when a callback is registered without
// an explicit expiry. It is the canonical value shared by the daemon store and
// every registration surface (CLI, MCP).
const GitHubCallbackDefaultExpiry = 24 * time.Hour

// GitHubCallbackMaxExpiry caps how far in the future a callback may be set to
// expire. Registrations beyond this are rejected. It is the canonical value
// shared by the daemon store and every registration surface (CLI, MCP).
const GitHubCallbackMaxExpiry = 30 * 24 * time.Hour

// GitHubCallbackTrigger is the pull-request event that fires a callback.
type GitHubCallbackTrigger string

const (
	// GitHubCallbackTriggerMerged fires when the PR is merged.
	GitHubCallbackTriggerMerged GitHubCallbackTrigger = "merged"
	// GitHubCallbackTriggerClosed fires when the PR is closed without merging.
	GitHubCallbackTriggerClosed GitHubCallbackTrigger = "closed"
	// GitHubCallbackTriggerChecksPassed fires when the PR's checks all pass.
	GitHubCallbackTriggerChecksPassed GitHubCallbackTrigger = "checks_passed"
	// GitHubCallbackTriggerChecksFailed fires when a PR check fails.
	GitHubCallbackTriggerChecksFailed GitHubCallbackTrigger = "checks_failed"
	// GitHubCallbackTriggerReadyForReview fires when the PR is open and not a
	// draft (i.e. on or after the draft-to-ready flip).
	GitHubCallbackTriggerReadyForReview GitHubCallbackTrigger = "ready_for_review"
	// GitHubCallbackTriggerChecksPassedReady fires when the PR's checks are all
	// green and the PR is not a draft — the merge-eligibility moment.
	GitHubCallbackTriggerChecksPassedReady GitHubCallbackTrigger = "checks_passed_ready"
)

// IsStandby reports whether t is a contingency trigger: one that fires only when
// the PR goes wrong (checks_failed) or leaves the workflow (closed). A chat
// parked only on standby triggers is not what holds the PR — some other chat or
// stage owns its forward progress — so displays and the daemon's reason
// selection rank it behind progress triggers (checks_passed,
// checks_passed_ready, ready_for_review, merged). Unknown triggers are not
// standby, so the failure mode is "shown as waiting", never "hidden".
func (t GitHubCallbackTrigger) IsStandby() bool {
	switch t {
	case GitHubCallbackTriggerChecksFailed, GitHubCallbackTriggerClosed:
		return true
	default:
		return false
	}
}

// GitHubCallbackState is the lifecycle position of a callback.
//
// Lifecycle: active -> leased -> triggered -> delivered, with canceled and
// expired as terminal states. A callback is registered active; a delivery
// worker leases it (leased); when the matching webhook event resolves, the
// group winner is marked triggered while its still-active/leased siblings are
// canceled; a successful chat delivery marks it delivered. A callback whose
// expiry passes before delivery becomes expired.
type GitHubCallbackState string

const (
	// GitHubCallbackStateActive is a registered callback awaiting its event.
	GitHubCallbackStateActive GitHubCallbackState = "active"
	// GitHubCallbackStateLeased is claimed by a delivery worker.
	GitHubCallbackStateLeased GitHubCallbackState = "leased"
	// GitHubCallbackStateTriggered is the group winner whose event has fired.
	GitHubCallbackStateTriggered GitHubCallbackState = "triggered"
	// GitHubCallbackStateDelivered is a terminal state: the message was delivered.
	GitHubCallbackStateDelivered GitHubCallbackState = "delivered"
	// GitHubCallbackStateCanceled is a terminal state: superseded by a sibling
	// trigger or explicitly deleted before firing.
	GitHubCallbackStateCanceled GitHubCallbackState = "canceled"
	// GitHubCallbackStateExpired is a terminal state: expiry passed before delivery.
	GitHubCallbackStateExpired GitHubCallbackState = "expired"
)

// GitHubCallbackComposerOccupiedWait is the last_error the delivery worker
// records while a triggered callback waits for its chat's input box to empty.
// It is written for the operator: `boss callback list` shows such a callback
// as waiting, with this sentence as the reason.
const GitHubCallbackComposerOccupiedWait = "The chat's input box holds unsent text. It's delivered once that's sent or cleared."

// GitHubCallback is a durable one-shot registration that delivers a chat message
// when a pull-request event fires. Nullable timing/diagnostic fields are pointers.
//
// Message holds the registered prompt body. It is a secret payload: it must never
// be written to logs or copied into last_error/last_event diagnostics.
type GitHubCallback struct {
	ID                      string
	GroupID                 *string // nil = ungrouped (a group of one)
	TargetChatID            string
	RepoOwner               string
	RepoName                string
	PRNumber                int
	Trigger                 GitHubCallbackTrigger
	State                   GitHubCallbackState
	Message                 string
	ShouldRequireTransition bool
	HasObservedBaseline     bool
	LeaseOwner              *string
	LeaseDeadlineAt         *time.Time
	AttemptCount            int
	NextAttemptAt           *time.Time
	TriggeredAt             *time.Time
	DeliveredAt             *time.Time
	LastError               *string
	LastEvent               *string
	ExpiresAt               time.Time
	CreatedAt               time.Time
	UpdatedAt               time.Time
}
