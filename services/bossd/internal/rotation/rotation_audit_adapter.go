package rotation

import (
	"context"

	"github.com/recurser/bossd/internal/db"
)

// AuditStoreAdapter adapts persisted rotation events to the audit port.
type AuditStoreAdapter struct {
	inner db.RotationEventStore
}

// NewAuditStoreAdapter wraps a RotationEventStore as an AuditStore.
func NewAuditStoreAdapter(inner db.RotationEventStore) *AuditStoreAdapter {
	return &AuditStoreAdapter{inner: inner}
}

// Create converts and persists one audit event.
func (a *AuditStoreAdapter) Create(ctx context.Context, ev AuditEvent) error {
	return a.inner.Create(ctx, db.RotationEvent{
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

// ListBySession returns recent audit events newest-first.
func (a *AuditStoreAdapter) ListBySession(ctx context.Context, sessionID string, limit int) ([]AuditEvent, error) {
	rows, err := a.inner.ListBySession(ctx, sessionID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]AuditEvent, 0, len(rows))
	for _, r := range rows {
		out = append(out, AuditEvent{
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
