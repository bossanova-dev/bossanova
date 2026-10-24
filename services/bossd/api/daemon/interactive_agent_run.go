package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/domain/agenttelemetry"
	"github.com/recurser/bossd/internal/session"
)

func (s *Server) recordInteractiveAgentRunStart(ctx context.Context, sess *models.Session, chat *domain.AgentChat, startedAt time.Time) {
	if s.agentRuns == nil || sess == nil || chat == nil || chat.AgentSessionID == "" {
		return
	}
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), interactiveAgentRunRecordTimeout)
	s.recordStaleInteractiveAgentRunStopped(cleanupCtx, sess, chat, startedAt)
	cleanupCancel()

	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), interactiveAgentRunRecordTimeout)
	defer cancel()
	// Both session fallbacks go through the agent-aware helper: a run record
	// that credits a claude model id to a codex run is wrong in the telemetry
	// AND in the cost join that reads it (BOS-1281). A cross-agent chat with no
	// model of its own records "" — the same "the runner resolved its own
	// default" answer the spawn path stores.
	model := chat.Model
	if model == "" {
		model = session.EffectiveModelForAgent(sess.AgentName, sess.EffectiveModel, chat.AgentName)
	}
	if model == "" {
		model = session.EffectiveModelForAgent(sess.AgentName, sess.Model, chat.AgentName)
	}
	effort := session.EffectiveEffortForAgent(sess.AgentName, sess.EffectiveEffort, chat.AgentName)
	if _, err := s.agentRuns.Create(recordCtx, domain.AgentRun{
		SessionID:      sess.ID,
		AgentSessionID: chat.AgentSessionID,
		AgentName:      chat.AgentName,
		Model:          model,
		Effort:         effort,
		StartedAt:      startedAt,
	}); err != nil {
		s.logger.Warn().Err(err).
			Str("session", sess.ID).
			Str("agent_session", chat.AgentSessionID).
			Str("agent", chat.AgentName).
			Msg("record interactive agent run start failed")
	}
}

func (s *Server) recordStaleInteractiveAgentRunStopped(ctx context.Context, sess *models.Session, chat *domain.AgentChat, stoppedAt time.Time) {
	run, ok := s.openInteractiveAgentRunAt(ctx, sess.ID, chat.AgentSessionID, stoppedAt)
	if !ok {
		return
	}
	s.recordInteractiveAgentRunTelemetry(ctx, sess, chat, run, stoppedAt)
	if err := s.agentRuns.MarkStopped(ctx, run.ID, domain.AgentRunStopStopped, stoppedAt); err != nil && !errors.Is(err, domain.ErrNotFound) {
		s.logger.Warn().Err(err).
			Str("session", sess.ID).
			Str("agent_session", chat.AgentSessionID).
			Str("agent", chat.AgentName).
			Str("run", run.ID).
			Msg("close stale interactive agent run failed")
	}
}

func (s *Server) openInteractiveAgentRunAt(ctx context.Context, sessionID, agentSessionID string, observedAt time.Time) (domain.AgentRun, bool) {
	runs, err := s.agentRuns.List(ctx, domain.AgentRunFilter{
		SessionID:         sessionID,
		IncludeOpen:       true,
		IncludeAllReasons: true,
		IncludeBackfilled: true,
	})
	if err != nil {
		s.logger.Warn().Err(err).Str("session", sessionID).Str("agent_session", agentSessionID).Msg("lookup stale interactive agent run failed")
		return domain.AgentRun{}, false
	}
	var selected domain.AgentRun
	for _, run := range runs {
		if run.AgentSessionID != agentSessionID || run.StoppedAt != nil {
			continue
		}
		if !observedAt.IsZero() && run.StartedAt.After(observedAt) {
			continue
		}
		if selected.ID == "" || run.StartedAt.After(selected.StartedAt) {
			selected = run
		}
	}
	return selected, selected.ID != ""
}

func (s *Server) recordInteractiveAgentRunTelemetry(ctx context.Context, sess *models.Session, chat *domain.AgentChat, run domain.AgentRun, until time.Time) {
	counts, err := tallyInteractiveAgentRunForServer(ctx, sess.WorktreePath, chat, run.StartedAt, until)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.logger.Warn().Err(err).
				Str("session", sess.ID).
				Str("agent_session", chat.AgentSessionID).
				Str("agent", chat.AgentName).
				Str("run", run.ID).
				Msg("tally stale interactive agent run telemetry failed")
		}
		return
	}
	if err := s.agentRuns.RecordTelemetry(ctx, run.ID, domain.AgentRunTelemetryFromCounts(counts)); err != nil && !errors.Is(err, domain.ErrNotFound) {
		s.logger.Warn().Err(err).
			Str("session", sess.ID).
			Str("agent_session", chat.AgentSessionID).
			Str("agent", chat.AgentName).
			Str("run", run.ID).
			Msg("record stale interactive agent run telemetry failed")
	}
}

func tallyInteractiveAgentRunForServer(ctx context.Context, worktreePath string, chat *domain.AgentChat, since, until time.Time) (agenttelemetry.Counts, error) {
	transcripts, err := interactiveTranscriptPaths(worktreePath, chat)
	if err != nil {
		return agenttelemetry.Counts{}, err
	}
	if chat.AgentName == "claude" {
		return agenttelemetry.TallyClaudePathWithChildrenWindowContext(ctx, transcripts[0], filepath.Dir(transcripts[0]), chatResumeSessionIDForServer(chat), since, until)
	}
	return agenttelemetry.TallyCodexPathsWindowContext(ctx, transcripts, since, until)
}

// interactiveTranscriptPaths resolves the transcript files a chat's tally
// reads: a claude chat's own transcript (its subagents' files add counts but
// never its LastOutputAt), or every codex rollout of the chat.
func interactiveTranscriptPaths(worktreePath string, chat *domain.AgentChat) ([]string, error) {
	if chat == nil || chat.AgentSessionID == "" {
		return nil, os.ErrNotExist
	}
	switch chat.AgentName {
	case "claude":
		transcript, err := agenttelemetry.ClaudeTranscriptPath(worktreePath, chatResumeSessionIDForServer(chat))
		if err != nil {
			return nil, err
		}
		return []string{transcript}, nil
	case "codex":
		return agenttelemetry.CodexTranscriptPaths(chatResumeSessionIDForServer(chat))
	default:
		return nil, os.ErrNotExist
	}
}
