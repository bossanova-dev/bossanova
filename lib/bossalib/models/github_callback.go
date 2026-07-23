package models

import "time"

// GithubCallbackDefaultExpiry is applied when a callback is registered without
// an explicit expiry. It is the canonical value shared by the daemon store and
// every registration surface (CLI, MCP).
const GithubCallbackDefaultExpiry = 24 * time.Hour

// GithubCallbackMaxExpiry caps how far in the future a callback may be set to
// expire. Registrations beyond this are rejected. It is the canonical value
// shared by the daemon store and every registration surface (CLI, MCP).
const GithubCallbackMaxExpiry = 30 * 24 * time.Hour

// GithubCallbackTrigger is the pull-request event that fires a callback.
type GithubCallbackTrigger string

const (
	// GithubCallbackTriggerMerged fires when the PR is merged.
	GithubCallbackTriggerMerged GithubCallbackTrigger = "merged"
	// GithubCallbackTriggerClosed fires when the PR is closed without merging.
	GithubCallbackTriggerClosed GithubCallbackTrigger = "closed"
	// GithubCallbackTriggerChecksPassed fires when the PR's checks all pass.
	GithubCallbackTriggerChecksPassed GithubCallbackTrigger = "checks_passed"
	// GithubCallbackTriggerChecksFailed fires when a PR check fails.
	GithubCallbackTriggerChecksFailed GithubCallbackTrigger = "checks_failed"
)

// GithubCallbackState is the lifecycle position of a callback.
//
// Lifecycle: active -> leased -> triggered -> delivered, with canceled and
// expired as terminal states. A callback is registered active; a delivery
// worker leases it (leased); when the matching webhook event resolves, the
// group winner is marked triggered while its still-active/leased siblings are
// canceled; a successful chat delivery marks it delivered. A callback whose
// expiry passes before delivery becomes expired.
type GithubCallbackState string

const (
	// GithubCallbackStateActive is a registered callback awaiting its event.
	GithubCallbackStateActive GithubCallbackState = "active"
	// GithubCallbackStateLeased is claimed by a delivery worker.
	GithubCallbackStateLeased GithubCallbackState = "leased"
	// GithubCallbackStateTriggered is the group winner whose event has fired.
	GithubCallbackStateTriggered GithubCallbackState = "triggered"
	// GithubCallbackStateDelivered is a terminal state: the message was delivered.
	GithubCallbackStateDelivered GithubCallbackState = "delivered"
	// GithubCallbackStateCanceled is a terminal state: superseded by a sibling
	// trigger or explicitly deleted before firing.
	GithubCallbackStateCanceled GithubCallbackState = "canceled"
	// GithubCallbackStateExpired is a terminal state: expiry passed before delivery.
	GithubCallbackStateExpired GithubCallbackState = "expired"
)

// GithubCallback is a durable one-shot registration that delivers a chat message
// when a pull-request event fires. Nullable timing/diagnostic fields are pointers.
//
// Message holds the registered prompt body. It is a secret payload: it must never
// be written to logs or copied into last_error/last_event diagnostics.
type GithubCallback struct {
	ID              string
	GroupID         *string // nil = ungrouped (a group of one)
	TargetChatID    string
	RepoOwner       string
	RepoName        string
	PRNumber        int
	Trigger         GithubCallbackTrigger
	State           GithubCallbackState
	Message         string
	LeaseOwner      *string
	LeaseDeadlineAt *time.Time
	AttemptCount    int
	NextAttemptAt   *time.Time
	TriggeredAt     *time.Time
	DeliveredAt     *time.Time
	LastError       *string
	LastEvent       *string
	ExpiresAt       time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}
