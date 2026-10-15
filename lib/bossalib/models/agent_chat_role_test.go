package models

import (
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestAgentChatRoleOwnershipAndWireMapping(t *testing.T) {
	for _, tc := range []struct {
		stored     string
		role       AgentChatRole
		agentOwned bool
		wire       pb.ChatRole
	}{
		{"build", AgentChatRoleBuild, true, pb.ChatRole_CHAT_ROLE_BUILD},
		{"repair", AgentChatRoleRepair, true, pb.ChatRole_CHAT_ROLE_REPAIR},
		{"verify", AgentChatRoleVerify, true, pb.ChatRole_CHAT_ROLE_VERIFY},
		{"operator", AgentChatRoleOperator, false, pb.ChatRole_CHAT_ROLE_OPERATOR},
		{"", AgentChatRoleUnknown, false, pb.ChatRole_CHAT_ROLE_UNSPECIFIED},
		{"Repair", AgentChatRoleUnknown, false, pb.ChatRole_CHAT_ROLE_UNSPECIFIED},
		{"janitor", AgentChatRoleUnknown, false, pb.ChatRole_CHAT_ROLE_UNSPECIFIED},
	} {
		role := ParseAgentChatRole(tc.stored)
		if role != tc.role {
			t.Errorf("ParseAgentChatRole(%q) = %q, want %q", tc.stored, role, tc.role)
		}
		if role.IsAgentOwned() != tc.agentOwned {
			t.Errorf("%q.IsAgentOwned() = %v, want %v", role, role.IsAgentOwned(), tc.agentOwned)
		}
		if got := AgentChatRoleToProto(role); got != tc.wire {
			t.Errorf("AgentChatRoleToProto(%q) = %v, want %v", role, got, tc.wire)
		}
		if got := AgentChatRoleFromProto(tc.wire); got != role {
			t.Errorf("AgentChatRoleFromProto(%v) = %q, want %q", tc.wire, got, role)
		}
	}
	if got := AgentChatRoleFromProto(pb.ChatRole(99)); got != AgentChatRoleUnknown {
		t.Errorf("AgentChatRoleFromProto(99) = %q, want unknown", got)
	}
}
