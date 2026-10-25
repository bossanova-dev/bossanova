package db

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/recurser/bossalib/contract/cloud/sqlutil"
	"github.com/recurser/bossd/internal/db/sqlcgen"
	"github.com/recurser/bossd/internal/domain"
)

var _ FixAttemptStore = (*SQLiteFixAttemptStore)(nil)

type SQLiteFixAttemptStore struct{ db *sql.DB }

func NewFixAttemptStore(db *sql.DB) *SQLiteFixAttemptStore { return &SQLiteFixAttemptStore{db: db} }
func (s *SQLiteFixAttemptStore) Create(ctx context.Context, params CreateFixAttemptParams) (*domain.FixAttempt, error) {
	id, err := sqlutil.NewID()
	if err != nil {
		return nil, fmt.Errorf("new attempt id: %w", err)
	}
	now := sqlutil.TimeNow()
	row, err := sqlcgen.New(s.db).CreateFixAttempt(ctx, sqlcgen.CreateFixAttemptParams{ID: id, SessionID: params.SessionID, Trigger: int64(params.Trigger), CreatedAt: now, UpdatedAt: now})
	if err != nil {
		return nil, fmt.Errorf("insert attempt: %w", err)
	}
	return fixAttemptFromRow(row), nil
}
func (s *SQLiteFixAttemptStore) Get(ctx context.Context, id string) (*domain.FixAttempt, error) {
	row, err := sqlcgen.New(s.db).GetFixAttempt(ctx, id)
	if err != nil {
		return nil, notFound(err)
	}
	return fixAttemptFromRow(row), nil
}
func (s *SQLiteFixAttemptStore) ListBySession(ctx context.Context, id string) ([]*domain.FixAttempt, error) {
	rows, err := sqlcgen.New(s.db).ListFixAttemptsBySession(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("list attempts: %w", err)
	}
	return mapRows(rows, fixAttemptFromRow), nil
}
func (s *SQLiteFixAttemptStore) Update(ctx context.Context, id string, params UpdateFixAttemptParams) (*domain.FixAttempt, error) {
	arg := sqlcgen.UpdateFixAttemptParams{ID: id, SetError: params.Error != nil, UpdatedAt: sqlutil.TimeNow()}
	if params.Result != nil {
		v := int64(*params.Result)
		arg.Result = &v
	}
	if params.Error != nil {
		arg.Error = *params.Error
	}
	row, err := sqlcgen.New(s.db).UpdateFixAttempt(ctx, arg)
	if err != nil {
		return nil, fmt.Errorf("update attempt: %w", notFound(err))
	}
	return fixAttemptFromRow(row), nil
}
func (s *SQLiteFixAttemptStore) Delete(ctx context.Context, id string) error {
	n, err := sqlcgen.New(s.db).DeleteFixAttempt(ctx, id)
	if err != nil {
		return fmt.Errorf("delete attempt: %w", err)
	}
	if n == 0 {
		return notFound(sql.ErrNoRows)
	}
	return nil
}
func fixAttemptFromRow(row sqlcgen.FixAttempt) *domain.FixAttempt {
	return &domain.FixAttempt{ID: row.ID, SessionID: row.SessionID, Trigger: domain.FixAttemptTrigger(row.Trigger), Result: domain.FixAttemptResult(row.Result), Error: row.Error, CreatedAt: textTime(row.CreatedAt), UpdatedAt: textTime(row.UpdatedAt)}
}
