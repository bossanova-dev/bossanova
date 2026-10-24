package server

import (
	"testing"

	"github.com/recurser/bossd/internal/domain"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestAgentChatRoleOwnershipAndWireMapping(t *testing.T) {
	for _, tc := range []struct {
		stored     string
		role       domain.AgentChatRole
		agentOwned bool
		wire       pb.ChatRole
	}{
		{"build", domain.AgentChatRoleBuild, true, pb.ChatRole_CHAT_ROLE_BUILD},
		{"repair", domain.AgentChatRoleRepair, true, pb.ChatRole_CHAT_ROLE_REPAIR},
		{"verify", domain.AgentChatRoleVerify, true, pb.ChatRole_CHAT_ROLE_VERIFY},
		{"operator", domain.AgentChatRoleOperator, false, pb.ChatRole_CHAT_ROLE_OPERATOR},
		{"", domain.AgentChatRoleUnknown, false, pb.ChatRole_CHAT_ROLE_UNSPECIFIED},
		{"Repair", domain.AgentChatRoleUnknown, false, pb.ChatRole_CHAT_ROLE_UNSPECIFIED},
		{"janitor", domain.AgentChatRoleUnknown, false, pb.ChatRole_CHAT_ROLE_UNSPECIFIED},
	} {
		role := domain.ParseAgentChatRole(tc.stored)
		if role != tc.role {
			t.Errorf("domain.ParseAgentChatRole(%q) = %q, want %q", tc.stored, role, tc.role)
		}
		if role.IsAgentOwned() != tc.agentOwned {
			t.Errorf("%q.IsAgentOwned() = %v, want %v", role, role.IsAgentOwned(), tc.agentOwned)
		}
		if got := agentChatRoleToProto(role); got != tc.wire {
			t.Errorf("agentChatRoleToProto(%q) = %v, want %v", role, got, tc.wire)
		}
		if got := agentChatRoleFromProto(tc.wire); got != role {
			t.Errorf("agentChatRoleFromProto(%v) = %q, want %q", tc.wire, got, role)
		}
	}
	if got := agentChatRoleFromProto(pb.ChatRole(99)); got != domain.AgentChatRoleUnknown {
		t.Errorf("agentChatRoleFromProto(99) = %q, want unknown", got)
	}
}
