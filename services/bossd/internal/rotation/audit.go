package rotation

import (
	"context"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/sqlutil"
)

// AuditEvent is one rotation-decision audit record (BOS-176). It is defined
// here — not in the db package — because db imports this package for the engine
// ports, so rotation must not import db. The db package provides a store
// adapter that converts AuditEvent to/from its persisted row type.
// Account fields carry labels/ids only — never credentials.
type AuditEvent struct {
	ID          string
	SessionID   string
	ChatID      string
	Provider    string
	Trigger     string // proto enum name, e.g. "ROTATION_TRIGGER_USAGE_LIMITED"
	FromAccount string
	ToAccount   string
	ResetAt     *time.Time
	Outcome     string // proto enum name, e.g. "ROTATION_OUTCOME_ROTATED"
	Detail      string
	CreatedAt   time.Time
}

// AuditStore is the narrow persistence port the Recorder needs. The db package
// adapts its SQLite store to this interface.
type AuditStore interface {
	Insert(ctx context.Context, ev AuditEvent) error
	RecentBySession(ctx context.Context, sessionID string, limit int) ([]AuditEvent, error)
}

// Recorder persists rotation audit events and mirrors each one to the
// structured daemon log. Auditing must never fail a rotation: Record logs
// insert errors instead of returning them. (BOS-176)
type Recorder struct {
	store  AuditStore
	logger zerolog.Logger
	now    func() time.Time
}

// NewRecorder builds a Recorder over the audit store.
func NewRecorder(store AuditStore, logger zerolog.Logger) *Recorder {
	return &Recorder{store: store, logger: logger, now: time.Now}
}

// Record fills ID/CreatedAt, persists the event, and writes a structured log.
// It never returns an error — auditing must never fail a rotation.
func (r *Recorder) Record(ctx context.Context, ev AuditEvent) {
	if r == nil {
		return
	}
	id, err := sqlutil.NewID()
	if err != nil {
		r.logger.Error().Err(err).Msg("rotation audit: id generation failed")
		return
	}
	ev.ID = id
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = r.now().UTC()
	}

	log := r.logger.Info().
		Str("session_id", ev.SessionID).
		Str("chat_id", ev.ChatID).
		Str("provider", ev.Provider).
		Str("trigger", ev.Trigger).
		Str("from_account", ev.FromAccount).
		Str("to_account", ev.ToAccount).
		Str("outcome", ev.Outcome).
		Str("detail", ev.Detail)
	if ev.ResetAt != nil {
		log = log.Time("reset_at", *ev.ResetAt)
	}
	log.Msg("rotation decision")

	if err := r.store.Insert(ctx, ev); err != nil {
		r.logger.Error().Err(err).Str("session_id", ev.SessionID).
			Msg("rotation audit: insert failed")
	}
}

// RecordExhausted records an all-accounts-exhausted event at most once per
// episode. An episode runs from the first exhausted decision until any
// non-exhausted decision (or a different reset minute) follows. Returns true
// when a new episode event was recorded — callers gate the single per-episode
// user notification/toast on this. (BOS-176)
func (r *Recorder) RecordExhausted(ctx context.Context, ev AuditEvent) bool {
	if r == nil {
		return false
	}
	recent, err := r.store.RecentBySession(ctx, ev.SessionID, 1)
	if err != nil {
		r.logger.Error().Err(err).Str("session_id", ev.SessionID).
			Msg("rotation audit: episode lookup failed; recording anyway")
	}
	if err == nil && len(recent) == 1 && recent[0].Outcome == ev.Outcome &&
		sameMinute(recent[0].ResetAt, ev.ResetAt) {
		return false // same episode — dedup
	}
	r.Record(ctx, ev)
	return true
}

// sameMinute reports whether two optional reset times fall in the same UTC
// minute (both nil counts as the same episode).
func sameMinute(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.UTC().Truncate(time.Minute).Equal(b.UTC().Truncate(time.Minute))
}
