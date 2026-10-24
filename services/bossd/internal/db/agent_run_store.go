package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/recurser/bossd/internal/domain"

	"github.com/rs/zerolog/log"

	"github.com/recurser/bossalib/sqlutil"
	"github.com/recurser/bossd/internal/db/sqlcgen"
)

const (
	AgentRunStopClean          = domain.AgentRunStopClean
	AgentRunStopStopped        = domain.AgentRunStopStopped
	AgentRunStopUsageExhausted = domain.AgentRunStopUsageExhausted
	AgentRunStopRateLimited    = domain.AgentRunStopRateLimited
	AgentRunStopDaemonRestart  = domain.AgentRunStopDaemonRestart
	AgentRunStopUnknown        = domain.AgentRunStopUnknown
)

// Terminal states a boss-build run can end in. The empty string is the recorded
// value for every run that is not a boss-build run (or whose transcript carried
// no terminal-state line) — "not recorded", never "failed".
const (
	AgentRunTerminalReviewReady = domain.AgentRunTerminalReviewReady
	AgentRunTerminalPartial     = domain.AgentRunTerminalPartial
	AgentRunTerminalBlocked     = domain.AgentRunTerminalBlocked
	AgentRunTerminalNoChange    = domain.AgentRunTerminalNoChange
	AgentRunTerminalUnrecorded  = domain.AgentRunTerminalUnrecorded
)

type AgentRunStore interface {
	Create(ctx context.Context, run domain.AgentRun) (domain.AgentRun, error)
	MarkStoppedByAgentSessionID(ctx context.Context, agentSessionID, reason string, stoppedAt time.Time) error
	MarkStopped(ctx context.Context, runID, reason string, stoppedAt time.Time) error
	RecordTelemetry(ctx context.Context, runID string, telemetry domain.AgentRunTelemetry) error
	RecordTelemetryByAgentSessionID(ctx context.Context, agentSessionID string, telemetry domain.AgentRunTelemetry) error
	MarkOpenStopped(ctx context.Context, now time.Time, activeAgentSessionIDs []string) (int64, error)
	List(ctx context.Context, filter domain.AgentRunFilter) ([]domain.AgentRun, error)
	CreateBackfilled(ctx context.Context, params domain.AgentRunBackfillParams) (domain.AgentRunBackfillSummary, error)
}

type SQLiteAgentRunStore struct {
	db *sql.DB
}

func NewAgentRunStore(db *sql.DB) *SQLiteAgentRunStore {
	return &SQLiteAgentRunStore{db: db}
}

func (s *SQLiteAgentRunStore) Create(ctx context.Context, run domain.AgentRun) (domain.AgentRun, error) {
	if run.SessionID == "" {
		return domain.AgentRun{}, fmt.Errorf("start agent run: session ID required")
	}
	if run.AgentSessionID == "" {
		return domain.AgentRun{}, fmt.Errorf("start agent run: agent session ID required")
	}
	if err := validateAgentRunCounts(run); err != nil {
		return domain.AgentRun{}, err
	}
	if run.ID == "" {
		id, err := sqlutil.NewID()
		if err != nil {
			return domain.AgentRun{}, fmt.Errorf("mint agent run id: %w", err)
		}
		run.ID = id
	}
	if run.StartedAt.IsZero() {
		run.StartedAt = time.Now()
	}
	run.StopReason = normalizeAgentRunStopReason(run.StopReason)
	run.TerminalState = NormalizeAgentRunTerminalState(run.TerminalState)
	err := sqlcgen.New(s.db).CreateAgentRun(ctx, agentRunCreateParams(run))
	if err != nil {
		return domain.AgentRun{}, fmt.Errorf("insert agent run: %w", err)
	}
	return run, nil
}

func (s *SQLiteAgentRunStore) MarkStoppedByAgentSessionID(ctx context.Context, agentSessionID, reason string, stoppedAt time.Time) error {
	if agentSessionID == "" {
		return fmt.Errorf("stop agent run: agent session ID required")
	}
	if stoppedAt.IsZero() {
		stoppedAt = time.Now()
	}
	n, err := sqlcgen.New(s.db).MarkAgentRunStoppedByAgentSessionID(ctx, sqlcgen.MarkAgentRunStoppedByAgentSessionIDParams{
		StoppedAt:      timeText(&stoppedAt),
		StopReason:     normalizeAgentRunStopReason(reason),
		UpdatedAt:      sqlutil.TimeNow(),
		AgentSessionID: agentSessionID,
	})
	if err != nil {
		return fmt.Errorf("stop agent run: %w", err)
	}
	if n == 0 {
		return notFound(sql.ErrNoRows)
	}
	return nil
}

func (s *SQLiteAgentRunStore) MarkStopped(ctx context.Context, runID, reason string, stoppedAt time.Time) error {
	if runID == "" {
		return fmt.Errorf("stop agent run: run ID required")
	}
	if stoppedAt.IsZero() {
		stoppedAt = time.Now()
	}
	n, err := sqlcgen.New(s.db).MarkAgentRunStopped(ctx, sqlcgen.MarkAgentRunStoppedParams{
		StoppedAt:  timeText(&stoppedAt),
		StopReason: normalizeAgentRunStopReason(reason),
		UpdatedAt:  sqlutil.TimeNow(),
		ID:         runID,
	})
	if err != nil {
		return fmt.Errorf("stop agent run: %w", err)
	}
	if n == 0 {
		return notFound(sql.ErrNoRows)
	}
	return nil
}

func (s *SQLiteAgentRunStore) RecordTelemetry(ctx context.Context, runID string, telemetry domain.AgentRunTelemetry) error {
	if runID == "" {
		return fmt.Errorf("record agent run telemetry: run ID required")
	}
	return s.recordTelemetry(ctx, runID, telemetry)
}

func (s *SQLiteAgentRunStore) RecordTelemetryByAgentSessionID(ctx context.Context, agentSessionID string, telemetry domain.AgentRunTelemetry) error {
	if agentSessionID == "" {
		return fmt.Errorf("record agent run telemetry: agent session ID required")
	}
	runID, err := s.latestAgentRunID(ctx, agentSessionID)
	if err != nil {
		return err
	}
	return s.RecordTelemetry(ctx, runID, telemetry)
}

func (s *SQLiteAgentRunStore) recordTelemetry(ctx context.Context, runID string, telemetry domain.AgentRunTelemetry) error {
	if err := validateTelemetryCounts(telemetry); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin agent telemetry transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	n, err := sqlcgen.New(tx).RecordAgentRunTelemetry(ctx, sqlcgen.RecordAgentRunTelemetryParams{
		ParentModelCallCount:  telemetry.ParentModelCallCount,
		ChildModelCallCount:   telemetry.ChildModelCallCount,
		ToolCallCount:         telemetry.ToolCallCount,
		SubagentCount:         telemetry.SubagentCount,
		DirectSubagentCount:   telemetry.DirectSubagentCount,
		OutputTokenCount:      telemetry.OutputTokenCount,
		ReasoningTokenCount:   telemetry.ReasoningTokenCount,
		ReviewerDispatchCount: telemetry.ReviewerDispatchCount,
		TerminalState:         NormalizeAgentRunTerminalState(telemetry.TerminalState),
		UpdatedAt:             sqlutil.TimeNow(),
		ID:                    runID,
	})
	if err != nil {
		return fmt.Errorf("update agent run telemetry: %w", err)
	}
	if n == 0 {
		return notFound(sql.ErrNoRows)
	}
	if err := sqlcgen.New(tx).DeleteAgentRunChildren(ctx, runID); err != nil {
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

func insertAgentRunChildren(ctx context.Context, tx *sql.Tx, runID string, children []domain.AgentRunChild) error {
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
		if err := sqlcgen.New(tx).CreateAgentRunChild(ctx, sqlcgen.CreateAgentRunChildParams{
			ID:                  id,
			AgentRunID:          runID,
			SpawnDepth:          child.SpawnDepth,
			StartedAt:           sqlutil.FormatTime(child.StartedAt),
			StoppedAt:           agentRunTimeText(child.StoppedAt),
			AgentSessionID:      child.AgentSessionID,
			ParentAgentID:       child.ParentAgentID,
			ModelCallCount:      child.ModelCallCount,
			ToolCallCount:       child.ToolCallCount,
			OutputTokenCount:    child.OutputTokenCount,
			ReasoningTokenCount: child.ReasoningTokenCount,
		}); err != nil {
			return fmt.Errorf("insert agent run child: %w", err)
		}
	}
	return nil
}

func (s *SQLiteAgentRunStore) latestAgentRunID(ctx context.Context, agentSessionID string) (string, error) {
	id, err := sqlcgen.New(s.db).GetLatestAgentRunID(ctx, agentSessionID)
	if err != nil {
		return "", fmt.Errorf("find latest agent run id: %w", notFound(err))
	}
	return id, nil
}

func (s *SQLiteAgentRunStore) MarkOpenStopped(ctx context.Context, now time.Time, activeAgentSessionIDs []string) (int64, error) {
	if now.IsZero() {
		now = time.Now()
	}
	activeAgentSessionIDs = uniqueNonEmptyStrings(activeAgentSessionIDs)
	n, err := sqlcgen.New(s.db).MarkOpenAgentRunsStopped(ctx, sqlcgen.MarkOpenAgentRunsStoppedParams{
		StoppedAt:  timeText(&now),
		StopReason: AgentRunStopDaemonRestart,
		UpdatedAt:  sqlutil.TimeNow(),
		NoActive:   len(activeAgentSessionIDs) == 0,
		ActiveIds:  activeAgentSessionIDs,
	})
	if err != nil {
		return 0, fmt.Errorf("reconcile open agent runs: %w", err)
	}
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

func (s *SQLiteAgentRunStore) List(ctx context.Context, filter domain.AgentRunFilter) ([]domain.AgentRun, error) {
	rows, err := sqlcgen.New(s.db).ListAgentRuns(ctx, sqlcgen.ListAgentRunsParams{
		FilterSessionID:       optionalText(filter.SessionID),
		FilterAgentName:       optionalText(filter.AgentName),
		FilterRepoDisplayName: optionalText(filter.RepoDisplayName),
		FilterSince:           timeText(filter.Since),
		FilterUntil:           timeText(filter.Until),
		IncludeOpen:           filter.IncludeOpen,
		IncludeAllReasons:     filter.IncludeAllReasons,
		IncludeBackfilled:     filter.IncludeBackfilled,
	})
	if err != nil {
		return nil, fmt.Errorf("list agent runs: %w", err)
	}
	out := mapRows(rows, agentRunFromRow)
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

func (s *SQLiteAgentRunStore) children(ctx context.Context, runID string) ([]domain.AgentRunChild, error) {
	rows, err := sqlcgen.New(s.db).ListAgentRunChildren(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list agent run children: %w", err)
	}
	return mapRows(rows, agentRunChildFromRow), nil
}
func agentRunFromRow(row sqlcgen.ListAgentRunsRow) domain.AgentRun {
	return domain.AgentRun{
		ID:                    row.ID,
		SessionID:             row.SessionID,
		AgentSessionID:        row.AgentSessionID,
		AgentName:             row.AgentName,
		Model:                 row.Model,
		Effort:                row.Effort,
		StopReason:            row.StopReason,
		ParentModelCallCount:  row.ParentModelCallCount,
		ChildModelCallCount:   row.ChildModelCallCount,
		ToolCallCount:         row.ToolCallCount,
		SubagentCount:         row.SubagentCount,
		DirectSubagentCount:   row.DirectSubagentCount,
		OutputTokenCount:      row.OutputTokenCount,
		ReasoningTokenCount:   row.ReasoningTokenCount,
		ReviewerDispatchCount: row.ReviewerDispatchCount,
		TerminalState:         row.TerminalState,
		IsBackfilled:          row.IsBackfilled,
		RepoDisplayName:       row.RepoDisplayName,
		StartedAt:             textTime(row.StartedAt),
		StoppedAt:             agentRunTimeFromRow(row.StoppedAt),
	}
}
func agentRunChildFromRow(row sqlcgen.AgentRunChild) domain.AgentRunChild {
	return domain.AgentRunChild{
		ID:                  row.ID,
		AgentRunID:          row.AgentRunID,
		AgentSessionID:      row.AgentSessionID,
		ParentAgentID:       row.ParentAgentID,
		ModelCallCount:      row.ModelCallCount,
		ToolCallCount:       row.ToolCallCount,
		OutputTokenCount:    row.OutputTokenCount,
		ReasoningTokenCount: row.ReasoningTokenCount,
		SpawnDepth:          row.SpawnDepth,
		StartedAt:           textTime(row.StartedAt),
		StoppedAt:           agentRunTimeFromRow(row.StoppedAt),
	}
}

// Preserve the legacy treatment of zero and malformed optional agent-run timestamps.
func agentRunTimeText(t *time.Time) *string {
	if t == nil || t.IsZero() {
		return nil
	}
	return timeText(t)
}
func agentRunTimeFromRow(t *string) *time.Time {
	if t == nil || *t == "" {
		return nil
	}
	parsed := textTime(*t)
	return &parsed
}

func validateTelemetryCounts(t domain.AgentRunTelemetry) error {
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
	if err := checkNonNegative("reviewer_dispatch_count", t.ReviewerDispatchCount); err != nil {
		return err
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

func validateAgentRunCounts(run domain.AgentRun) error {
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
	if err := checkNonNegative("reviewer_dispatch_count", run.ReviewerDispatchCount); err != nil {
		return err
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

// NormalizeAgentRunTerminalState keeps an unrecognized token out of the column
// rather than letting it fail the migration's CHECK at write time. The SQL CHECK
// is the backstop; this is the same belt-and-braces shape stop_reason uses, and
// it is what makes an unexpected token a warned-and-dropped value instead of a
// lost telemetry write.
func NormalizeAgentRunTerminalState(state string) string {
	switch state {
	case AgentRunTerminalReviewReady, AgentRunTerminalPartial, AgentRunTerminalBlocked,
		AgentRunTerminalNoChange, AgentRunTerminalUnrecorded:
		return state
	default:
		log.Warn().Str("terminal_state", state).Msg("normalizing unrecognized agent run terminal state")
		return AgentRunTerminalUnrecorded
	}
}

func agentRunCreateParams(run domain.AgentRun) sqlcgen.CreateAgentRunParams {
	return sqlcgen.CreateAgentRunParams{
		ID:                    run.ID,
		SessionID:             run.SessionID,
		AgentSessionID:        run.AgentSessionID,
		AgentName:             run.AgentName,
		Model:                 run.Model,
		Effort:                run.Effort,
		StopReason:            run.StopReason,
		ParentModelCallCount:  run.ParentModelCallCount,
		ChildModelCallCount:   run.ChildModelCallCount,
		ToolCallCount:         run.ToolCallCount,
		SubagentCount:         run.SubagentCount,
		DirectSubagentCount:   run.DirectSubagentCount,
		OutputTokenCount:      run.OutputTokenCount,
		ReasoningTokenCount:   run.ReasoningTokenCount,
		ReviewerDispatchCount: run.ReviewerDispatchCount,
		TerminalState:         run.TerminalState,
		IsBackfilled:          run.IsBackfilled,
		StartedAt:             sqlutil.FormatTime(run.StartedAt),
		StoppedAt:             agentRunTimeText(run.StoppedAt),
		UpdatedAt:             sqlutil.TimeNow(),
	}
}
