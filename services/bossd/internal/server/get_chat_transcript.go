package server

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// GetChatTranscript reads a chat's conversation by routing to the owning agent
// plugin's ReadTranscript RPC. It resolves the owning agent + worktree from the
// agent_chats row when one exists; when it does not (e.g. a freshly created
// headless run whose row hasn't been created/propagated yet) it falls back to
// the session, which carries the same agent_session_id and worktree. The
// fallback requires a session_id (the ownership scope) and an exact
// agent_session_id match, so it cannot read a chat the caller didn't scope.
func (s *Server) GetChatTranscript(ctx context.Context, req *connect.Request[pb.GetChatTranscriptRequest]) (*connect.Response[pb.GetChatTranscriptResponse], error) {
	agentSessionID := req.Msg.GetAgentSessionId()

	var (
		worktreePath string
		agentName    string
	)

	chat, err := s.agentChats.GetByAgentSessionID(ctx, agentSessionID)
	if err == nil && chat != nil {
		if reqID := req.Msg.GetSessionId(); reqID != "" && chat.SessionID != reqID {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("chat %s not found in session %s", agentSessionID, reqID))
		}
		sess, serr := s.sessions.Get(ctx, chat.SessionID)
		if serr != nil || sess == nil {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session not found for chat %s", agentSessionID))
		}
		worktreePath = sess.WorktreePath
		agentName = chat.AgentName
	} else {
		// No agent_chats row — fall back to the session. Requires session_id
		// (ownership scope) and an exact agent_session_id match.
		reqID := req.Msg.GetSessionId()
		if reqID == "" {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("chat not found: %s", agentSessionID))
		}
		sess, serr := s.sessions.Get(ctx, reqID)
		if serr != nil || sess == nil || sess.AgentSessionID == nil || *sess.AgentSessionID != agentSessionID {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("chat not found: %s", agentSessionID))
		}
		worktreePath = sess.WorktreePath
		agentName = sess.AgentName
	}

	if agentName == "" {
		agentName = defaultLegacyAgent
	}
	client, ok := s.agentClients[agentName]
	if !ok {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("agent runner not loaded for agent %q", agentName))
	}

	pluginResp, err := client.ReadTranscript(ctx, &pb.ReadTranscriptRequest{
		WorkDir:        worktreePath,
		AgentSessionId: agentSessionID,
		MaxMessages:    req.Msg.GetMaxMessages(),
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("read transcript: %w", err))
	}

	return connect.NewResponse(&pb.GetChatTranscriptResponse{
		Messages:           pluginResp.GetMessages(),
		FinalAssistantText: pluginResp.GetFinalAssistantText(),
		Exists:             pluginResp.GetExists(),
	}), nil
}
