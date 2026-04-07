package views

import (
	"fmt"
	"strconv"
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

	// Which row is being edited (-1 = none).
	editingRow int

	worktreeDirInput  textinput.Model
	pollIntervalInput textinput.Model

	width int
}

const (
	settingsRowSkipPerms    = 0
	settingsRowWorktree     = 1
	settingsRowPollInterval = 2
	settingsRowCount        = 3
)

// NewSettingsModel creates a SettingsModel, loading current settings.
func NewSettingsModel() SettingsModel {
	s, _ := config.Load()

	wtIn := textinput.New()
	wtIn.Placeholder = "Worktree base directory"
	wtIn.SetWidth(60)
	wtIn.SetValue(s.WorktreeBaseDir)

	piIn := textinput.New()
	piIn.Placeholder = "30"
	piIn.SetWidth(10)
	if s.PollIntervalSeconds > 0 {
		piIn.SetValue(strconv.Itoa(s.PollIntervalSeconds))
	}

	return SettingsModel{
		settings:          s,
		editingRow:        -1,
		worktreeDirInput:  wtIn,
		pollIntervalInput: piIn,
	}
}

func (m SettingsModel) Init() tea.Cmd {
	return nil
}

func (m SettingsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// When editing a text field, forward all message types (not just KeyMsg)
	// to the textinput so that paste messages are handled correctly.
	if m.editingRow >= 0 {
		return m.updateEditing(msg)
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
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
			if m.cursor < settingsRowCount-1 {
				m.cursor++
			}
		case "enter", "space":
			return m.activateRow()
		}
	}

	return m, nil
}

func (m SettingsModel) updateEditing(msg tea.Msg) (tea.Model, tea.Cmd) {
	if keyMsg, ok := msg.(tea.KeyMsg); ok {
		switch keyMsg.String() {
		case "enter":
			return m.commitEdit()
		case "esc":
			return m.cancelEdit()
		}
	}

	var cmd tea.Cmd
	switch m.editingRow {
	case settingsRowWorktree:
		m.worktreeDirInput, cmd = m.worktreeDirInput.Update(msg)
	case settingsRowPollInterval:
		m.pollIntervalInput, cmd = m.pollIntervalInput.Update(msg)
	}
	return m, cmd
}

func (m SettingsModel) commitEdit() (tea.Model, tea.Cmd) {
	switch m.editingRow {
	case settingsRowWorktree:
		dir := m.worktreeDirInput.Value()
		if dir == "" {
			m.err = fmt.Errorf("directory cannot be empty")
			return m, nil
		}
		m.editingRow = -1
		m.err = nil
		m.worktreeDirInput.Blur()
		m.settings.WorktreeBaseDir = dir
		if err := config.Save(m.settings); err != nil {
			m.err = err
		}

	case settingsRowPollInterval:
		val := m.pollIntervalInput.Value()
		if val == "" {
			// Empty means use default (clear the setting).
			m.editingRow = -1
			m.err = nil
			m.pollIntervalInput.Blur()
			m.settings.PollIntervalSeconds = 0
			if err := config.Save(m.settings); err != nil {
				m.err = err
			}
			return m, nil
		}
		n, err := strconv.Atoi(val)
		if err != nil || n < 1 {
			m.err = fmt.Errorf("poll interval must be a positive integer")
			return m, nil
		}
		m.editingRow = -1
		m.err = nil
		m.pollIntervalInput.Blur()
		m.settings.PollIntervalSeconds = n
		if err := config.Save(m.settings); err != nil {
			m.err = err
		}
	}
	return m, nil
}

func (m SettingsModel) cancelEdit() (tea.Model, tea.Cmd) {
	switch m.editingRow {
	case settingsRowWorktree:
		m.worktreeDirInput.Blur()
		m.worktreeDirInput.SetValue(m.settings.WorktreeBaseDir)
	case settingsRowPollInterval:
		m.pollIntervalInput.Blur()
		if m.settings.PollIntervalSeconds > 0 {
			m.pollIntervalInput.SetValue(strconv.Itoa(m.settings.PollIntervalSeconds))
		} else {
			m.pollIntervalInput.SetValue("")
		}
	}
	m.editingRow = -1
	m.err = nil
	return m, nil
}

func (m SettingsModel) activateRow() (tea.Model, tea.Cmd) {
	switch m.cursor {
	case settingsRowSkipPerms:
		m.settings.DangerouslySkipPermissions = !m.settings.DangerouslySkipPermissions
		if err := config.Save(m.settings); err != nil {
			m.err = err
		}
	case settingsRowWorktree:
		m.editingRow = settingsRowWorktree
		return m, m.worktreeDirInput.Focus()
	case settingsRowPollInterval:
		m.editingRow = settingsRowPollInterval
		return m, m.pollIntervalInput.Focus()
	}
	return m, nil
}

// Cancelled returns true if the user exited the settings view.
func (m SettingsModel) Cancelled() bool { return m.cancel }

func (m SettingsModel) View() tea.View {
	var b strings.Builder

	if m.err != nil {
		b.WriteString(renderError(fmt.Sprintf("Error: %v", m.err), m.width))
		b.WriteString("\n")
	}

	editing := m.editingRow >= 0

	// Row 0: dangerously skip permissions toggle
	check := " "
	if m.settings.DangerouslySkipPermissions {
		check = "x"
	}
	cursor := "  "
	if m.cursor == settingsRowSkipPerms && !editing {
		cursor = cursorChevron + " "
	}
	line := fmt.Sprintf("%s[%s] Enable Claude --dangerously-skip-permissions", cursor, check)
	if m.cursor == settingsRowSkipPerms && !editing {
		line = styleSelected.Render(line)
	}
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(line))
	b.WriteString("\n\n")

	// Row 1: worktree base dir
	cursor = "  "
	if m.cursor == settingsRowWorktree && !editing {
		cursor = cursorChevron + " "
	}
	if m.editingRow == settingsRowWorktree {
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

	// Row 2: poll interval
	cursor = "  "
	if m.cursor == settingsRowPollInterval && !editing {
		cursor = cursorChevron + " "
	}
	if m.editingRow == settingsRowPollInterval {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("  Poll interval (seconds):"))
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 4).Render(m.pollIntervalInput.View()))
		b.WriteString("\n")
	} else {
		intervalStr := "30 (default)"
		if m.settings.PollIntervalSeconds > 0 {
			intervalStr = strconv.Itoa(m.settings.PollIntervalSeconds)
		}
		line = fmt.Sprintf("%sPoll interval (seconds): %s", cursor, intervalStr)
		if m.cursor == settingsRowPollInterval {
			line = styleSelected.Render(line)
		}
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(line))
		b.WriteString("\n")
	}

	if editing {
		b.WriteString(actionBar([]string{"[enter] save", "[esc] cancel"}))
	} else {
		b.WriteString(actionBar([]string{"[enter/space] toggle/edit"}, []string{"[esc] back"}))
	}

	return tea.NewView(b.String())
}
