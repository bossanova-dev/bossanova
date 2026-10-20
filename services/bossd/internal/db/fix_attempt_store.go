package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sqlutil"
)

var _ FixAttemptStore = (*SQLiteFixAttemptStore)(nil)

// SQLiteFixAttemptStore implements FixAttemptStore using SQLite.
type SQLiteFixAttemptStore struct {
	db *sql.DB
}

// NewFixAttemptStore creates a new SQLite-backed FixAttemptStore.
func NewFixAttemptStore(db *sql.DB) *SQLiteFixAttemptStore {
	return &SQLiteFixAttemptStore{db: db}
}

func (s *SQLiteFixAttemptStore) Create(ctx context.Context, params CreateFixAttemptParams) (*models.FixAttempt, error) {
	id, err := sqlutil.NewID()
	if err != nil {
		return nil, fmt.Errorf("new attempt id: %w", err)
	}
	now := sqlutil.TimeNow()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO fix_attempts (id, session_id, trigger, result, created_at, updated_at)
		 VALUES (?, ?, ?, 0, ?, ?)`,
		id, params.SessionID, params.Trigger, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert attempt: %w", err)
	}
	return s.Get(ctx, id)
}

func (s *SQLiteFixAttemptStore) Get(ctx context.Context, id string) (*models.FixAttempt, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, session_id, trigger, result, error, created_at, updated_at
		 FROM fix_attempts WHERE id = ?`, id)
	return scanFixAttempt(row)
}

func (s *SQLiteFixAttemptStore) ListBySession(ctx context.Context, sessionID string) ([]*models.FixAttempt, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, session_id, trigger, result, error, created_at, updated_at
		 FROM fix_attempts WHERE session_id = ? ORDER BY created_at ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list attempts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var attempts []*models.FixAttempt
	for rows.Next() {
		a, err := scanFixAttempt(rows)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, a)
	}
	return attempts, rows.Err()
}

func (s *SQLiteFixAttemptStore) Update(ctx context.Context, id string, params UpdateFixAttemptParams) (*models.FixAttempt, error) {
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
	// #nosec G202 -- dynamic UPDATE...SET; concatenated fragments are code-literal `col = ?` set-clauses; every value is bound via ?, not user text
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	query := "UPDATE fix_attempts SET " + strings.Join(sets, ", ") + " WHERE id = ?"
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("update attempt: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, sql.ErrNoRows
	}
	return s.Get(ctx, id)
}

func (s *SQLiteFixAttemptStore) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM fix_attempts WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete attempt: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func scanFixAttempt(s sqlutil.Scanner) (*models.FixAttempt, error) {
	var a models.FixAttempt
	var trigger, result int
	var createdAt, updatedAt string
	err := s.Scan(&a.ID, &a.SessionID, &trigger, &result, &a.Error, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	a.Trigger = models.FixAttemptTrigger(trigger)
	a.Result = models.FixAttemptResult(result)
	a.CreatedAt = sqlutil.ParseTime(createdAt)
	a.UpdatedAt = sqlutil.ParseTime(updatedAt)
	return &a, nil
}
