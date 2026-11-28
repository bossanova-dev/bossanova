package plugin

import (
	"context"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/domain"
	lifecycleplugin "github.com/recurser/bossd/internal/plugin"
	"github.com/rs/zerolog/log"
)

// SetLivePaneChecker installs the tmux pane liveness predicate consulted during
// the cold-tracker window after a daemon restart, before TmuxStatusPoller has
// warmed chatTracker. Nil disables the cold-tracker fallback.
func (s *HostServiceServer) SetLivePaneChecker(fn func(ctx context.Context, tmuxSessionName string) bool) {
	s.livePaneChecker = fn
}

func (s *HostServiceServer) setRepairQueued(sessionID string) (*bossanovav1.SetRepairStatusResponse, error) {
	s.repairLease.MarkQueued(sessionID)
	return &bossanovav1.SetRepairStatusResponse{}, nil
}

func (s *HostServiceServer) hasLiveRepairPane(ctx context.Context, chat *domain.AgentChat) bool {
	if s.livePaneChecker == nil || chat == nil || chat.TmuxSessionName == nil || *chat.TmuxSessionName == "" {
		return false
	}
	return s.livePaneChecker(ctx, *chat.TmuxSessionName)
}

func (s *HostServiceServer) isDisplaceableRepairChat(chat *domain.AgentChat, now time.Time) bool {
	if chat.Role != domain.AgentChatRoleRepair {
		return false
	}
	return s.chatDisplaceable(chat.AgentSessionID, time.Time{}, lifecycleplugin.DisplacePolicy{
		MinIdle:      lifecycleplugin.RepairDisplaceMinIdle,
		QuestionIdle: lifecycleplugin.RepairDisplaceQuestionIdle,
	}, now) == nil
}

func (s *HostServiceServer) sessionActiveChats(ctx context.Context, sessionID string, isRepairing bool) (bool, time.Time, bool) {
	if s.agentChats == nil || s.chatTracker == nil {
		return false, time.Time{}, isRepairing
	}
	chats, err := s.agentChats.ListBySession(ctx, sessionID)
	if err != nil {
		log.Warn().Err(err).Str("session_id", sessionID).Msg("failed to list chats for active chat check, assuming active")
		return true, time.Time{}, isRepairing
	}
	hasActiveChat := false
	var latestChatActivity time.Time
	now := time.Now()
	for _, chat := range chats {
		if chat.IsRetired() {
			continue
		}
		// BOS-153: a repair chat the tracker deems displaceable (idle past the
		// repair window, at a long-stuck question, or stopped) is a dead/dying
		// run — do not count it as active or repairing.
		if s.isDisplaceableRepairChat(chat, now) {
			continue
		}
		chatEntry := s.chatTracker.Get(chat.AgentSessionID)
		if chatEntry == nil {
			if chat.Role == domain.AgentChatRoleRepair && s.hasLiveRepairPane(ctx, chat) {
				hasActiveChat = true
				isRepairing = true
			}
			continue
		}
		hasActiveChat = true
		if chat.Role == domain.AgentChatRoleRepair && chatEntry.Status == bossanovav1.ChatStatus_CHAT_STATUS_WORKING {
			isRepairing = true
		}
		if chatEntry.LastOutputAt.After(latestChatActivity) {
			latestChatActivity = chatEntry.LastOutputAt
		}
	}
	return hasActiveChat, latestChatActivity, isRepairing
}
