package db

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/recurser/bossalib/contract/cloud/sqlutil"
	"github.com/recurser/bossd/internal/db/sqlcgen"
	"github.com/recurser/bossd/internal/domain"
)

var _ TaskMappingStore = (*SQLiteTaskMappingStore)(nil)

// SQLiteTaskMappingStore implements TaskMappingStore using SQLite.
type SQLiteTaskMappingStore struct{ db *sql.DB }

// NewTaskMappingStore creates a new SQLite-backed TaskMappingStore.
func NewTaskMappingStore(db *sql.DB) *SQLiteTaskMappingStore { return &SQLiteTaskMappingStore{db: db} }

func (s *SQLiteTaskMappingStore) Create(ctx context.Context, params CreateTaskMappingParams) (*domain.TaskMapping, error) {
	id, err := sqlutil.NewID()
	if err != nil {
		return nil, fmt.Errorf("new task mapping id: %w", err)
	}
	now := sqlutil.TimeNow()
	row, err := sqlcgen.New(s.db).CreateTaskMapping(ctx, sqlcgen.CreateTaskMappingParams{ID: id, ExternalID: params.ExternalID, PluginName: params.PluginName, RepoID: params.RepoID, Status: int64(domain.TaskMappingStatusPending), RetryCount: int64(params.RetryCount), CreatedAt: now, UpdatedAt: now})
	if err != nil {
		return nil, fmt.Errorf("insert task mapping: %w", err)
	}
	return taskMappingFromRow(row), nil
}

func (s *SQLiteTaskMappingStore) Get(ctx context.Context, id string) (*domain.TaskMapping, error) {
	row, err := sqlcgen.New(s.db).GetTaskMapping(ctx, id)
	if err != nil {
		return nil, notFound(err)
	}
	return taskMappingFromRow(row), nil
}

func (s *SQLiteTaskMappingStore) GetByExternalID(ctx context.Context, externalID string) (*domain.TaskMapping, error) {
	row, err := sqlcgen.New(s.db).GetTaskMappingByExternalID(ctx, externalID)
	if err != nil {
		return nil, notFound(err)
	}
	return taskMappingFromRow(row), nil
}

func (s *SQLiteTaskMappingStore) GetBySessionID(ctx context.Context, sessionID string) (*domain.TaskMapping, error) {
	row, err := sqlcgen.New(s.db).GetTaskMappingBySessionID(ctx, &sessionID)
	if err != nil {
		return nil, notFound(err)
	}
	return taskMappingFromRow(row), nil
}

func (s *SQLiteTaskMappingStore) Update(ctx context.Context, id string, params UpdateTaskMappingParams) (*domain.TaskMapping, error) {
	arg := sqlcgen.UpdateTaskMappingParams{ID: id, UpdatedAt: sqlutil.TimeNow(), SetSessionID: params.SessionID != nil, SetLastError: params.LastError != nil, SetPendingUpdateStatus: params.PendingUpdateStatus != nil, SetPendingUpdateDetails: params.PendingUpdateDetails != nil}
	if params.SessionID != nil {
		arg.SessionID = *params.SessionID
	}
	if params.Status != nil {
		value := int64(*params.Status)
		arg.Status = &value
	}
	if params.LastError != nil {
		arg.LastError = *params.LastError
	}
	if params.PendingUpdateStatus != nil && *params.PendingUpdateStatus != nil {
		value := int64(**params.PendingUpdateStatus)
		arg.PendingUpdateStatus = &value
	}
	if params.PendingUpdateDetails != nil {
		arg.PendingUpdateDetails = *params.PendingUpdateDetails
	}
	if params.RetryCount != nil {
		value := int64(*params.RetryCount)
		arg.RetryCount = &value
	}
	row, err := sqlcgen.New(s.db).UpdateTaskMapping(ctx, arg)
	if err != nil {
		return nil, fmt.Errorf("update task mapping: %w", notFound(err))
	}
	return taskMappingFromRow(row), nil
}

func (s *SQLiteTaskMappingStore) Delete(ctx context.Context, id string) error {
	n, err := sqlcgen.New(s.db).DeleteTaskMapping(ctx, id)
	if err != nil {
		return fmt.Errorf("delete task mapping: %w", err)
	}
	if n == 0 {
		return notFound(sql.ErrNoRows)
	}
	return nil
}

func (s *SQLiteTaskMappingStore) ListByPendingUpdateStatus(ctx context.Context) ([]*domain.TaskMapping, error) {
	rows, err := sqlcgen.New(s.db).ListTaskMappingsByPendingUpdateStatus(ctx)
	if err != nil {
		return nil, fmt.Errorf("list pending task mappings: %w", err)
	}
	return mapRows(rows, taskMappingFromRow), nil
}

func (s *SQLiteTaskMappingStore) ListByRecentFailure(ctx context.Context, limit int) ([]*domain.TaskMapping, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := sqlcgen.New(s.db).ListTaskMappingsByRecentFailure(ctx, sqlcgen.ListTaskMappingsByRecentFailureParams{Status: int64(domain.TaskMappingStatusFailed), Limit: int64(limit)})
	if err != nil {
		return nil, fmt.Errorf("list recent failed task mappings: %w", err)
	}
	return mapRows(rows, taskMappingFromRow), nil
}

// MarkOrphaned recovers pending and in-progress mappings whose driving goroutines were lost on restart.
func (s *SQLiteTaskMappingStore) MarkOrphaned(ctx context.Context) (int64, error) {
	n, err := sqlcgen.New(s.db).MarkTaskMappingsOrphaned(ctx, sqlcgen.MarkTaskMappingsOrphanedParams{Status: int64(domain.TaskMappingStatusOrphaned), UpdatedAt: sqlutil.TimeNow(), Status_2: int64(domain.TaskMappingStatusPending), Status_3: int64(domain.TaskMappingStatusInProgress)})
	if err != nil {
		return 0, fmt.Errorf("orphan in-progress task mappings: %w", err)
	}
	return n, nil
}

func taskMappingFromRow(row sqlcgen.TaskMapping) *domain.TaskMapping {
	m := &domain.TaskMapping{ID: row.ID, ExternalID: row.ExternalID, PluginName: row.PluginName, SessionID: row.SessionID, RepoID: row.RepoID, Status: domain.TaskMappingStatus(row.Status), LastError: row.LastError, PendingUpdateDetails: row.PendingUpdateDetails, RetryCount: int(row.RetryCount), CreatedAt: textTime(row.CreatedAt), UpdatedAt: textTime(row.UpdatedAt)}
	if row.PendingUpdateStatus != nil {
		value := domain.TaskMappingStatus(*row.PendingUpdateStatus)
		m.PendingUpdateStatus = &value
	}
	return m
}
