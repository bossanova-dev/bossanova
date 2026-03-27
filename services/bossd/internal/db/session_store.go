package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sqlutil"
)

var _ SessionStore = (*SQLiteSessionStore)(nil)

// SQLiteSessionStore implements SessionStore using SQLite.
type SQLiteSessionStore struct {
	db *sql.DB
}

// NewSessionStore creates a new SQLite-backed SessionStore.
func NewSessionStore(db *sql.DB) *SQLiteSessionStore {
	return &SQLiteSessionStore{db: db}
}

func (s *SQLiteSessionStore) Create(ctx context.Context, params CreateSessionParams) (*models.Session, error) {
	id := sqlutil.NewID()
	now := sqlutil.TimeNow()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, repo_id, title, plan, worktree_path, branch_name, base_branch, state, pr_number, pr_url, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, params.RepoID, params.Title, params.Plan,
		params.WorktreePath, params.BranchName, params.BaseBranch,
		int(machine.CreatingWorktree), params.PRNumber, params.PRURL, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert session: %w", err)
	}
	return s.Get(ctx, id)
}

func (s *SQLiteSessionStore) Get(ctx context.Context, id string) (*models.Session, error) {
	row := s.db.QueryRowContext(ctx, sessionSelectSQL+" WHERE s.id = ?", id)
	return scanSession(row)
}

func (s *SQLiteSessionStore) List(ctx context.Context, repoID string) ([]*models.Session, error) {
	if repoID == "" {
		query := sessionSelectSQL + " ORDER BY s.created_at DESC"
		return s.querySessionList(ctx, query)
	}
	query := sessionSelectSQL + " WHERE s.repo_id = ? ORDER BY s.created_at DESC"
	return s.querySessionList(ctx, query, repoID)
}

func (s *SQLiteSessionStore) ListActive(ctx context.Context, repoID string) ([]*models.Session, error) {
	if repoID == "" {
		query := sessionSelectSQL + " WHERE s.archived_at IS NULL ORDER BY s.created_at DESC"
		return s.querySessionList(ctx, query)
	}
	query := sessionSelectSQL + " WHERE s.repo_id = ? AND s.archived_at IS NULL ORDER BY s.created_at DESC"
	return s.querySessionList(ctx, query, repoID)
}

func (s *SQLiteSessionStore) ListArchived(ctx context.Context, repoID string) ([]*models.Session, error) {
	if repoID == "" {
		query := sessionSelectSQL + " WHERE s.archived_at IS NOT NULL ORDER BY s.created_at DESC"
		return s.querySessionList(ctx, query)
	}
	query := sessionSelectSQL + " WHERE s.repo_id = ? AND s.archived_at IS NOT NULL ORDER BY s.created_at DESC"
	return s.querySessionList(ctx, query, repoID)
}

func (s *SQLiteSessionStore) Update(ctx context.Context, id string, params UpdateSessionParams) (*models.Session, error) {
	now := sqlutil.TimeNow()
	sets := []string{"updated_at = ?"}
	args := []any{now}

	if params.State != nil {
		sets = append(sets, "state = ?")
		args = append(args, *params.State)
	}
	if params.WorktreePath != nil {
		sets = append(sets, "worktree_path = ?")
		args = append(args, *params.WorktreePath)
	}
	if params.BranchName != nil {
		sets = append(sets, "branch_name = ?")
		args = append(args, *params.BranchName)
	}
	if params.ClaudeSessionID != nil {
		sets = append(sets, "claude_session_id = ?")
		args = append(args, *params.ClaudeSessionID)
	}
	if params.PRNumber != nil {
		sets = append(sets, "pr_number = ?")
		args = append(args, *params.PRNumber)
	}
	if params.PRURL != nil {
		sets = append(sets, "pr_url = ?")
		args = append(args, *params.PRURL)
	}
	if params.LastCheckState != nil {
		sets = append(sets, "last_check_state = ?")
		args = append(args, *params.LastCheckState)
	}
	if params.AutomationEnabled != nil {
		sets = append(sets, "automation_enabled = ?")
		args = append(args, sqlutil.BoolToInt(*params.AutomationEnabled))
	}
	if params.AttemptCount != nil {
		sets = append(sets, "attempt_count = ?")
		args = append(args, *params.AttemptCount)
	}
	if params.BlockedReason != nil {
		sets = append(sets, "blocked_reason = ?")
		args = append(args, *params.BlockedReason)
	}
	if params.ArchivedAt != nil {
		sets = append(sets, "archived_at = ?")
		args = append(args, *params.ArchivedAt)
	}

	args = append(args, id)
	query := "UPDATE sessions SET " + strings.Join(sets, ", ") + " WHERE id = ?"
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("update session: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, sql.ErrNoRows
	}
	return s.Get(ctx, id)
}

func (s *SQLiteSessionStore) Archive(ctx context.Context, id string) error {
	now := sqlutil.TimeNow()
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET archived_at = ?, updated_at = ? WHERE id = ? AND archived_at IS NULL`,
		now, now, id)
	if err != nil {
		return fmt.Errorf("archive session: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *SQLiteSessionStore) Resurrect(ctx context.Context, id string) error {
	now := sqlutil.TimeNow()
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET archived_at = NULL, updated_at = ? WHERE id = ? AND archived_at IS NOT NULL`,
		now, id)
	if err != nil {
		return fmt.Errorf("resurrect session: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// AdvanceOrphanedSessions moves sessions stuck in ImplementingPlan to
// AwaitingChecks when there are no running workflows for them. This cleans
// up sessions whose driving autopilot goroutines were lost during a daemon
// restart.
func (s *SQLiteSessionStore) AdvanceOrphanedSessions(ctx context.Context) (int64, error) {
	now := sqlutil.TimeNow()
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET state = ?, updated_at = ?
		 WHERE state = ?
		   AND NOT EXISTS (
		       SELECT 1 FROM workflows
		       WHERE workflows.session_id = sessions.id
		         AND workflows.status IN ('running', 'pending')
		   )`,
		int(machine.AwaitingChecks), now, int(machine.ImplementingPlan))
	if err != nil {
		return 0, fmt.Errorf("advance orphaned sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (s *SQLiteSessionStore) Delete(ctx context.Context, id string) error {
	// Use a transaction to ensure atomic cleanup of all dependent records.
	// We don't rely on ON DELETE CASCADE because the PRAGMA foreign_keys=ON
	// setting is per-connection and may not persist if database/sql recycles
	// the connection.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `UPDATE task_mappings SET session_id = NULL WHERE session_id = ?`, id); err != nil {
		return fmt.Errorf("detach task mappings: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM workflows WHERE session_id = ?`, id); err != nil {
		return fmt.Errorf("delete workflows: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM claude_chats WHERE session_id = ?`, id); err != nil {
		return fmt.Errorf("delete claude chats: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM attempts WHERE session_id = ?`, id); err != nil {
		return fmt.Errorf("delete attempts: %w", err)
	}

	res, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

func (s *SQLiteSessionStore) querySessionList(ctx context.Context, query string, args ...any) ([]*models.Session, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var sessions []*models.Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, sess)
	}
	return sessions, rows.Err()
}

const sessionSelectSQL = `SELECT s.id, s.repo_id, s.title, s.plan, s.worktree_path, s.branch_name, s.base_branch,
	s.state, s.claude_session_id, s.pr_number, s.pr_url, s.last_check_state,
	s.automation_enabled, s.attempt_count, s.blocked_reason, s.archived_at, s.created_at, s.updated_at
	FROM sessions s`

func scanSession(s sqlutil.Scanner) (*models.Session, error) {
	var sess models.Session
	var state, lastCheckState, automationEnabled int
	var archivedAt, createdAt, updatedAt *string
	err := s.Scan(&sess.ID, &sess.RepoID, &sess.Title, &sess.Plan,
		&sess.WorktreePath, &sess.BranchName, &sess.BaseBranch,
		&state, &sess.ClaudeSessionID, &sess.PRNumber, &sess.PRURL,
		&lastCheckState, &automationEnabled, &sess.AttemptCount,
		&sess.BlockedReason, &archivedAt, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	sess.State = machine.State(state)
	sess.LastCheckState = machine.CheckState(lastCheckState)
	sess.AutomationEnabled = automationEnabled != 0
	if archivedAt != nil {
		t := sqlutil.ParseTime(*archivedAt)
		sess.ArchivedAt = &t
	}
	if createdAt != nil {
		sess.CreatedAt = sqlutil.ParseTime(*createdAt)
	}
	if updatedAt != nil {
		sess.UpdatedAt = sqlutil.ParseTime(*updatedAt)
	}
	return &sess, nil
}
