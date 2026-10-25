package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/recurser/bossd/internal/db/sqlcgen"
	"github.com/recurser/bossd/internal/domain"

	"github.com/recurser/bossalib/contract/cloud/sqlutil"
	"github.com/recurser/bossalib/contract/plugin/jsonlscan"
	"github.com/recurser/bossalib/kit/config"
	"github.com/recurser/bossd/internal/domain/agenttelemetry"
)

const (
	codexBackfillAdjacentGap = 30 * time.Minute
	backfillRunWindowSlack   = 2 * time.Minute
)

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

func (s *SQLiteAgentRunStore) CreateBackfilled(ctx context.Context, params domain.AgentRunBackfillParams) (domain.AgentRunBackfillSummary, error) {
	sessions, err := s.backfillSessions(ctx)
	if err != nil {
		return domain.AgentRunBackfillSummary{}, err
	}
	candidates, err := backfillCandidates(params)
	if err != nil {
		return domain.AgentRunBackfillSummary{}, err
	}
	var summary domain.AgentRunBackfillSummary
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
		if err := s.startBackfilledRun(ctx, domain.AgentRun{
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
		}, domain.AgentRunTelemetryFromCounts(candidate.Counts)); err != nil {
			return summary, err
		}
		summary.InsertedCount++
	}
	return summary, nil
}

func (s *SQLiteAgentRunStore) startBackfilledRun(ctx context.Context, run domain.AgentRun, telemetry domain.AgentRunTelemetry) error {
	if run.SessionID == "" {
		return fmt.Errorf("start backfilled agent run: session ID required")
	}
	if run.AgentSessionID == "" {
		return fmt.Errorf("start backfilled agent run: agent session ID required")
	}
	if err := validateAgentRunCounts(run); err != nil {
		return err
	}
	if err := validateTelemetryCounts(telemetry); err != nil {
		return err
	}
	run.TerminalState = NormalizeAgentRunTerminalState(run.TerminalState)
	if run.ID == "" {
		id, err := sqlutil.NewID()
		if err != nil {
			return fmt.Errorf("mint agent run id: %w", err)
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
		return fmt.Errorf("begin backfilled agent run transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := sqlcgen.New(tx).CreateAgentRun(ctx, agentRunCreateParams(run)); err != nil {
		return fmt.Errorf("insert backfilled agent run: %w", err)
	}
	if err := insertAgentRunChildren(ctx, tx, run.ID, telemetry.Children); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit backfilled agent run transaction: %w", err)
	}
	return nil
}

func (s *SQLiteAgentRunStore) backfillSessions(ctx context.Context) ([]backfillSession, error) {
	rows, err := sqlcgen.New(s.db).ListAgentRunBackfillSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list sessions for run backfill: %w", err)
	}
	var out []backfillSession
	byID := make(map[string]int)
	for _, row := range rows {
		sess := backfillSession{ID: row.ID, WorktreePath: row.WorktreePath, AgentName: row.AgentName, Model: row.Model, Effort: row.EffectiveEffort}

		idx, ok := byID[sess.ID]
		if !ok {
			sess.AgentSessionID = textValue(row.AgentSessionID)
			sess.CreatedAt = textTime(row.CreatedAt)
			if row.ArchivedAt != nil {
				archivedAt := textTime(*row.ArchivedAt)
				sess.ArchivedAt = &archivedAt
			}
			byID[sess.ID] = len(out)
			out = append(out, sess)
			idx = len(out) - 1
		}
		if row.ProviderSessionID != "" {
			out[idx].ProviderSessions = append(out[idx].ProviderSessions, backfillProviderSession{
				ID:        row.ProviderSessionID,
				AgentName: row.ProviderAgentName,
				Model:     row.ProviderModel,
				Effort:    backfillEffectiveEffortForAgent(sess.AgentName, sess.Effort, row.ProviderAgentName),
			})
		}
	}
	return out, nil
}

func (s *SQLiteAgentRunStore) backfillRunExists(ctx context.Context, sessionID string, candidate backfillCandidate) (bool, error) {
	lower := candidate.StartedAt.Add(-backfillRunWindowSlack)
	upper := candidate.StoppedAt.Add(backfillRunWindowSlack)
	if upper.Before(lower) {
		upper = lower
	}
	_, err := sqlcgen.New(s.db).GetExistingAgentRunBackfill(ctx, sqlcgen.GetExistingAgentRunBackfillParams{
		SessionID:         sessionID,
		AgentName:         candidate.AgentName,
		AgentSessionID:    candidate.AgentSessionID,
		ProviderSessionID: &candidate.AgentSessionID,
		StartedAt:         sqlutil.FormatTime(lower),
		StartedAt_2:       sqlutil.FormatTime(upper),
	})
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("find existing backfilled agent run: %w", err)
	}
	return true, nil
}

func backfillCandidates(params domain.AgentRunBackfillParams) ([]backfillCandidate, error) {
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

func claudeBackfillCandidates(params domain.AgentRunBackfillParams) ([]backfillCandidate, error) {
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

func codexBackfillCandidates(params domain.AgentRunBackfillParams) ([]backfillCandidate, error) {
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
		relativePath, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		f, err := os.OpenInRoot(root, relativePath)
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

func filterBackfillWindow(candidates []backfillCandidate, params domain.AgentRunBackfillParams) []backfillCandidate {
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
	// Line() counts skipped records too, so a value other than 1 means the real
	// first record was over budget and this is a later one. The meta header is
	// line 1 by contract, so a skip here is a miss rather than a fallback —
	// otherwise line 2 is unmarshalled as the header and yields a wrong
	// id/cwd/timestamp instead of a clean "not found".
	if !scanner.Scan() || scanner.Line() != 1 {
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

// newBackfillScanner is the one place this package names its JSONL reader, so
// its two callers cannot drift apart or reintroduce the private 10 MiB ceiling
// this used to carry. Which budget that is, and why, now lives in jsonlscan's
// package doc rather than here.
func newBackfillScanner(f *os.File) *jsonlscan.Reader {
	return jsonlscan.New(f)
}

func inBackfillWindow(start time.Time, params domain.AgentRunBackfillParams) bool {
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
