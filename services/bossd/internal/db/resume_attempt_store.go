package db

import (
	"context"
	"database/sql"
	"fmt"
	"sync"

	"github.com/recurser/bossd/internal/domain"

	"github.com/recurser/bossalib/contract/cloud/sqlutil"
	"github.com/recurser/bossd/internal/db/sqlcgen"
	"github.com/recurser/bossd/internal/domain/resumepolicy"
)

var _ ResumeAttemptStore = (*SQLiteResumeAttemptStore)(nil)

// SQLiteResumeAttemptStore implements ResumeAttemptStore over the
// session_resume_attempts table.
type SQLiteResumeAttemptStore struct {
	db         *sql.DB
	onChangeMu sync.RWMutex
	onChange   func(sessionID string)
}

// SetOnChange sets the callback called after a successful ledger change.
// The callback may read the store and replace itself.
func (s *SQLiteResumeAttemptStore) SetOnChange(fn func(sessionID string)) {
	s.onChangeMu.Lock()
	s.onChange = fn
	s.onChangeMu.Unlock()
}

func (s *SQLiteResumeAttemptStore) notifyChange(sessionID string) {
	s.onChangeMu.RLock()
	fn := s.onChange
	s.onChangeMu.RUnlock()
	if fn != nil {
		fn(sessionID)
	}
}

// NewResumeAttemptStore creates a SQLite-backed ResumeAttemptStore.
func NewResumeAttemptStore(db *sql.DB) *SQLiteResumeAttemptStore {
	return &SQLiteResumeAttemptStore{db: db}
}

// Record appends an attempt to its budget. The attempt number is computed inside
// the INSERT (one past the budget's current maximum), so two writers cannot read
// the same maximum and both claim the next number; the unique index on
// (session_id, budget_key, attempt_number) rejects a duplicate if one ever does.
func (s *SQLiteResumeAttemptStore) RecordResume(ctx context.Context, params RecordResumeAttemptParams) (*domain.ResumeAttempt, error) {
	if params.SessionID == "" {
		return nil, fmt.Errorf("record resume attempt: session ID required")
	}
	if params.BudgetKey == "" {
		return nil, fmt.Errorf("record resume attempt: budget key required")
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
	row, err := sqlcgen.New(s.db).RecordResumeAttempt(ctx, sqlcgen.RecordResumeAttemptParams{
		ID: id, SessionID: params.SessionID, BudgetKey: params.BudgetKey, AttemptedAt: sqlutil.FormatTime(params.AttemptedAt), FailureClass: string(class), FailureDetail: params.FailureDetail,
	})
	if err != nil {
		return nil, fmt.Errorf("insert resume attempt: %w", notFound(err))
	}
	attempt := resumeAttemptFromRow(row)
	s.notifyChange(params.SessionID)
	return attempt, nil
}

// ListByBudget returns one budget's attempts, lowest attempt number first.
func (s *SQLiteResumeAttemptStore) ListByBudget(ctx context.Context, sessionID, budgetKey string) ([]*domain.ResumeAttempt, error) {
	rows, err := sqlcgen.New(s.db).ListResumeAttemptsByBudget(ctx, sqlcgen.ListResumeAttemptsByBudgetParams{SessionID: sessionID, BudgetKey: budgetKey})
	if err != nil {
		return nil, fmt.Errorf("list resume attempts: %w", err)
	}
	return mapRows(rows, resumeAttemptFromRow), nil
}

// Delete removes one budget's attempts. Deleting a budget with no attempts is
// a successful no-op (0 rows): a manual resume clears whatever budget exists.
func (s *SQLiteResumeAttemptStore) Delete(ctx context.Context, sessionID, budgetKey string) (int64, error) {
	n, err := sqlcgen.New(s.db).DeleteResumeAttempts(ctx, sqlcgen.DeleteResumeAttemptsParams{SessionID: sessionID, BudgetKey: budgetKey})
	if err != nil {
		return 0, fmt.Errorf("delete resume attempts: %w", err)
	}
	if n > 0 {
		s.notifyChange(sessionID)
	}
	return n, nil
}

func resumeAttemptFromRow(row sqlcgen.SessionResumeAttempt) *domain.ResumeAttempt {
	return &domain.ResumeAttempt{ID: row.ID, SessionID: row.SessionID, BudgetKey: row.BudgetKey, AttemptNumber: int(row.AttemptNumber), AttemptedAt: textTime(row.AttemptedAt), FailureClass: row.FailureClass, FailureDetail: row.FailureDetail, CreatedAt: textTime(row.CreatedAt)}
}

// SetFailure updates a recorded launch after its replacement has actually
// failed. The launch is persisted before spawn with an unknown outcome.
func (s *SQLiteResumeAttemptStore) SetFailure(ctx context.Context, id, failureClass string) error {
	if _, ok := resumepolicy.ParseFailureClass(failureClass); !ok {
		return fmt.Errorf("unknown resume failure class %q", failureClass)
	}
	row, err := sqlcgen.New(s.db).GetResumeAttempt(ctx, id)
	if err != nil {
		return notFound(err)
	}
	if err := sqlcgen.New(s.db).SetResumeAttemptFailure(ctx, sqlcgen.SetResumeAttemptFailureParams{ID: id, FailureClass: failureClass}); err != nil {
		return err
	}
	s.notifyChange(row.SessionID)
	return nil
}
