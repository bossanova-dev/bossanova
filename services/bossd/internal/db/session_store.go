package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

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

// UpdateStateConditional performs the idempotency-gate conditional UPDATE
// used by FinalizeSession: exactly one row is affected only if the session
// was still in expectedState when we arrived.
func (s *SQLiteSessionStore) UpdateStateConditional(ctx context.Context, id string, newState, expectedState int) (bool, error) {
	now := sqlutil.TimeNow()
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET state = ?, updated_at = ? WHERE id = ? AND state = ?`,
		newState, now, id, expectedState)
	if err != nil {
		return false, fmt.Errorf("update state conditional: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n == 1, nil
}

// OrphanHeadlessRun atomically records the daemon-restart marker with the
// ImplementingPlan→Orphaned transition. Keeping both writes in one UPDATE
// avoids leaving a markerless Orphaned row that an auto-resume sweep cannot
// identify safely.
func (s *SQLiteSessionStore) OrphanHeadlessRun(ctx context.Context, id, reason string) (bool, error) {
	now := sqlutil.TimeNow()
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET state = ?, blocked_reason = ?, updated_at = ? WHERE id = ? AND state = ?`,
		machine.Orphaned, reason, now, id, machine.ImplementingPlan)
	if err != nil {
		return false, fmt.Errorf("orphan headless run: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n == 1, nil
}

// ClaimUnarchivedOrphan atomically advances an orphan only while it remains
// unarchived with the expected daemon-restart marker, so a user archive or
// marker change between the sweep list and this claim prevents a new headless
// run from starting.
func (s *SQLiteSessionStore) ClaimUnarchivedOrphan(ctx context.Context, id, reason string) (bool, error) {
	now := sqlutil.TimeNow()
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET state = ?, updated_at = ? WHERE id = ? AND state = ? AND archived_at IS NULL AND blocked_reason = ?`,
		machine.ImplementingPlan, now, id, machine.Orphaned, reason)
	if err != nil {
		return false, fmt.Errorf("claim unarchived orphan: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n == 1, nil
}

// ResurrectToState clears archived_at and installs the live state in ONE
// conditional UPDATE: `SET archived_at = NULL, state = ? WHERE id = ? AND
// archived_at IS NOT NULL`. ResurrectSession (session/lifecycle.go) is the only
// caller.
//
// The single statement is the whole point. Clearing archived_at and writing the
// state as two writes leaves the row wearing {archived_at NULL, state Merged}
// in between — byte-for-byte the predicate archiveMergedButUnarchived
// (session/reconcile.go) selects on — so a reconcile tick landing in that window
// dispatches a detached archive that can delete the worktree the resurrect just
// recreated. SQLite applies this UPDATE atomically, so that intermediate shape
// is never observable.
//
// The `archived_at IS NOT NULL` guard is inherited from the Resurrect method
// this replaces: without it the write would silently succeed against a live
// session and overwrite its state. Returns true when exactly one row moved;
// false means the row was no longer archived when this statement ran (a lost
// race), which the caller must treat as a failed resurrect rather than
// proceeding.
func (s *SQLiteSessionStore) ResurrectToState(ctx context.Context, id string, newState int) (bool, error) {
	now := sqlutil.TimeNow()
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET archived_at = NULL, state = ?, updated_at = ? WHERE id = ? AND archived_at IS NOT NULL`,
		newState, now, id)
	if err != nil {
		return false, fmt.Errorf("resurrect session to state: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n == 1, nil
}

// RollbackFailedResurrect undoes a resurrect whose agent start failed, also in
// one statement: `SET archived_at = ?, state = ? WHERE id = ? AND archived_at
// IS NULL AND state = ?`. ResurrectSession is the only caller.
//
// archivedAt is the timestamp the row wore before the resurrect, so the row
// returns exactly where the failed attempt found it instead of having its trash
// age silently reset. restoreState is that same pre-resurrect state;
// expectState is the state ResurrectToState wrote (ImplementingPlan) — the two
// have opposite meanings and swapping them would silently disable the rollback.
// Re-archiving in two writes would transiently recreate the {archived_at NULL,
// state Merged} shape ResurrectToState exists to avoid, which is why this is one
// statement too.
//
// Returns true when exactly one row was restored. A false return is NOT a
// harmless no-op: it means a concurrent writer moved the row off the shape this
// call wrote, leaving the session live, agent-less and un-retryable, so the
// caller logs the state it actually observed rather than swallowing it.
func (s *SQLiteSessionStore) RollbackFailedResurrect(ctx context.Context, id string, archivedAt time.Time, restoreState, expectState int) (bool, error) {
	now := sqlutil.TimeNow()
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET archived_at = ?, state = ?, updated_at = ? WHERE id = ? AND archived_at IS NULL AND state = ?`,
		sqlutil.FormatTime(archivedAt), restoreState, now, id, expectState)
	if err != nil {
		return false, fmt.Errorf("rollback failed resurrect: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n == 1, nil
}

// CommitOrphanResume records a spawned replacement agent only while the
// original orphan claim remains exclusively owned by this handoff. It does not
// call Get after the UPDATE: an affected-row result is unambiguous even when a
// later read would fail, and the predicates prevent an old completion or user
// archive from being overwritten.
func (s *SQLiteSessionStore) CommitOrphanResume(ctx context.Context, id, reason string, priorAgentSession *string, newAgentSessionID string) (bool, error) {
	now := sqlutil.TimeNow()
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET agent_session_id = ?, blocked_reason = NULL, updated_at = ?
		 WHERE id = ? AND state = ? AND archived_at IS NULL AND blocked_reason = ? AND agent_session_id IS ?`,
		newAgentSessionID, now, id, machine.ImplementingPlan, reason, priorAgentSession)
	if err != nil {
		return false, fmt.Errorf("commit orphan resume: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n == 1, nil
}

// ReleaseOrphanResumeClaim returns a claimed orphan to its original candidate
// state when spawning failed before a replacement agent was persisted. The
// expected marker and prior agent ID prevent this cleanup from overwriting a
// winner that changed the row after the claim. RetrySession intentionally
// clears blocked_reason without starting a replacement; accepting a NULL marker
// in that one claimed shape releases the dead run while preserving Retry's
// markerless state.
func (s *SQLiteSessionStore) ReleaseOrphanResumeClaim(ctx context.Context, id, reason string, priorAgentSession *string) (bool, error) {
	now := sqlutil.TimeNow()
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET state = ?, updated_at = ?
		 WHERE id = ? AND state = ? AND archived_at IS NULL AND (blocked_reason = ? OR blocked_reason IS NULL) AND agent_session_id IS ?`,
		machine.Orphaned, now, id, machine.ImplementingPlan, reason, priorAgentSession)
	if err != nil {
		return false, fmt.Errorf("release orphan resume claim: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n == 1, nil
}

// ReparkOrphanResume restores the pre-resume orphan shape only while this
// handoff still owns the newly persisted agent ID. A completion, archive, or
// manual intervention that won after the commit makes this a harmless no-op.
func (s *SQLiteSessionStore) ReparkOrphanResume(ctx context.Context, id, reason string, priorAgentSession *string, newAgentSessionID string) (bool, error) {
	now := sqlutil.TimeNow()
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET state = ?, agent_session_id = ?, blocked_reason = ?, updated_at = ?
		 WHERE id = ? AND state = ? AND archived_at IS NULL AND blocked_reason IS NULL AND agent_session_id = ?`,
		machine.Orphaned, priorAgentSession, reason, now, id, machine.ImplementingPlan, newAgentSessionID)
	if err != nil {
		return false, fmt.Errorf("repark orphan resume: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n == 1, nil
}

// UpdateStateConditionalFrom is the from-set variant of UpdateStateConditional:
// it transitions the session to newState only when its current state is one of
// expectedStates. It backs the broadened stranded-cron reap (BOS-333), whose
// finalize entry must accept several interrupted states, while the Stop-hook
// path keeps the single-state UpdateStateConditional. An empty expectedStates
// slice is a no-op (returns false without querying) so the idempotency guard
// degenerates safely.
func (s *SQLiteSessionStore) UpdateStateConditionalFrom(ctx context.Context, id string, newState int, expectedStates []int) (bool, error) {
	if len(expectedStates) == 0 {
		return false, nil
	}
	placeholders := make([]string, len(expectedStates))
	args := make([]any, 0, len(expectedStates)+3)
	now := sqlutil.TimeNow()
	args = append(args, newState, now, id)
	for i, st := range expectedStates {
		placeholders[i] = "?"
		args = append(args, st)
	}
	// #nosec G202 -- `... IN (` + static `?` placeholder builder + `)`; states bound via ?, not user text
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET state = ?, updated_at = ? WHERE id = ? AND state IN (`+strings.Join(placeholders, ", ")+`)`,
		args...)
	if err != nil {
		return false, fmt.Errorf("update state conditional from: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n > 0, nil
}

func (s *SQLiteSessionStore) Create(ctx context.Context, params CreateSessionParams) (*models.Session, error) {
	id, err := sqlutil.NewID()
	if err != nil {
		return nil, fmt.Errorf("new session id: %w", err)
	}
	now := sqlutil.TimeNow()
	agentName := params.AgentName
	if agentName == "" {
		agentName = "claude"
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, repo_id, title, plan, worktree_path, branch_name, base_branch, state, agent_name, model, account_id, pr_number, pr_url, tracker_id, tracker_url, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, params.RepoID, params.Title, params.Plan,
		params.WorktreePath, params.BranchName, params.BaseBranch,
		int(machine.CreatingWorktree), agentName, params.Model, params.AccountID, params.PRNumber, params.PRURL,
		params.TrackerID, params.TrackerURL, now, now,
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

// ListByState returns every session currently in the given state, regardless
// of repo or archived status. Used by the daemon-startup recovery path to
// find sessions left in transient states (e.g. Finalizing) when the daemon
// crashed.
func (s *SQLiteSessionStore) ListByState(ctx context.Context, state int) ([]*models.Session, error) {
	query := sessionSelectSQL + " WHERE s.state = ? ORDER BY s.updated_at DESC"
	return s.querySessionList(ctx, query, state)
}

// ListByStates returns every session whose state is one of the given states,
// regardless of repo or archived status. It is the multi-state variant of
// ListByState used by the broadened stranded-cron reap (BOS-333) to find
// unattended sessions interrupted mid-transition. An empty states slice
// returns nil without querying.
func (s *SQLiteSessionStore) ListByStates(ctx context.Context, states []int) ([]*models.Session, error) {
	if len(states) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(states))
	args := make([]any, len(states))
	for i, st := range states {
		placeholders[i] = "?"
		args[i] = st
	}
	query := sessionSelectSQL + " WHERE s.state IN (" + strings.Join(placeholders, ", ") + ") ORDER BY s.updated_at DESC"
	return s.querySessionList(ctx, query, args...)
}

// ListTmuxSessionNames returns every non-empty sessions.tmux_session_name in
// the database, across all repos and including archived rows.
//
// It exists for the orphaned-tmux reaper (BOS-846), which needs a whitelist of
// every pane name any row can account for: every other SessionStore list is
// repo-scoped, so there was no global read. Archived rows are deliberately
// included — BOS-428 routes `--detach` runs through durable tmux so they
// outlive the daemon, and an archived-but-still-running unattended run is
// exactly the pane a repo-scoped or active-only read would leave reapable.
//
// This leg is near-dead by design: tmux.SessionName has no production callers
// and the only non-test writer clears the column, so in practice the result is
// usually empty. It is kept so a legacy row's pane is not reaped; an empty leg
// costs nothing and a missing one costs a live pane.
func (s *SQLiteSessionStore) ListTmuxSessionNames(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT tmux_session_name FROM sessions
		 WHERE tmux_session_name IS NOT NULL AND tmux_session_name != ''`)
	if err != nil {
		return nil, fmt.Errorf("list session tmux names: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan session tmux name: %w", err)
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

func (s *SQLiteSessionStore) ListActive(ctx context.Context, repoID string) ([]*models.Session, error) {
	if repoID == "" {
		query := sessionSelectSQL + " WHERE s.archived_at IS NULL ORDER BY s.created_at DESC"
		return s.querySessionList(ctx, query)
	}
	query := sessionSelectSQL + " WHERE s.repo_id = ? AND s.archived_at IS NULL ORDER BY s.created_at DESC"
	return s.querySessionList(ctx, query, repoID)
}

// ListActiveWithRepo returns active (non-archived) sessions in one query
// that joins repos to populate RepoDisplayName. Eliminates the per-session
// Repo.Get round trip that the previous list-then-loop approach required.
func (s *SQLiteSessionStore) ListActiveWithRepo(ctx context.Context, repoID string) ([]*SessionWithRepo, error) {
	query := sessionSelectWithRepoSQL + " WHERE s.archived_at IS NULL"
	args := []any{}
	if repoID != "" {
		query += " AND s.repo_id = ?"
		args = append(args, repoID)
	}
	query += " ORDER BY s.created_at DESC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list sessions with repo: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return collectSessionsWithRepo(rows)
}

// ListWithRepo returns both active and archived sessions with their repo
// display name populated. Used by the upstream sync adapter so archive
// transitions reach bosso (instead of an archived session just dropping out
// of the snapshot, which looks identical to deletion to the receiver).
func (s *SQLiteSessionStore) ListWithRepo(ctx context.Context, repoID string) ([]*SessionWithRepo, error) {
	query := sessionSelectWithRepoSQL
	args := []any{}
	if repoID != "" {
		query += " WHERE s.repo_id = ?"
		args = append(args, repoID)
	}
	query += " ORDER BY s.created_at DESC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list sessions with repo: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return collectSessionsWithRepo(rows)
}

func (s *SQLiteSessionStore) ListByRepoAndPR(ctx context.Context, repoID string, prNumber int) ([]*SessionWithRepo, error) {
	query := sessionSelectWithRepoSQL + " WHERE s.repo_id = ? AND s.pr_number = ? AND s.archived_at IS NULL ORDER BY s.created_at DESC"
	rows, err := s.db.QueryContext(ctx, query, repoID, prNumber)
	if err != nil {
		return nil, fmt.Errorf("list sessions by repo and PR: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return collectSessionsWithRepo(rows)
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

	if params.Title != nil {
		sets = append(sets, "title = ?")
		args = append(args, *params.Title)
	}
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
	if params.AgentSessionID != nil {
		sets = append(sets, "agent_session_id = ?")
		args = append(args, *params.AgentSessionID)
	}
	if params.PRNumber != nil {
		sets = append(sets, "pr_number = ?")
		args = append(args, *params.PRNumber)
	}
	if params.PRURL != nil {
		sets = append(sets, "pr_url = ?")
		args = append(args, *params.PRURL)
	}
	if params.TrackerID != nil {
		sets = append(sets, "tracker_id = ?")
		args = append(args, *params.TrackerID)
	}
	if params.TrackerURL != nil {
		sets = append(sets, "tracker_url = ?")
		args = append(args, *params.TrackerURL)
	}
	if params.TmuxSessionName != nil {
		sets = append(sets, "tmux_session_name = ?")
		args = append(args, *params.TmuxSessionName)
	}
	if params.LastCheckState != nil {
		sets = append(sets, "last_check_state = ?")
		args = append(args, *params.LastCheckState)
	}
	if params.LastObservedReviewState != nil {
		sets = append(sets, "last_observed_review_state = ?")
		args = append(args, *params.LastObservedReviewState)
	}
	if params.IsAutomationEnabled != nil {
		sets = append(sets, "is_automation_enabled = ?")
		args = append(args, sqlutil.BoolToInt(*params.IsAutomationEnabled))
	}
	if params.AttemptCount != nil {
		sets = append(sets, "attempt_count = ?")
		args = append(args, *params.AttemptCount)
	}
	if params.BlockedReason != nil {
		sets = append(sets, "blocked_reason = ?")
		args = append(args, *params.BlockedReason)
	}
	if params.LastAttemptHeadSHA != nil {
		sets = append(sets, "last_attempt_head_sha = ?")
		args = append(args, *params.LastAttemptHeadSHA)
	}
	if params.RotationAttemptCount != nil {
		sets = append(sets, "rotation_attempt_count = ?")
		args = append(args, *params.RotationAttemptCount)
	}
	if params.RotationResumeAt != nil {
		sets = append(sets, "rotation_resume_at = ?")
		args = append(args, *params.RotationResumeAt)
	}
	if params.ArchivedAt != nil {
		sets = append(sets, "archived_at = ?")
		args = append(args, *params.ArchivedAt)
	}
	if params.CronJobID != nil {
		sets = append(sets, "cron_job_id = ?")
		args = append(args, *params.CronJobID)
	}
	if params.HookToken != nil {
		sets = append(sets, "hook_token = ?")
		args = append(args, *params.HookToken)
	}
	if params.AccountID != nil {
		sets = append(sets, "account_id = ?")
		args = append(args, *params.AccountID)
	}
	if params.IsTmuxUnattended != nil {
		sets = append(sets, "is_tmux_unattended = ?")
		args = append(args, sqlutil.BoolToInt(*params.IsTmuxUnattended))
	}
	if params.Detach != nil {
		sets = append(sets, "is_detached = ?")
		args = append(args, sqlutil.BoolToInt(*params.Detach))
	}
	if params.IsQuickChat != nil {
		sets = append(sets, "is_quick_chat = ?")
		args = append(args, sqlutil.BoolToInt(*params.IsQuickChat))
	}
	if params.DisplayLabel != nil {
		sets = append(sets, "display_label = ?")
		args = append(args, *params.DisplayLabel)
	}
	if params.DisplayIntent != nil {
		sets = append(sets, "display_intent = ?")
		args = append(args, int(*params.DisplayIntent))
	}
	if params.DisplaySpinner != nil {
		sets = append(sets, "has_display_spinner = ?")
		args = append(args, sqlutil.BoolToInt(*params.DisplaySpinner))
	}
	if params.SetupError != nil {
		sets = append(sets, "setup_error = ?")
		args = append(args, *params.SetupError)
	}

	args = append(args, id)
	// #nosec G202 -- dynamic UPDATE...SET builder; concatenated fragments are code-literal `col = ?` set-clauses; every value is bound via ?, not user text
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
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

// UpdateRepairDiagnostics writes the per-attempt outcome onto the session
// row in a single statement. The repair plugin calls this from its
// deferred cleanup so every attempt — clean or failed — leaves a
// structured trace in SQLite that survives daemon restarts and is
// queryable from the TUI.
//
// last_repair_attempt_count tracks *consecutive failures* rather than
// total attempts: a clean run resets it to 0, a failed run bumps it by
// one. That keeps the TUI's "⚠ repair failed (N×)" hint honest after a
// fail → succeed → fail sequence, where the previous all-attempts
// counter would have rendered "(3×)" when only one failure had actually
// happened since the last clean repair.
func (s *SQLiteSessionStore) UpdateRepairDiagnostics(ctx context.Context, params UpdateRepairDiagnosticsParams) error {
	if params.SessionID == "" {
		return fmt.Errorf("update repair diagnostics: session ID required")
	}
	startedAt := sqlutil.FormatTime(params.StartedAt)
	countExpr := "0"
	if params.RunnerError != "" || params.ExitError != "" {
		countExpr = "last_repair_attempt_count + 1"
	}
	// #nosec G202 -- countExpr is a static literal (`0` / `last_repair_attempt_count + 1`), not user text; all values bound via ?
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions
		 SET last_repair_started_at    = ?,
		     last_repair_runner_error  = ?,
		     last_repair_exit_error    = ?,
		     last_repair_attempt_count = `+countExpr+`,
		     last_repair_head_sha       = ?,
		     last_repair_display_status = ?,
		     last_repair_review_fingerprint = COALESCE(?, last_repair_review_fingerprint),
		     last_repair_blocked_reason = '',
		     last_repair_blocked_at     = NULL
		 WHERE id = ?`,
		startedAt, params.RunnerError, params.ExitError, params.HeadSHA,
		params.DisplayStatus, params.ReviewFingerprint, params.SessionID)
	if err != nil {
		return fmt.Errorf("update repair diagnostics: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// UpdateRepairBlocked records a blocked-refusal outcome: the daemon refused
// to START a repair chat (a FailedPrecondition displace/reclaim refusal)
// rather than running one. It writes last_repair_blocked_reason +
// last_repair_blocked_at ONLY, deliberately leaving last_repair_attempt_count
// and the runner/exit error fields untouched, so a start-refusal never counts
// as a repair failure or feeds the exponential backoff. The next real repair
// outcome (via UpdateRepairDiagnostics) clears the blocked pair.
func (s *SQLiteSessionStore) UpdateRepairBlocked(ctx context.Context, sessionID string, at time.Time, reason string) error {
	if sessionID == "" {
		return fmt.Errorf("update repair blocked: session ID required")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions
		 SET last_repair_blocked_reason = ?,
		     last_repair_blocked_at     = ?
		 WHERE id = ?`,
		reason, sqlutil.FormatTime(at), sessionID)
	if err != nil {
		return fmt.Errorf("update repair blocked: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_check_snapshots WHERE session_id = ?`, id); err != nil {
		return fmt.Errorf("delete check snapshots: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM workflows WHERE session_id = ?`, id); err != nil {
		return fmt.Errorf("delete workflows: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM agent_chats WHERE session_id = ?`, id); err != nil {
		return fmt.Errorf("delete claude chats: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM attempts WHERE session_id = ?`, id); err != nil {
		return fmt.Errorf("delete attempts: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE cron_jobs SET last_run_session_id = NULL WHERE last_run_session_id = ?`, id); err != nil {
		return fmt.Errorf("detach cron job last run session: %w", err)
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
	s.state, s.agent_session_id, s.pr_number, s.pr_url, s.tracker_id, s.tracker_url, s.tmux_session_name,
	s.last_check_state, s.last_observed_review_state, s.is_automation_enabled, s.attempt_count, s.blocked_reason, s.archived_at, s.cron_job_id, s.hook_token, s.is_tmux_unattended, s.is_quick_chat, s.is_detached, s.created_at, s.updated_at,
	s.display_label, s.display_intent, s.has_display_spinner, s.agent_name, s.model,
	s.last_repair_started_at, s.last_repair_runner_error, s.last_repair_exit_error, s.last_repair_attempt_count,
	s.last_repair_head_sha, s.last_repair_display_status, s.last_repair_review_fingerprint, s.setup_error,
	s.last_repair_blocked_reason, s.last_repair_blocked_at, s.last_attempt_head_sha,
	s.rotation_attempt_count, s.rotation_resume_at, s.account_id
	FROM sessions s`

// sessionSelectWithRepoSQL joins sessions with repos so ListActiveWithRepo
// can return display names alongside each session in a single round trip.
// LEFT JOIN keeps sessions whose repo was concurrently deleted — the row
// still appears with an empty display name.
const sessionSelectWithRepoSQL = `SELECT s.id, s.repo_id, s.title, s.plan, s.worktree_path, s.branch_name, s.base_branch,
	s.state, s.agent_session_id, s.pr_number, s.pr_url, s.tracker_id, s.tracker_url, s.tmux_session_name,
	s.last_check_state, s.last_observed_review_state, s.is_automation_enabled, s.attempt_count, s.blocked_reason, s.archived_at, s.cron_job_id, s.hook_token, s.is_tmux_unattended, s.is_quick_chat, s.is_detached, s.created_at, s.updated_at,
	s.display_label, s.display_intent, s.has_display_spinner, s.agent_name, s.model,
	s.last_repair_started_at, s.last_repair_runner_error, s.last_repair_exit_error, s.last_repair_attempt_count,
	s.last_repair_head_sha, s.last_repair_display_status, s.last_repair_review_fingerprint, s.setup_error,
	s.last_repair_blocked_reason, s.last_repair_blocked_at, s.last_attempt_head_sha,
	s.rotation_attempt_count, s.rotation_resume_at, s.account_id,
	COALESCE(r.display_name, ''), COALESCE(r.origin_url, '')
	FROM sessions s LEFT JOIN repos r ON r.id = s.repo_id`

// collectSessionsWithRepo drains rows produced by a sessionSelectWithRepoSQL
// query into SessionWithRepo values. Shared by the List*WithRepo queries, whose
// iteration loops were previously copy-pasted verbatim.
func collectSessionsWithRepo(rows *sql.Rows) ([]*SessionWithRepo, error) {
	var out []*SessionWithRepo
	for rows.Next() {
		sess, repoName, repoOriginURL, err := scanSessionWithRepo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, &SessionWithRepo{Session: sess, RepoDisplayName: repoName, RepoOriginURL: repoOriginURL})
	}
	return out, rows.Err()
}

func scanSessionWithRepo(s sqlutil.Scanner) (*models.Session, string, string, error) {
	var sess models.Session
	var state, lastCheckState, lastObservedReviewState, automationEnabled, tmuxUnattended, quickChat, detach int
	var archivedAt, createdAt, updatedAt *string
	var displayIntent int
	var displaySpinner int
	var lastRepairStartedAt *string
	var lastRepairBlockedAt *string
	var rotationResumeAt *string
	var repoDisplayName, repoOriginURL string
	err := s.Scan(&sess.ID, &sess.RepoID, &sess.Title, &sess.Plan,
		&sess.WorktreePath, &sess.BranchName, &sess.BaseBranch,
		&state, &sess.AgentSessionID, &sess.PRNumber, &sess.PRURL,
		&sess.TrackerID, &sess.TrackerURL, &sess.TmuxSessionName,
		&lastCheckState, &lastObservedReviewState, &automationEnabled, &sess.AttemptCount,
		&sess.BlockedReason, &archivedAt, &sess.CronJobID, &sess.HookToken, &tmuxUnattended, &quickChat, &detach, &createdAt, &updatedAt,
		&sess.DisplayLabel, &displayIntent, &displaySpinner, &sess.AgentName, &sess.Model,
		&lastRepairStartedAt, &sess.LastRepairRunnerError, &sess.LastRepairExitError, &sess.LastRepairAttemptCount,
		&sess.LastRepairHeadSHA, &sess.LastRepairDisplayStatus, &sess.LastRepairReviewFingerprint, &sess.SetupError,
		&sess.LastRepairBlockedReason, &lastRepairBlockedAt, &sess.LastAttemptHeadSHA,
		&sess.RotationAttemptCount, &rotationResumeAt, &sess.AccountID, &repoDisplayName, &repoOriginURL)
	if err != nil {
		return nil, "", "", err
	}
	sess.RotationResumeAt = sqlutil.ParseOptionalTime(rotationResumeAt)
	sess.State = machine.State(state)
	sess.LastCheckState = machine.CheckState(lastCheckState)
	sess.LastObservedReviewState = lastObservedReviewState
	sess.IsAutomationEnabled = automationEnabled != 0
	sess.IsTmuxUnattended = tmuxUnattended != 0
	sess.IsQuickChat = quickChat != 0
	sess.Detach = detach != 0
	sess.DisplayIntent = clampInt32(displayIntent)
	sess.DisplaySpinner = displaySpinner != 0
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
	sess.LastRepairStartedAt = sqlutil.ParseOptionalTime(lastRepairStartedAt)
	sess.LastRepairBlockedAt = sqlutil.ParseOptionalTime(lastRepairBlockedAt)
	return &sess, repoDisplayName, repoOriginURL, nil
}

func scanSession(s sqlutil.Scanner) (*models.Session, error) {
	var sess models.Session
	var state, lastCheckState, lastObservedReviewState, automationEnabled, tmuxUnattended, quickChat, detach int
	var archivedAt, createdAt, updatedAt *string
	var displayIntent int
	var displaySpinner int
	var lastRepairStartedAt *string
	var lastRepairBlockedAt *string
	var rotationResumeAt *string
	err := s.Scan(&sess.ID, &sess.RepoID, &sess.Title, &sess.Plan,
		&sess.WorktreePath, &sess.BranchName, &sess.BaseBranch,
		&state, &sess.AgentSessionID, &sess.PRNumber, &sess.PRURL,
		&sess.TrackerID, &sess.TrackerURL, &sess.TmuxSessionName,
		&lastCheckState, &lastObservedReviewState, &automationEnabled, &sess.AttemptCount,
		&sess.BlockedReason, &archivedAt, &sess.CronJobID, &sess.HookToken, &tmuxUnattended, &quickChat, &detach, &createdAt, &updatedAt,
		&sess.DisplayLabel, &displayIntent, &displaySpinner, &sess.AgentName, &sess.Model,
		&lastRepairStartedAt, &sess.LastRepairRunnerError, &sess.LastRepairExitError, &sess.LastRepairAttemptCount,
		&sess.LastRepairHeadSHA, &sess.LastRepairDisplayStatus, &sess.LastRepairReviewFingerprint, &sess.SetupError,
		&sess.LastRepairBlockedReason, &lastRepairBlockedAt, &sess.LastAttemptHeadSHA,
		&sess.RotationAttemptCount, &rotationResumeAt, &sess.AccountID)
	if err != nil {
		return nil, err
	}
	sess.RotationResumeAt = sqlutil.ParseOptionalTime(rotationResumeAt)
	sess.State = machine.State(state)
	sess.LastCheckState = machine.CheckState(lastCheckState)
	sess.LastObservedReviewState = lastObservedReviewState
	sess.IsAutomationEnabled = automationEnabled != 0
	sess.IsTmuxUnattended = tmuxUnattended != 0
	sess.IsQuickChat = quickChat != 0
	sess.Detach = detach != 0
	sess.DisplayIntent = clampInt32(displayIntent)
	sess.DisplaySpinner = displaySpinner != 0
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
	sess.LastRepairStartedAt = sqlutil.ParseOptionalTime(lastRepairStartedAt)
	sess.LastRepairBlockedAt = sqlutil.ParseOptionalTime(lastRepairBlockedAt)
	return &sess, nil
}
