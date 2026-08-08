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
//
// Cursor precedence is preferredAgent (Settings.DefaultAgent, kept current by
// every confirmed pick) → the session's own agent → row 0. The configured
// default has to lead: sessions.agent_name is written once at create time and
// no code path ever updates it, so seeding from the session alone left a
// session created with one runner defaulting to that runner for every new chat
// forever, however many chats you since started on another. The
// session agent survives as the fallback for the case the default names a
// runner this daemon has not loaded.
func (m *ChatPickerModel) buildAgentTable() {
	names := make([]string, len(m.agents))
	for i, a := range m.agents {
		names[i] = a.Name
	}
	cursor := agentIndex(m.agents, m.preferredAgent)
	if cursor < 0 && m.session != nil {
		cursor = agentIndex(m.agents, m.session.AgentName)
	}
	if cursor < 0 {
		cursor = 0
	}
	rcols := []responsiveColumn{
		{col: cursorColumn, priority: 0, minWidth: 1},
		{col: table.Column{Title: "AGENT", Width: maxColWidth("AGENT", names, 20) + tableColumnSep}, priority: 0, minWidth: 8},
	}
	cols := fitColumns(rcols, m.chatPickerTableAvailWidth())
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
		// Persist the pick so the next [n] — and the new-session wizard, which
		// writes the same setting — opens on this runner. A save failure goes to
		// statusMsg, not m.err: m.err takes over the whole view with an error
		// screen, and a settings write that failed must not stop the chat the
		// operator just asked for. In-memory preferredAgent is updated either
		// way, so the picker is consistent for this model's lifetime even when
		// the write did not land.
		m.preferredAgent = agentName
		if m.onAgentSelected != nil {
			if err := m.onAgentSelected(agentName); err != nil {
				m.statusMsg = rpcStatusMessage("Couldn't save default agent", err)
			}
		}
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
