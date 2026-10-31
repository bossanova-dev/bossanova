package main

import (
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	server "github.com/recurser/bossd/api/daemon"
	"github.com/recurser/bossd/internal/domain"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// chatMetadata is the shared wire view for chat deltas and reconnect snapshots.
func chatMetadata(chat *domain.AgentChat) *pb.AgentChatMetadata {
	return &pb.AgentChatMetadata{
		Id:             chat.ID,
		SessionId:      chat.SessionID,
		AgentSessionId: chat.AgentSessionID,
		AgentName:      chat.AgentName,
		Title:          chat.Title,
		DaemonId:       chat.DaemonID,
		CreatedAt:      timestamppb.New(chat.CreatedAt),
		Role:           server.AgentChatRoleToProto(chat.Role),
	}
}
