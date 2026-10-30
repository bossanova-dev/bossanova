package session

import (
	"context"
	"strings"
	"time"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sessionreason"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/domain"
)

// sessionTurnErrored reads the newest turn outcome only after callers establish
// idle evidence. Unknown transcripts never identify a failed run.
func (l *Lifecycle) sessionTurnErrored(ctx context.Context, s *models.Session) (string, bool) {
	if s == nil || s.AgentSessionID == nil || l.agentChats == nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	chats, err := l.agentChats.ListBySession(ctx, s.ID)
	if err != nil {
		return "", false
	}
	chat := runChat(chats, *s.AgentSessionID)
	if chat == nil {
		return "", false
	}
	client := l.agents[chat.AgentName]
	if client == nil {
		return "", false
	}
	ids := []string{chat.AgentSessionID}
	if p := knownProviderSessionID(chat); p != "" && p != chat.AgentSessionID {
		ids = []string{p, chat.AgentSessionID}
	}
	for _, id := range ids {
		message, exists := agent.ReadLastTurnError(ctx, client, s.WorktreePath, id)
		if !exists {
			continue
		}
		message = strings.TrimSpace(message)
		return message, message != ""
	}
	return "", false
}

// SessionTurnErrored exposes the newest idle turn outcome to the completion gate.
func (l *Lifecycle) SessionTurnErrored(s *models.Session) bool {
	_, errored := l.sessionTurnErrored(context.Background(), s)
	return errored
}

func (l *Lifecycle) droppedRunResumeEnabled(s *models.Session) bool {
	cfg, ok := l.currentRotationConfig()
	return ok && cfg.AutoResumeDroppedRunsEnabled() && l.runResumeStore != nil && CanResumeSessionRun(s) && !isZeroOutputSession(s)
}

// runReachedFinish shares the publish receipt and terminal telemetry exception.
func (l *Lifecycle) runReachedFinish(ctx context.Context, s *models.Session) (bool, error) {
	if seen, err := l.runBuildReceipt(ctx, s); seen || err != nil {
		return seen, err
	}
	if l.agentRuns == nil {
		return false, nil
	}
	runs, err := l.agentRuns.List(ctx, domain.AgentRunFilter{SessionID: s.ID, IncludeOpen: true, IncludeAllReasons: true})
	if err != nil {
		return false, err
	}
	for _, r := range runs {
		if s.AgentSessionID != nil && r.AgentSessionID == *s.AgentSessionID && r.TerminalState != "" {
			return true, nil
		}
	}
	return false, nil
}

// liveRunPaneWedged permits recovery of the run's own idle pane only. A live
// sibling or an unreadable/current turn keeps its veto over automatic recovery.
func (l *Lifecycle) liveRunPaneWedged(ctx context.Context, s *models.Session, chat *domain.AgentChat, auth bool) bool {
	if s.AgentSessionID == nil || chat.AgentSessionID != *s.AgentSessionID {
		return false
	}
	if auth {
		return true
	}
	if over, _ := l.cronRunCompletionEvidence(s); !over {
		return false
	}
	_, errored := l.sessionTurnErrored(ctx, s)
	return errored
}

// handleStrandedRunFailure runs only after cheap idle evidence. Failed turns
// belong to recovery, except when their publish receipt or terminal state proves
// the run already finished. The returned reason counts successful blocks only.
func (l *Lifecycle) handleStrandedRunFailure(ctx context.Context, s *models.Session, expected []int, evidence string, phase reapPhase) (reason string, handled bool) {
	// Startup re-derives auth failure from panes before the status poller has run.
	if l.SessionChatAuthFailed(s) || (phase == reapStartup && l.startupPaneShowsLoginRequired(ctx, s)) {
		if l.blockFailedStrandedSession(ctx, s, expected, evidence, sessionreason.AgentAuthFailed(), "auth_failed") {
			return "auth_failed", true
		}
		return "", true
	}
	message, errored := l.sessionTurnErrored(ctx, s)
	if !errored {
		return "", false
	}
	finished, _ := l.runReachedFinish(ctx, s)
	if finished {
		return "", false
	}
	l.logger.Warn().Str("session", s.ID).Str("completion_evidence", evidence).Str("provider_error", message).
		Msg("stranded unattended session's last turn ended on a provider error; deferring to the resume lane")
	if !l.droppedRunResumeEnabled(s) && l.blockFailedStrandedSession(ctx, s, expected, evidence, sessionreason.AgentProviderError(), "provider_error") {
		return "provider_error", true
	}
	return "", true
}
