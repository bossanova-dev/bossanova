package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// RotationEvent is one persisted rotation-decision audit record (BOS-176).
// Account fields carry labels/ids only — never credentials.
type RotationEvent struct {
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

// RotationEventStore persists rotation audit events.
type RotationEventStore interface {
	Insert(ctx context.Context, ev RotationEvent) error
	RecentBySession(ctx context.Context, sessionID string, limit int) ([]RotationEvent, error)
}

// SQLiteRotationEventStore is the SQLite-backed implementation.
type SQLiteRotationEventStore struct {
	db *sql.DB
}

// NewRotationEventStore returns a SQLiteRotationEventStore.
func NewRotationEventStore(db *sql.DB) *SQLiteRotationEventStore {
	return &SQLiteRotationEventStore{db: db}
}

// Insert appends one audit row. Times are stored as Unix seconds; reset_at is
// NULL when the event carried no reset. CreatedAt defaults to now when unset.
func (s *SQLiteRotationEventStore) Insert(ctx context.Context, ev RotationEvent) error {
	if ev.SessionID == "" {
		return fmt.Errorf("insert rotation event: session ID required")
	}
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = time.Now()
	}
	var resetUnix *int64
	if ev.ResetAt != nil {
		u := ev.ResetAt.Unix()
		resetUnix = &u
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO rotation_events
		   (id, session_id, chat_id, provider, trigger_kind, from_account,
		    to_account, reset_at, outcome, detail, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.ID, ev.SessionID, ev.ChatID, ev.Provider, ev.Trigger, ev.FromAccount,
		ev.ToAccount, resetUnix, ev.Outcome, ev.Detail, ev.CreatedAt.Unix())
	if err != nil {
		return fmt.Errorf("insert rotation event: %w", err)
	}
	return nil
}

// RecentBySession returns up to `limit` events, newest-first.
func (s *SQLiteRotationEventStore) RecentBySession(ctx context.Context, sessionID string, limit int) ([]RotationEvent, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, session_id, chat_id, provider, trigger_kind, from_account,
		        to_account, reset_at, outcome, detail, created_at
		 FROM rotation_events
		 WHERE session_id = ?
		 ORDER BY created_at DESC, id DESC
		 LIMIT ?`,
		sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("list rotation events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []RotationEvent
	for rows.Next() {
		var (
			ev         RotationEvent
			resetUnix  sql.NullInt64
			createUnix int64
		)
		if err := rows.Scan(&ev.ID, &ev.SessionID, &ev.ChatID, &ev.Provider,
			&ev.Trigger, &ev.FromAccount, &ev.ToAccount, &resetUnix,
			&ev.Outcome, &ev.Detail, &createUnix); err != nil {
			return nil, fmt.Errorf("scan rotation event: %w", err)
		}
		if resetUnix.Valid {
			t := time.Unix(resetUnix.Int64, 0).UTC()
			ev.ResetAt = &t
		}
		ev.CreatedAt = time.Unix(createUnix, 0).UTC()
		out = append(out, ev)
	}
	return out, rows.Err()
}
