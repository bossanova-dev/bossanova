package session

import (
	"context"
	"fmt"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/vcs"
)

// SessionForPR identifies a session attached to a pull request.
type SessionForPR struct {
	ID string
}

// SessionLookup finds sessions associated with a pull request.
type SessionLookup interface {
	SessionsForPR(ctx context.Context, repoOriginURL string, prNumber int) ([]SessionForPR, error)
}

// SessionEventEmitter emits webhook-derived events to matching sessions.
type SessionEventEmitter struct {
	lookup SessionLookup
	ch     chan<- SessionEvent
	logger zerolog.Logger
}

func NewSessionEventEmitter(
	lookup SessionLookup,
	ch chan<- SessionEvent,
	logger zerolog.Logger,
) *SessionEventEmitter {
	return &SessionEventEmitter{
		lookup: lookup,
		ch:     ch,
		logger: logger,
	}
}

func (e *SessionEventEmitter) EmitForPR(
	ctx context.Context,
	repoOriginURL string,
	prNumber int,
	events []vcs.Event,
) error {
	if len(events) == 0 {
		return nil
	}

	sessions, err := e.lookup.SessionsForPR(ctx, repoOriginURL, prNumber)
	if err != nil {
		return fmt.Errorf("lookup sessions for %s#%d: %w", repoOriginURL, prNumber, err)
	}

	for _, sess := range sessions {
		for _, ev := range events {
			select {
			case e.ch <- SessionEvent{SessionID: sess.ID, Event: ev}:
				e.logger.Debug().
					Str("session_id", sess.ID).
					Str("repo_origin_url", repoOriginURL).
					Int("pr_number", prNumber).
					Str("event_type", fmt.Sprintf("%T", ev)).
					Msg("emitted webhook event")
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}

	return nil
}
