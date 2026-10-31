package main

import (
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/domain"
)

func TestChatMetadata(t *testing.T) {
	for _, tc := range []struct {
		role domain.AgentChatRole
		want pb.ChatRole
	}{
		{domain.AgentChatRoleUnknown, pb.ChatRole_CHAT_ROLE_UNSPECIFIED},
		{domain.AgentChatRoleBuild, pb.ChatRole_CHAT_ROLE_BUILD},
		{domain.AgentChatRoleVerify, pb.ChatRole_CHAT_ROLE_VERIFY},
		{domain.AgentChatRoleRepair, pb.ChatRole_CHAT_ROLE_REPAIR},
		{domain.AgentChatRoleOperator, pb.ChatRole_CHAT_ROLE_OPERATOR},
		{domain.AgentChatRole("future"), pb.ChatRole_CHAT_ROLE_UNSPECIFIED},
	} {
		t.Run(string(tc.role), func(t *testing.T) {
			created := time.Date(2026, 10, 31, 1, 2, 3, 456, time.UTC)
			chat := &domain.AgentChat{ID: "chat", SessionID: "session", AgentSessionID: "agent-session", AgentName: "codex", Title: "stored title", DaemonID: "daemon", CreatedAt: created, Role: tc.role}
			got := chatMetadata(chat)
			if got.Role != tc.want {
				t.Errorf("role = %v, want %v", got.Role, tc.want)
			}
			if got.Id != chat.ID || got.SessionId != chat.SessionID || got.AgentSessionId != chat.AgentSessionID || got.AgentName != chat.AgentName || got.Title != chat.Title || got.DaemonId != chat.DaemonID || !got.CreatedAt.AsTime().Equal(created) {
				t.Errorf("metadata lost chat fields: %v", got)
			}
		})
	}
}
