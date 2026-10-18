package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/recurser/bossalib/sqlutil"
)

// PendingTrackerMerge is a merged session whose post-merge tracker write did
// not land: the next try is due at NextAttemptAt, after AttemptCount tries,
// the latest of which failed with LastReason.
type PendingTrackerMerge struct {
	SessionID     string
	AttemptCount  int
	NextAttemptAt time.Time
	LastReason    string
}

// PendingTrackerMergeStore is the durable queue of post-merge tracker writes
// still owed, so a failed one is retried across daemon restarts.
type PendingTrackerMergeStore interface {
	// Schedule records the session's next try, replacing any earlier entry.
	Schedule(ctx context.Context, p PendingTrackerMerge) error
	// ListDue returns the entries due at or before now, earliest first.
	ListDue(ctx context.Context, now time.Time) ([]PendingTrackerMerge, error)
	// Delete drops the session's entry; deleting a missing one is not an error.
	Delete(ctx context.Context, sessionID string) error
}

var _ PendingTrackerMergeStore = (*SQLitePendingTrackerMergeStore)(nil)

// SQLitePendingTrackerMergeStore implements PendingTrackerMergeStore over the
// pending_tracker_merges table.
type SQLitePendingTrackerMergeStore struct {
	db *sql.DB
}

// NewPendingTrackerMergeStore creates a SQLite-backed PendingTrackerMergeStore.
func NewPendingTrackerMergeStore(db *sql.DB) *SQLitePendingTrackerMergeStore {
	return &SQLitePendingTrackerMergeStore{db: db}
}

func (s *SQLitePendingTrackerMergeStore) Schedule(ctx context.Context, p PendingTrackerMerge) error {
	if p.SessionID == "" {
		return fmt.Errorf("schedule pending tracker merge: session ID required")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO pending_tracker_merges (session_id, attempt_count, next_attempt_at, last_reason)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT (session_id) DO UPDATE SET
		   attempt_count = excluded.attempt_count,
		   next_attempt_at = excluded.next_attempt_at,
		   last_reason = excluded.last_reason,
		   updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')`,
		p.SessionID, p.AttemptCount, sqlutil.FormatTime(p.NextAttemptAt), p.LastReason)
	if err != nil {
		return fmt.Errorf("schedule pending tracker merge: %w", err)
	}
	return nil
}

func (s *SQLitePendingTrackerMergeStore) ListDue(ctx context.Context, now time.Time) ([]PendingTrackerMerge, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT session_id, attempt_count, next_attempt_at, last_reason
		   FROM pending_tracker_merges
		  WHERE next_attempt_at <= ?
		  ORDER BY next_attempt_at, session_id`,
		sqlutil.FormatTime(now))
	if err != nil {
		return nil, fmt.Errorf("list due pending tracker merges: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []PendingTrackerMerge
	for rows.Next() {
		var p PendingTrackerMerge
		var at string
		if err := rows.Scan(&p.SessionID, &p.AttemptCount, &at, &p.LastReason); err != nil {
			return nil, fmt.Errorf("scan pending tracker merge: %w", err)
		}
		p.NextAttemptAt = sqlutil.ParseTime(at)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list due pending tracker merges: %w", err)
	}
	return out, nil
}

func (s *SQLitePendingTrackerMergeStore) Delete(ctx context.Context, sessionID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM pending_tracker_merges WHERE session_id = ?`, sessionID); err != nil {
		return fmt.Errorf("delete pending tracker merge: %w", err)
	}
	return nil
}
