package session

import (
	"context"
	"fmt"

	"github.com/recurser/bossd/internal/domain"
)

// StopChat stops one chat's agent and leaves its row, so a later message wakes
// it, and the session's other chats keep running. It ends the chat's pane the
// way the idle reaper does (ReapIdleChatTmuxSession): clearing the row's
// pointer first, so the session guard never sees a pointed row over a dead
// pane and finalizes the session. A chat with no pane is already stopped.
func (l *Lifecycle) StopChat(ctx context.Context, sessionID, agentSessionID string) error {
	if l.agentChats == nil {
		return fmt.Errorf("agent chat store not configured")
	}
	chat, err := l.agentChats.GetByAgentSessionID(ctx, agentSessionID)
	if err != nil {
		return fmt.Errorf("get agent chat %s: %w", agentSessionID, err)
	}
	if chat == nil {
		return fmt.Errorf("agent chat %s: %w", agentSessionID, domain.ErrNotFound)
	}
	if chat.SessionID != sessionID {
		return fmt.Errorf("agent chat %s belongs to another session: %w", agentSessionID, domain.ErrNotFound)
	}
	if chat.TmuxSessionName == nil || *chat.TmuxSessionName == "" {
		return nil
	}
	return l.ReapIdleChatTmuxSession(ctx, sessionID, agentSessionID, *chat.TmuxSessionName)
}
