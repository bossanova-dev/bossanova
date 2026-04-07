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

// repoSettingsLoadedMsg carries the loaded repo for the settings view.
type repoSettingsLoadedMsg struct {
	repo *pb.Repo
	err  error
}

// repoSettingsSavedMsg carries the result of saving repo settings.
type repoSettingsSavedMsg struct {
	repo *pb.Repo
	err  error
}

const (
	repoSettingsRowName                    = 0
	repoSettingsRowSetupScript             = 1
	repoSettingsRowMergeStrategy           = 2
	repoSettingsRowCanAutoMerge            = 3
	repoSettingsRowCanAutoMergeDependabot  = 4
	repoSettingsRowCanAutoAddressReviews   = 5
	repoSettingsRowCanAutoResolveConflicts = 6
	repoSettingsRowLinearApiKey            = 7
	repoSettingsRowCount                   = 8
)

// mergeStrategies is the cycle order for the merge strategy setting.
var mergeStrategies = []string{"merge", "rebase", "squash"}

// mergeStrategyLabel returns a human-readable label for a merge strategy.
func mergeStrategyLabel(s string) string {
	switch s {
	case "rebase":
		return "Rebase"
	case "squash":
		return "Squash"
	default:
		return "Merge commit"
	}
}

// maskAPIKey masks an API key, showing only the last 4 characters.
func maskAPIKey(key string) string {
	if key == "" {
		return "(not set)"
	}
	if len(key) <= 4 {
		return key
	}
	return strings.Repeat("*", len(key)-4) + key[len(key)-4:]
}

// RepoSettingsModel is the TUI view for editing per-repo settings.
type RepoSettingsModel struct {
	client client.BossClient
	ctx    context.Context
	repoID string
	repo   *pb.Repo
	cursor int
	cancel bool
	done   bool
	err    error

	// Inline editing (-1 = not editing, otherwise the row being edited)
	editingField      int
	nameInput         textinput.Model
	setupInput        textinput.Model
	linearApiKeyInput textinput.Model

	width int
}

// NewRepoSettingsModel creates a RepoSettingsModel for the given repo ID.
func NewRepoSettingsModel(c client.BossClient, ctx context.Context, repoID string) RepoSettingsModel {
	ni := textinput.New()
	ni.Placeholder = "Repository name"
	ni.SetWidth(60)

	si := textinput.New()
	si.Placeholder = "Optional, e.g. make setup"
	si.SetWidth(60)

	aki := textinput.New()
	aki.Placeholder = "lin_api_..."
	aki.SetWidth(60)

	return RepoSettingsModel{
		client:            c,
		ctx:               ctx,
		repoID:            repoID,
		editingField:      -1,
		nameInput:         ni,
		setupInput:        si,
		linearApiKeyInput: aki,
	}
}

func (m RepoSettingsModel) Init() tea.Cmd {
	return func() tea.Msg {
		repos, err := m.client.ListRepos(m.ctx)
		if err != nil {
			return repoSettingsLoadedMsg{err: err}
		}
		for _, r := range repos {
			if r.Id == m.repoID {
				return repoSettingsLoadedMsg{repo: r}
			}
		}
		return repoSettingsLoadedMsg{err: fmt.Errorf("repo not found")}
	}
}

func (m RepoSettingsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// When editing a text field, forward all message types (not just KeyMsg)
	// to the textinput so that paste messages are handled correctly.
	if m.editingField >= 0 {
		return m.updateEditing(msg)
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil

	case repoSettingsLoadedMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.repo = msg.repo
		m.nameInput.SetValue(m.repo.DisplayName)
		m.setupInput.SetValue(m.repo.GetSetupScript())
		// Note: API key is NOT pre-filled (always full replace for security)
		return m, nil

	case repoSettingsSavedMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.repo = msg.repo
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
			if m.cursor < repoSettingsRowCount-1 {
				m.cursor++
			}
		case "enter", "space":
			return m.activateRow()
		}
	}

	return m, nil
}

func (m RepoSettingsModel) updateEditing(msg tea.Msg) (tea.Model, tea.Cmd) {
	if keyMsg, ok := msg.(tea.KeyMsg); ok {
		switch keyMsg.String() {
		case "enter":
			return m.commitEdit()
		case "esc":
			return m.cancelEdit(), nil
		}
	}

	var cmd tea.Cmd
	switch m.editingField {
	case repoSettingsRowName:
		m.nameInput, cmd = m.nameInput.Update(msg)
	case repoSettingsRowSetupScript:
		m.setupInput, cmd = m.setupInput.Update(msg)
	case repoSettingsRowLinearApiKey:
		m.linearApiKeyInput, cmd = m.linearApiKeyInput.Update(msg)
	}
	return m, cmd
}

func (m RepoSettingsModel) commitEdit() (tea.Model, tea.Cmd) {
	switch m.editingField {
	case repoSettingsRowName:
		name := strings.TrimSpace(m.nameInput.Value())
		if name == "" {
			m.err = fmt.Errorf("name cannot be empty")
			return m, nil
		}
		m.editingField = -1
		m.err = nil
		m.nameInput.Blur()
		return m, m.saveSettings(&pb.UpdateRepoRequest{
			Id:          m.repoID,
			DisplayName: &name,
		})
	case repoSettingsRowSetupScript:
		val := strings.TrimSpace(m.setupInput.Value())
		m.editingField = -1
		m.err = nil
		m.setupInput.Blur()
		// Empty string clears the setup command.
		return m, m.saveSettings(&pb.UpdateRepoRequest{
			Id:          m.repoID,
			SetupScript: &val,
		})
	case repoSettingsRowLinearApiKey:
		val := strings.TrimSpace(m.linearApiKeyInput.Value())
		m.editingField = -1
		m.err = nil
		m.linearApiKeyInput.Blur()
		return m, m.saveSettings(&pb.UpdateRepoRequest{
			Id:           m.repoID,
			LinearApiKey: &val,
		})
	}
	return m, nil
}

func (m RepoSettingsModel) cancelEdit() RepoSettingsModel {
	switch m.editingField {
	case repoSettingsRowName:
		m.nameInput.Blur()
		if m.repo != nil {
			m.nameInput.SetValue(m.repo.DisplayName)
		}
	case repoSettingsRowSetupScript:
		m.setupInput.Blur()
		if m.repo != nil {
			m.setupInput.SetValue(m.repo.GetSetupScript())
		}
	case repoSettingsRowLinearApiKey:
		m.linearApiKeyInput.Blur()
		m.linearApiKeyInput.SetValue("") // Always empty (full replace)
	}
	m.editingField = -1
	m.err = nil
	return m
}

func (m RepoSettingsModel) activateRow() (tea.Model, tea.Cmd) {
	if m.repo == nil {
		return m, nil
	}

	switch m.cursor {
	case repoSettingsRowName:
		m.editingField = repoSettingsRowName
		return m, m.nameInput.Focus()
	case repoSettingsRowSetupScript:
		m.editingField = repoSettingsRowSetupScript
		return m, m.setupInput.Focus()
	case repoSettingsRowMergeStrategy:
		// Cycle through merge strategies.
		current := m.repo.MergeStrategy
		next := mergeStrategies[0]
		for i, s := range mergeStrategies {
			if s == current {
				next = mergeStrategies[(i+1)%len(mergeStrategies)]
				break
			}
		}
		m.repo.MergeStrategy = next
		return m, m.saveSettings(&pb.UpdateRepoRequest{
			Id:            m.repoID,
			MergeStrategy: &next,
		})
	case repoSettingsRowCanAutoMerge:
		v := !m.repo.CanAutoMerge
		m.repo.CanAutoMerge = v
		return m, m.saveSettings(&pb.UpdateRepoRequest{
			Id:           m.repoID,
			CanAutoMerge: &v,
		})
	case repoSettingsRowCanAutoMergeDependabot:
		v := !m.repo.CanAutoMergeDependabot
		m.repo.CanAutoMergeDependabot = v
		return m, m.saveSettings(&pb.UpdateRepoRequest{
			Id:                     m.repoID,
			CanAutoMergeDependabot: &v,
		})
	case repoSettingsRowCanAutoAddressReviews:
		v := !m.repo.CanAutoAddressReviews
		m.repo.CanAutoAddressReviews = v
		return m, m.saveSettings(&pb.UpdateRepoRequest{
			Id:                    m.repoID,
			CanAutoAddressReviews: &v,
		})
	case repoSettingsRowCanAutoResolveConflicts:
		v := !m.repo.CanAutoResolveConflicts
		m.repo.CanAutoResolveConflicts = v
		return m, m.saveSettings(&pb.UpdateRepoRequest{
			Id:                      m.repoID,
			CanAutoResolveConflicts: &v,
		})
	case repoSettingsRowLinearApiKey:
		m.editingField = repoSettingsRowLinearApiKey
		m.linearApiKeyInput.SetValue("") // Full replace, not edit
		return m, m.linearApiKeyInput.Focus()
	}
	return m, nil
}

func (m RepoSettingsModel) saveSettings(req *pb.UpdateRepoRequest) tea.Cmd {
	return func() tea.Msg {
		repo, err := m.client.UpdateRepo(m.ctx, req)
		return repoSettingsSavedMsg{repo: repo, err: err}
	}
}

// Cancelled returns true if the user exited the settings view.
func (m RepoSettingsModel) Cancelled() bool { return m.cancel }

// Done returns true if settings were saved and the view should close.
func (m RepoSettingsModel) Done() bool { return m.done }

func (m RepoSettingsModel) View() tea.View {
	if m.repo == nil {
		if m.err != nil {
			return tea.NewView(
				renderError(fmt.Sprintf("Error: %v", m.err), m.width) + "\n" +
					styleActionBar.Render("[esc] back"),
			)
		}
		return tea.NewView(lipgloss.NewStyle().Padding(1, 2).Render("Loading repository..."))
	}

	var b strings.Builder

	if m.err != nil {
		b.WriteString(renderError(fmt.Sprintf("Error: %v", m.err), m.width))
		b.WriteString("\n")
	}

	// Row 0: Name
	if m.editingField == repoSettingsRowName {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("  Name:"))
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 4).Render(m.nameInput.View()))
		b.WriteString("\n")
	} else {
		cursor := "  "
		if m.cursor == repoSettingsRowName {
			cursor = cursorChevron + " "
		}
		line := fmt.Sprintf("%sName: %s", cursor, m.repo.DisplayName)
		if m.cursor == repoSettingsRowName {
			line = styleSelected.Render(line)
		}
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(line))
		b.WriteString("\n")
	}

	// Row 1: Setup command
	if m.editingField == repoSettingsRowSetupScript {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("  Setup command:"))
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 4).Render(m.setupInput.View()))
		b.WriteString("\n")
	} else {
		cursor := "  "
		if m.cursor == repoSettingsRowSetupScript {
			cursor = cursorChevron + " "
		}
		val := m.repo.GetSetupScript()
		if val == "" {
			val = "(none)"
		}
		line := fmt.Sprintf("%sSetup command: %s", cursor, val)
		if m.cursor == repoSettingsRowSetupScript {
			line = styleSelected.Render(line)
		}
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(line))
		b.WriteString("\n")
	}

	// Row 2: Merge strategy
	{
		cursor := "  "
		if m.cursor == repoSettingsRowMergeStrategy {
			cursor = cursorChevron + " "
		}
		line := fmt.Sprintf("%sMerge strategy: %s", cursor, mergeStrategyLabel(m.repo.MergeStrategy))
		if m.cursor == repoSettingsRowMergeStrategy {
			line = styleSelected.Render(line)
		}
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(line))
		b.WriteString("\n")
	}

	b.WriteString("\n")

	// Checkbox rows
	checkboxes := []struct {
		label   string
		checked bool
		row     int
	}{
		{"Auto-merge PRs", m.repo.CanAutoMerge, repoSettingsRowCanAutoMerge},
		{"Auto-merge Dependabot PRs", m.repo.CanAutoMergeDependabot, repoSettingsRowCanAutoMergeDependabot},
		{"Auto-address review feedback", m.repo.CanAutoAddressReviews, repoSettingsRowCanAutoAddressReviews},
		{"Auto-resolve merge conflicts", m.repo.CanAutoResolveConflicts, repoSettingsRowCanAutoResolveConflicts},
	}

	for _, cb := range checkboxes {
		check := " "
		if cb.checked {
			check = "x"
		}
		cursor := "  "
		if m.cursor == cb.row && m.editingField < 0 {
			cursor = cursorChevron + " "
		}
		line := fmt.Sprintf("%s[%s] %s", cursor, check, cb.label)
		if m.cursor == cb.row && m.editingField < 0 {
			line = styleSelected.Render(line)
		}
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(line))
		b.WriteString("\n")
	}

	b.WriteString("\n")

	// Row 7: Linear API key
	if m.editingField == repoSettingsRowLinearApiKey {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("  Linear API key:"))
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 4).Render(m.linearApiKeyInput.View()))
		b.WriteString("\n")
	} else {
		cursor := "  "
		if m.cursor == repoSettingsRowLinearApiKey {
			cursor = cursorChevron + " "
		}
		line := fmt.Sprintf("%sLinear API key: %s", cursor, maskAPIKey(m.repo.LinearApiKey))
		if m.cursor == repoSettingsRowLinearApiKey {
			line = styleSelected.Render(line)
		}
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(line))
		b.WriteString("\n")
	}

	if m.editingField >= 0 {
		b.WriteString(actionBar([]string{"[enter] save", "[esc] cancel"}))
	} else {
		b.WriteString(actionBar([]string{"[enter/space] toggle/edit"}, []string{"[esc] back"}))
	}

	return tea.NewView(b.String())
}
