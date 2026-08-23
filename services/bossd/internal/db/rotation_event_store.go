package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/recurser/bossalib/sqlutil"
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
	ConfirmedAuthInvalidationSince(ctx context.Context, sessionID, chatID string, since time.Time) (bool, error)
}

// SQLiteRotationEventStore is the SQLite-backed implementation.
type SQLiteRotationEventStore struct {
	db *sql.DB
}

// NewRotationEventStore returns a SQLiteRotationEventStore.
func NewRotationEventStore(db *sql.DB) *SQLiteRotationEventStore {
	return &SQLiteRotationEventStore{db: db}
}

// Insert appends one audit row. Times are stored as ISO-8601 TEXT; reset_at is
// NULL when the event carried no reset. CreatedAt defaults to now when unset.
func (s *SQLiteRotationEventStore) Insert(ctx context.Context, ev RotationEvent) error {
	if ev.SessionID == "" {
		return fmt.Errorf("insert rotation event: session ID required")
	}
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = time.Now()
	}
	var resetAt *string
	if ev.ResetAt != nil {
		s := sqlutil.FormatTime(*ev.ResetAt)
		resetAt = &s
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO rotation_events
		   (id, session_id, chat_id, provider, trigger_kind, from_account,
		    to_account, reset_at, outcome, detail, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.ID, ev.SessionID, ev.ChatID, ev.Provider, ev.Trigger, ev.FromAccount,
		ev.ToAccount, resetAt, ev.Outcome, ev.Detail, sqlutil.FormatTime(ev.CreatedAt))
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
			ev        RotationEvent
			resetAt   sql.NullString
			createdAt string
		)
		if err := rows.Scan(&ev.ID, &ev.SessionID, &ev.ChatID, &ev.Provider,
			&ev.Trigger, &ev.FromAccount, &ev.ToAccount, &resetAt,
			&ev.Outcome, &ev.Detail, &createdAt); err != nil {
			return nil, fmt.Errorf("scan rotation event: %w", err)
		}
		if resetAt.Valid {
			ev.ResetAt = sqlutil.ParseOptionalTime(&resetAt.String)
		}
		ev.CreatedAt = sqlutil.ParseTime(createdAt)
		out = append(out, ev)
	}
	return out, rows.Err()
}

// ConfirmedAuthInvalidationSince reports whether the session has a corroborating
// auth-invalidation audit row for the current auth-failed episode of chatID.
func (s *SQLiteRotationEventStore) ConfirmedAuthInvalidationSince(ctx context.Context, sessionID, chatID string, since time.Time) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1
		 FROM rotation_events
		 WHERE session_id = ?
		   AND chat_id = ?
		   AND trigger_kind = 'ROTATION_TRIGGER_AUTH_INVALIDATED'
		   AND outcome NOT IN ('', 'ROTATION_OUTCOME_UNSPECIFIED', 'ROTATION_OUTCOME_STATUS_ONLY_DISABLED')
		   AND created_at >= ?
		 ORDER BY created_at DESC, id DESC
		 LIMIT 1`,
		sessionID, chatID, sqlutil.FormatTime(since)).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("find confirmed auth invalidation: %w", err)
	}
	return true, nil
}
