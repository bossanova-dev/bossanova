package server

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// GetChatTranscript reads a chat's conversation by routing to the owning
// agent plugin's ReadTranscript RPC.
func (s *Server) GetChatTranscript(ctx context.Context, req *connect.Request[pb.GetChatTranscriptRequest]) (*connect.Response[pb.GetChatTranscriptResponse], error) {
	chat, err := s.agentChats.GetByAgentSessionID(ctx, req.Msg.GetAgentSessionId())
	if err != nil || chat == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("chat not found: %s", req.Msg.GetAgentSessionId()))
	}

	if id := req.Msg.GetSessionId(); id != "" && chat.SessionID != id {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("chat %s not found in session %s", req.Msg.GetAgentSessionId(), id))
	}

	sess, err := s.sessions.Get(ctx, chat.SessionID)
	if err != nil || sess == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session not found for chat %s", req.Msg.GetAgentSessionId()))
	}

	agentName := chat.AgentName
	if agentName == "" {
		agentName = defaultLegacyAgent
	}
	client, ok := s.agentClients[agentName]
	if !ok {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("agent runner not loaded for agent %q", agentName))
	}

	pluginResp, err := client.ReadTranscript(ctx, &pb.ReadTranscriptRequest{
		WorkDir:        sess.WorktreePath,
		AgentSessionId: chat.AgentSessionID,
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
