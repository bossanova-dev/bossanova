package chatpicker

import (
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/table"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/recurser/boss/api/tui/ui"
	"github.com/recurser/boss/internal/domain/display"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// A single-cell probe follows the renderer's final choice, including overrides
// that replace a working spinner with a static failed or parked status.
const chatSpinnerMarker = "\ue000"

type chatRowLabels struct {
	created, active string
	animates        bool
}

type chatTableRows struct {
	labels                  []chatRowLabels
	statusWidth, frameWidth int
	project                 func(table.Row) table.Row
}

func (m *Model) refreshSpinnerRows() {
	if len(m.tableRows.labels) != len(m.chats) {
		m.buildTableRows()
		return
	}
	for i, chat := range m.chats {
		labels := m.tableRows.labels[i]
		if display.RelativeTime(chat.CreatedAt.AsTime()) != labels.created || display.RelativeTime(m.chatLastActive(chat)) != labels.active {
			m.buildTableRows()
			return
		}
	}
	// A future spinner with unequal-width frames must refit the STATUS column.
	if lipgloss.Width(m.spinner.View()) != m.tableRows.frameWidth {
		m.buildTableRows()
		return
	}
	start, end := m.table.VisibleRange()
	for i := start; i < end; i++ {
		labels := m.tableRows.labels[i]
		if labels.animates {
			m.table.SetRow(i, m.tableRows.project(m.chatTableRow(i, labels.created, labels.active)))
		}
	}
}

func (m Model) chatTableRow(i int, created, active string) table.Row {
	chat := m.chats[i]
	indicator := ""
	if i == m.table.Cursor() {
		indicator = ui.CursorChevron
	}
	row := table.Row{indicator, m.chatDisplayTitle(chat), m.chatAgentName(chat), ui.StyleSubtle.Render(created), ui.StyleSubtle.Render(active), m.chatStatusCell(chat, m.spinner, m.tableRows.statusWidth)}
	if len(m.agents) <= 1 {
		row = append(row[:2:2], row[3:]...)
	}
	return row
}

func (m Model) chatStatusCell(chat *pb.AgentChat, sp spinner.Model, statusWidth int) string {
	daemon := m.daemonStatuses[chat.AgentSessionId]
	statusStr := ui.RenderClaudeStatus(daemon, sp)
	wait := m.chatWait(chat.GetAgentSessionId())
	if label := m.detailChatStatus(chat); m.session.GetAttention() != nil && label != "" {
		statusStr = ui.StyleStatusMuted.Render(ansi.Truncate(label, statusWidth, "…"))
	} else if wait == waitDone {
		statusStr = ui.StyleStatusMuted.Render(chatDoneLabel)
	} else if wait == waitStandby {
		statusStr = ui.RenderStandbyStatus()
	} else if label := m.statusDetail(chat); label != "" {
		if m.stuckCallback(chat) {
			statusStr = ui.StyleStatusWarning.Render(ansi.Truncate(label, statusWidth, "…"))
		} else {
			style := ui.StyleStatusSuccess
			if daemon == ui.StatusWaiting {
				style = ui.StyleStatusInfo
			}
			statusStr = style.Render(ansi.Truncate(sp.View()+label, statusWidth, "…"))
		}
	}
	// A chat with start_error set never came up — the agent_chats
	// row was created but StartTmuxChat hit a failure (e.g.
	// SendPlan timeout from claude's broken --print regression
	// pre-#350). The row is preserved so the operator can see the
	// attempt; surface that explicitly here instead of letting the
	// row look like a fresh "stopped" chat.
	//
	// A chat working now is past that: a start error or replaced mark still
	// on its row is stale, and the live state wins over it.
	if daemon != ui.StatusWorking {
		if chat.GetStartError() != "" {
			statusStr = ui.RenderChatStartFailed()
		} else if chat.GetReclaimedAt() != nil {
			statusStr = renderChatReplaced()
		}
	}
	if chat.AgentSessionId != "" && chat.AgentSessionId == m.deletingAgentSessionID {
		statusStr = ui.RenderRowPendingStatus(sp, "deleting")
	}
	return statusStr
}
