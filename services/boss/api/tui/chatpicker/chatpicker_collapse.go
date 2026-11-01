package chatpicker

import (
	"fmt"
	"strings"

	"github.com/recurser/boss/api/tui/ui"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

const earlierToggleChatIndex = -1

type chatPickerRow struct {
	chatIndex int
	label     string
}

func projectChatRows(chats []*pb.AgentChat, statuses map[string]string, attentionID, highlightID string, expanded bool) []chatPickerRow {
	newest := make(map[pb.ChatRole]int)
	for i, chat := range chats {
		role := chat.GetRole()
		if j, ok := newest[role]; !ok || chat.GetCreatedAt().AsTime().After(chats[j].GetCreatedAt().AsTime()) {
			newest[role] = i
		}
	}
	earlier := make([]bool, len(chats))
	counts := make(map[pb.ChatRole]int)
	count := 0
	for i, chat := range chats {
		role, id := chat.GetRole(), chat.GetAgentSessionId()
		if role != pb.ChatRole_CHAT_ROLE_BUILD && role != pb.ChatRole_CHAT_ROLE_VERIFY && role != pb.ChatRole_CHAT_ROLE_REPAIR {
			continue
		}
		status := statuses[id]
		if newest[role] == i || status == ui.StatusWorking || status == ui.StatusQuestion || status == ui.StatusWaiting || status == ui.StatusLimited || chat.GetStartError() != "" || (id != "" && (id == attentionID || id == highlightID)) {
			continue
		}
		earlier[i] = true
		counts[role]++
		count++
	}
	rows := make([]chatPickerRow, 0, len(chats)+1)
	for i := range chats {
		if count < 2 || expanded || !earlier[i] {
			rows = append(rows, chatPickerRow{chatIndex: i})
		}
	}
	if count >= 2 {
		rows = append(rows, chatPickerRow{chatIndex: earlierToggleChatIndex, label: earlierChatsLabel(count, counts, expanded)})
	}

	return rows
}

func earlierChatsLabel(count int, counts map[pb.ChatRole]int, expanded bool) string {
	if expanded {
		return fmt.Sprintf("hide %d earlier", count)
	}
	var parts []string
	for _, entry := range []struct {
		role pb.ChatRole
		name string
	}{
		{pb.ChatRole_CHAT_ROLE_BUILD, "build"},
		{pb.ChatRole_CHAT_ROLE_VERIFY, "verify"},
		{pb.ChatRole_CHAT_ROLE_REPAIR, "repair"},
	} {
		if n := counts[entry.role]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, entry.name))
		}
	}
	return fmt.Sprintf("+%d earlier (%s)", count, strings.Join(parts, ", "))
}

func (m Model) visibleChatRows() []chatPickerRow {
	if m.chatRows == nil {
		return projectChatRows(m.chats, m.daemonStatuses, m.session.GetAttention().GetTargetChatId(), m.highlightID, m.showEarlier)
	}
	return m.chatRows
}

func (m Model) projectedChat(row int) *pb.AgentChat {
	rows := m.visibleChatRows()
	if row < 0 || row >= len(rows) {
		return nil
	}
	index := rows[row].chatIndex
	if index < 0 || index >= len(m.chats) {
		return nil
	}
	return m.chats[index]
}

func (m Model) chatRowIndex(chatIndex int) int {
	for i, row := range m.chatRows {
		if row.chatIndex == chatIndex {
			return i
		}
	}
	return -1
}

func (m Model) earlierRowSelected() bool {
	i := m.table.Cursor()
	return i >= 0 && i < len(m.chatRows) && m.chatRows[i].chatIndex == earlierToggleChatIndex
}
