package db

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/resumepolicy"
	"github.com/recurser/bossalib/sqlutil"
)

var _ ResumeAttemptStore = (*SQLiteResumeAttemptStore)(nil)

// SQLiteResumeAttemptStore implements ResumeAttemptStore over the
// session_resume_attempts table.
type SQLiteResumeAttemptStore struct {
	db *sql.DB
}

// NewResumeAttemptStore creates a SQLite-backed ResumeAttemptStore.
func NewResumeAttemptStore(db *sql.DB) *SQLiteResumeAttemptStore {
	return &SQLiteResumeAttemptStore{db: db}
}

const resumeAttemptColumns = `id, session_id, run_key, attempt_number, attempted_at, failure_class, failure_detail, created_at`

// Record appends an attempt to its run. The attempt number is computed inside
// the INSERT (one past the run's current maximum), so two writers cannot read
// the same maximum and both claim the next number; the unique index on
// (session_id, run_key, attempt_number) rejects a duplicate if one ever does.
func (s *SQLiteResumeAttemptStore) Record(ctx context.Context, params RecordResumeAttemptParams) (*models.ResumeAttempt, error) {
	if params.SessionID == "" {
		return nil, fmt.Errorf("record resume attempt: session ID required")
	}
	if params.RunKey == "" {
		return nil, fmt.Errorf("record resume attempt: run key required")
	}
	class := resumepolicy.FailureUnknown
	if params.FailureClass != "" {
		parsed, ok := resumepolicy.ParseFailureClass(params.FailureClass)
		if !ok {
			return nil, fmt.Errorf("record resume attempt: unknown failure class %q", params.FailureClass)
		}
		class = parsed
	}
	if params.AttemptedAt.IsZero() {
		return nil, fmt.Errorf("record resume attempt: attempted-at time required")
	}
	id, err := sqlutil.NewID()
	if err != nil {
		return nil, fmt.Errorf("new resume attempt id: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO session_resume_attempts
		   (id, session_id, run_key, attempt_number, attempted_at, failure_class, failure_detail)
		 SELECT ?, ?, ?, COALESCE(MAX(attempt_number), 0) + 1, ?, ?, ?
		   FROM session_resume_attempts
		  WHERE session_id = ? AND run_key = ?`,
		id, params.SessionID, params.RunKey, sqlutil.FormatTime(params.AttemptedAt),
		string(class), params.FailureDetail, params.SessionID, params.RunKey,
	)
	if err != nil {
		return nil, fmt.Errorf("insert resume attempt: %w", err)
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+resumeAttemptColumns+` FROM session_resume_attempts WHERE id = ?`, id)
	return scanResumeAttempt(row)
}

// ListForRun returns one run's attempts, lowest attempt number first.
func (s *SQLiteResumeAttemptStore) ListForRun(ctx context.Context, sessionID, runKey string) ([]*models.ResumeAttempt, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+resumeAttemptColumns+` FROM session_resume_attempts
		  WHERE session_id = ? AND run_key = ?
		  ORDER BY attempt_number ASC`, sessionID, runKey)
	if err != nil {
		return nil, fmt.Errorf("list resume attempts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*models.ResumeAttempt
	for rows.Next() {
		a, err := scanResumeAttempt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteForRun removes one run's attempts. Deleting a run with no attempts is
// a successful no-op (0 rows): a manual resume clears whatever budget exists.
func (s *SQLiteResumeAttemptStore) DeleteForRun(ctx context.Context, sessionID, runKey string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM session_resume_attempts WHERE session_id = ? AND run_key = ?`, sessionID, runKey)
	if err != nil {
		return 0, fmt.Errorf("delete resume attempts: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return n, nil
}

func scanResumeAttempt(s sqlutil.Scanner) (*models.ResumeAttempt, error) {
	var a models.ResumeAttempt
	var attemptedAt, createdAt string
	if err := s.Scan(&a.ID, &a.SessionID, &a.RunKey, &a.AttemptNumber, &attemptedAt,
		&a.FailureClass, &a.FailureDetail, &createdAt); err != nil {
		return nil, err
	}
	a.AttemptedAt = sqlutil.ParseTime(attemptedAt)
	a.CreatedAt = sqlutil.ParseTime(createdAt)
	return &a, nil
}
