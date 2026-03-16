package db

import (
	"context"
	"database/sql"
	"fmt"
)

// SQLiteAuditStore implements AuditStore using SQLite.
type SQLiteAuditStore struct {
	db *sql.DB
}

// NewAuditStore creates a new SQLite-backed AuditStore.
func NewAuditStore(db *sql.DB) *SQLiteAuditStore {
	return &SQLiteAuditStore{db: db}
}

func (s *SQLiteAuditStore) Create(ctx context.Context, params CreateAuditParams) (*AuditEntry, error) {
	id := newID()
	now := timeNow()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_log (id, user_id, action, resource, detail, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		id, params.UserID, params.Action, params.Resource, params.Detail, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert audit entry: %w", err)
	}
	return s.get(ctx, id)
}

func (s *SQLiteAuditStore) List(ctx context.Context, opts AuditListOpts) ([]*AuditEntry, error) {
	query := `SELECT id, user_id, action, resource, detail, created_at FROM audit_log`
	var where []string
	var args []any

	if opts.UserID != nil {
		where = append(where, "user_id = ?")
		args = append(args, *opts.UserID)
	}
	if opts.Action != nil {
		where = append(where, "action = ?")
		args = append(args, *opts.Action)
	}

	if len(where) > 0 {
		query += " WHERE " + where[0]
		for _, w := range where[1:] {
			query += " AND " + w
		}
	}

	query += " ORDER BY created_at DESC"

	limit := opts.Limit
	if limit <= 0 {
		limit = 100
	}
	query += fmt.Sprintf(" LIMIT %d", limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list audit entries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var entries []*AuditEntry
	for rows.Next() {
		e, err := scanAuditRows(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

func (s *SQLiteAuditStore) get(ctx context.Context, id string) (*AuditEntry, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, action, resource, detail, created_at FROM audit_log WHERE id = ?`, id)
	var e AuditEntry
	var createdAt string
	err := row.Scan(&e.ID, &e.UserID, &e.Action, &e.Resource, &e.Detail, &createdAt)
	if err != nil {
		return nil, err
	}
	e.CreatedAt = parseTime(createdAt)
	return &e, nil
}

func scanAuditRows(rows *sql.Rows) (*AuditEntry, error) {
	var e AuditEntry
	var createdAt string
	err := rows.Scan(&e.ID, &e.UserID, &e.Action, &e.Resource, &e.Detail, &createdAt)
	if err != nil {
		return nil, err
	}
	e.CreatedAt = parseTime(createdAt)
	return &e, nil
}
