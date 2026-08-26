package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/recurser/bossalib/sqlutil"
)

const (
	AgentRunStopClean          = "clean"
	AgentRunStopStopped        = "stopped"
	AgentRunStopUsageExhausted = "usage_exhausted"
	AgentRunStopRateLimited    = "rate_limited"
	AgentRunStopDaemonRestart  = "daemon_restart"
	AgentRunStopUnknown        = "unknown"
)

// AgentRun is one daemon-observed AgentRunner.StartRun lifecycle.
type AgentRun struct {
	ID                   string
	SessionID            string
	AgentSessionID       string
	AgentName            string
	Model                string
	Effort               string
	StartedAt            time.Time
	StoppedAt            *time.Time
	StopReason           string
	ParentModelCallCount int64
	ChildModelCallCount  int64
	ToolCallCount        int64
	SubagentCount        int64
	DirectSubagentCount  int64
	OutputTokenCount     *int64
	ReasoningTokenCount  *int64
	IsBackfilled         bool
	RepoDisplayName      string
	Children             []AgentRunChild
}

// AgentRunChild is one child span attributed to an agent run.
type AgentRunChild struct {
	ID                  string
	AgentRunID          string
	AgentSessionID      string
	ParentAgentID       string
	SpawnDepth          int32
	StartedAt           time.Time
	StoppedAt           *time.Time
	ModelCallCount      int64
	ToolCallCount       int64
	OutputTokenCount    *int64
	ReasoningTokenCount *int64
}

type AgentRunTelemetry struct {
	ParentModelCallCount int64
	ChildModelCallCount  int64
	ToolCallCount        int64
	SubagentCount        int64
	DirectSubagentCount  int64
	OutputTokenCount     *int64
	ReasoningTokenCount  *int64
	Children             []AgentRunChild
}

type AgentRunFilter struct {
	SessionID         string
	AgentName         string
	RepoDisplayName   string
	Since             *time.Time
	Until             *time.Time
	IncludeOpen       bool
	IncludeAllReasons bool
	IncludeBackfilled bool
	OmitChildren      bool
}

type AgentRunStore interface {
	Start(ctx context.Context, run AgentRun) (AgentRun, error)
	Stop(ctx context.Context, agentSessionID, reason string, stoppedAt time.Time) error
	StopRun(ctx context.Context, runID, reason string, stoppedAt time.Time) error
	RecordTelemetry(ctx context.Context, runID string, telemetry AgentRunTelemetry) error
	RecordTelemetryByAgentSessionID(ctx context.Context, agentSessionID string, telemetry AgentRunTelemetry) error
	ReconcileOpen(ctx context.Context, now time.Time, activeAgentSessionIDs []string) (int64, error)
	List(ctx context.Context, filter AgentRunFilter) ([]AgentRun, error)
	Backfill(ctx context.Context, params AgentRunBackfillParams) (AgentRunBackfillSummary, error)
}

type SQLiteAgentRunStore struct {
	db *sql.DB
}

func NewAgentRunStore(db *sql.DB) *SQLiteAgentRunStore {
	return &SQLiteAgentRunStore{db: db}
}

func (s *SQLiteAgentRunStore) Start(ctx context.Context, run AgentRun) (AgentRun, error) {
	if run.SessionID == "" {
		return AgentRun{}, fmt.Errorf("start agent run: session ID required")
	}
	if run.AgentSessionID == "" {
		return AgentRun{}, fmt.Errorf("start agent run: agent session ID required")
	}
	if err := validateAgentRunCounts(run); err != nil {
		return AgentRun{}, err
	}
	if run.ID == "" {
		id, err := sqlutil.NewID()
		if err != nil {
			return AgentRun{}, fmt.Errorf("mint agent run id: %w", err)
		}
		run.ID = id
	}
	if run.StartedAt.IsZero() {
		run.StartedAt = time.Now()
	}
	run.StopReason = normalizeAgentRunStopReason(run.StopReason)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO agent_runs
		   (id, session_id, agent_session_id, agent_name, model, effort, started_at, stopped_at,
		    stop_reason, parent_model_call_count, child_model_call_count, tool_call_count,
		    subagent_count, direct_subagent_count, output_token_count, reasoning_token_count,
		    is_backfilled, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.ID, run.SessionID, run.AgentSessionID, run.AgentName, run.Model, run.Effort,
		sqlutil.FormatTime(run.StartedAt), optionalTimeString(run.StoppedAt), run.StopReason,
		run.ParentModelCallCount, run.ChildModelCallCount, run.ToolCallCount,
		run.SubagentCount, run.DirectSubagentCount, optionalInt(run.OutputTokenCount),
		optionalInt(run.ReasoningTokenCount), sqlutil.BoolToInt(run.IsBackfilled),
		sqlutil.TimeNow())
	if err != nil {
		return AgentRun{}, fmt.Errorf("insert agent run: %w", err)
	}
	return run, nil
}

func (s *SQLiteAgentRunStore) Stop(ctx context.Context, agentSessionID, reason string, stoppedAt time.Time) error {
	if agentSessionID == "" {
		return fmt.Errorf("stop agent run: agent session ID required")
	}
	if stoppedAt.IsZero() {
		stoppedAt = time.Now()
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE agent_runs
		 SET stopped_at = ?, stop_reason = ?, updated_at = ?
		 WHERE agent_session_id = ? AND stopped_at IS NULL`,
		sqlutil.FormatTime(stoppedAt), normalizeAgentRunStopReason(reason), sqlutil.TimeNow(), agentSessionID)
	if err != nil {
		return fmt.Errorf("stop agent run: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *SQLiteAgentRunStore) StopRun(ctx context.Context, runID, reason string, stoppedAt time.Time) error {
	if runID == "" {
		return fmt.Errorf("stop agent run: run ID required")
	}
	if stoppedAt.IsZero() {
		stoppedAt = time.Now()
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE agent_runs
		 SET stopped_at = ?, stop_reason = ?, updated_at = ?
		 WHERE id = ? AND stopped_at IS NULL`,
		sqlutil.FormatTime(stoppedAt), normalizeAgentRunStopReason(reason), sqlutil.TimeNow(), runID)
	if err != nil {
		return fmt.Errorf("stop agent run: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *SQLiteAgentRunStore) RecordTelemetry(ctx context.Context, runID string, telemetry AgentRunTelemetry) error {
	if runID == "" {
		return fmt.Errorf("record agent run telemetry: run ID required")
	}
	return s.recordTelemetry(ctx, "id", runID, telemetry)
}

func (s *SQLiteAgentRunStore) RecordTelemetryByAgentSessionID(ctx context.Context, agentSessionID string, telemetry AgentRunTelemetry) error {
	if agentSessionID == "" {
		return fmt.Errorf("record agent run telemetry: agent session ID required")
	}
	runID, err := s.latestAgentRunID(ctx, agentSessionID)
	if err != nil {
		return err
	}
	return s.RecordTelemetry(ctx, runID, telemetry)
}

func (s *SQLiteAgentRunStore) recordTelemetry(ctx context.Context, keyColumn, key string, telemetry AgentRunTelemetry) error {
	if err := validateTelemetryCounts(telemetry); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin agent telemetry transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx,
		`UPDATE agent_runs
		 SET parent_model_call_count = ?, child_model_call_count = ?, tool_call_count = ?,
		     subagent_count = ?, direct_subagent_count = ?, output_token_count = ?,
		     reasoning_token_count = ?, updated_at = ?
		 WHERE `+keyColumn+` = ?`,
		telemetry.ParentModelCallCount, telemetry.ChildModelCallCount, telemetry.ToolCallCount,
		telemetry.SubagentCount, telemetry.DirectSubagentCount, optionalInt(telemetry.OutputTokenCount),
		optionalInt(telemetry.ReasoningTokenCount), sqlutil.TimeNow(), key)
	if err != nil {
		return fmt.Errorf("update agent run telemetry: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	runID, err := agentRunIDByKey(ctx, tx, keyColumn, key)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM agent_run_children WHERE agent_run_id = ?`, runID); err != nil {
		return fmt.Errorf("replace agent run children: %w", err)
	}
	if err := insertAgentRunChildren(ctx, tx, runID, telemetry.Children); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit agent telemetry transaction: %w", err)
	}
	return nil
}

func insertAgentRunChildren(ctx context.Context, tx *sql.Tx, runID string, children []AgentRunChild) error {
	for _, child := range children {
		if child.StartedAt.IsZero() {
			return fmt.Errorf("insert agent run child: child started_at is required")
		}
		id := child.ID
		if id == "" {
			var err error
			id, err = sqlutil.NewID()
			if err != nil {
				return fmt.Errorf("mint child id: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO agent_run_children
			   (id, agent_run_id, agent_session_id, parent_agent_id, spawn_depth, started_at,
			    stopped_at, model_call_count, tool_call_count, output_token_count, reasoning_token_count)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, runID, child.AgentSessionID, child.ParentAgentID, child.SpawnDepth,
			sqlutil.FormatTime(child.StartedAt), optionalTimeString(child.StoppedAt),
			child.ModelCallCount, child.ToolCallCount, optionalInt(child.OutputTokenCount),
			optionalInt(child.ReasoningTokenCount)); err != nil {
			return fmt.Errorf("insert agent run child: %w", err)
		}
	}
	return nil
}

type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func agentRunIDByKey(ctx context.Context, q queryRower, keyColumn, key string) (string, error) {
	if keyColumn == "id" {
		return key, nil
	}
	var id string
	if err := q.QueryRowContext(ctx, `SELECT id FROM agent_runs WHERE `+keyColumn+` = ?`, key).Scan(&id); err != nil {
		return "", fmt.Errorf("find agent run id: %w", err)
	}
	return id, nil
}

func (s *SQLiteAgentRunStore) latestAgentRunID(ctx context.Context, agentSessionID string) (string, error) {
	var id string
	if err := s.db.QueryRowContext(ctx,
		`SELECT id FROM agent_runs
		 WHERE agent_session_id = ?
		 ORDER BY stopped_at IS NOT NULL, started_at DESC, id DESC
		 LIMIT 1`, agentSessionID).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", sql.ErrNoRows
		}
		return "", fmt.Errorf("find latest agent run id: %w", err)
	}
	return id, nil
}

func (s *SQLiteAgentRunStore) ReconcileOpen(ctx context.Context, now time.Time, activeAgentSessionIDs []string) (int64, error) {
	if now.IsZero() {
		now = time.Now()
	}
	query := `UPDATE agent_runs
		 SET stopped_at = ?, stop_reason = ?, updated_at = ?
		 WHERE stopped_at IS NULL`
	args := []any{sqlutil.FormatTime(now), AgentRunStopDaemonRestart, sqlutil.TimeNow()}
	activeAgentSessionIDs = uniqueNonEmptyStrings(activeAgentSessionIDs)
	if len(activeAgentSessionIDs) > 0 {
		query += ` AND agent_session_id NOT IN (` + strings.TrimRight(strings.Repeat("?,", len(activeAgentSessionIDs)), ",") + `)`
		for _, id := range activeAgentSessionIDs {
			args = append(args, id)
		}
	}
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("reconcile open agent runs: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func uniqueNonEmptyStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func (s *SQLiteAgentRunStore) List(ctx context.Context, filter AgentRunFilter) ([]AgentRun, error) {
	where, args := agentRunWhere(filter)
	rows, err := s.db.QueryContext(ctx,
		`SELECT ar.id, ar.session_id, ar.agent_session_id, COALESCE(NULLIF(ar.agent_name, ''), s.agent_name), ar.model, ar.effort,
		        ar.started_at, ar.stopped_at, ar.stop_reason, ar.parent_model_call_count,
		        ar.child_model_call_count, ar.tool_call_count, ar.subagent_count,
		        ar.direct_subagent_count, ar.output_token_count, ar.reasoning_token_count,
		        ar.is_backfilled, COALESCE(r.display_name, '')
		 FROM agent_runs ar
		 JOIN sessions s ON s.id = ar.session_id
		 LEFT JOIN repos r ON r.id = s.repo_id`+where+`
		 ORDER BY ar.started_at DESC, ar.id DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("list agent runs: %w", err)
	}
	var out []AgentRun
	for rows.Next() {
		run, err := scanAgentRun(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, run)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close agent run rows: %w", err)
	}
	if !filter.OmitChildren {
		for i := range out {
			children, err := s.children(ctx, out[i].ID)
			if err != nil {
				return nil, err
			}
			out[i].Children = children
		}
	}
	return out, nil
}

func (s *SQLiteAgentRunStore) children(ctx context.Context, runID string) ([]AgentRunChild, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, agent_run_id, agent_session_id, parent_agent_id, spawn_depth, started_at,
		        stopped_at, model_call_count, tool_call_count, output_token_count, reasoning_token_count
		 FROM agent_run_children
		 WHERE agent_run_id = ?
		 ORDER BY started_at, id`, runID)
	if err != nil {
		return nil, fmt.Errorf("list agent run children: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []AgentRunChild
	for rows.Next() {
		var child AgentRunChild
		var startedAt string
		var stoppedAt sql.NullString
		var outputTokens, reasoningTokens sql.NullInt64
		if err := rows.Scan(&child.ID, &child.AgentRunID, &child.AgentSessionID, &child.ParentAgentID,
			&child.SpawnDepth, &startedAt, &stoppedAt, &child.ModelCallCount, &child.ToolCallCount,
			&outputTokens, &reasoningTokens); err != nil {
			return nil, fmt.Errorf("scan agent run child: %w", err)
		}
		child.StartedAt = sqlutil.ParseTime(startedAt)
		child.StoppedAt = parseNullTime(stoppedAt)
		child.OutputTokenCount = nullIntPtr(outputTokens)
		child.ReasoningTokenCount = nullIntPtr(reasoningTokens)
		out = append(out, child)
	}
	return out, rows.Err()
}

func agentRunWhere(filter AgentRunFilter) (string, []any) {
	var clauses []string
	var args []any
	if filter.SessionID != "" {
		clauses = append(clauses, "ar.session_id = ?")
		args = append(args, filter.SessionID)
	}
	if filter.AgentName != "" {
		clauses = append(clauses, "COALESCE(NULLIF(ar.agent_name, ''), s.agent_name) = ?")
		args = append(args, filter.AgentName)
	}
	if filter.RepoDisplayName != "" {
		clauses = append(clauses, "r.display_name = ?")
		args = append(args, filter.RepoDisplayName)
	}
	if filter.Since != nil {
		clauses = append(clauses, "ar.started_at >= ?")
		args = append(args, sqlutil.FormatTime(*filter.Since))
	}
	if filter.Until != nil {
		clauses = append(clauses, "ar.started_at < ?")
		args = append(args, sqlutil.FormatTime(*filter.Until))
	}
	if !filter.IncludeOpen {
		clauses = append(clauses, "ar.stopped_at IS NOT NULL")
	}
	if !filter.IncludeAllReasons {
		reasonClause := "ar.stop_reason IN ('clean', 'stopped', 'usage_exhausted', 'rate_limited')"
		if filter.IncludeOpen {
			reasonClause = "(ar.stopped_at IS NULL OR " + reasonClause + ")"
		}
		clauses = append(clauses, reasonClause)
	}
	if !filter.IncludeBackfilled {
		clauses = append(clauses, "ar.is_backfilled = 0")
	}
	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

func scanAgentRun(rows *sql.Rows) (AgentRun, error) {
	var run AgentRun
	var startedAt string
	var stoppedAt sql.NullString
	var outputTokens, reasoningTokens sql.NullInt64
	var isBackfilled int
	if err := rows.Scan(&run.ID, &run.SessionID, &run.AgentSessionID, &run.AgentName, &run.Model,
		&run.Effort, &startedAt, &stoppedAt, &run.StopReason, &run.ParentModelCallCount,
		&run.ChildModelCallCount, &run.ToolCallCount, &run.SubagentCount, &run.DirectSubagentCount,
		&outputTokens, &reasoningTokens, &isBackfilled, &run.RepoDisplayName); err != nil {
		return AgentRun{}, fmt.Errorf("scan agent run: %w", err)
	}
	run.StartedAt = sqlutil.ParseTime(startedAt)
	run.StoppedAt = parseNullTime(stoppedAt)
	run.OutputTokenCount = nullIntPtr(outputTokens)
	run.ReasoningTokenCount = nullIntPtr(reasoningTokens)
	run.IsBackfilled = isBackfilled != 0
	return run, nil
}

func validateTelemetryCounts(t AgentRunTelemetry) error {
	if err := checkNonNegative("parent_model_call_count", t.ParentModelCallCount); err != nil {
		return err
	}
	if err := checkNonNegative("child_model_call_count", t.ChildModelCallCount); err != nil {
		return err
	}
	if err := checkNonNegative("tool_call_count", t.ToolCallCount); err != nil {
		return err
	}
	if err := checkNonNegative("subagent_count", t.SubagentCount); err != nil {
		return err
	}
	if err := checkNonNegative("direct_subagent_count", t.DirectSubagentCount); err != nil {
		return err
	}
	if t.OutputTokenCount != nil {
		if err := checkNonNegative("output_token_count", *t.OutputTokenCount); err != nil {
			return err
		}
	}
	if t.ReasoningTokenCount != nil {
		if err := checkNonNegative("reasoning_token_count", *t.ReasoningTokenCount); err != nil {
			return err
		}
	}
	for _, child := range t.Children {
		if child.StartedAt.IsZero() {
			return fmt.Errorf("record agent run telemetry: child started_at is required")
		}
		if err := checkNonNegative("child_spawn_depth", int64(child.SpawnDepth)); err != nil {
			return err
		}
		if err := checkNonNegative("child_model_call_count", child.ModelCallCount); err != nil {
			return err
		}
		if err := checkNonNegative("child_tool_call_count", child.ToolCallCount); err != nil {
			return err
		}
		if child.OutputTokenCount != nil {
			if err := checkNonNegative("child_output_token_count", *child.OutputTokenCount); err != nil {
				return err
			}
		}
		if child.ReasoningTokenCount != nil {
			if err := checkNonNegative("child_reasoning_token_count", *child.ReasoningTokenCount); err != nil {
				return err
			}
		}
		if child.StoppedAt != nil && child.StoppedAt.Before(child.StartedAt) {
			return fmt.Errorf("record agent run telemetry: child stopped_at cannot be before started_at")
		}
	}
	return nil
}

func validateAgentRunCounts(run AgentRun) error {
	if err := checkNonNegative("parent_model_call_count", run.ParentModelCallCount); err != nil {
		return err
	}
	if err := checkNonNegative("child_model_call_count", run.ChildModelCallCount); err != nil {
		return err
	}
	if err := checkNonNegative("tool_call_count", run.ToolCallCount); err != nil {
		return err
	}
	if err := checkNonNegative("subagent_count", run.SubagentCount); err != nil {
		return err
	}
	if err := checkNonNegative("direct_subagent_count", run.DirectSubagentCount); err != nil {
		return err
	}
	if run.OutputTokenCount != nil {
		if err := checkNonNegative("output_token_count", *run.OutputTokenCount); err != nil {
			return err
		}
	}
	if run.ReasoningTokenCount != nil {
		if err := checkNonNegative("reasoning_token_count", *run.ReasoningTokenCount); err != nil {
			return err
		}
	}
	return nil
}

func checkNonNegative(name string, value int64) error {
	if value < 0 {
		return fmt.Errorf("record agent run telemetry: %s cannot be negative", name)
	}
	return nil
}

func normalizeAgentRunStopReason(reason string) string {
	switch reason {
	case AgentRunStopClean, AgentRunStopStopped, AgentRunStopUsageExhausted, AgentRunStopRateLimited, AgentRunStopDaemonRestart, AgentRunStopUnknown:
		return reason
	case "":
		return AgentRunStopUnknown
	default:
		log.Warn().Str("stop_reason", reason).Msg("normalizing unrecognized agent run stop reason")
		return AgentRunStopUnknown
	}
}

func optionalTimeString(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	formatted := sqlutil.FormatTime(*t)
	return formatted
}

func parseNullTime(value sql.NullString) *time.Time {
	if !value.Valid || value.String == "" {
		return nil
	}
	t := sqlutil.ParseTime(value.String)
	return &t
}

func optionalInt(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullIntPtr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	out := v.Int64
	return &out
}
