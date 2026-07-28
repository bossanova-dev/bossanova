package views

// The chat picker's agent-select overlay — the one-shot picker shown when [n]
// is pressed and more than one agent runner is loaded — plus chatAgentName, the
// per-chat provider-name resolver the table and the swit[c]h-account key also
// read. Split out of chatpicker.go (BOS-527); the declarations are unchanged.

import (
	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func (m *ChatPickerModel) chatAgentName(chat *pb.ClaudeChat) string {
	if chat.GetAgentName() != "" {
		return chat.GetAgentName()
	}
	if m.session != nil && m.session.GetAgentName() != "" {
		return m.session.GetAgentName()
	}
	return "-"
}

// buildAgentTable populates m.agentTable from m.agents. Single AGENT
// column, mirrors the new-session wizard's agent select shape.
func (m *ChatPickerModel) buildAgentTable() {
	names := make([]string, len(m.agents))
	for i, a := range m.agents {
		names[i] = a.Name
	}
	preferred := ""
	if m.session != nil {
		preferred = m.session.AgentName
	}
	cursor := agentIndex(m.agents, preferred)
	if cursor < 0 {
		cursor = 0
	}
	cols := []table.Column{
		cursorColumn,
		{Title: "AGENT", Width: maxColWidth("AGENT", names, 20) + tableColumnSep},
	}
	rows := make([]table.Row, len(m.agents))
	for i := range m.agents {
		indicator := ""
		if i == cursor {
			indicator = cursorChevron
		}
		rows[i] = table.Row{indicator, names[i]}
	}
	m.agentTable = newBossTable(cols, rows, len(m.agents)+1)
	m.agentTable.SetCursor(cursor)
	m.agentTable.SetWidth(columnsWidth(cols))
}

// updateAgentSelect handles key input while the agent-select sub-phase is
// showing. Esc cancels back to the chat picker; Enter confirms the
// selection and transitions to ViewAttach with the chosen agent override.
func (m ChatPickerModel) updateAgentSelect(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.pickingAgent = false
		return m, nil
	case "enter", " ", "space":
		idx := m.agentTable.Cursor()
		if idx < 0 || idx >= len(m.agents) {
			return m, nil
		}
		agentName := m.agents[idx].Name
		m.pickingAgent = false
		sessionID := m.sessionID
		return m, func() tea.Msg {
			return switchViewMsg{
				view:      ViewAttach,
				sessionID: sessionID,
				agentName: agentName,
			}
		}
	default:
		var cmd tea.Cmd
		m.agentTable, cmd = m.agentTable.Update(msg)
		updateCursorColumn(&m.agentTable)
		return m, cmd
	}
}
