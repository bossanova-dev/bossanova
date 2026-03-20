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

	// Name inline editing
	editing   bool
	nameInput textinput.Model

	width int
}

// NewSessionSettingsModel creates a SessionSettingsModel for the given session ID.
func NewSessionSettingsModel(c client.BossClient, ctx context.Context, sessionID string) SessionSettingsModel {
	ni := textinput.New()
	ni.Placeholder = "Session name"
	ni.SetWidth(60)

	return SessionSettingsModel{
		client:    c,
		ctx:       ctx,
		sessionID: sessionID,
		nameInput: ni,
	}
}

func (m SessionSettingsModel) Init() tea.Cmd {
	return func() tea.Msg {
		sess, err := m.client.GetSession(m.ctx, m.sessionID)
		if err != nil {
			return sessionSettingsLoadedMsg{err: err}
		}
		return sessionSettingsLoadedMsg{session: sess}
	}
}

func (m SessionSettingsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
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
		if m.editing {
			return m.updateEditing(msg)
		}

		switch msg.String() {
		case "esc", "q":
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
		case "enter":
			return m.activateRow()
		}
	}

	return m, nil
}

func (m SessionSettingsModel) updateEditing(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		name := strings.TrimSpace(m.nameInput.Value())
		if name == "" {
			m.err = fmt.Errorf("name cannot be empty")
			return m, nil
		}
		m.editing = false
		m.err = nil
		m.nameInput.Blur()
		return m, m.saveSettings(&pb.UpdateSessionRequest{
			Id:    m.sessionID,
			Title: &name,
		})
	case "esc":
		m.editing = false
		m.err = nil
		m.nameInput.Blur()
		if m.session != nil {
			m.nameInput.SetValue(m.session.Title)
		}
		return m, nil
	}

	var cmd tea.Cmd
	m.nameInput, cmd = m.nameInput.Update(msg)
	return m, cmd
}

func (m SessionSettingsModel) activateRow() (tea.Model, tea.Cmd) {
	if m.session == nil {
		return m, nil
	}

	switch m.cursor {
	case sessionSettingsRowName:
		m.editing = true
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

func (m SessionSettingsModel) View() tea.View {
	if m.session == nil {
		if m.err != nil {
			return tea.NewView(
				renderError(fmt.Sprintf("Error: %v", m.err), m.width) + "\n" +
					styleActionBar.Render("[esc] back"),
			)
		}
		return tea.NewView(lipgloss.NewStyle().Padding(1, 2).Render("Loading session..."))
	}

	var b strings.Builder

	if m.err != nil {
		b.WriteString(renderError(fmt.Sprintf("Error: %v", m.err), m.width))
		b.WriteString("\n")
	}

	// Row 0: Name
	if m.editing && m.cursor == sessionSettingsRowName {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("  Name:"))
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 4).Render(m.nameInput.View()))
		b.WriteString("\n")
	} else {
		cursor := "  "
		if m.cursor == sessionSettingsRowName {
			cursor = cursorChevron + " "
		}
		line := fmt.Sprintf("%sName: %s", cursor, m.session.Title)
		if m.cursor == sessionSettingsRowName {
			line = styleSelected.Render(line)
		}
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(line))
		b.WriteString("\n")
	}

	if m.editing {
		b.WriteString(styleActionBar.Render("[enter] save  [esc] cancel"))
	} else {
		b.WriteString(styleActionBar.Render("[enter] edit  [esc] back"))
	}

	return tea.NewView(b.String())
}
