package db

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/recurser/bossalib/agenttelemetry"
	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/sqlutil"
)

const (
	codexBackfillAdjacentGap = 30 * time.Minute
	backfillRunWindowSlack   = 2 * time.Minute
)

// AgentRunBackfillParams bounds a transcript replay.
type AgentRunBackfillParams struct {
	Since *time.Time
	Until *time.Time
}

type AgentRunBackfillSummary struct {
	InsertedCount int64
	SkippedCount  int64
}

type backfillSession struct {
	ID               string
	AgentSessionID   string
	ProviderSessions []backfillProviderSession
	WorktreePath     string
	AgentName        string
	Model            string
	Effort           string
	CreatedAt        time.Time
	ArchivedAt       *time.Time
}

type backfillProviderSession struct {
	ID        string
	AgentName string
	Model     string
	Effort    string
}

type backfillCandidate struct {
	AgentSessionID string
	AgentName      string
	Path           string
	ProjectKey     string
	WorktreePath   string
	StartedAt      time.Time
	StoppedAt      time.Time
	Counts         agenttelemetry.Counts
}

func (s *SQLiteAgentRunStore) Backfill(ctx context.Context, params AgentRunBackfillParams) (AgentRunBackfillSummary, error) {
	sessions, err := s.backfillSessions(ctx)
	if err != nil {
		return AgentRunBackfillSummary{}, err
	}
	candidates, err := backfillCandidates(params)
	if err != nil {
		return AgentRunBackfillSummary{}, err
	}
	var summary AgentRunBackfillSummary
	for _, candidate := range candidates {
		session, ok := matchBackfillSession(candidate, sessions)
		if !ok {
			summary.SkippedCount++
			continue
		}
		exists, err := s.backfillRunExists(ctx, session.ID, candidate)
		if err != nil {
			return summary, err
		}
		if exists {
			summary.SkippedCount++
			continue
		}
		if _, err := s.startBackfilledRun(ctx, AgentRun{
			SessionID:            session.ID,
			AgentSessionID:       candidate.AgentSessionID,
			AgentName:            candidate.AgentName,
			Model:                session.Model,
			Effort:               session.Effort,
			StartedAt:            candidate.StartedAt,
			StoppedAt:            &candidate.StoppedAt,
			StopReason:           AgentRunStopClean,
			ParentModelCallCount: candidate.Counts.ParentModelCallCount,
			ChildModelCallCount:  candidate.Counts.ChildModelCallCount,
			ToolCallCount:        candidate.Counts.ToolCallCount,
			SubagentCount:        candidate.Counts.SubagentCount,
			DirectSubagentCount:  candidate.Counts.DirectSubagentCount,
			OutputTokenCount:     candidate.Counts.OutputTokenCount,
			ReasoningTokenCount:  candidate.Counts.ReasoningTokenCount,

			ReviewerDispatchCount: candidate.Counts.ReviewerDispatchCount,
			TerminalState:         candidate.Counts.TerminalState,
			IsBackfilled:          true,
		}, telemetryFromCounts(candidate.Counts)); err != nil {
			return summary, err
		}
		summary.InsertedCount++
	}
	return summary, nil
}

func (s *SQLiteAgentRunStore) startBackfilledRun(ctx context.Context, run AgentRun, telemetry AgentRunTelemetry) (AgentRun, error) {
	if run.SessionID == "" {
		return AgentRun{}, fmt.Errorf("start backfilled agent run: session ID required")
	}
	if run.AgentSessionID == "" {
		return AgentRun{}, fmt.Errorf("start backfilled agent run: agent session ID required")
	}
	if err := validateAgentRunCounts(run); err != nil {
		return AgentRun{}, err
	}
	if err := validateTelemetryCounts(telemetry); err != nil {
		return AgentRun{}, err
	}
	run.TerminalState = NormalizeAgentRunTerminalState(run.TerminalState)
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
	run.IsBackfilled = true

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AgentRun{}, fmt.Errorf("begin backfilled agent run transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO agent_runs
		   (id, session_id, agent_session_id, agent_name, model, effort, started_at, stopped_at,
		    stop_reason, parent_model_call_count, child_model_call_count, tool_call_count,
		    subagent_count, direct_subagent_count, output_token_count, reasoning_token_count,
		    reviewer_dispatch_count, terminal_state, is_backfilled, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.ID, run.SessionID, run.AgentSessionID, run.AgentName, run.Model, run.Effort,
		sqlutil.FormatTime(run.StartedAt), optionalTimeString(run.StoppedAt), run.StopReason,
		run.ParentModelCallCount, run.ChildModelCallCount, run.ToolCallCount,
		run.SubagentCount, run.DirectSubagentCount, optionalInt(run.OutputTokenCount),
		optionalInt(run.ReasoningTokenCount), run.ReviewerDispatchCount, run.TerminalState,
		sqlutil.BoolToInt(run.IsBackfilled), sqlutil.TimeNow()); err != nil {
		return AgentRun{}, fmt.Errorf("insert backfilled agent run: %w", err)
	}
	if err := insertAgentRunChildren(ctx, tx, run.ID, telemetry.Children); err != nil {
		return AgentRun{}, err
	}
	if err := tx.Commit(); err != nil {
		return AgentRun{}, fmt.Errorf("commit backfilled agent run transaction: %w", err)
	}
	return run, nil
}

func (s *SQLiteAgentRunStore) backfillSessions(ctx context.Context) ([]backfillSession, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT sessions.id,
		       sessions.agent_session_id,
		       COALESCE(agent_chats.provider_session_id, ''),
		       COALESCE(NULLIF(agent_chats.agent_name, ''), ''),
		       COALESCE(NULLIF(agent_chats.model, ''), ''),
		       sessions.worktree_path,
		       sessions.agent_name,
		       COALESCE(NULLIF(sessions.effective_model, ''), sessions.model),
		       sessions.effective_effort,
		       sessions.created_at,
		       sessions.archived_at
		  FROM sessions
		  LEFT JOIN agent_chats
		    ON agent_chats.session_id = sessions.id
		   AND agent_chats.provider_session_id IS NOT NULL
		   AND agent_chats.provider_session_id != ''
		 ORDER BY sessions.created_at, sessions.id, agent_chats.created_at`)
	if err != nil {
		return nil, fmt.Errorf("list sessions for run backfill: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []backfillSession
	byID := make(map[string]int)
	for rows.Next() {
		var sess backfillSession
		var agentSessionID sql.NullString
		var providerSessionID string
		var providerAgentName string
		var providerModel string
		var archivedAt sql.NullString
		var createdAt string
		if err := rows.Scan(&sess.ID, &agentSessionID, &providerSessionID, &providerAgentName, &providerModel, &sess.WorktreePath, &sess.AgentName, &sess.Model, &sess.Effort, &createdAt, &archivedAt); err != nil {
			return nil, fmt.Errorf("scan run backfill session: %w", err)
		}
		idx, ok := byID[sess.ID]
		if !ok {
			sess.AgentSessionID = agentSessionID.String
			sess.CreatedAt = sqlutil.ParseTime(createdAt)
			if archivedAt.Valid {
				t := sqlutil.ParseTime(archivedAt.String)
				sess.ArchivedAt = &t
			}
			byID[sess.ID] = len(out)
			out = append(out, sess)
			idx = len(out) - 1
		}
		if providerSessionID != "" {
			out[idx].ProviderSessions = append(out[idx].ProviderSessions, backfillProviderSession{
				ID:        providerSessionID,
				AgentName: providerAgentName,
				Model:     providerModel,
				Effort:    backfillEffectiveEffortForAgent(sess.AgentName, sess.Effort, providerAgentName),
			})
		}
	}
	return out, rows.Err()
}

func (s *SQLiteAgentRunStore) backfillRunExists(ctx context.Context, sessionID string, candidate backfillCandidate) (bool, error) {
	var one int
	lower := candidate.StartedAt.Add(-backfillRunWindowSlack)
	upper := candidate.StoppedAt.Add(backfillRunWindowSlack)
	if upper.Before(lower) {
		upper = lower
	}
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM agent_runs
		 WHERE session_id = ? AND agent_name = ?
		   AND (agent_session_id = ? OR EXISTS (
		     SELECT 1 FROM agent_chats
		      WHERE agent_chats.session_id = agent_runs.session_id
		        AND agent_chats.agent_session_id = agent_runs.agent_session_id
		        AND agent_chats.provider_session_id = ?
		   ))
		   AND started_at >= ? AND started_at <= ?
		 LIMIT 1`,
		sessionID, candidate.AgentName, candidate.AgentSessionID, candidate.AgentSessionID, sqlutil.FormatTime(lower), sqlutil.FormatTime(upper)).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("find existing backfilled agent run: %w", err)
	}
	return true, nil
}

func backfillCandidates(params AgentRunBackfillParams) ([]backfillCandidate, error) {
	var out []backfillCandidate
	claude, err := claudeBackfillCandidates(params)
	if err != nil {
		return nil, err
	}
	out = append(out, claude...)
	codex, err := codexBackfillCandidates(params)
	if err != nil {
		return nil, err
	}
	out = append(out, codex...)
	return out, nil
}

func claudeBackfillCandidates(params AgentRunBackfillParams) ([]backfillCandidate, error) {
	root := filepath.Join(os.Getenv("HOME"), ".claude", "projects")
	var out []backfillCandidate
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		if strings.Contains(path, string(filepath.Separator)+"subagents"+string(filepath.Separator)) {
			return nil
		}
		counts, err := agenttelemetry.TallyClaudePath(path)
		if err != nil {
			return nil
		}
		start, stop, ok := transcriptBounds(path)
		if !ok || !inBackfillWindow(start, params) {
			return nil
		}
		out = append(out, backfillCandidate{
			AgentSessionID: strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)),
			AgentName:      "claude",
			Path:           path,
			ProjectKey:     filepath.Base(filepath.Dir(path)),
			StartedAt:      start,
			StoppedAt:      stop,
			Counts:         counts,
		})
		return nil
	})
	if os.IsNotExist(err) {
		return nil, nil
	}
	return out, err
}

func codexBackfillCandidates(params AgentRunBackfillParams) ([]backfillCandidate, error) {
	root := codexBackfillRoot()
	var out []backfillCandidate
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		meta, ok := readCodexMeta(path)
		if !ok {
			return nil
		}
		f, err := os.Open(filepath.Clean(path))
		if err != nil {
			return nil
		}
		counts, tallyErr := agenttelemetry.TallyCodex(f)
		_ = f.Close()
		if tallyErr != nil {
			return nil
		}
		_, stop, ok := transcriptBounds(path)
		if !ok {
			stop = meta.StartedAt
		}
		out = append(out, backfillCandidate{
			AgentSessionID: meta.ID,
			AgentName:      "codex",
			Path:           path,
			WorktreePath:   meta.CWD,
			StartedAt:      meta.StartedAt,
			StoppedAt:      stop,
			Counts:         counts,
		})
		return nil
	})
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return filterBackfillWindow(groupCodexBackfillCandidates(out), params), nil
}

func codexBackfillRoot() string {
	if codexHome := strings.TrimSpace(os.Getenv("CODEX_HOME")); codexHome != "" {
		return filepath.Join(codexHome, "sessions")
	}
	return filepath.Join(os.Getenv("HOME"), ".codex", "sessions")
}

func groupCodexBackfillCandidates(candidates []backfillCandidate) []backfillCandidate {
	if len(candidates) <= 1 {
		return candidates
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].AgentSessionID == candidates[j].AgentSessionID {
			return candidates[i].StartedAt.Before(candidates[j].StartedAt)
		}
		return candidates[i].AgentSessionID < candidates[j].AgentSessionID
	})
	out := make([]backfillCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if len(out) == 0 || !sameCodexBackfillRun(out[len(out)-1], candidate) {
			out = append(out, candidate)
			continue
		}
		mergeBackfillCandidate(&out[len(out)-1], candidate)
	}
	return out
}

func filterBackfillWindow(candidates []backfillCandidate, params AgentRunBackfillParams) []backfillCandidate {
	if params.Since == nil && params.Until == nil {
		return candidates
	}
	out := candidates[:0]
	for _, candidate := range candidates {
		if inBackfillWindow(candidate.StartedAt, params) {
			out = append(out, candidate)
		}
	}
	return out
}

func sameCodexBackfillRun(a, b backfillCandidate) bool {
	return a.AgentName == "codex" &&
		b.AgentName == "codex" &&
		a.AgentSessionID != "" &&
		a.AgentSessionID == b.AgentSessionID &&
		agenttelemetry.SameWorkDir(a.WorktreePath, b.WorktreePath) &&
		!b.StartedAt.After(a.StoppedAt.Add(codexBackfillAdjacentGap))
}

func mergeBackfillCandidate(dst *backfillCandidate, src backfillCandidate) {
	if src.StartedAt.Before(dst.StartedAt) {
		dst.StartedAt = src.StartedAt
	}
	if src.StoppedAt.After(dst.StoppedAt) {
		dst.StoppedAt = src.StoppedAt
	}
	dst.Path += string(os.PathListSeparator) + src.Path
	dst.Counts.ParentModelCallCount += src.Counts.ParentModelCallCount
	dst.Counts.ChildModelCallCount += src.Counts.ChildModelCallCount
	dst.Counts.ToolCallCount += src.Counts.ToolCallCount
	dst.Counts.SubagentCount += src.Counts.SubagentCount
	dst.Counts.DirectSubagentCount += src.Counts.DirectSubagentCount
	dst.Counts.ReviewerDispatchCount += src.Counts.ReviewerDispatchCount
	// Grouped codex segments are one logical run replayed across several transcript
	// files, so the run's terminal state is whichever segment last printed one. A
	// later segment that printed none must not erase an earlier segment's state.
	if src.Counts.TerminalState != "" {
		dst.Counts.TerminalState = src.Counts.TerminalState
	}
	if src.Counts.OutputTokenCount != nil {
		addIntPtr(&dst.Counts.OutputTokenCount, *src.Counts.OutputTokenCount)
	}
	if src.Counts.ReasoningTokenCount != nil {
		addIntPtr(&dst.Counts.ReasoningTokenCount, *src.Counts.ReasoningTokenCount)
	}
	dst.Counts.Children = append(dst.Counts.Children, src.Counts.Children...)
}

func addIntPtr(dst **int64, value int64) {
	if *dst == nil {
		v := int64(0)
		*dst = &v
	}
	**dst += value
}

type codexMeta struct {
	ID        string
	CWD       string
	StartedAt time.Time
}

func readCodexMeta(path string) (codexMeta, bool) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return codexMeta{}, false
	}
	defer func() { _ = f.Close() }()
	scanner := newBackfillScanner(f)
	if !scanner.Scan() {
		return codexMeta{}, false
	}
	var line struct {
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &line); err != nil || len(line.Payload) == 0 {
		return codexMeta{}, false
	}
	var payload struct {
		ID        string `json:"id"`
		Timestamp string `json:"timestamp"`
		CWD       string `json:"cwd"`
	}
	if err := json.Unmarshal(line.Payload, &payload); err != nil {
		return codexMeta{}, false
	}
	started, err := time.Parse(time.RFC3339Nano, payload.Timestamp)
	if err != nil || payload.ID == "" || payload.CWD == "" {
		return codexMeta{}, false
	}
	return codexMeta{ID: payload.ID, CWD: payload.CWD, StartedAt: started}, true
}

func transcriptBounds(path string) (time.Time, time.Time, bool) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	defer func() { _ = f.Close() }()
	scanner := newBackfillScanner(f)
	var first, last time.Time
	for scanner.Scan() {
		var line struct {
			Timestamp string `json:"timestamp"`
			TS        string `json:"ts"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			continue
		}
		value := line.Timestamp
		if value == "" {
			value = line.TS
		}
		ts, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			continue
		}
		if first.IsZero() {
			first = ts
		}
		last = ts
	}
	return first, last, !first.IsZero() && !last.IsZero()
}

func newBackfillScanner(f *os.File) *bufio.Scanner {
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
	return scanner
}

func inBackfillWindow(start time.Time, params AgentRunBackfillParams) bool {
	if params.Since != nil && start.Before(*params.Since) {
		return false
	}
	if params.Until != nil && !start.Before(*params.Until) {
		return false
	}
	return true
}

func matchBackfillSession(candidate backfillCandidate, sessions []backfillSession) (backfillSession, bool) {
	var best backfillSession
	var found bool
	for _, session := range sessions {
		if candidate.AgentSessionID != "" && session.AgentSessionID == candidate.AgentSessionID {
			return session, true
		}
		if candidate.AgentSessionID != "" {
			if provider, ok := backfillSessionProvider(session, candidate.AgentSessionID); ok {
				matched := session
				if provider.Model != "" {
					matched.Model = provider.Model
				}
				if provider.Effort != "" {
					matched.Effort = provider.Effort
				}
				return matched, true
			}
		}
		if candidate.AgentName != "" && session.AgentName != "" && candidate.AgentName != session.AgentName {
			continue
		}
		if candidate.WorktreePath != "" && !agenttelemetry.SameWorkDir(candidate.WorktreePath, session.WorktreePath) {
			continue
		}
		if candidate.WorktreePath == "" && (candidate.AgentName != "claude" || candidate.ProjectKey != projectKey(session.WorktreePath)) {
			continue
		}
		if !sessionCoversBackfillCandidate(session, candidate) {
			continue
		}
		if !found || session.CreatedAt.After(best.CreatedAt) {
			best = session
			found = true
		}
	}
	return best, found
}

func backfillSessionProvider(session backfillSession, providerSessionID string) (backfillProviderSession, bool) {
	for _, candidate := range session.ProviderSessions {
		if candidate.ID == providerSessionID {
			return candidate, true
		}
	}
	return backfillProviderSession{}, false
}

func backfillEffectiveEffortForAgent(sessionAgentName, sessionEffectiveEffort, providerAgentName string) string {
	if sessionAgentName == "" || providerAgentName == "" || providerAgentName == sessionAgentName {
		return sessionEffectiveEffort
	}
	if settings, err := config.Load(); err == nil {
		if value := config.PluginConfigString(&settings, providerAgentName, "effort"); value != "" {
			return value
		}
	}
	switch providerAgentName {
	case "claude":
		return "high"
	case "codex":
		return "medium"
	default:
		return ""
	}
}

func sessionCoversBackfillCandidate(session backfillSession, candidate backfillCandidate) bool {
	if !session.CreatedAt.IsZero() && candidate.StoppedAt.Before(session.CreatedAt) {
		return false
	}
	if session.ArchivedAt != nil && candidate.StartedAt.After(*session.ArchivedAt) {
		return false
	}
	return true
}

func projectKey(path string) string {
	var b strings.Builder
	for _, r := range filepath.Clean(path) {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

func telemetryFromCounts(counts agenttelemetry.Counts) AgentRunTelemetry {
	out := AgentRunTelemetry{
		ParentModelCallCount: counts.ParentModelCallCount,
		ChildModelCallCount:  counts.ChildModelCallCount,
		ToolCallCount:        counts.ToolCallCount,
		SubagentCount:        counts.SubagentCount,
		DirectSubagentCount:  counts.DirectSubagentCount,
		OutputTokenCount:     counts.OutputTokenCount,
		ReasoningTokenCount:  counts.ReasoningTokenCount,

		ReviewerDispatchCount: counts.ReviewerDispatchCount,
		TerminalState:         counts.TerminalState,
	}
	for _, child := range counts.Children {
		out.Children = append(out.Children, AgentRunChild{
			AgentSessionID:      child.AgentSessionID,
			ParentAgentID:       child.ParentAgentID,
			SpawnDepth:          child.SpawnDepth,
			StartedAt:           child.StartedAt,
			StoppedAt:           child.StoppedAt,
			ModelCallCount:      child.ModelCallCount,
			ToolCallCount:       child.ToolCallCount,
			OutputTokenCount:    child.OutputTokenCount,
			ReasoningTokenCount: child.ReasoningTokenCount,
		})
	}
	return out
}
