package status

import (
	"context"
	"sync/atomic"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossd/internal/db"
)

// TerminalReapObserver connects persisted terminal transitions to the reaper.
// Binding is deferred until startup has constructed the reaper; earlier
// transitions are covered by the periodic sweep.
type TerminalReapObserver struct {
	reaper atomic.Pointer[TmuxReaper]
}

var _ db.SessionTransitionObserver = (*TerminalReapObserver)(nil)

// Bind installs the daemon's reaper after its dependencies are available.
func (o *TerminalReapObserver) Bind(reaper *TmuxReaper) { o.reaper.Store(reaper) }

// OnSessionState never waits for the sweep or changes the store's outcome.
func (o *TerminalReapObserver) OnSessionState(_ context.Context, sessionID string, to machine.State) error {
	if to == machine.Merged || to == machine.Closed {
		if reaper := o.reaper.Load(); reaper != nil {
			reaper.NudgeSession(sessionID)
		}
	}
	return nil
}
