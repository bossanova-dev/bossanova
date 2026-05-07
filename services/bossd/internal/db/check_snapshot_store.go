package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// CheckSnapshot is a single per-poll record of what the DisplayPoller saw
// for a session's CI checks plus the DisplayStatus it computed from them.
// Used by `boss session checks <id>` to answer "why did bossd think this
// PR was passing?" without re-running gh by hand.
type CheckSnapshot struct {
	ID             int64
	SessionID      string
	PolledAt       time.Time
	HeadSHA        string
	RawJSON        string
	ComputedStatus int
}

// CheckSnapshotStore persists DisplayPoller snapshots so they outlive the
// daemon's in-memory tracker. The DisplayPoller calls Insert after each
// successful poll; the boss CLI reads via RecentBySession.
type CheckSnapshotStore interface {
	Insert(ctx context.Context, snap CheckSnapshot) error
	RecentBySession(ctx context.Context, sessionID string, limit int) ([]CheckSnapshot, error)
}

// SQLiteCheckSnapshotStore is the SQLite-backed implementation.
type SQLiteCheckSnapshotStore struct {
	db *sql.DB
}

// NewCheckSnapshotStore returns a SQLiteCheckSnapshotStore.
func NewCheckSnapshotStore(db *sql.DB) *SQLiteCheckSnapshotStore {
	return &SQLiteCheckSnapshotStore{db: db}
}

// Insert appends a snapshot row. PolledAt is stored as a Unix timestamp.
// We keep every row — there's no vacuum here. The volume is bounded by the
// poll interval (default 30s) × number of active sessions, which is fine
// for ops-debugging timelines and trivially purgeable later.
func (s *SQLiteCheckSnapshotStore) Insert(ctx context.Context, snap CheckSnapshot) error {
	if snap.SessionID == "" {
		return fmt.Errorf("insert check snapshot: session ID required")
	}
	if snap.PolledAt.IsZero() {
		snap.PolledAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO session_check_snapshots
		   (session_id, polled_at, head_sha, raw_json, computed_status)
		 VALUES (?, ?, ?, ?, ?)`,
		snap.SessionID, snap.PolledAt.Unix(), snap.HeadSHA, snap.RawJSON, snap.ComputedStatus)
	if err != nil {
		return fmt.Errorf("insert check snapshot: %w", err)
	}
	return nil
}

// RecentBySession returns up to `limit` snapshots, newest-first.
func (s *SQLiteCheckSnapshotStore) RecentBySession(ctx context.Context, sessionID string, limit int) ([]CheckSnapshot, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, session_id, polled_at, head_sha, raw_json, computed_status
		 FROM session_check_snapshots
		 WHERE session_id = ?
		 ORDER BY polled_at DESC, id DESC
		 LIMIT ?`,
		sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("list check snapshots: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []CheckSnapshot
	for rows.Next() {
		var snap CheckSnapshot
		var polledUnix int64
		if err := rows.Scan(&snap.ID, &snap.SessionID, &polledUnix, &snap.HeadSHA, &snap.RawJSON, &snap.ComputedStatus); err != nil {
			return nil, fmt.Errorf("scan check snapshot: %w", err)
		}
		snap.PolledAt = time.Unix(polledUnix, 0)
		out = append(out, snap)
	}
	return out, rows.Err()
}
