package db

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/recurser/bossalib/contract/cloud/sqlutil"
	"github.com/recurser/bossd/internal/db/sqlcgen"
	"github.com/recurser/bossd/internal/domain"
)

var _ WorkflowStore = (*SQLiteWorkflowStore)(nil)

// SQLiteWorkflowStore implements WorkflowStore using SQLite.
type SQLiteWorkflowStore struct{ db *sql.DB }

// NewWorkflowStore creates a new SQLite-backed WorkflowStore.
func NewWorkflowStore(db *sql.DB) *SQLiteWorkflowStore { return &SQLiteWorkflowStore{db: db} }

func (s *SQLiteWorkflowStore) Create(ctx context.Context, params CreateWorkflowParams) (*domain.Workflow, error) {
	id, err := sqlutil.NewID()
	if err != nil {
		return nil, fmt.Errorf("new workflow id: %w", err)
	}
	now := sqlutil.TimeNow()
	row, err := sqlcgen.New(s.db).CreateWorkflow(ctx, sqlcgen.CreateWorkflowParams{ID: id, SessionID: params.SessionID, RepoID: params.RepoID, PlanPath: params.PlanPath, MaxLegs: int64(params.MaxLegs), StartCommitSha: params.StartCommitSHA, ConfigJson: params.ConfigJSON, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		return nil, fmt.Errorf("insert workflow: %w", err)
	}
	return workflowFromRow(row), nil
}

func (s *SQLiteWorkflowStore) Get(ctx context.Context, id string) (*domain.Workflow, error) {
	row, err := sqlcgen.New(s.db).GetWorkflow(ctx, id)
	if err != nil {
		return nil, notFound(err)
	}
	return workflowFromRow(row), nil
}

func (s *SQLiteWorkflowStore) Update(ctx context.Context, id string, params UpdateWorkflowParams) (*domain.Workflow, error) {
	arg := sqlcgen.UpdateWorkflowParams{ID: id, Status: params.Status, CurrentStep: params.CurrentStep, SetLastError: params.LastError != nil, UpdatedAt: sqlutil.TimeNow()}
	if params.LastError != nil {
		arg.LastError = *params.LastError
	}
	if params.FlightLeg != nil {
		value := int64(*params.FlightLeg)
		arg.FlightLeg = &value
	}
	row, err := sqlcgen.New(s.db).UpdateWorkflow(ctx, arg)
	if err != nil {
		return nil, fmt.Errorf("update workflow: %w", notFound(err))
	}
	return workflowFromRow(row), nil
}

func (s *SQLiteWorkflowStore) List(ctx context.Context) ([]*domain.Workflow, error) {
	rows, err := sqlcgen.New(s.db).ListWorkflows(ctx)
	if err != nil {
		return nil, fmt.Errorf("list workflows: %w", err)
	}
	return mapRows(rows, workflowFromRow), nil
}

func (s *SQLiteWorkflowStore) ListByStatus(ctx context.Context, status string) ([]*domain.Workflow, error) {
	rows, err := sqlcgen.New(s.db).ListWorkflowsByStatus(ctx, status)
	if err != nil {
		return nil, fmt.Errorf("list workflows by status: %w", err)
	}
	return mapRows(rows, workflowFromRow), nil
}

// ListByActiveSessionIDs excludes terminal workflows from display-status computation.
func (s *SQLiteWorkflowStore) ListByActiveSessionIDs(ctx context.Context, sessionIDs []string) ([]*domain.Workflow, error) {
	if len(sessionIDs) == 0 {
		return nil, nil
	}
	rows, err := sqlcgen.New(s.db).ListWorkflowsByActiveSessionIDs(ctx, sessionIDs)
	if err != nil {
		return nil, fmt.Errorf("list active workflows by session IDs: %w", err)
	}
	return mapRows(rows, workflowFromRow), nil
}

// MarkFailed cleans up pending and running workflows after a daemon restart.
func (s *SQLiteWorkflowStore) MarkFailed(ctx context.Context) (int64, error) {
	n, err := sqlcgen.New(s.db).MarkWorkflowsFailed(ctx, sqlutil.TimeNow())
	if err != nil {
		return 0, fmt.Errorf("fail orphaned workflows: %w", err)
	}
	return n, nil
}

func workflowFromRow(row sqlcgen.Workflow) *domain.Workflow {
	return &domain.Workflow{ID: row.ID, SessionID: row.SessionID, RepoID: row.RepoID, PlanPath: row.PlanPath, Status: domain.WorkflowStatus(row.Status), CurrentStep: domain.WorkflowStep(row.CurrentStep), FlightLeg: int(row.FlightLeg), MaxLegs: int(row.MaxLegs), LastError: row.LastError, StartCommitSHA: row.StartCommitSha, ConfigJSON: row.ConfigJson, CreatedAt: textTime(row.CreatedAt), UpdatedAt: textTime(row.UpdatedAt)}
}
