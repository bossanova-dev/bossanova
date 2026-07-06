package db

import (
	"context"

	"github.com/recurser/bossd/internal/rotation"
)

// RotationAuditStore adapts the SQLite RotationEventStore to the
// rotation.AuditStore port the Recorder consumes. It lives here (not in the
// rotation package) because db already imports rotation for the engine ports,
// so the conversion between rotation.AuditEvent and the persisted RotationEvent
// row type must happen on this side of the dependency edge. (BOS-176)
type RotationAuditStore struct {
	inner RotationEventStore
}

// NewRotationAuditStore wraps a RotationEventStore as a rotation.AuditStore.
func NewRotationAuditStore(inner RotationEventStore) *RotationAuditStore {
	return &RotationAuditStore{inner: inner}
}

// Insert converts and persists one audit event.
func (a *RotationAuditStore) Insert(ctx context.Context, ev rotation.AuditEvent) error {
	return a.inner.Insert(ctx, RotationEvent{
		ID:          ev.ID,
		SessionID:   ev.SessionID,
		ChatID:      ev.ChatID,
		Provider:    ev.Provider,
		Trigger:     ev.Trigger,
		FromAccount: ev.FromAccount,
		ToAccount:   ev.ToAccount,
		ResetAt:     ev.ResetAt,
		Outcome:     ev.Outcome,
		Detail:      ev.Detail,
		CreatedAt:   ev.CreatedAt,
	})
}

// RecentBySession returns recent audit events newest-first.
func (a *RotationAuditStore) RecentBySession(ctx context.Context, sessionID string, limit int) ([]rotation.AuditEvent, error) {
	rows, err := a.inner.RecentBySession(ctx, sessionID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]rotation.AuditEvent, 0, len(rows))
	for _, r := range rows {
		out = append(out, rotation.AuditEvent{
			ID:          r.ID,
			SessionID:   r.SessionID,
			ChatID:      r.ChatID,
			Provider:    r.Provider,
			Trigger:     r.Trigger,
			FromAccount: r.FromAccount,
			ToAccount:   r.ToAccount,
			ResetAt:     r.ResetAt,
			Outcome:     r.Outcome,
			Detail:      r.Detail,
			CreatedAt:   r.CreatedAt,
		})
	}
	return out, nil
}
