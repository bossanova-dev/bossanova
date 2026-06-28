package server

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/tmux"
)

// SendChatMessage delivers a user message into a chat's live agent, optionally
// waking an asleep session first.
func (s *Server) SendChatMessage(ctx context.Context, req *connect.Request[pb.SendChatMessageRequest]) (*connect.Response[pb.SendChatMessageResponse], error) {
	chat, err := s.agentChats.GetByAgentSessionID(ctx, req.Msg.GetAgentSessionId())
	if err != nil || chat == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("chat not found: %s", req.Msg.GetAgentSessionId()))
	}

	sess, err := s.sessions.Get(ctx, chat.SessionID)
	if err != nil || sess == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session not found for chat %s", req.Msg.GetAgentSessionId()))
	}

	tmuxName := tmux.ChatSessionName(sess.RepoID, chat.AgentSessionID)

	// Resolve the tmux interface: prefer the test hook, fall back to the live client.
	var spawner tmuxSpawner
	if s.wakeHook.spawner != nil {
		spawner = s.wakeHook.spawner
	} else if s.tmux != nil {
		spawner = liveTmuxSpawner{c: s.tmux}
	}

	isLive := spawner != nil && spawner.HasSession(ctx, tmuxName)

	if !isLive {
		if !req.Msg.GetWakeIfAsleep() {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("chat %s is not live and wake_if_asleep is false", req.Msg.GetAgentSessionId()))
		}
		_, _, _, wakeErr := s.WakeChatInternal(ctx, req.Msg.GetAgentSessionId(), false)
		if wakeErr != nil {
			if errors.Is(wakeErr, ErrWorktreeMissing) {
				return nil, connect.NewError(connect.CodeFailedPrecondition, wakeErr)
			}
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("wake chat: %w", wakeErr))
		}
	}

	if spawner == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("tmux not available"))
	}

	if err := spawner.SendMessage(ctx, tmuxName, req.Msg.GetMessage()); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("send message: %w", err))
	}

	return connect.NewResponse(&pb.SendChatMessageResponse{
		TmuxSessionName: tmuxName,
		Delivered:       true,
	}), nil
}
