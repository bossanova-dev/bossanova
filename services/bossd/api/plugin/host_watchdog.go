package plugin

import (
	"context"
	"fmt"
	"time"

	"github.com/recurser/bossalib/contract/plugin/agenterr"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/domain"
	"github.com/rs/zerolog/log"
)

// handleChatRunWatchdog runs after the displaceability recheck and hook drain.
func (s *HostServiceServer) handleChatRunWatchdog(agentSessionID string, run activeChatRun, idleWindow time.Duration) *bossanovav1.WaitChatRunHostResponse {
	providerSessionID := s.providerSessionIDForAgentSession(context.Background(), agentSessionID)
	if s.watchdogCompletedTurn(agentSessionID, providerSessionID, run.startedAt) {
		log.Info().Str("agent_session", agentSessionID).
			Msg("WaitChatRun: idle watchdog found a completed hookless turn; recording completion")
		s.finalizeCompletedChatRun(agentSessionID, run, completionResult{})
		return &bossanovav1.WaitChatRunHostResponse{ProviderSessionId: providerSessionID}
	}
	log.Warn().
		Str("agent_session", agentSessionID).
		Dur("idle_window", idleWindow).
		Msg("WaitChatRun: idle watchdog fired; agent went idle without completing; reclaiming pane")
	s.runMu.Lock()
	sessionID := s.runSessionByID[agentSessionID]
	s.runMu.Unlock()
	if s.lifecycle != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), watchdogReclaimTimeout)
		if _, err := s.lifecycle.ReclaimRepairChat(cleanupCtx, sessionID, agentSessionID, "watchdog: agent went idle without completing"); err != nil {
			log.Warn().Err(err).
				Str("agent_session", agentSessionID).
				Msg("WaitChatRun: watchdog pane reclaim failed; leaving run open")
			cancel()
			s.finalizeChatRunWhenDone(agentSessionID, run)
			return &bossanovav1.WaitChatRunHostResponse{
				ExitError:         fmt.Sprintf("agent went idle without completing and pane reclaim failed (no output for %s; watchdog): %v", idleWindow, err),
				ProviderSessionId: providerSessionID,
			}
		}
		cancel()
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	s.recordAgentRunStop(stopCtx, agentSessionID, domain.AgentRunStopUnknown)
	stopCancel()
	telemetryCtx, telemetryCancel := context.WithTimeout(context.Background(), 10*time.Second)
	s.recordChatRunTelemetry(telemetryCtx, agentSessionID, run)
	telemetryCancel()
	s.removeRunHookBestEffort(agentSessionID)
	s.cleanupRun(agentSessionID)
	return &bossanovav1.WaitChatRunHostResponse{
		ExitError:         fmt.Sprintf("agent went idle without completing (no output for %s; watchdog)", idleWindow),
		ProviderSessionId: providerSessionID,
	}
}

// watchdogCompletedTurn uses the chat's own transcript: a prompt without a
// current assistant turn must retain the watchdog failure and retry semantics.
func (s *HostServiceServer) watchdogCompletedTurn(agentSessionID, providerSessionID string, startedAt time.Time) bool {
	if s.agentChats == nil || startedAt.IsZero() {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	chat, err := s.agentChats.GetByAgentSessionID(ctx, agentSessionID)
	if err != nil || chat == nil {
		return false
	}
	client := s.agentClients[chat.AgentName]
	if client == nil {
		return false
	}
	workDir := s.chatRunWorktreePath(ctx, agentSessionID)
	if workDir == "" {
		return false
	}
	ids := []string{agentSessionID}
	if providerSessionID != "" && providerSessionID != agentSessionID {
		ids = []string{providerSessionID, agentSessionID}
	}
	for _, id := range ids {
		resp, err := client.ReadTranscript(ctx, &bossanovav1.ReadTranscriptRequest{WorkDir: workDir, AgentSessionId: id, MaxMessages: 1})
		if err != nil || !resp.GetExists() {
			continue
		}
		msgs := resp.GetMessages()
		if len(msgs) == 0 {
			return false
		}
		msg := msgs[len(msgs)-1]
		timestamp, err := time.Parse(time.RFC3339Nano, msg.GetTimestamp())
		if msg.GetRole() != "assistant" || err != nil || timestamp.Before(startedAt.Truncate(time.Millisecond)) {
			return false
		}
		switch agenterr.Classify(msg.GetText(), time.Now()).Kind {
		case agenterr.KindUsageExhausted, agenterr.KindRateLimited:
			return false
		default:
			return true
		}
	}
	return false
}
