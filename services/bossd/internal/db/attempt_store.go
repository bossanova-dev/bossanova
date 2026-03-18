package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sqlutil"
)

var _ AttemptStore = (*SQLiteAttemptStore)(nil)

// SQLiteAttemptStore implements AttemptStore using SQLite.
type SQLiteAttemptStore struct {
	db *sql.DB
}

// NewAttemptStore creates a new SQLite-backed AttemptStore.
func NewAttemptStore(db *sql.DB) *SQLiteAttemptStore {
	return &SQLiteAttemptStore{db: db}
}

func (s *SQLiteAttemptStore) Create(ctx context.Context, params CreateAttemptParams) (*models.Attempt, error) {
	id := sqlutil.NewID()
	now := sqlutil.TimeNow()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO attempts (id, session_id, trigger, result, created_at, updated_at)
		 VALUES (?, ?, ?, 0, ?, ?)`,
		id, params.SessionID, params.Trigger, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert attempt: %w", err)
	}
	return s.Get(ctx, id)
}

func (s *SQLiteAttemptStore) Get(ctx context.Context, id string) (*models.Attempt, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, session_id, trigger, result, error, created_at, updated_at
		 FROM attempts WHERE id = ?`, id)
	return scanAttempt(row)
}

func (s *SQLiteAttemptStore) ListBySession(ctx context.Context, sessionID string) ([]*models.Attempt, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, session_id, trigger, result, error, created_at, updated_at
		 FROM attempts WHERE session_id = ? ORDER BY created_at ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list attempts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var attempts []*models.Attempt
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, a)
	}
	return attempts, rows.Err()
}

func (s *SQLiteAttemptStore) Update(ctx context.Context, id string, params UpdateAttemptParams) (*models.Attempt, error) {
	now := sqlutil.TimeNow()
	sets := []string{"updated_at = ?"}
	args := []any{now}

	if params.Result != nil {
		sets = append(sets, "result = ?")
		args = append(args, *params.Result)
	}
	if params.Error != nil {
		sets = append(sets, "error = ?")
		args = append(args, *params.Error)
	}

	args = append(args, id)
	query := "UPDATE attempts SET " + strings.Join(sets, ", ") + " WHERE id = ?"
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("update attempt: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, sql.ErrNoRows
	}
	return s.Get(ctx, id)
}

func (s *SQLiteAttemptStore) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM attempts WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete attempt: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func scanAttempt(s sqlutil.Scanner) (*models.Attempt, error) {
	var a models.Attempt
	var trigger, result int
	var createdAt, updatedAt string
	err := s.Scan(&a.ID, &a.SessionID, &trigger, &result, &a.Error, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	a.Trigger = models.AttemptTrigger(trigger)
	a.Result = models.AttemptResult(result)
	a.CreatedAt = sqlutil.ParseTime(createdAt)
	a.UpdatedAt = sqlutil.ParseTime(updatedAt)
	return &a, nil
}
