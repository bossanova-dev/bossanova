package views

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"charm.land/bubbles/v2/table"
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
	table   table.Model
	err     error
	cancel  bool
	loading bool

	// Remove confirmation
	confirming bool

	// Layout
	width  int
	height int
}

// NewRepoListModel creates a RepoListModel.
func NewRepoListModel(c client.BossClient, ctx context.Context) RepoListModel {
	return RepoListModel{
		client:  c,
		ctx:     ctx,
		loading: true,
		table:   newBossTable(nil, nil, 0),
	}
}

func (m RepoListModel) Init() tea.Cmd {
	return func() tea.Msg {
		repos, err := m.client.ListRepos(m.ctx)
		return repoListLoadedMsg{repos: repos, err: err}
	}
}

func (m *RepoListModel) buildTable() {
	if len(m.repos) == 0 {
		return
	}

	ids := make([]string, len(m.repos))
	names := make([]string, len(m.repos))
	paths := make([]string, len(m.repos))
	for i, repo := range m.repos {
		id := repo.Id
		if len(id) > shortIDLen {
			id = id[:shortIDLen]
		}
		ids[i] = id
		names[i] = repo.DisplayName
		paths[i] = repo.LocalPath
	}

	cols := []table.Column{
		cursorColumn,
		{Title: "ID", Width: maxColWidth("ID", ids, shortIDLen) + tableColumnSep},
		{Title: "NAME", Width: maxColWidth("NAME", names, 30) + tableColumnSep},
		{Title: "PATH", Width: maxColWidth("PATH", paths, 60) + tableColumnSep},
	}

	cursor := m.table.Cursor()
	rows := make([]table.Row, len(m.repos))
	for i := range m.repos {
		indicator := ""
		if i == cursor {
			indicator = cursorChevron
		}
		rows[i] = table.Row{indicator, ids[i], names[i], paths[i]}
	}
	m.table.SetColumns(cols)
	m.table.SetRows(rows)
	m.table.SetWidth(columnsWidth(cols))
	m.table.SetHeight(m.tableHeight())
	m.table.SetCursor(cursor)
}

func (m RepoListModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.table.SetHeight(m.tableHeight())
		m.table.SetWidth(msg.Width)
		return m, nil

	case repoListLoadedMsg:
		m.loading = false
		m.repos = msg.repos
		m.err = msg.err
		slices.SortFunc(m.repos, func(a, b *pb.Repo) int {
			return strings.Compare(strings.ToLower(a.DisplayName), strings.ToLower(b.DisplayName))
		})
		m.buildTable()
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
		case "a":
			return m, func() tea.Msg { return switchViewMsg{view: ViewRepoAdd} }
		case "d":
			if len(m.repos) > 0 {
				m.confirming = true
			}
			return m, nil
		case "s", "enter":
			if len(m.repos) > 0 {
				repo := m.repos[m.table.Cursor()]
				return m, func() tea.Msg { return switchViewMsg{view: ViewRepoSettings, sessionID: repo.Id} }
			}
			return m, nil
		}

		// Forward navigation keys to the table.
		var cmd tea.Cmd
		m.table, cmd = m.table.Update(msg)
		updateCursorColumn(&m.table)
		return m, cmd
	}

	return m, nil
}

func (m RepoListModel) updateConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "enter":
		repo := m.repos[m.table.Cursor()]
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

// tableHeight returns the height to pass to table.SetHeight.
func (m RepoListModel) tableHeight() int {
	return clampedTableHeight(len(m.repos), m.height, 4) // title + blank + blank + action bar
}

func (m RepoListModel) View() tea.View {
	if m.err != nil {
		return tea.NewView(
			renderError(fmt.Sprintf("Error: %v", m.err), m.width) + "\n" +
				styleActionBar.Render("[esc] back"),
		)
	}

	if m.loading {
		return tea.NewView(lipgloss.NewStyle().Padding(1, 2).Render("Loading repositories..."))
	}

	var b strings.Builder
	b.WriteString(styleTitle.Render("Repositories"))
	b.WriteString("\n\n")

	if len(m.repos) == 0 {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("No repositories registered."))
		b.WriteString("\n")
		b.WriteString(styleActionBar.Render("[a]dd  [esc] back"))
		return tea.NewView(b.String())
	}

	b.WriteString(lipgloss.NewStyle().Padding(0, 1).Render(m.table.View()))
	b.WriteString("\n")

	if m.confirming {
		b.WriteString("\n")
		repo := m.repos[m.table.Cursor()]
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorDanger).Render(
			fmt.Sprintf("Remove %q?", repo.DisplayName)))
		b.WriteString("\n")
		b.WriteString(styleActionBar.Render("[y/enter] confirm  [n/esc] cancel"))
	} else {
		b.WriteString(styleActionBar.Render("[s/enter] settings  [a]dd  [d] remove  [esc] back"))
	}

	return tea.NewView(b.String())
}
