package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sqlutil"
)

var _ TaskMappingStore = (*SQLiteTaskMappingStore)(nil)

// SQLiteTaskMappingStore implements TaskMappingStore using SQLite.
type SQLiteTaskMappingStore struct {
	db *sql.DB
}

// NewTaskMappingStore creates a new SQLite-backed TaskMappingStore.
func NewTaskMappingStore(db *sql.DB) *SQLiteTaskMappingStore {
	return &SQLiteTaskMappingStore{db: db}
}

func (s *SQLiteTaskMappingStore) Create(ctx context.Context, params CreateTaskMappingParams) (*models.TaskMapping, error) {
	id := sqlutil.NewID()
	now := sqlutil.TimeNow()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO task_mappings (id, external_id, plugin_name, repo_id, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, params.ExternalID, params.PluginName, params.RepoID,
		int(models.TaskMappingStatusPending), now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert task mapping: %w", err)
	}
	return s.Get(ctx, id)
}

func (s *SQLiteTaskMappingStore) GetByExternalID(ctx context.Context, externalID string) (*models.TaskMapping, error) {
	row := s.db.QueryRowContext(ctx, taskMappingSelectSQL+" WHERE external_id = ?", externalID)
	return scanTaskMapping(row)
}

func (s *SQLiteTaskMappingStore) GetBySessionID(ctx context.Context, sessionID string) (*models.TaskMapping, error) {
	row := s.db.QueryRowContext(ctx, taskMappingSelectSQL+" WHERE session_id = ?", sessionID)
	return scanTaskMapping(row)
}

func (s *SQLiteTaskMappingStore) Update(ctx context.Context, id string, params UpdateTaskMappingParams) (*models.TaskMapping, error) {
	now := sqlutil.TimeNow()
	sets := []string{"updated_at = ?"}
	args := []any{now}

	if params.SessionID != nil {
		sets = append(sets, "session_id = ?")
		args = append(args, *params.SessionID)
	}
	if params.Status != nil {
		sets = append(sets, "status = ?")
		args = append(args, int(*params.Status))
	}
	if params.PendingUpdateStatus != nil {
		if *params.PendingUpdateStatus == nil {
			sets = append(sets, "pending_update_status = NULL")
		} else {
			sets = append(sets, "pending_update_status = ?")
			args = append(args, int(**params.PendingUpdateStatus))
		}
	}
	if params.PendingUpdateDetails != nil {
		if *params.PendingUpdateDetails == nil {
			sets = append(sets, "pending_update_details = NULL")
		} else {
			sets = append(sets, "pending_update_details = ?")
			args = append(args, **params.PendingUpdateDetails)
		}
	}

	args = append(args, id)
	query := "UPDATE task_mappings SET " + strings.Join(sets, ", ") + " WHERE id = ?"
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("update task mapping: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, sql.ErrNoRows
	}
	return s.Get(ctx, id)
}

func (s *SQLiteTaskMappingStore) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM task_mappings WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete task mapping: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *SQLiteTaskMappingStore) ListPending(ctx context.Context) ([]*models.TaskMapping, error) {
	rows, err := s.db.QueryContext(ctx, taskMappingSelectSQL+" WHERE pending_update_status IS NOT NULL ORDER BY created_at ASC")
	if err != nil {
		return nil, fmt.Errorf("list pending task mappings: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var mappings []*models.TaskMapping
	for rows.Next() {
		m, err := scanTaskMapping(rows)
		if err != nil {
			return nil, err
		}
		mappings = append(mappings, m)
	}
	return mappings, rows.Err()
}

func (s *SQLiteTaskMappingStore) Get(ctx context.Context, id string) (*models.TaskMapping, error) {
	row := s.db.QueryRowContext(ctx, taskMappingSelectSQL+" WHERE id = ?", id)
	return scanTaskMapping(row)
}

// FailOrphanedMappings marks all Pending/InProgress task mappings as Failed.
// This is a startup cleanup to recover from a previous daemon crash where
// tasks were left in non-terminal states with no driving goroutine.
func (s *SQLiteTaskMappingStore) FailOrphanedMappings(ctx context.Context) (int64, error) {
	now := sqlutil.TimeNow()
	res, err := s.db.ExecContext(ctx,
		`UPDATE task_mappings SET status = ?, updated_at = ? WHERE status IN (?, ?)`,
		int(models.TaskMappingStatusFailed), now,
		int(models.TaskMappingStatusPending), int(models.TaskMappingStatusInProgress),
	)
	if err != nil {
		return 0, fmt.Errorf("fail orphaned task mappings: %w", err)
	}
	return res.RowsAffected()
}

const taskMappingSelectSQL = `SELECT id, external_id, plugin_name, session_id, repo_id,
	status, pending_update_status, pending_update_details, created_at, updated_at
	FROM task_mappings`

func scanTaskMapping(s sqlutil.Scanner) (*models.TaskMapping, error) {
	var m models.TaskMapping
	var status int
	var pendingStatus *int
	var createdAt, updatedAt string
	err := s.Scan(&m.ID, &m.ExternalID, &m.PluginName, &m.SessionID, &m.RepoID,
		&status, &pendingStatus, &m.PendingUpdateDetails,
		&createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	m.Status = models.TaskMappingStatus(status)
	if pendingStatus != nil {
		ps := models.TaskMappingStatus(*pendingStatus)
		m.PendingUpdateStatus = &ps
	}
	m.CreatedAt = sqlutil.ParseTime(createdAt)
	m.UpdatedAt = sqlutil.ParseTime(updatedAt)
	return &m, nil
}
