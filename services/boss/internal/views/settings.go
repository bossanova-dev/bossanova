package views

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/internal/client"
	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/telemetry"
)

// settingsRowKind tags a row with the action it represents. The kind drives
// behaviour on enter/space (toggle vs. edit vs. cycle vs. select).
type settingsRowKind int

const (
	settingsRowKindBool          settingsRowKind = iota // checkbox toggle (plugin Bool config)
	settingsRowKindString                               // text input (plugin String config)
	settingsRowKindEnum                                 // cycle picker (plugin Enum config)
	settingsRowKindWorktree                             // built-in worktree base directory
	settingsRowKindPollInterval                         // built-in poll interval seconds
	settingsRowKindDefaultAgent                         // cycle picker over loaded agents
	settingsRowKindAgentHeader                          // pseudo-row: section header (non-interactive)
	settingsRowKindTracingHeader                        // pseudo-row: tracing section header (non-interactive)
	settingsRowKindEventTracing                         // built-in event tracing toggle
	settingsRowKindPostHogToken                         // built-in PostHog project token
	settingsRowKindPostHogHost                          // built-in PostHog host
)

// settingsRow is a single addressable line in the settings TUI. Header
// rows have IsHeader=true and are skipped during cursor navigation.
type settingsRow struct {
	Kind     settingsRowKind
	Plugin   string   // plugin name for plugin-config rows
	Key      string   // setting key for plugin-config rows
	Label    string   // text shown to the user
	Allowed  []string // for enum rows
	IsHeader bool     // header rows are non-interactive
}

// SettingsModel renders both the built-in global settings and a
// per-agent block for every loaded agent runner. Each agent contributes
// one row per UserSetting it advertises through ListAgents.
type SettingsModel struct {
	client client.BossClient
	ctx    context.Context

	settings   config.Settings
	agents     []client.AgentInfo
	rows       []settingsRow
	cursor     int
	cancel     bool
	err        error
	editingRow int // index into rows; -1 = not editing

	worktreeDirInput  textinput.Model
	pollIntervalInput textinput.Model
	stringInput       textinput.Model // shared for plugin String rows

	width int
}

// NewSettingsModel constructs the settings view. With a non-nil client,
// the view loads agents via ListAgents and renders per-agent settings
// sections. A nil client (legacy callers / tests) renders only the
// built-in rows.
func NewSettingsModel(c client.BossClient, ctx context.Context) SettingsModel {
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

	strIn := textinput.New()
	strIn.SetWidth(40)

	m := SettingsModel{
		client:            c,
		ctx:               ctx,
		settings:          s,
		editingRow:        -1,
		worktreeDirInput:  wtIn,
		pollIntervalInput: piIn,
		stringInput:       strIn,
	}

	if c != nil {
		// A failed agent fetch is non-fatal — we degrade to the built-in
		// rows so the user can still edit worktree dir / poll interval.
		agents, err := c.ListAgents(ctx)
		if err == nil {
			m.agents = agents
		}
	}

	m.rebuildRows()
	return m
}

// rebuildRows reconstructs m.rows from m.settings + m.agents. Called on
// construction and after agent / setting mutations that change row counts.
func (m *SettingsModel) rebuildRows() {
	m.rows = m.rows[:0]

	// Built-in global settings come first.
	m.rows = append(m.rows,
		settingsRow{Kind: settingsRowKindWorktree, Label: "Worktree base directory"},
		settingsRow{Kind: settingsRowKindPollInterval, Label: "Poll interval (seconds)"},
	)

	// Default agent picker — only meaningful when >1 agent is loaded.
	if len(m.agents) > 1 {
		allowed := make([]string, len(m.agents))
		for i, a := range m.agents {
			allowed[i] = a.Name
		}
		m.rows = append(m.rows, settingsRow{
			Kind:    settingsRowKindDefaultAgent,
			Label:   "Default agent",
			Allowed: allowed,
		})
	}

	// Per-agent sections.
	for _, a := range m.agents {
		if len(a.UserSettings) == 0 {
			continue
		}
		m.rows = append(m.rows, settingsRow{
			Kind:     settingsRowKindAgentHeader,
			Label:    a.Name,
			Plugin:   a.Name,
			IsHeader: true,
		})
		for _, us := range a.UserSettings {
			row := settingsRow{
				Plugin:  a.Name,
				Key:     us.Key,
				Label:   us.Label,
				Allowed: us.AllowedValues,
			}
			switch us.Type {
			case client.SettingTypeBool:
				row.Kind = settingsRowKindBool
			case client.SettingTypeEnum:
				row.Kind = settingsRowKindEnum
			default:
				// Unspecified or String both render as text input.
				row.Kind = settingsRowKindString
			}
			m.rows = append(m.rows, row)
		}
	}

	m.rows = append(m.rows,
		settingsRow{Kind: settingsRowKindTracingHeader, Label: "tracing", IsHeader: true},
		settingsRow{Kind: settingsRowKindEventTracing, Label: "Enable event tracing (for debugging problems)"},
	)
	if m.settings.EventTracingEnabled {
		m.rows = append(m.rows,
			settingsRow{Kind: settingsRowKindPostHogToken, Label: "PostHog project token"},
			settingsRow{Kind: settingsRowKindPostHogHost, Label: "PostHog host"},
		)
	}

	// Clamp cursor to a non-header row.
	if m.cursor >= len(m.rows) {
		m.cursor = 0
	}
	for m.cursor < len(m.rows) && m.rows[m.cursor].IsHeader {
		m.cursor++
	}
}

func (m SettingsModel) Init() tea.Cmd { return nil }

func (m SettingsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
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
			m.moveCursor(-1)
		case "down", "j":
			m.moveCursor(+1)
		case "enter", "space", " ":
			return m.activateRow()
		}
	}
	return m, nil
}

// moveCursor advances the cursor by `delta`, skipping header pseudo-rows.
func (m *SettingsModel) moveCursor(delta int) {
	if len(m.rows) == 0 {
		return
	}
	c := m.cursor + delta
	for c >= 0 && c < len(m.rows) && m.rows[c].IsHeader {
		c += delta
	}
	if c < 0 || c >= len(m.rows) {
		return
	}
	m.cursor = c
}

func (m SettingsModel) activateRow() (tea.Model, tea.Cmd) {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return m, nil
	}
	row := m.rows[m.cursor]
	switch row.Kind {
	case settingsRowKindAgentHeader, settingsRowKindTracingHeader:
		// Header rows are non-interactive and the cursor never lands on
		// one (moveCursor skips them); nothing to do.
		return m, nil
	case settingsRowKindBool:
		current := config.PluginConfigBool(&m.settings, row.Plugin, row.Key)
		config.SetPluginConfigBool(&m.settings, row.Plugin, row.Key, !current)
		if err := config.Save(m.settings); err != nil {
			m.err = err
		}
	case settingsRowKindEnum:
		// Cycle to the next allowed value.
		if len(row.Allowed) == 0 {
			return m, nil
		}
		current := config.PluginConfigString(&m.settings, row.Plugin, row.Key)
		next := nextEnumValue(row.Allowed, current)
		if err := config.SetPluginConfigEnum(&m.settings, row.Plugin, row.Key, next, row.Allowed); err != nil {
			m.err = err
			return m, nil
		}
		if err := config.Save(m.settings); err != nil {
			m.err = err
		}
	case settingsRowKindString:
		m.editingRow = m.cursor
		m.stringInput.SetValue(config.PluginConfigString(&m.settings, row.Plugin, row.Key))
		return m, m.stringInput.Focus()
	case settingsRowKindEventTracing:
		m.settings.EventTracingEnabled = !m.settings.EventTracingEnabled
		if m.settings.EventTracingEnabled {
			if m.settings.PostHogProjectToken == "" {
				m.settings.PostHogProjectToken = telemetry.ProductionProjectToken
			}
			if m.settings.PostHogHost == "" {
				m.settings.PostHogHost = telemetry.DefaultHost
			}
		}
		if err := config.Save(m.settings); err != nil {
			m.err = err
		} else {
			m.rebuildRows()
		}
	case settingsRowKindPostHogToken:
		m.editingRow = m.cursor
		m.stringInput.SetValue(m.settings.PostHogProjectToken)
		return m, m.stringInput.Focus()
	case settingsRowKindPostHogHost:
		m.editingRow = m.cursor
		m.stringInput.SetValue(m.settings.PostHogHost)
		return m, m.stringInput.Focus()
	case settingsRowKindWorktree:
		m.editingRow = m.cursor
		return m, m.worktreeDirInput.Focus()
	case settingsRowKindPollInterval:
		m.editingRow = m.cursor
		return m, m.pollIntervalInput.Focus()
	case settingsRowKindDefaultAgent:
		if len(row.Allowed) == 0 {
			return m, nil
		}
		next := nextEnumValue(row.Allowed, m.settings.DefaultAgent)
		m.settings.DefaultAgent = next
		if err := config.Save(m.settings); err != nil {
			m.err = err
		}
	}
	return m, nil
}

// nextEnumValue returns the value after current in allowed, wrapping
// around at the end. Returns the first value when current is empty or
// not present.
func nextEnumValue(allowed []string, current string) string {
	for i, v := range allowed {
		if v == current {
			return allowed[(i+1)%len(allowed)]
		}
	}
	return allowed[0]
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

	row := m.rows[m.editingRow]
	var cmd tea.Cmd
	switch row.Kind { //nolint:exhaustive // only edit-capable kinds reach here
	case settingsRowKindWorktree:
		m.worktreeDirInput, cmd = m.worktreeDirInput.Update(msg)
	case settingsRowKindPollInterval:
		m.pollIntervalInput, cmd = m.pollIntervalInput.Update(msg)
	case settingsRowKindString:
		m.stringInput, cmd = m.stringInput.Update(msg)
	case settingsRowKindPostHogToken, settingsRowKindPostHogHost:
		m.stringInput, cmd = m.stringInput.Update(msg)
	}
	return m, cmd
}

func (m SettingsModel) commitEdit() (tea.Model, tea.Cmd) {
	row := m.rows[m.editingRow]
	switch row.Kind { //nolint:exhaustive // only edit-capable kinds reach here
	case settingsRowKindWorktree:
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

	case settingsRowKindPollInterval:
		val := m.pollIntervalInput.Value()
		if val == "" {
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

	case settingsRowKindString:
		val := m.stringInput.Value()
		config.SetPluginConfigString(&m.settings, row.Plugin, row.Key, val)
		if err := config.Save(m.settings); err != nil {
			m.err = err
			return m, nil
		}
		m.editingRow = -1
		m.err = nil
		m.stringInput.Blur()
	case settingsRowKindPostHogToken:
		m.settings.PostHogProjectToken = strings.TrimSpace(m.stringInput.Value())
		if err := config.Save(m.settings); err != nil {
			m.err = err
			return m, nil
		}
		m.editingRow = -1
		m.err = nil
		m.stringInput.Blur()
	case settingsRowKindPostHogHost:
		host := strings.TrimSpace(m.stringInput.Value())
		if host == "" {
			host = telemetry.DefaultHost
		}
		m.settings.PostHogHost = host
		if err := config.Save(m.settings); err != nil {
			m.err = err
			return m, nil
		}
		m.editingRow = -1
		m.err = nil
		m.stringInput.Blur()
	}
	return m, nil
}

func (m SettingsModel) cancelEdit() (tea.Model, tea.Cmd) {
	row := m.rows[m.editingRow]
	switch row.Kind { //nolint:exhaustive // only edit-capable kinds reach here
	case settingsRowKindWorktree:
		m.worktreeDirInput.Blur()
		m.worktreeDirInput.SetValue(m.settings.WorktreeBaseDir)
	case settingsRowKindPollInterval:
		m.pollIntervalInput.Blur()
		if m.settings.PollIntervalSeconds > 0 {
			m.pollIntervalInput.SetValue(strconv.Itoa(m.settings.PollIntervalSeconds))
		} else {
			m.pollIntervalInput.SetValue("")
		}
	case settingsRowKindString:
		m.stringInput.Blur()
	case settingsRowKindPostHogToken, settingsRowKindPostHogHost:
		m.stringInput.Blur()
	}
	m.editingRow = -1
	m.err = nil
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

	for i, row := range m.rows {
		if row.IsHeader {
			b.WriteString("\n")
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Bold(true).Render(row.Label))
			b.WriteString("\n")
			continue
		}
		m.renderRow(&b, i, row, editing)
	}

	b.WriteString("\n")
	if editing {
		b.WriteString(actionBar([]string{"[enter] save", "[esc] cancel"}))
	} else {
		b.WriteString(actionBar([]string{"[enter/space] toggle/edit"}, []string{"[esc] back"}))
	}

	return tea.NewView(b.String())
}

// renderRow writes a single non-header row to b.
func (m SettingsModel) renderRow(b *strings.Builder, i int, row settingsRow, editing bool) {
	cursor := "  "
	if i == m.cursor && !editing {
		cursor = cursorChevron + " "
	}

	// Editing branches show the input inline.
	if m.editingRow == i {
		switch row.Kind { //nolint:exhaustive // only edit-capable kinds need a branch
		case settingsRowKindWorktree:
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("  Worktree base directory:"))
			b.WriteString("\n")
			b.WriteString(lipgloss.NewStyle().Padding(0, 4).Render(m.worktreeDirInput.View()))
			b.WriteString("\n")
			return
		case settingsRowKindPollInterval:
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("  Poll interval (seconds):"))
			b.WriteString("\n")
			b.WriteString(lipgloss.NewStyle().Padding(0, 4).Render(m.pollIntervalInput.View()))
			b.WriteString("\n")
			return
		case settingsRowKindString:
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(fmt.Sprintf("  %s:", row.Label)))
			b.WriteString("\n")
			b.WriteString(lipgloss.NewStyle().Padding(0, 4).Render(m.stringInput.View()))
			b.WriteString("\n")
			return
		case settingsRowKindPostHogToken, settingsRowKindPostHogHost:
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(fmt.Sprintf("  %s:", row.Label)))
			b.WriteString("\n")
			b.WriteString(lipgloss.NewStyle().Padding(0, 4).Render(m.stringInput.View()))
			b.WriteString("\n")
			return
		}
	}

	var line string
	switch row.Kind { //nolint:exhaustive // header rows take an early return path in View
	case settingsRowKindBool:
		check := " "
		if config.PluginConfigBool(&m.settings, row.Plugin, row.Key) {
			check = "x"
		}
		line = fmt.Sprintf("%s[%s] %s", cursor, check, row.Label)
	case settingsRowKindString:
		val := config.PluginConfigString(&m.settings, row.Plugin, row.Key)
		if val == "" {
			val = "(not set)"
		}
		line = fmt.Sprintf("%s%s: %s", cursor, row.Label, val)
	case settingsRowKindEnum:
		val := config.PluginConfigString(&m.settings, row.Plugin, row.Key)
		if val == "" && len(row.Allowed) > 0 {
			// When Allowed[0] is the empty string the plugin advertises ""
			// as the explicit "use plugin default" sentinel — render it as
			// "(default)" rather than " (default)" so the row reads
			// cleanly without a leading space.
			if row.Allowed[0] == "" {
				val = "(default)"
			} else {
				val = row.Allowed[0] + " (default)"
			}
		}
		line = fmt.Sprintf("%s%s: %s", cursor, row.Label, val)
	case settingsRowKindWorktree:
		line = fmt.Sprintf("%sWorktree base directory: %s", cursor, m.settings.WorktreeBaseDir)
	case settingsRowKindPollInterval:
		intervalStr := "30 (default)"
		if m.settings.PollIntervalSeconds > 0 {
			intervalStr = strconv.Itoa(m.settings.PollIntervalSeconds)
		}
		line = fmt.Sprintf("%sPoll interval (seconds): %s", cursor, intervalStr)
	case settingsRowKindEventTracing:
		check := " "
		if m.settings.EventTracingEnabled {
			check = "x"
		}
		line = fmt.Sprintf("%s[%s] %s", cursor, check, row.Label)
	case settingsRowKindPostHogToken:
		val := m.settings.PostHogProjectToken
		if val == "" {
			val = "(not set)"
		}
		line = fmt.Sprintf("%s%s: %s", cursor, row.Label, val)
	case settingsRowKindPostHogHost:
		val := m.settings.PostHogHost
		if val == "" {
			val = telemetry.DefaultHost
		}
		line = fmt.Sprintf("%s%s: %s", cursor, row.Label, val)
	case settingsRowKindDefaultAgent:
		val := m.settings.DefaultAgent
		if val == "" {
			val = "(unset)"
		}
		line = fmt.Sprintf("%s%s: %s", cursor, row.Label, val)
	}

	if i == m.cursor && !editing {
		line = styleSelected.Render(line)
	}
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(line))
	b.WriteString("\n")
}
