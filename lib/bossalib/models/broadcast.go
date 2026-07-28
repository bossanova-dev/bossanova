package models

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

// ErrUnknownBroadcastState is returned when a broadcast state string does not
// name a known state. Persisted state is parsed rather than cast so an
// unrecognised value from the database is a loud error at the read boundary
// instead of a silent zero value that later reads as "not pending".
var ErrUnknownBroadcastState = errors.New("unknown broadcast state")

// ErrUnknownBroadcastDeliveryState is the delivery-state counterpart of
// ErrUnknownBroadcastState.
var ErrUnknownBroadcastDeliveryState = errors.New("unknown broadcast delivery state")

// ErrUnknownBroadcastSubscriptionState is the subscription-state counterpart of
// ErrUnknownBroadcastState.
var ErrUnknownBroadcastSubscriptionState = errors.New("unknown broadcast subscription state")

// ErrUnknownBroadcastTrigger is returned when a trigger string does not name a
// known trigger class. An unrecognised trigger read as a silent zero value would
// match no class and quietly never fire, which is exactly the failure a standing
// subscription must not have.
var ErrUnknownBroadcastTrigger = errors.New("unknown broadcast trigger")

// BroadcastState is the lifecycle position of a broadcast.
//
// Lifecycle: pending -> resolved -> completed, with canceled and expired as
// terminal states. A broadcast is created pending; resolving its selector
// materialises the audience into broadcast_deliveries and moves it to resolved;
// once no delivery is still pending or leased it becomes completed. A broadcast
// whose expiry passes before it settles becomes expired; an operator may cancel
// one outright.
type BroadcastState string

const (
	// BroadcastStatePending is a created broadcast whose audience is not yet
	// resolved.
	BroadcastStatePending BroadcastState = "pending"
	// BroadcastStateResolved has its audience materialised as delivery rows.
	BroadcastStateResolved BroadcastState = "resolved"
	// BroadcastStateCompleted is a terminal state: no delivery is still pending
	// or leased.
	BroadcastStateCompleted BroadcastState = "completed"
	// BroadcastStateExpired is a terminal state: expiry passed before the
	// broadcast settled.
	BroadcastStateExpired BroadcastState = "expired"
	// BroadcastStateCanceled is a terminal state: retired explicitly before it
	// settled. No store method writes it yet — the cancel RPC is a later child.
	// Whoever adds it must also settle the broadcast's outstanding deliveries:
	// the expiry sweep's cascade only reaps expired parents, and AcquireLease
	// only admits pending/resolved ones, so deliveries left under a canceled
	// broadcast would be permanently unclaimable and unreapable.
	BroadcastStateCanceled BroadcastState = "canceled"
)

// BroadcastStates returns the canonical broadcast-state vocabulary in lifecycle
// order. It is the single list every validation path derives from, so adding a
// state cannot leave one surface silently accepting a value another rejects.
func BroadcastStates() []BroadcastState {
	return []BroadcastState{
		BroadcastStatePending,
		BroadcastStateResolved,
		BroadcastStateCompleted,
		BroadcastStateExpired,
		BroadcastStateCanceled,
	}
}

// String returns the canonical storage form of the state.
func (s BroadcastState) String() string { return string(s) }

// Valid reports whether s is a known broadcast state. Membership is exact — no
// trimming or case folding — so the stored value is always canonical.
func (s BroadcastState) Valid() bool { return slices.Contains(BroadcastStates(), s) }

// ParseBroadcastState converts a stored string into a BroadcastState, returning
// an error wrapping ErrUnknownBroadcastState for any value outside the
// vocabulary. Read paths must use this rather than a bare conversion.
func ParseBroadcastState(s string) (BroadcastState, error) {
	state := BroadcastState(s)
	if !state.Valid() {
		return "", fmt.Errorf("%w: %q", ErrUnknownBroadcastState, s)
	}
	return state, nil
}

// BroadcastDeliveryState is the lifecycle position of a single delivery.
//
// Lifecycle: pending -> leased -> delivered, with failed and skipped as
// terminal states. A worker leases a claimable delivery (leased), delivers the
// message (delivered), or gives up after its retries are exhausted (failed).
// skipped means the target vanished before delivery — the chat is gone, or its
// parent broadcast expired while the delivery was still pending.
type BroadcastDeliveryState string

const (
	// BroadcastDeliveryStatePending is a resolved target awaiting a worker.
	BroadcastDeliveryStatePending BroadcastDeliveryState = "pending"
	// BroadcastDeliveryStateLeased is claimed by a delivery worker.
	BroadcastDeliveryStateLeased BroadcastDeliveryState = "leased"
	// BroadcastDeliveryStateDelivered is a terminal state: the message reached
	// the target chat.
	BroadcastDeliveryStateDelivered BroadcastDeliveryState = "delivered"
	// BroadcastDeliveryStateFailed is a terminal state: delivery gave up.
	// Written by the delivery worker's retry cap, not by the store — the store's
	// ScheduleRetry always returns a row to pending, because the cap is a worker
	// policy rather than a persistence concern.
	BroadcastDeliveryStateFailed BroadcastDeliveryState = "failed"
	// BroadcastDeliveryStateSkipped is a terminal state: no delivery was
	// recorded. Usually the target vanished, or the parent broadcast expired
	// while the row was still outstanding.
	//
	// It means "not recorded as delivered", NOT "nothing was sent". Delivery is
	// at-least-once, so an expiry sweep that lands between a worker's send and
	// its MarkDelivered retires the row as skipped even though the message did
	// reach the chat. Nothing resends a skipped row, so this costs an
	// under-report, never a duplicate — but a consumer must not read skipped as
	// proof the target was never messaged.
	BroadcastDeliveryStateSkipped BroadcastDeliveryState = "skipped"
)

// BroadcastDeliveryStates returns the canonical delivery-state vocabulary in
// lifecycle order, for the same single-source reason as BroadcastStates.
func BroadcastDeliveryStates() []BroadcastDeliveryState {
	return []BroadcastDeliveryState{
		BroadcastDeliveryStatePending,
		BroadcastDeliveryStateLeased,
		BroadcastDeliveryStateDelivered,
		BroadcastDeliveryStateFailed,
		BroadcastDeliveryStateSkipped,
	}
}

// String returns the canonical storage form of the state.
func (s BroadcastDeliveryState) String() string { return string(s) }

// Valid reports whether s is a known delivery state. Membership is exact.
func (s BroadcastDeliveryState) Valid() bool {
	return slices.Contains(BroadcastDeliveryStates(), s)
}

// ParseBroadcastDeliveryState converts a stored string into a
// BroadcastDeliveryState, returning an error wrapping
// ErrUnknownBroadcastDeliveryState for any value outside the vocabulary.
func ParseBroadcastDeliveryState(s string) (BroadcastDeliveryState, error) {
	state := BroadcastDeliveryState(s)
	if !state.Valid() {
		return "", fmt.Errorf("%w: %q", ErrUnknownBroadcastDeliveryState, s)
	}
	return state, nil
}

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
	State        BroadcastState
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
	State           BroadcastDeliveryState
	LeaseOwner      *string
	LeaseDeadlineAt *time.Time
	AttemptCount    int
	NextAttemptAt   *time.Time
	DeliveredAt     *time.Time
	LastError       *string // diagnostics only: never the message body
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// BroadcastSubscriptionState is the lifecycle position of a standing broadcast
// subscription.
//
// Lifecycle: active -> fired, with canceled and expired as the other terminal
// states. A subscription is created active; the owning session reaching a state
// that matches its trigger fires it exactly once (a compare-and-swap on active,
// so a concurrent double transition cannot fire twice) and records the resulting
// broadcast. An operator may cancel one outright; the reconcile sweep retires
// one whose expiry passed before its session ever settled.
type BroadcastSubscriptionState string

const (
	// BroadcastSubscriptionStateActive is a live standing registration awaiting
	// its trigger.
	BroadcastSubscriptionStateActive BroadcastSubscriptionState = "active"
	// BroadcastSubscriptionStateFired is a terminal state: the trigger was
	// observed and a broadcast was issued. It is written by the CAS that makes
	// firing exactly-once, and is never rolled back — a send failure leaves the
	// subscription fired, because the broadcast's own delivery worker owns retry
	// and un-firing would risk a duplicate on the next transition.
	BroadcastSubscriptionStateFired BroadcastSubscriptionState = "fired"
	// BroadcastSubscriptionStateCanceled is a terminal state: retired explicitly
	// before its trigger was observed.
	BroadcastSubscriptionStateCanceled BroadcastSubscriptionState = "canceled"
	// BroadcastSubscriptionStateExpired is a terminal state: expiry passed before
	// the owning session reached a trigger state. This is also how a subscription
	// whose session vanished is retired — the row carries no foreign key, so
	// nothing else would ever reap it.
	BroadcastSubscriptionStateExpired BroadcastSubscriptionState = "expired"
)

// BroadcastSubscriptionStates returns the canonical subscription-state
// vocabulary in lifecycle order, for the same single-source reason as
// BroadcastStates.
func BroadcastSubscriptionStates() []BroadcastSubscriptionState {
	return []BroadcastSubscriptionState{
		BroadcastSubscriptionStateActive,
		BroadcastSubscriptionStateFired,
		BroadcastSubscriptionStateCanceled,
		BroadcastSubscriptionStateExpired,
	}
}

// String returns the canonical storage form of the state.
func (s BroadcastSubscriptionState) String() string { return string(s) }

// Valid reports whether s is a known subscription state. Membership is exact —
// no trimming or case folding — so the stored value is always canonical.
func (s BroadcastSubscriptionState) Valid() bool {
	return slices.Contains(BroadcastSubscriptionStates(), s)
}

// ParseBroadcastSubscriptionState converts a stored string into a
// BroadcastSubscriptionState, returning an error wrapping
// ErrUnknownBroadcastSubscriptionState for any value outside the vocabulary.
// Read paths must use this rather than a bare conversion.
func ParseBroadcastSubscriptionState(s string) (BroadcastSubscriptionState, error) {
	state := BroadcastSubscriptionState(s)
	if !state.Valid() {
		return "", fmt.Errorf("%w: %q", ErrUnknownBroadcastSubscriptionState, s)
	}
	return state, nil
}

// BroadcastTrigger is the class of session outcome a subscription waits for.
//
// The classes are deliberately coarse: a subscriber cares that a child finished
// or failed, not which of the fourteen machine states expressed it. The state ->
// class mapping lives in exactly one place in bossd so a newly added session
// state is a visible, deliberate classification decision rather than a silent
// no-fire.
type BroadcastTrigger string

const (
	// BroadcastTriggerCompleted fires on a successful outcome.
	BroadcastTriggerCompleted BroadcastTrigger = "completed"
	// BroadcastTriggerErrored fires on a failed or abandoned outcome.
	BroadcastTriggerErrored BroadcastTrigger = "errored"
	// BroadcastTriggerSettled fires on either — the "tell me when it is over,
	// whatever happened" case. It matches both other classes, never neither.
	BroadcastTriggerSettled BroadcastTrigger = "settled"
)

// BroadcastTriggers returns the canonical trigger vocabulary, narrowest first,
// for the same single-source reason as BroadcastStates.
func BroadcastTriggers() []BroadcastTrigger {
	return []BroadcastTrigger{
		BroadcastTriggerCompleted,
		BroadcastTriggerErrored,
		BroadcastTriggerSettled,
	}
}

// String returns the canonical storage form of the trigger.
func (t BroadcastTrigger) String() string { return string(t) }

// Valid reports whether t is a known trigger. Membership is exact.
func (t BroadcastTrigger) Valid() bool { return slices.Contains(BroadcastTriggers(), t) }

// ParseBroadcastTrigger converts a stored string into a BroadcastTrigger,
// returning an error wrapping ErrUnknownBroadcastTrigger for any value outside
// the vocabulary.
func ParseBroadcastTrigger(s string) (BroadcastTrigger, error) {
	trigger := BroadcastTrigger(s)
	if !trigger.Valid() {
		return "", fmt.Errorf("%w: %q", ErrUnknownBroadcastTrigger, s)
	}
	return trigger, nil
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
	TriggerEvent     BroadcastTrigger
	Selector         string // Selector.String(), the byte-stable canonical form
	Message          string // SECRET: never echoed back, logged, or put in a diagnostic
	State            BroadcastSubscriptionState
	FiredAt          *time.Time
	FiredBroadcastID *string // the broadcast the winning CAS produced
	ExpiresAt        time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}
