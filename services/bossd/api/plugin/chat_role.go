package plugin

import (
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/domain"
)

// --- domain.AgentChatRole conversion ---

var agentChatRoleProto = map[domain.AgentChatRole]pb.ChatRole{
	domain.AgentChatRoleUnknown:  pb.ChatRole_CHAT_ROLE_UNSPECIFIED,
	domain.AgentChatRoleBuild:    pb.ChatRole_CHAT_ROLE_BUILD,
	domain.AgentChatRoleRepair:   pb.ChatRole_CHAT_ROLE_REPAIR,
	domain.AgentChatRoleVerify:   pb.ChatRole_CHAT_ROLE_VERIFY,
	domain.AgentChatRoleOperator: pb.ChatRole_CHAT_ROLE_OPERATOR,
}

// agentChatRoleFromProto maps the wire enum onto a role. An enum value this
// binary does not know reads as the unknown role.
func agentChatRoleFromProto(p pb.ChatRole) domain.AgentChatRole {
	for role, wire := range agentChatRoleProto {
		if wire == p {
			return role
		}
	}
	return domain.AgentChatRoleUnknown
}
