package chatpicker

import (
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/chatlabel"
)

func (m *Model) refreshChatLabels() {
	chats := make([]chatlabel.Chat, len(m.chats))
	for i, chat := range m.chats {
		var role chatlabel.Role
		switch chat.GetRole() {
		case pb.ChatRole_CHAT_ROLE_UNSPECIFIED:
			// Older chats keep their stored title.
		case pb.ChatRole_CHAT_ROLE_BUILD:
			role = chatlabel.RoleBuild
		case pb.ChatRole_CHAT_ROLE_VERIFY:
			role = chatlabel.RoleVerify
		case pb.ChatRole_CHAT_ROLE_REPAIR:
			role = chatlabel.RoleRepair
		case pb.ChatRole_CHAT_ROLE_OPERATOR:
			role = chatlabel.RoleOperator
		}
		chats[i] = chatlabel.Chat{ID: chat.GetAgentSessionId(), Role: role, Title: chat.GetTitle(), CreatedAt: chat.GetCreatedAt().AsTime()}
	}
	m.chatLabels = chatlabel.Labels(chats, m.session.GetTitle())
}
