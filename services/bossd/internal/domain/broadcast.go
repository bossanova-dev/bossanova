package domain

import (
	"time"

	"github.com/recurser/bossalib/models"
)

// Broadcast is one message addressed to a selector-resolved audience. Nullable
// columns are pointers.
//
// Message holds the broadcast body. It is a SECRET payload: it is stored
// verbatim because delivery needs it, but it must never be echoed back on a
// read surface, never written to logs, and never copied into a
// BroadcastDelivery.LastError diagnostic.
type Broadcast struct {
	ID           string
	OriginChatID *string // nil = operator-issued, no originating chat
	Selector     string  // Selector.String(), the byte-stable canonical form
	Message      string  // SECRET: never echoed back, logged, or put in LastError
	State        models.BroadcastState
	TargetCount  int
	ExpiresAt    time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// BroadcastDelivery is one resolved target of a broadcast, carrying the lease
// and backoff state a delivery worker needs. Nullable columns are pointers.
//
// LastError is a diagnostic string only. It must never carry the broadcast
// message body.
type BroadcastDelivery struct {
	ID              string
	BroadcastID     string
	TargetChatID    string
	TargetDaemonID  string // "" = this daemon; set for cross-daemon targets
	State           models.BroadcastDeliveryState
	LeaseOwner      *string
	LeaseDeadlineAt *time.Time
	AttemptCount    int
	NextAttemptAt   *time.Time
	DeliveredAt     *time.Time
	LastError       *string // diagnostics only: never the message body
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// BroadcastSubscription is a standing rule: when OwnerSessionID reaches an
// outcome matching TriggerEvent, broadcast Message to the audience Selector
// resolves. Nullable columns are pointers.
//
// It is a notification primitive, not an orchestration one — it delivers a
// message, it does not schedule work.
//
// Message holds the body the subscription will broadcast. It is a SECRET
// payload: it is stored verbatim because firing needs it, but it must never be
// echoed back on a read surface (the list/create RPC responses omit it), never
// written to a log line or an error string, and never persisted into a
// diagnostic column. Message is the only field on this struct that is unsafe to
// print; everything else may be logged freely.
//
// OwnerSessionID carries no foreign key on purpose: a subscription may outlive
// the session it watches, so expiry — not a referential cascade — is what
// retires one whose session will never settle. See the migration's prose block.
type BroadcastSubscription struct {
	ID               string
	OwnerSessionID   string  // the session whose outcome fires this; no FK
	OriginChatID     *string // nil = operator-issued, no registering chat
	TriggerEvent     models.BroadcastTrigger
	Selector         string // Selector.String(), the byte-stable canonical form
	Message          string // SECRET: never echoed back, logged, or put in a diagnostic
	State            models.BroadcastSubscriptionState
	FiredAt          *time.Time
	FiredBroadcastID *string // the broadcast the winning CAS produced
	ExpiresAt        time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}
