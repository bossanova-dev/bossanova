package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/recurser/bossalib/contract/cloud/sqlutil"
	"github.com/recurser/bossd/internal/db/sqlcgen"
	"github.com/recurser/bossd/internal/domain"
)

type RotationEvent = domain.RotationEvent

// RotationEventStore persists rotation audit events.
type RotationEventStore interface {
	Create(context.Context, domain.RotationEvent) error
	ListBySession(context.Context, string, int) ([]domain.RotationEvent, error)
	GetByConfirmedAuthInvalidationSince(context.Context, string, string, time.Time) (bool, error)
}

type SQLiteRotationEventStore struct{ queries *sqlcgen.Queries }

func NewRotationEventStore(db *sql.DB) *SQLiteRotationEventStore {
	return &SQLiteRotationEventStore{queries: sqlcgen.New(db)}
}

// Create appends one audit row. CreatedAt defaults to now when unset.
func (s *SQLiteRotationEventStore) Create(ctx context.Context, ev domain.RotationEvent) error {
	if ev.SessionID == "" {
		return fmt.Errorf("insert rotation event: session ID required")
	}
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = time.Now()
	}
	err := s.queries.CreateRotationEvent(ctx, sqlcgen.CreateRotationEventParams{
		ID: ev.ID, SessionID: ev.SessionID, ChatID: ev.ChatID, Provider: ev.Provider,
		TriggerKind: ev.Trigger, FromAccount: ev.FromAccount, ToAccount: ev.ToAccount,
		ResetAt: timeText(ev.ResetAt), Outcome: ev.Outcome, Detail: ev.Detail,
		CreatedAt: sqlutil.FormatTime(ev.CreatedAt),
	})
	if err != nil {
		return fmt.Errorf("insert rotation event: %w", err)
	}
	return nil
}

func (s *SQLiteRotationEventStore) ListBySession(ctx context.Context, sessionID string, limit int) ([]domain.RotationEvent, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.queries.ListRotationEventsBySession(ctx, sqlcgen.ListRotationEventsBySessionParams{SessionID: sessionID, Limit: int64(limit)})
	if err != nil {
		return nil, fmt.Errorf("list rotation events: %w", err)
	}
	return mapRows(rows, rotationEventFromRow), nil
}

func (s *SQLiteRotationEventStore) GetByConfirmedAuthInvalidationSince(ctx context.Context, sessionID, chatID string, since time.Time) (bool, error) {
	_, err := s.queries.GetRotationEventByConfirmedAuthInvalidationSince(ctx, sqlcgen.GetRotationEventByConfirmedAuthInvalidationSinceParams{SessionID: sessionID, ChatID: chatID, CreatedAt: sqlutil.FormatTime(since)})
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("find confirmed auth invalidation: %w", err)
	}
	return true, nil
}

func rotationEventFromRow(row sqlcgen.RotationEvent) domain.RotationEvent {
	return domain.RotationEvent{ID: row.ID, SessionID: row.SessionID, ChatID: row.ChatID,
		Provider: row.Provider, Trigger: row.TriggerKind, FromAccount: row.FromAccount,
		ToAccount: row.ToAccount, ResetAt: optionalTextTime(row.ResetAt), Outcome: row.Outcome,
		Detail: row.Detail, CreatedAt: textTime(row.CreatedAt)}
}
