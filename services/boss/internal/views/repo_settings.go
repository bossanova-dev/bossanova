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
	repoSettingsRowCanAutoMerge            = 1
	repoSettingsRowCanAutoMergeDependabot  = 2
	repoSettingsRowCanAutoAddressReviews   = 3
	repoSettingsRowCanAutoResolveConflicts = 4
	repoSettingsRowCount                   = 5
)

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

	// Name inline editing
	editing   bool
	nameInput textinput.Model

	width int
}

// NewRepoSettingsModel creates a RepoSettingsModel for the given repo ID.
func NewRepoSettingsModel(c client.BossClient, ctx context.Context, repoID string) RepoSettingsModel {
	ni := textinput.New()
	ni.Placeholder = "Repository name"
	ni.SetWidth(60)

	return RepoSettingsModel{
		client:    c,
		ctx:       ctx,
		repoID:    repoID,
		nameInput: ni,
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
			if m.cursor < repoSettingsRowCount-1 {
				m.cursor++
			}
		case "enter", "space":
			return m.activateRow()
		}
	}

	return m, nil
}

func (m RepoSettingsModel) updateEditing(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
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
		return m, m.saveSettings(&pb.UpdateRepoRequest{
			Id:          m.repoID,
			DisplayName: &name,
		})
	case "esc":
		m.editing = false
		m.err = nil
		m.nameInput.Blur()
		if m.repo != nil {
			m.nameInput.SetValue(m.repo.DisplayName)
		}
		return m, nil
	}

	var cmd tea.Cmd
	m.nameInput, cmd = m.nameInput.Update(msg)
	return m, cmd
}

func (m RepoSettingsModel) activateRow() (tea.Model, tea.Cmd) {
	if m.repo == nil {
		return m, nil
	}

	switch m.cursor {
	case repoSettingsRowName:
		m.editing = true
		return m, m.nameInput.Focus()
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
	b.WriteString(styleTitle.Render("Repository Settings"))
	b.WriteString("\n\n")

	if m.err != nil {
		b.WriteString(renderError(fmt.Sprintf("Error: %v", m.err), m.width))
		b.WriteString("\n")
	}

	// Row 0: Name
	if m.editing && m.cursor == repoSettingsRowName {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("  Name:"))
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 4).Render(m.nameInput.View()))
		b.WriteString("\n")
	} else {
		cursor := "  "
		if m.cursor == repoSettingsRowName {
			cursor = "> "
		}
		line := fmt.Sprintf("%sName: %s", cursor, m.repo.DisplayName)
		if m.cursor == repoSettingsRowName {
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
		if m.cursor == cb.row && !m.editing {
			cursor = "> "
		}
		line := fmt.Sprintf("%s[%s] %s", cursor, check, cb.label)
		if m.cursor == cb.row && !m.editing {
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
