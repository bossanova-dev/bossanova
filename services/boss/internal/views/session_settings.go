package views

import (
	"context"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// sessionSettingsLoadedMsg carries the loaded session for the settings view.
type sessionSettingsLoadedMsg struct {
	session *pb.Session
	err     error
}

// sessionSettingsSavedMsg carries the result of saving session settings.
type sessionSettingsSavedMsg struct {
	session *pb.Session
	err     error
}

const (
	sessionSettingsRowName  = 0
	sessionSettingsRowCount = 1
)

// SessionSettingsModel is the TUI view for editing per-session settings.
type SessionSettingsModel struct {
	client    client.BossClient
	ctx       context.Context
	sessionID string
	session   *pb.Session
	cursor    int
	cancel    bool
	done      bool
	err       error

	// Inline editing (-1 = not editing, otherwise the row being edited)
	editingField int
	nameInput    textinput.Model

	width int
}

// NewSessionSettingsModel creates a SessionSettingsModel for the given session ID.
func NewSessionSettingsModel(c client.BossClient, ctx context.Context, sessionID string) SessionSettingsModel {
	ni := textinput.New()
	ni.Placeholder = "Session name"
	ni.SetWidth(60)

	return SessionSettingsModel{
		client:       c,
		ctx:          ctx,
		sessionID:    sessionID,
		editingField: -1,
		nameInput:    ni,
	}
}

func (m SessionSettingsModel) Init() tea.Cmd {
	return func() tea.Msg {
		sess, err := m.client.GetSession(m.ctx, m.sessionID, client.SessionReadOptions{})
		if err != nil {
			return sessionSettingsLoadedMsg{err: err}
		}
		return sessionSettingsLoadedMsg{session: sess}
	}
}

func (m SessionSettingsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// When editing a text field, forward all message types (not just KeyMsg)
	// to the textinput so that paste messages are handled correctly.
	if m.editingField >= 0 {
		return m.updateEditing(msg)
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil

	case sessionSettingsLoadedMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.session = msg.session
		m.nameInput.SetValue(m.session.Title)
		return m, nil

	case sessionSettingsSavedMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.session = msg.session
		m.err = nil
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "esc":
			m.cancel = true
			return m, nil
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < sessionSettingsRowCount-1 {
				m.cursor++
			}
		case "enter", "space":
			return m.activateRow()
		}
	}

	return m, nil
}

func (m SessionSettingsModel) updateEditing(msg tea.Msg) (tea.Model, tea.Cmd) {
	if keyMsg, ok := msg.(tea.KeyMsg); ok {
		switch keyMsg.String() {
		case "enter":
			return m.commitEdit()
		case "esc":
			return m.cancelEdit(), nil
		}
	}

	var cmd tea.Cmd
	if m.editingField == sessionSettingsRowName {
		m.nameInput, cmd = m.nameInput.Update(msg)
	}
	return m, cmd
}

func (m SessionSettingsModel) commitEdit() (tea.Model, tea.Cmd) {
	if m.editingField == sessionSettingsRowName {
		name := strings.TrimSpace(m.nameInput.Value())
		if name == "" {
			m.err = fmt.Errorf("name cannot be empty")
			return m, nil
		}
		m.editingField = -1
		m.err = nil
		m.nameInput.Blur()
		return m, m.saveSettings(&pb.UpdateSessionRequest{
			Id:    m.sessionID,
			Title: &name,
		})
	}
	return m, nil
}

func (m SessionSettingsModel) cancelEdit() SessionSettingsModel {
	if m.editingField == sessionSettingsRowName {
		m.nameInput.Blur()
		if m.session != nil {
			m.nameInput.SetValue(m.session.Title)
		}
	}
	m.editingField = -1
	m.err = nil
	return m
}

func (m SessionSettingsModel) activateRow() (tea.Model, tea.Cmd) {
	if m.session == nil {
		return m, nil
	}

	if m.cursor == sessionSettingsRowName {
		m.editingField = sessionSettingsRowName
		return m, m.nameInput.Focus()
	}
	return m, nil
}

func (m SessionSettingsModel) saveSettings(req *pb.UpdateSessionRequest) tea.Cmd {
	return func() tea.Msg {
		sess, err := m.client.UpdateSession(m.ctx, req)
		return sessionSettingsSavedMsg{session: sess, err: err}
	}
}

// Cancelled returns true if the user exited the settings view.
func (m SessionSettingsModel) Cancelled() bool { return m.cancel }

// Done returns true if settings were saved and the view should close.
func (m SessionSettingsModel) Done() bool { return m.done }

// textEntryActive reports whether a row is in inline edit mode with its
// textinput focused, so App can leave ctrl+x alone rather than aliasing it onto
// Esc (BOS-660). editingField is -1 when nothing is being edited, and Esc there
// just exits the view.
func (m SessionSettingsModel) textEntryActive() bool { return m.editingField >= 0 }

func (m SessionSettingsModel) View() tea.View {
	if m.session == nil {
		if m.err != nil {
			return tea.NewView(
				renderError(rpcErrorMessage(m.err), m.width) + "\n" +
					styleActionBar.Render("[esc] back"),
			)
		}
		return tea.NewView(lipgloss.NewStyle().Padding(0, 2).Render("Loading session…"))
	}

	var b strings.Builder

	if m.err != nil {
		b.WriteString(renderError(rpcErrorMessage(m.err), m.width))
		b.WriteString("\n")
	}

	// Row 0: Name
	if m.editingField == sessionSettingsRowName {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("  Name:"))
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 4).Render(m.nameInput.View()))
		b.WriteString("\n")
	} else {
		b.WriteString(renderFieldRow(
			m.cursor == sessionSettingsRowName,
			fmt.Sprintf("Name: %s", m.session.Title),
		))
		b.WriteString("\n")
	}

	if m.editingField >= 0 {
		b.WriteString(actionBarWidth(m.width, []string{"[enter] save", "[esc] cancel"}))
	} else {
		b.WriteString(actionBarWidth(m.width, []string{"[enter/space] edit"}, []string{"[esc] back"}))
	}

	return tea.NewView(b.String())
}
