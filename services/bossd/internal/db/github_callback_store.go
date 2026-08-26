package db

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/recurser/bossalib/githubcallback"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sqlutil"
)

var _ GithubCallbackStore = (*SQLiteGithubCallbackStore)(nil)

// SQLiteGithubCallbackStore implements GithubCallbackStore using SQLite.
type SQLiteGithubCallbackStore struct {
	db *sql.DB
}

// NewGithubCallbackStore creates a new SQLite-backed GithubCallbackStore.
func NewGithubCallbackStore(db *sql.DB) *SQLiteGithubCallbackStore {
	return &SQLiteGithubCallbackStore{db: db}
}

// validGithubCallbackTrigger reports whether t is a known trigger event. This
// store is the authority that rejects a registration, so it deliberately
// derives from githubcallback.ValidTriggers() — the same canonical list the CLI
// and MCP schemas validate against — rather than re-enumerating the vocabulary
// here. A hand-maintained third copy would let a newly added trigger be
// accepted by every other surface and rejected only at write time, with the
// whole suite still green. Membership is exact (no trimming or case folding) so
// the stored value is always a canonical trigger string.
func validGithubCallbackTrigger(t models.GithubCallbackTrigger) bool {
	return slices.Contains(githubcallback.ValidTriggers(), t)
}

var mutuallyExclusiveGithubCallbackTriggers = map[models.GithubCallbackTrigger]map[models.GithubCallbackTrigger]bool{
	models.GithubCallbackTriggerMerged: {
		models.GithubCallbackTriggerClosed: true,
	},
	models.GithubCallbackTriggerClosed: {
		models.GithubCallbackTriggerMerged: true,
	},
	models.GithubCallbackTriggerChecksPassed: {
		models.GithubCallbackTriggerChecksFailed: true,
	},
	models.GithubCallbackTriggerChecksFailed: {
		models.GithubCallbackTriggerChecksPassed: true,
	},
}

func githubCallbackTriggersCoSatisfiable(a, b models.GithubCallbackTrigger) bool {
	if a == b {
		return true
	}
	return !mutuallyExclusiveGithubCallbackTriggers[a][b]
}

func (s *SQLiteGithubCallbackStore) Create(ctx context.Context, params CreateGithubCallbackParams) (*models.GithubCallback, error) {
	owner := strings.ToLower(strings.TrimSpace(params.RepoOwner))
	name := strings.ToLower(strings.TrimSpace(params.RepoName))
	chatID := strings.TrimSpace(params.TargetChatID)

	if chatID == "" {
		return nil, fmt.Errorf("%w: target chat id is required", ErrGithubCallbackInvalid)
	}
	if owner == "" || name == "" {
		return nil, fmt.Errorf("%w: repository owner and name are required", ErrGithubCallbackInvalid)
	}
	if params.PRNumber <= 0 {
		return nil, fmt.Errorf("%w: pr number must be positive, got %d", ErrGithubCallbackInvalid, params.PRNumber)
	}
	if !validGithubCallbackTrigger(params.Trigger) {
		return nil, fmt.Errorf("%w: unknown trigger %q", ErrGithubCallbackInvalid, params.Trigger)
	}
	if strings.TrimSpace(params.Message) == "" {
		return nil, fmt.Errorf("%w: message is required", ErrGithubCallbackInvalid)
	}

	now := time.Now().UTC()
	expiresAt := now.Add(GithubCallbackDefaultExpiry)
	if params.ExpiresAt != nil {
		expiresAt = params.ExpiresAt.UTC()
		if !expiresAt.After(now) {
			return nil, fmt.Errorf("%w: expiry must be in the future", ErrGithubCallbackInvalid)
		}
		if expiresAt.Sub(now) > GithubCallbackMaxExpiry {
			return nil, fmt.Errorf("%w: expiry must be within %s", ErrGithubCallbackInvalid, GithubCallbackMaxExpiry)
		}
	}

	id, err := sqlutil.NewID()
	if err != nil {
		return nil, fmt.Errorf("new github callback id: %w", err)
	}
	nowStr := sqlutil.FormatTime(now)

	var groupIDVal any
	if params.GroupID != nil {
		if g := strings.TrimSpace(*params.GroupID); g != "" {
			groupIDVal = g
		}
	}

	conn, err := beginImmediate(ctx, s.db, "github callback create")
	if err != nil {
		return nil, err
	}
	committed := false
	defer closeImmediate(ctx, conn, &committed)

	if groupID, ok := groupIDVal.(string); ok {
		rows, err := conn.QueryContext(ctx,
			`SELECT trigger_event
			 FROM github_callbacks
			 WHERE group_id = ? AND state IN (?, ?)`,
			groupID,
			string(models.GithubCallbackStateActive),
			string(models.GithubCallbackStateLeased),
		)
		if err != nil {
			return nil, fmt.Errorf("validate github callback group: %w", err)
		}
		for rows.Next() {
			var existing string
			if err := rows.Scan(&existing); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan github callback group trigger: %w", err)
			}
			existingTrigger := models.GithubCallbackTrigger(existing)
			if githubCallbackTriggersCoSatisfiable(existingTrigger, params.Trigger) {
				_ = rows.Close()
				return nil, fmt.Errorf("%w: group %q already has co-satisfiable trigger %q; cannot add %q", ErrGithubCallbackInvalid, groupID, existingTrigger, params.Trigger)
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("iterate github callback group triggers: %w", err)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("close github callback group triggers: %w", err)
		}
	}

	_, err = conn.ExecContext(ctx,
		`INSERT INTO github_callbacks
			(id, group_id, target_chat_id, repo_owner, repo_name, pr_number, trigger_event,
			 state, message, should_require_transition, has_observed_baseline, attempt_count,
			 expires_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?, ?)`,
		id, groupIDVal, chatID, owner, name, params.PRNumber, string(params.Trigger),
		string(models.GithubCallbackStateActive), params.Message, boolToInt(params.ShouldRequireTransition),
		sqlutil.FormatTime(expiresAt), nowStr, nowStr,
	)
	if err != nil {
		return nil, fmt.Errorf("insert github callback: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, fmt.Errorf("commit github callback create: %w", err)
	}
	committed = true
	row := conn.QueryRowContext(ctx, githubCallbackSelectSQL+" WHERE id = ?", id)
	return scanGithubCallback(row)
}

func (s *SQLiteGithubCallbackStore) Get(ctx context.Context, id string) (*models.GithubCallback, error) {
	row := s.db.QueryRowContext(ctx, githubCallbackSelectSQL+" WHERE id = ?", id)
	return scanGithubCallback(row)
}

func (s *SQLiteGithubCallbackStore) List(ctx context.Context, filter ListGithubCallbacksFilter) ([]*models.GithubCallback, error) {
	var where []string
	var args []any
	if filter.TargetChatID != nil {
		where = append(where, "target_chat_id = ?")
		args = append(args, *filter.TargetChatID)
	}
	if filter.RepoOwner != nil {
		where = append(where, "repo_owner = ?")
		args = append(args, strings.ToLower(strings.TrimSpace(*filter.RepoOwner)))
	}
	if filter.RepoName != nil {
		where = append(where, "repo_name = ?")
		args = append(args, strings.ToLower(strings.TrimSpace(*filter.RepoName)))
	}
	if filter.PRNumber != nil {
		where = append(where, "pr_number = ?")
		args = append(args, *filter.PRNumber)
	}
	if filter.Trigger != nil {
		where = append(where, "trigger_event = ?")
		args = append(args, string(*filter.Trigger))
	}
	if filter.State != nil {
		where = append(where, "state = ?")
		args = append(args, string(*filter.State))
	}
	query := githubCallbackSelectSQL
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY created_at ASC, id ASC"
	// #nosec G202 -- WHERE is built from code-literal `col = ?` fragments; every
	// value is bound via ?, not concatenated user text; owner=@recurser; review-by=2026-10-22; issue=BOS-467
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list github callbacks: %w", err)
	}
	return collectGithubCallbacks(rows)
}

func (s *SQLiteGithubCallbackStore) Delete(ctx context.Context, id, expectTargetChatID string) (DeleteGithubCallbackOutcome, error) {
	conn, err := beginImmediate(ctx, s.db, "github callback delete")
	if err != nil {
		return "", err
	}
	committed := false
	defer closeImmediate(ctx, conn, &committed)

	res, err := conn.ExecContext(ctx,
		`DELETE FROM github_callbacks
		 WHERE id = ? AND (? = '' OR target_chat_id = ?)`,
		id, expectTargetChatID, expectTargetChatID,
	)
	if err != nil {
		return "", fmt.Errorf("delete github callback: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return "", fmt.Errorf("commit github callback delete: %w", err)
		}
		committed = true
		return DeleteGithubCallbackOutcomeDeleted, nil
	}

	var existingChatID string
	err = conn.QueryRowContext(ctx, "SELECT target_chat_id FROM github_callbacks WHERE id = ?", id).Scan(&existingChatID)
	if err == sql.ErrNoRows {
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return "", fmt.Errorf("commit github callback not-found delete: %w", err)
		}
		committed = true
		return DeleteGithubCallbackOutcomeNotFound, nil
	}
	if err != nil {
		return "", fmt.Errorf("select github callback after delete miss: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return "", fmt.Errorf("commit github callback not-owned delete: %w", err)
	}
	committed = true
	return DeleteGithubCallbackOutcomeNotOwned, ErrGithubCallbackNotOwned
}

func (s *SQLiteGithubCallbackStore) ObserveBaseline(ctx context.Context, id string, now time.Time) error {
	nowStr := sqlutil.FormatTime(now)
	res, err := s.db.ExecContext(ctx,
		`UPDATE github_callbacks
		 SET has_observed_baseline = 1, updated_at = ?
		 WHERE id = ? AND state = ? AND should_require_transition = 1 AND has_observed_baseline = 0`,
		nowStr, id, string(models.GithubCallbackStateActive),
	)
	if err != nil {
		return fmt.Errorf("observe github callback baseline: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, gerr := s.Get(ctx, id); gerr != nil {
			return gerr
		}
		return ErrGithubCallbackTriggerConflict
	}
	return nil
}

// ExpiredGithubCallback is a callback made terminal by one expiry sweep.
// It contains only the bounded fields needed for terminal-outcome telemetry.
type ExpiredGithubCallback struct {
	Trigger      models.GithubCallbackTrigger
	AttemptCount int
}

// ExpireOverdueCallbacks expires overdue callbacks and returns only rows that
// transitioned in this statement, allowing callers to emit each outcome once.
func (s *SQLiteGithubCallbackStore) ExpireOverdueCallbacks(ctx context.Context, now time.Time) (int, []ExpiredGithubCallback, error) {
	nowStr := sqlutil.FormatTime(now)
	rows, err := s.db.QueryContext(ctx,
		`UPDATE github_callbacks
		 SET state = ?, updated_at = ?
		 WHERE state IN (?, ?, ?) AND expires_at <= ?
		 RETURNING trigger_event, attempt_count`,
		string(models.GithubCallbackStateExpired), nowStr,
		string(models.GithubCallbackStateActive),
		string(models.GithubCallbackStateLeased),
		string(models.GithubCallbackStateTriggered),
		nowStr,
	)
	if err != nil {
		return 0, nil, fmt.Errorf("expire overdue github callbacks: %w", err)
	}
	var expired []ExpiredGithubCallback
	for rows.Next() {
		var callback ExpiredGithubCallback
		if err := rows.Scan(&callback.Trigger, &callback.AttemptCount); err != nil {
			_ = rows.Close()
			return 0, nil, fmt.Errorf("scan expired github callback: %w", err)
		}
		expired = append(expired, callback)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, nil, fmt.Errorf("iterate expired github callbacks: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, nil, fmt.Errorf("close expired github callbacks: %w", err)
	}
	return len(expired), expired, nil
}

func (s *SQLiteGithubCallbackStore) ExpireOverdue(ctx context.Context, now time.Time) (int, error) {
	n, _, err := s.ExpireOverdueCallbacks(ctx, now)
	return n, err
}

func (s *SQLiteGithubCallbackStore) AcquireLease(ctx context.Context, id, owner string, now time.Time, leaseFor time.Duration) (*models.GithubCallback, error) {
	nowStr := sqlutil.FormatTime(now)
	deadline := sqlutil.FormatTime(now.Add(leaseFor))
	// Claimable when: not terminal (active/leased/triggered), not past expiry, the
	// retry backoff has elapsed, and the lease is free — unset, already ours, or
	// past its deadline (crash recovery). Promote active -> leased; keep
	// leased/triggered as-is so a recovered claim does not lose its triggered status.
	//
	// The expires_at > now guard keeps acquisition consistent with the terminal
	// expired invariant enforced in TriggerGroup and MarkDelivered: expiry is only
	// swept lazily (list RPC / ExpireOverdue), so an overdue-but-unswept row is
	// still active/triggered here. Without this guard a delivery worker could lease
	// and send an expired callback before MarkDelivered rejects the state change.
	// expires_at is stored in the fixed-width UTC layout, so the string compare is
	// chronological, matching ExpireOverdue's expires_at <= ? predicate.
	res, err := s.db.ExecContext(ctx,
		`UPDATE github_callbacks
		 SET state = CASE WHEN state = ? THEN ? ELSE state END,
		     lease_owner = ?, lease_deadline_at = ?, updated_at = ?
		 WHERE id = ?
		   AND state IN (?, ?, ?)
		   AND expires_at > ?
		   AND (next_attempt_at IS NULL OR next_attempt_at <= ?)
		   AND (lease_owner IS NULL OR lease_owner = ? OR lease_deadline_at IS NULL OR lease_deadline_at <= ?)`,
		string(models.GithubCallbackStateActive), string(models.GithubCallbackStateLeased),
		owner, deadline, nowStr,
		id,
		string(models.GithubCallbackStateActive),
		string(models.GithubCallbackStateLeased),
		string(models.GithubCallbackStateTriggered),
		nowStr,
		nowStr,
		owner, nowStr,
	)
	if err != nil {
		return nil, fmt.Errorf("acquire github callback lease: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Distinguish an absent row from a live-but-unclaimable one.
		if _, gerr := s.Get(ctx, id); gerr != nil {
			return nil, gerr
		}
		return nil, ErrGithubCallbackLeaseConflict
	}
	return s.Get(ctx, id)
}

func (s *SQLiteGithubCallbackStore) ReleaseLease(ctx context.Context, id, owner string, now time.Time) error {
	nowStr := sqlutil.FormatTime(now)
	res, err := s.db.ExecContext(ctx,
		`UPDATE github_callbacks
		 SET lease_owner = NULL, lease_deadline_at = NULL,
		     state = CASE WHEN state = ? THEN ? ELSE state END,
		     updated_at = ?
		 WHERE id = ? AND lease_owner = ?`,
		string(models.GithubCallbackStateLeased), string(models.GithubCallbackStateActive),
		nowStr, id, owner,
	)
	if err != nil {
		return fmt.Errorf("release github callback lease: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, gerr := s.Get(ctx, id); gerr != nil {
			return gerr
		}
		return ErrGithubCallbackLeaseConflict
	}
	return nil
}

func (s *SQLiteGithubCallbackStore) TriggerGroup(ctx context.Context, id, event string, now time.Time) (*models.GithubCallback, error) {
	nowStr := sqlutil.FormatTime(now)
	conn, err := beginImmediate(ctx, s.db, "github callback")
	if err != nil {
		return nil, err
	}
	committed := false
	defer closeImmediate(ctx, conn, &committed)

	var state, groupID, expiresAt string
	var groupIDNull sql.NullString
	err = conn.QueryRowContext(ctx, "SELECT state, group_id, expires_at FROM github_callbacks WHERE id = ?", id).
		Scan(&state, &groupIDNull, &expiresAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, sql.ErrNoRows
		}
		return nil, fmt.Errorf("select github callback for trigger: %w", err)
	}
	groupID = groupIDNull.String

	if state != string(models.GithubCallbackStateActive) && state != string(models.GithubCallbackStateLeased) {
		return nil, ErrGithubCallbackTriggerConflict
	}

	// Reject overdue callbacks before mutating: expiry is only swept lazily (on
	// the list RPC / ExpireOverdue), so a row past expires_at can still be active
	// or leased here. Triggering it would fire a callback that should be terminally
	// expired and would cancel its siblings. Guarding on state alone is not enough.
	if !now.Before(sqlutil.ParseTime(expiresAt)) {
		return nil, ErrGithubCallbackTriggerConflict
	}

	if _, err := conn.ExecContext(ctx,
		`UPDATE github_callbacks
		 SET state = ?, triggered_at = ?, last_event = ?, lease_owner = NULL,
		     lease_deadline_at = NULL, updated_at = ?
		 WHERE id = ?`,
		string(models.GithubCallbackStateTriggered), nowStr, event, nowStr, id,
	); err != nil {
		return nil, fmt.Errorf("mark github callback triggered: %w", err)
	}

	if groupIDNull.Valid && groupID != "" {
		if _, err := conn.ExecContext(ctx,
			`UPDATE github_callbacks
			 SET state = ?, updated_at = ?
			 WHERE group_id = ? AND id != ? AND state IN (?, ?)`,
			string(models.GithubCallbackStateCanceled), nowStr, groupID, id,
			string(models.GithubCallbackStateActive), string(models.GithubCallbackStateLeased),
		); err != nil {
			return nil, fmt.Errorf("cancel github callback siblings: %w", err)
		}
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, fmt.Errorf("commit github callback trigger: %w", err)
	}
	committed = true
	// Read back on the same connection: it is still checked out (the deferred
	// closeImmediate has not run yet), so calling s.Get here would deadlock on a
	// single-connection (in-memory) pool. Post-COMMIT the conn is in autocommit.
	row := conn.QueryRowContext(ctx, githubCallbackSelectSQL+" WHERE id = ?", id)
	return scanGithubCallback(row)
}

func (s *SQLiteGithubCallbackStore) MarkDelivered(ctx context.Context, id, owner string, now time.Time) error {
	nowStr := sqlutil.FormatTime(now)
	// The expires_at > now guard keeps delivery consistent with the terminal
	// expired state: if expiry passes while the worker still holds the lease,
	// the row must not be marked delivered (it belongs to ExpireOverdue's sweep).
	// expires_at is stored in the fixed-width UTC layout, so the string compare is
	// chronological, matching ExpireOverdue's expires_at <= ? predicate.
	res, err := s.db.ExecContext(ctx,
		`UPDATE github_callbacks
		 SET state = ?, delivered_at = ?, lease_owner = NULL, lease_deadline_at = NULL,
		     last_error = NULL, updated_at = ?
		 WHERE id = ? AND state = ? AND lease_owner = ? AND expires_at > ?`,
		string(models.GithubCallbackStateDelivered), nowStr, nowStr,
		id, string(models.GithubCallbackStateTriggered), owner, nowStr,
	)
	if err != nil {
		return fmt.Errorf("mark github callback delivered: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, gerr := s.Get(ctx, id); gerr != nil {
			return gerr
		}
		return ErrGithubCallbackLeaseConflict
	}
	return nil
}

func (s *SQLiteGithubCallbackStore) ScheduleRetry(ctx context.Context, id, owner string, params ScheduleGithubCallbackRetryParams) error {
	now := sqlutil.TimeNow()
	attemptIncrement := 1
	if params.PreserveAttemptCount {
		attemptIncrement = 0
	}
	var lastEventVal any
	if params.LastEvent != "" {
		lastEventVal = params.LastEvent
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE github_callbacks
		 SET attempt_count = attempt_count + ?,
		     next_attempt_at = ?, last_error = ?, last_event = COALESCE(?, last_event),
		     lease_owner = NULL, lease_deadline_at = NULL, updated_at = ?
		 WHERE id = ? AND lease_owner = ?`,
		attemptIncrement, sqlutil.FormatTime(params.NextAttemptAt), params.LastError, lastEventVal, now,
		id, owner,
	)
	if err != nil {
		return fmt.Errorf("schedule github callback retry: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, gerr := s.Get(ctx, id); gerr != nil {
			return gerr
		}
		return ErrGithubCallbackLeaseConflict
	}
	return nil
}

const githubCallbackSelectSQL = `SELECT id, group_id, target_chat_id, repo_owner, repo_name, pr_number,
	trigger_event, state, message, should_require_transition, has_observed_baseline,
	lease_owner, lease_deadline_at, attempt_count,
	next_attempt_at, triggered_at, delivered_at, last_error, last_event,
	expires_at, created_at, updated_at
	FROM github_callbacks`

func collectGithubCallbacks(rows *sql.Rows) ([]*models.GithubCallback, error) {
	defer func() { _ = rows.Close() }()
	var cbs []*models.GithubCallback
	for rows.Next() {
		cb, err := scanGithubCallback(rows)
		if err != nil {
			return nil, err
		}
		cbs = append(cbs, cb)
	}
	return cbs, rows.Err()
}

func scanGithubCallback(sc sqlutil.Scanner) (*models.GithubCallback, error) {
	var cb models.GithubCallback
	var trigger, state string
	var groupID, leaseOwner, leaseDeadlineAt, nextAttemptAt, triggeredAt, deliveredAt, lastError, lastEvent sql.NullString
	var expiresAt, createdAt, updatedAt string
	var requiresTransition, hasObservedBaseline int
	err := sc.Scan(
		&cb.ID, &groupID, &cb.TargetChatID, &cb.RepoOwner, &cb.RepoName, &cb.PRNumber,
		&trigger, &state, &cb.Message, &requiresTransition, &hasObservedBaseline,
		&leaseOwner, &leaseDeadlineAt, &cb.AttemptCount,
		&nextAttemptAt, &triggeredAt, &deliveredAt, &lastError, &lastEvent,
		&expiresAt, &createdAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}
	cb.Trigger = models.GithubCallbackTrigger(trigger)
	cb.State = models.GithubCallbackState(state)
	cb.ShouldRequireTransition = requiresTransition != 0
	cb.HasObservedBaseline = hasObservedBaseline != 0
	if groupID.Valid {
		g := groupID.String
		cb.GroupID = &g
	}
	if leaseOwner.Valid {
		o := leaseOwner.String
		cb.LeaseOwner = &o
	}
	if lastError.Valid {
		e := lastError.String
		cb.LastError = &e
	}
	if lastEvent.Valid {
		e := lastEvent.String
		cb.LastEvent = &e
	}
	cb.LeaseDeadlineAt = optionalTime(leaseDeadlineAt)
	cb.NextAttemptAt = optionalTime(nextAttemptAt)
	cb.TriggeredAt = optionalTime(triggeredAt)
	cb.DeliveredAt = optionalTime(deliveredAt)
	cb.ExpiresAt = sqlutil.ParseTime(expiresAt)
	cb.CreatedAt = sqlutil.ParseTime(createdAt)
	cb.UpdatedAt = sqlutil.ParseTime(updatedAt)
	return &cb, nil
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// optionalTime parses a nullable timestamp column into a *time.Time.
func optionalTime(ns sql.NullString) *time.Time {
	if !ns.Valid {
		return nil
	}
	t := sqlutil.ParseTime(ns.String)
	if t.IsZero() {
		return nil
	}
	return &t
}
