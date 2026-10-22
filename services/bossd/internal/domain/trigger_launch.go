package domain

import "time"

// TriggerLaunch is the daemon's idempotency record for one inbound trigger
// invocation (BOS-1418). A row with an empty SessionID is a claim whose launch
// is still in flight; a row with a SessionID is a finished launch that every
// replay of the same invocation must return.
type TriggerLaunch struct {
	InvocationID string
	TriggerID    string
	// SessionID is empty while the launch is in flight.
	SessionID string
	// AcceptedSessionID records a create accepted by the daemon, before bootstrap settles.
	AcceptedSessionID string
	CreatedAt         time.Time
}
