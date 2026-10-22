package broadcast

import (
	"context"
	"errors"

	"github.com/recurser/bossalib/machine"
)

// SessionTransitionObserver receives a committed state edge and logs its own errors.
type SessionTransitionObserver interface {
	OnSessionState(context.Context, string, machine.State) error
}

// TransitionObservers notifies observers in order, even when an earlier observer fails.
// Each observer remains responsible for logging its own errors.
type TransitionObservers []SessionTransitionObserver

// OnSessionState implements SessionTransitionObserver.
func (observers TransitionObservers) OnSessionState(ctx context.Context, sessionID string, to machine.State) error {
	var errs []error
	for _, observer := range observers {
		if err := observer.OnSessionState(ctx, sessionID, to); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
