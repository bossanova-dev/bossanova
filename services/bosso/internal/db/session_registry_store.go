package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// SQLiteSessionRegistryStore implements SessionRegistryStore using SQLite.
type SQLiteSessionRegistryStore struct {
	db *sql.DB
}

// NewSessionRegistryStore creates a new SQLite-backed SessionRegistryStore.
func NewSessionRegistryStore(db *sql.DB) *SQLiteSessionRegistryStore {
	return &SQLiteSessionRegistryStore{db: db}
}

func (s *SQLiteSessionRegistryStore) Create(ctx context.Context, params CreateSessionEntryParams) (*SessionEntry, error) {
	now := timeNow()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions_registry (session_id, daemon_id, title, state, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		params.SessionID, params.DaemonID, params.Title, params.State, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert session entry: %w", err)
	}
	return s.Get(ctx, params.SessionID)
}

func (s *SQLiteSessionRegistryStore) Get(ctx context.Context, sessionID string) (*SessionEntry, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT session_id, daemon_id, title, state, created_at, updated_at
		 FROM sessions_registry WHERE session_id = ?`, sessionID)
	return scanSessionEntry(row)
}

func (s *SQLiteSessionRegistryStore) ListByDaemon(ctx context.Context, daemonID string) ([]*SessionEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT session_id, daemon_id, title, state, created_at, updated_at
		 FROM sessions_registry WHERE daemon_id = ? ORDER BY created_at DESC`, daemonID)
	if err != nil {
		return nil, fmt.Errorf("list session entries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var entries []*SessionEntry
	for rows.Next() {
		e, err := scanSessionEntryRows(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

func (s *SQLiteSessionRegistryStore) Update(ctx context.Context, sessionID string, params UpdateSessionEntryParams) (*SessionEntry, error) {
	now := timeNow()
	sets := []string{"updated_at = ?"}
	args := []any{now}

	if params.DaemonID != nil {
		sets = append(sets, "daemon_id = ?")
		args = append(args, *params.DaemonID)
	}
	if params.Title != nil {
		sets = append(sets, "title = ?")
		args = append(args, *params.Title)
	}
	if params.State != nil {
		sets = append(sets, "state = ?")
		args = append(args, *params.State)
	}

	args = append(args, sessionID)
	query := "UPDATE sessions_registry SET " + strings.Join(sets, ", ") + " WHERE session_id = ?"
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("update session entry: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, sql.ErrNoRows
	}
	return s.Get(ctx, sessionID)
}

func (s *SQLiteSessionRegistryStore) Delete(ctx context.Context, sessionID string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions_registry WHERE session_id = ?`, sessionID)
	if err != nil {
		return fmt.Errorf("delete session entry: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func scanSessionEntry(row *sql.Row) (*SessionEntry, error) {
	var e SessionEntry
	var createdAt, updatedAt string
	err := row.Scan(&e.SessionID, &e.DaemonID, &e.Title, &e.State, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	e.CreatedAt = parseTime(createdAt)
	e.UpdatedAt = parseTime(updatedAt)
	return &e, nil
}

func scanSessionEntryRows(rows *sql.Rows) (*SessionEntry, error) {
	var e SessionEntry
	var createdAt, updatedAt string
	err := rows.Scan(&e.SessionID, &e.DaemonID, &e.Title, &e.State, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	e.CreatedAt = parseTime(createdAt)
	e.UpdatedAt = parseTime(updatedAt)
	return &e, nil
}
