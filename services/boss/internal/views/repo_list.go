package views

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// --- Repo List View ---

// repoListLoadedMsg carries repos for the list view.
type repoListLoadedMsg struct {
	repos []*pb.Repo
	err   error
}

// repoRemovedMsg carries the result of removing a repo.
type repoRemovedMsg struct {
	err error
}

// RepoListModel displays registered repos with remove functionality.
type RepoListModel struct {
	client client.BossClient
	ctx    context.Context

	repos   []*pb.Repo
	cursor  int
	err     error
	cancel  bool
	loading bool

	// Remove confirmation
	confirming bool
}

// NewRepoListModel creates a RepoListModel.
func NewRepoListModel(c client.BossClient, ctx context.Context) RepoListModel {
	return RepoListModel{
		client:  c,
		ctx:     ctx,
		loading: true,
	}
}

func (m RepoListModel) Init() tea.Cmd {
	return func() tea.Msg {
		repos, err := m.client.ListRepos(m.ctx)
		return repoListLoadedMsg{repos: repos, err: err}
	}
}

func (m RepoListModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case repoListLoadedMsg:
		m.loading = false
		m.repos = msg.repos
		m.err = msg.err
		return m, nil

	case repoRemovedMsg:
		m.confirming = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		// Refresh list.
		m.loading = true
		return m, m.Init()

	case tea.KeyMsg:
		if m.confirming {
			return m.updateConfirm(msg)
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
			if m.cursor < len(m.repos)-1 {
				m.cursor++
			}
		case "a":
			return m, func() tea.Msg { return switchViewMsg{view: ViewRepoAdd} }
		case "d":
			if len(m.repos) > 0 {
				m.confirming = true
			}
		}
	}

	return m, nil
}

func (m RepoListModel) updateConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y":
		repo := m.repos[m.cursor]
		return m, func() tea.Msg {
			err := m.client.RemoveRepo(m.ctx, repo.Id)
			return repoRemovedMsg{err: err}
		}
	case "n", "esc":
		m.confirming = false
	}
	return m, nil
}

// Cancelled returns true if the user exited the list.
func (m RepoListModel) Cancelled() bool { return m.cancel }

func (m RepoListModel) View() tea.View {
	if m.err != nil {
		return tea.NewView(
			styleError.Render(fmt.Sprintf("Error: %v", m.err)) + "\n" +
				styleActionBar.Render("[esc] back"),
		)
	}

	if m.loading {
		return tea.NewView(lipgloss.NewStyle().Padding(1, 2).Render("Loading repositories..."))
	}

	var b strings.Builder
	b.WriteString(styleTitle.Render("Repositories"))
	b.WriteString("\n")

	if len(m.repos) == 0 {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("No repositories registered."))
		b.WriteString("\n\n")
		b.WriteString(styleActionBar.Render("[a] add  [esc] back"))
		return tea.NewView(b.String())
	}

	// Table header.
	header := fmt.Sprintf("  %-10s %-20s %-30s %-12s %-10s",
		"ID", "NAME", "PATH", "BRANCH", "SETUP")
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Faint(true).Render(header))
	b.WriteString("\n")

	for i, repo := range m.repos {
		cursor := "  "
		if i == m.cursor {
			cursor = "> "
		}
		id := truncate(repo.Id, 8)
		name := truncate(repo.DisplayName, 20)
		path := truncate(repo.LocalPath, 30)
		branch := truncate(repo.DefaultBaseBranch, 12)
		setup := "-"
		if repo.SetupScript != nil {
			setup = truncate(*repo.SetupScript, 10)
		}

		row := fmt.Sprintf("%s%-10s %-20s %-30s %-12s %-10s",
			cursor, id, name, path, branch, setup)
		if i == m.cursor {
			row = styleSelected.Render(row)
		}
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(row))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	if m.confirming {
		repo := m.repos[m.cursor]
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorRed).Render(
			fmt.Sprintf("Remove %q? [y/n]", repo.DisplayName)))
	} else {
		b.WriteString(styleActionBar.Render("[a] add  [d] remove  [esc] back"))
	}

	return tea.NewView(b.String())
}
