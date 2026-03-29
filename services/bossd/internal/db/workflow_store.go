package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sqlutil"
)

var _ WorkflowStore = (*SQLiteWorkflowStore)(nil)

// SQLiteWorkflowStore implements WorkflowStore using SQLite.
type SQLiteWorkflowStore struct {
	db *sql.DB
}

// NewWorkflowStore creates a new SQLite-backed WorkflowStore.
func NewWorkflowStore(db *sql.DB) *SQLiteWorkflowStore {
	return &SQLiteWorkflowStore{db: db}
}

func (s *SQLiteWorkflowStore) Create(ctx context.Context, params CreateWorkflowParams) (*models.Workflow, error) {
	id := sqlutil.NewID()
	now := sqlutil.TimeNow()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO workflows (id, session_id, repo_id, plan_path, max_legs, start_commit_sha, config_json, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, params.SessionID, params.RepoID, params.PlanPath, params.MaxLegs,
		params.StartCommitSHA, params.ConfigJSON, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert workflow: %w", err)
	}
	return s.Get(ctx, id)
}

func (s *SQLiteWorkflowStore) Get(ctx context.Context, id string) (*models.Workflow, error) {
	row := s.db.QueryRowContext(ctx, workflowSelectSQL+" WHERE id = ?", id)
	return scanWorkflow(row)
}

func (s *SQLiteWorkflowStore) Update(ctx context.Context, id string, params UpdateWorkflowParams) (*models.Workflow, error) {
	now := sqlutil.TimeNow()
	sets := []string{"updated_at = ?"}
	args := []any{now}

	if params.Status != nil {
		sets = append(sets, "status = ?")
		args = append(args, *params.Status)
	}
	if params.CurrentStep != nil {
		sets = append(sets, "current_step = ?")
		args = append(args, *params.CurrentStep)
	}
	if params.FlightLeg != nil {
		sets = append(sets, "flight_leg = ?")
		args = append(args, *params.FlightLeg)
	}
	if params.LastError != nil {
		if *params.LastError == nil {
			sets = append(sets, "last_error = NULL")
		} else {
			sets = append(sets, "last_error = ?")
			args = append(args, **params.LastError)
		}
	}

	args = append(args, id)
	query := "UPDATE workflows SET " + strings.Join(sets, ", ") + " WHERE id = ?"
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("update workflow: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, sql.ErrNoRows
	}
	return s.Get(ctx, id)
}

func (s *SQLiteWorkflowStore) List(ctx context.Context) ([]*models.Workflow, error) {
	rows, err := s.db.QueryContext(ctx, workflowSelectSQL+" ORDER BY created_at DESC")
	if err != nil {
		return nil, fmt.Errorf("list workflows: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var workflows []*models.Workflow
	for rows.Next() {
		w, err := scanWorkflow(rows)
		if err != nil {
			return nil, err
		}
		workflows = append(workflows, w)
	}
	return workflows, rows.Err()
}

// FailOrphaned transitions any workflows in "running" or "pending" state to
// "failed". This should be called on daemon startup to clean up workflows whose
// driving goroutines were lost due to a daemon restart.
func (s *SQLiteWorkflowStore) FailOrphaned(ctx context.Context) (int64, error) {
	now := sqlutil.TimeNow()
	res, err := s.db.ExecContext(ctx,
		`UPDATE workflows SET status = 'failed', last_error = 'daemon restarted', updated_at = ?
		 WHERE status IN ('running', 'pending')`, now)
	if err != nil {
		return 0, fmt.Errorf("fail orphaned workflows: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ListActiveBySessionIDs returns workflows with displayable statuses for the
// given session IDs. This includes active workflows (pending, running, paused)
// and recently-terminated ones (failed, cancelled) so the session list can
// show terminal workflow states. Completed workflows are excluded because
// they represent successful finishes that need no user attention.
func (s *SQLiteWorkflowStore) ListActiveBySessionIDs(ctx context.Context, sessionIDs []string) ([]*models.Workflow, error) {
	if len(sessionIDs) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(sessionIDs))
	args := make([]any, len(sessionIDs))
	for i, id := range sessionIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	query := workflowSelectSQL +
		" WHERE session_id IN (" + strings.Join(placeholders, ",") + ")" +
		" AND status IN ('pending', 'running', 'paused', 'failed', 'cancelled')" +
		" ORDER BY created_at DESC"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list active workflows by session IDs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var workflows []*models.Workflow
	for rows.Next() {
		w, err := scanWorkflow(rows)
		if err != nil {
			return nil, err
		}
		workflows = append(workflows, w)
	}
	return workflows, rows.Err()
}

func (s *SQLiteWorkflowStore) ListByStatus(ctx context.Context, status string) ([]*models.Workflow, error) {
	rows, err := s.db.QueryContext(ctx, workflowSelectSQL+" WHERE status = ? ORDER BY created_at DESC", status)
	if err != nil {
		return nil, fmt.Errorf("list workflows by status: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var workflows []*models.Workflow
	for rows.Next() {
		w, err := scanWorkflow(rows)
		if err != nil {
			return nil, err
		}
		workflows = append(workflows, w)
	}
	return workflows, rows.Err()
}

const workflowSelectSQL = `SELECT id, session_id, repo_id, plan_path, status, current_step,
	flight_leg, max_legs, last_error, start_commit_sha, config_json, created_at, updated_at
	FROM workflows`

func scanWorkflow(s sqlutil.Scanner) (*models.Workflow, error) {
	var w models.Workflow
	var status, currentStep string
	var createdAt, updatedAt string
	err := s.Scan(&w.ID, &w.SessionID, &w.RepoID, &w.PlanPath,
		&status, &currentStep, &w.FlightLeg, &w.MaxLegs,
		&w.LastError, &w.StartCommitSHA, &w.ConfigJSON,
		&createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	w.Status = models.WorkflowStatus(status)
	w.CurrentStep = models.WorkflowStep(currentStep)
	w.CreatedAt = sqlutil.ParseTime(createdAt)
	w.UpdatedAt = sqlutil.ParseTime(updatedAt)
	return &w, nil
}
