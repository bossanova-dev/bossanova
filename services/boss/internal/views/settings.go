package views

import (
	"fmt"
	"os"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/bossalib/config"
)

// SettingsModel is the TUI view for editing global settings.
type SettingsModel struct {
	settings config.Settings
	cursor   int
	cancel   bool
	err      error

	// Worktree dir inline editing
	editing          bool
	worktreeDirInput textinput.Model

	width int
}

const (
	settingsRowSkipPerms = 0
	settingsRowWorktree  = 1
	settingsRowCount     = 2
)

// NewSettingsModel creates a SettingsModel, loading current settings.
func NewSettingsModel() SettingsModel {
	s, _ := config.Load()

	wtIn := textinput.New()
	wtIn.Placeholder = "Worktree base directory"
	wtIn.SetWidth(60)
	wtIn.SetValue(s.WorktreeBaseDir)

	return SettingsModel{
		settings:         s,
		worktreeDirInput: wtIn,
	}
}

func (m SettingsModel) Init() tea.Cmd {
	return nil
}

func (m SettingsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
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
			if m.cursor < settingsRowCount-1 {
				m.cursor++
			}
		case "enter", "space":
			return m.activateRow()
		}
	}

	return m, nil
}

func (m SettingsModel) updateEditing(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		dir := m.worktreeDirInput.Value()
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			m.err = fmt.Errorf("directory does not exist: %s", dir)
			return m, nil
		}
		m.editing = false
		m.err = nil
		m.worktreeDirInput.Blur()
		m.settings.WorktreeBaseDir = dir
		if err := config.Save(m.settings); err != nil {
			m.err = err
		}
		return m, nil
	case "esc":
		m.editing = false
		m.err = nil
		m.worktreeDirInput.Blur()
		m.worktreeDirInput.SetValue(m.settings.WorktreeBaseDir)
		return m, nil
	}

	var cmd tea.Cmd
	m.worktreeDirInput, cmd = m.worktreeDirInput.Update(msg)
	return m, cmd
}

func (m SettingsModel) activateRow() (tea.Model, tea.Cmd) {
	switch m.cursor {
	case settingsRowSkipPerms:
		m.settings.DangerouslySkipPermissions = !m.settings.DangerouslySkipPermissions
		if err := config.Save(m.settings); err != nil {
			m.err = err
		}
	case settingsRowWorktree:
		m.editing = true
		return m, m.worktreeDirInput.Focus()
	}
	return m, nil
}

// Cancelled returns true if the user exited the settings view.
func (m SettingsModel) Cancelled() bool { return m.cancel }

func (m SettingsModel) View() tea.View {
	var b strings.Builder
	b.WriteString(styleTitle.Render("Settings"))
	b.WriteString("\n\n")

	if m.err != nil {
		b.WriteString(renderError(fmt.Sprintf("Error: %v", m.err), m.width))
		b.WriteString("\n")
	}

	// Row 0: dangerously skip permissions toggle
	check := " "
	if m.settings.DangerouslySkipPermissions {
		check = "x"
	}
	cursor := "  "
	if m.cursor == settingsRowSkipPerms && !m.editing {
		cursor = "> "
	}
	line := fmt.Sprintf("%s[%s] Enable Claude --dangerously-skip-permissions", cursor, check)
	if m.cursor == settingsRowSkipPerms && !m.editing {
		line = styleSelected.Render(line)
	}
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(line))
	b.WriteString("\n\n")

	// Row 1: worktree base dir
	cursor = "  "
	if m.cursor == settingsRowWorktree && !m.editing {
		cursor = "> "
	}
	if m.editing {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("  Worktree base directory:"))
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 4).Render(m.worktreeDirInput.View()))
		b.WriteString("\n")
	} else {
		line = fmt.Sprintf("%sWorktree base directory: %s", cursor, m.settings.WorktreeBaseDir)
		if m.cursor == settingsRowWorktree {
			line = styleSelected.Render(line)
		}
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(line))
		b.WriteString("\n")
	}

	if m.editing {
		b.WriteString(styleActionBar.Render("[enter] save  [esc] cancel"))
	} else {
		b.WriteString(styleActionBar.Render("[enter/space] toggle/edit  [esc] back"))
	}

	return tea.NewView(b.String())
}
