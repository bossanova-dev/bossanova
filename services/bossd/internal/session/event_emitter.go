package session

import (
	"context"
	"fmt"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/vcs"
)

// ForPR identifies a session attached to a pull request.
type ForPR struct {
	ID string
}

// Lookup finds sessions associated with a pull request.
type Lookup interface {
	SessionsForPR(ctx context.Context, repoOriginURL string, prNumber int) ([]ForPR, error)
}

// EventEmitter emits webhook-derived events to matching sessions.
type EventEmitter struct {
	lookup Lookup
	ch     chan<- Event
	logger zerolog.Logger
}

func NewSessionEventEmitter(
	lookup Lookup,
	ch chan<- Event,
	logger zerolog.Logger,
) *EventEmitter {
	return &EventEmitter{
		lookup: lookup,
		ch:     ch,
		logger: logger,
	}
}

func (e *EventEmitter) EmitForPR(
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

	e.logger.Info().
		Str("repo_origin_url", repoOriginURL).
		Int("pr_number", prNumber).
		Int("sessions_matched", len(sessions)).
		Int("event_count", len(events)).
		Msg("matched sessions for webhook event")

	for _, sess := range sessions {
		for _, ev := range events {
			select {
			case e.ch <- Event{SessionID: sess.ID, Event: ev}:
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
