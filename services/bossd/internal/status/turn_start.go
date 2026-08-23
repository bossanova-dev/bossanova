package status

import (
	"context"
	"time"
)

// TurnStartObservationBudget bounds the optional post-submit turn-start wait.
// It is long enough to span several poller ticks, but stays inside the daemon's
// interactive RPC budget.
var TurnStartObservationBudget = 15 * time.Second

// TurnStartObservationTick is the observer's sampling cadence. It deliberately
// differs from the tmux poll interval so tests and production avoid aliasing on
// a single cadence.
var TurnStartObservationTick = time.Second

// TurnStartVerdict is the status-layer answer to "did this chat begin a turn
// after the send?". No verdict permits sending the same payload again.
type TurnStartVerdict int

const (
	TurnStartUnobservable TurnStartVerdict = iota
	TurnStartObserved
	TurnStartNotObserved
)

func (v TurnStartVerdict) String() string {
	switch v {
	case TurnStartObserved:
		return "observed"
	case TurnStartNotObserved:
		return "not_observed"
	case TurnStartUnobservable:
		return "unobservable"
	default:
		return "unobservable"
	}
}

func (v TurnStartVerdict) ResendPermitted() bool {
	switch v {
	case TurnStartObserved, TurnStartNotObserved, TurnStartUnobservable:
		return false
	default:
		return false
	}
}

// TurnStartBaseline is the pre-send liveness marker the observer anchors to.
type TurnStartBaseline struct {
	observation LivenessObservation
}

// CaptureTurnStartBaseline records the chat's current poller marker. A nil
// tracker or absent marker is still represented, so the later await can report
// UNOBSERVABLE without panicking.
func CaptureTurnStartBaseline(tracker *Tracker, agentSessionID string) TurnStartBaseline {
	if tracker == nil {
		return TurnStartBaseline{}
	}
	return TurnStartBaseline{observation: tracker.LivenessObservation(agentSessionID)}
}

// AwaitTurnStart waits for a bounded, post-submit observation of turn start.
func AwaitTurnStart(ctx context.Context, tracker *Tracker, agentSessionID string, baseline TurnStartBaseline) TurnStartVerdict {
	if tracker == nil || !baseline.observation.Present {
		return TurnStartUnobservable
	}
	deadline := time.NewTimer(TurnStartObservationBudget)
	defer deadline.Stop()
	ticker := time.NewTicker(TurnStartObservationTick)
	defer ticker.Stop()

	sawObservation := false
	for {
		select {
		case <-ctx.Done():
			return TurnStartUnobservable
		case <-deadline.C:
			if sawObservation {
				return TurnStartNotObserved
			}
			return TurnStartUnobservable
		case <-ticker.C:
			current := tracker.LivenessObservation(agentSessionID)
			if !current.Present {
				return TurnStartUnobservable
			}
			if !current.ObservedAt.After(baseline.observation.ObservedAt) {
				continue
			}
			sawObservation = true
			if !current.LastSubstantiveOutputSeed &&
				current.LastSubstantiveOutputAt.After(baseline.observation.LastSubstantiveOutputAt) {
				return TurnStartObserved
			}
		}
	}
}
