package models

import (
	"errors"
	"fmt"
	"slices"
)

// ErrUnknownBroadcastTrigger is returned when a trigger string does not name a
// known trigger class. An unrecognised trigger read as a silent zero value would
// match no class and quietly never fire, which is exactly the failure a standing
// subscription must not have.
var ErrUnknownBroadcastTrigger = errors.New("unknown broadcast trigger")

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
