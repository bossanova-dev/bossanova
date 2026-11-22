package server

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/domain"
)

// StopChat stops one chat's agent and leaves the session's other chats
// running. The chat stays, asleep: a later message wakes it. The session must
// own the chat, which the command target gate has already checked for a cloud
// command and which this checks again for any caller.
//
// A chat whose agent runs headless (codex exec, claude --print) owns no pane
// to end, so stopping it here would report a stop that never happened; it is
// refused, and stopping the session ends it.
func (s *Server) StopChat(ctx context.Context, cmd *pb.StopChatCommand) error {
	if cmd.GetSessionId() == "" || cmd.GetAgentSessionId() == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("session_id and agent_session_id are required"))
	}
	chat, sess, err := s.chatMessageTarget(ctx, cmd.GetAgentSessionId())
	if err != nil {
		return err
	}
	if chat.SessionID != cmd.GetSessionId() {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("chat %s not found in session %s", cmd.GetAgentSessionId(), cmd.GetSessionId()))
	}
	if !domain.CloudRepoAllowed(ctx, sess.RepoID) {
		return connect.NewError(connect.CodePermissionDenied, domain.ErrCloudTarget)
	}
	if s.runsHeadless(ctx, chat, sess.RepoID) {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("this chat's agent runs without a terminal; stop the session to end it"))
	}
	stop := s.wakeHook.stopChat
	if stop == nil && s.lifecycle != nil {
		stop = s.lifecycle.StopChat
	}
	if stop == nil {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("chat stopping is not available"))
	}
	if err := stop(ctx, cmd.GetSessionId(), cmd.GetAgentSessionId()); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return connect.NewError(connect.CodeNotFound, err)
		}
		return connect.NewError(connect.CodeInternal, fmt.Errorf("stop chat: %w", err))
	}
	if s.chatStatus != nil {
		s.chatStatus.Remove(chat.AgentSessionID)
	}
	s.logger.Info().
		Str("chat_id", chat.ID).
		Str("agent_session_id", chat.AgentSessionID).
		Str("session_id", chat.SessionID).
		Msg("chat stopped")
	return nil
}

// runsHeadless reports a chat working with no pane of its own, which is how a
// headless run shows (see WakeChatInternal).
func (s *Server) runsHeadless(ctx context.Context, chat *domain.AgentChat, repoID string) bool {
	if s.chatStatus == nil {
		return false
	}
	entry := s.chatStatus.Get(chat.AgentSessionID)
	if entry == nil || entry.Status != pb.ChatStatus_CHAT_STATUS_WORKING {
		return false
	}
	return s.chatTmux == nil || !s.chatTmux.HasSession(ctx, chatTmuxName(chat, repoID))
}
