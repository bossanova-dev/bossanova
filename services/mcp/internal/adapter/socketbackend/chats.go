package socketbackend

import (
	"context"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// --- Chats ---

func (b *Backend) RecordChat(ctx context.Context, sessionID, agentSessionID, title, agentName string, resume bool) (*pb.AgentChat, error) {
	req := &pb.RecordChatRequest{
		SessionId:      sessionID,
		AgentSessionId: agentSessionID,
		Title:          title,
		Resume:         resume,
	}
	if agentName != "" {
		req.AgentName = &agentName
	}
	resp, err := b.rpc.RecordChat(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetChat(), nil
}

func (b *Backend) ListChats(ctx context.Context, sessionID string) ([]*pb.AgentChat, error) {
	resp, err := b.rpc.ListChats(ctx, connect.NewRequest(&pb.ListChatsRequest{SessionId: sessionID}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetChats(), nil
}

func (b *Backend) UpdateChatTitle(ctx context.Context, agentSessionID, title string) error {
	_, err := b.rpc.UpdateChatTitle(ctx, connect.NewRequest(&pb.UpdateChatTitleRequest{
		AgentSessionId: agentSessionID,
		Title:          title,
	}))
	return err
}

// DeleteChat stamps AGENT_REQUESTED rather than passing the zero value through.
// Everything reaching this backend is an agent's delete_chat MCP tool call —
// that is the only thing the path can mean — so the reason is stated at the
// boundary instead of defaulting to UNSPECIFIED, which is reserved for callers
// predating the field. AGENT_REQUESTED is never gated by the daemon; only the
// transcript-absent cleanup reason is.
func (b *Backend) DeleteChat(ctx context.Context, agentSessionID string) error {
	_, err := b.rpc.DeleteChat(ctx, connect.NewRequest(&pb.DeleteChatRequest{
		AgentSessionId: agentSessionID,
		Reason:         pb.DeleteChatRequest_DELETION_REASON_AGENT_REQUESTED,
	}))
	return err
}

func (b *Backend) WakeChat(ctx context.Context, _, agentSessionID string, forceFresh bool) (*pb.WakeChatResponse, error) {
	resp, err := b.rpc.WakeChat(ctx, connect.NewRequest(&pb.WakeChatRequest{
		AgentSessionId: agentSessionID,
		ForceFresh:     forceFresh,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (b *Backend) GetChatStatuses(ctx context.Context, sessionID string) ([]*pb.ChatStatusEntry, error) {
	resp, err := b.rpc.GetChatStatuses(ctx, connect.NewRequest(&pb.GetChatStatusesRequest{SessionId: sessionID}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetStatuses(), nil
}

func (b *Backend) GetChatTranscript(ctx context.Context, req *pb.GetChatTranscriptRequest) (*pb.GetChatTranscriptResponse, error) {
	resp, err := b.rpc.GetChatTranscript(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (b *Backend) SendChatMessage(ctx context.Context, req *pb.SendChatMessageRequest) (*pb.SendChatMessageResponse, error) {
	resp, err := b.rpc.SendChatMessage(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}
