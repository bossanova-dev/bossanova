package views

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"charm.land/bubbles/v2/spinner"
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
	id  string
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
	confirming     bool
	deletingRepoID string
	spinner        spinner.Model

	// Navigation
	highlightRepoID string // repo to auto-highlight after returning from settings
	returnView      View   // view to route back to on cancel

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
		spinner: newStatusSpinner(),
		table:   newBossTable(nil, nil, 0),
	}
}

func (m RepoListModel) Init() tea.Cmd {
	fetch := func() tea.Msg {
		repos, err := m.client.ListRepos(m.ctx)
		return repoListLoadedMsg{repos: repos, err: err}
	}
	return tea.Batch(fetch, m.spinner.Tick)
}

func (m *RepoListModel) buildTable() {
	if len(m.repos) == 0 {
		m.table.SetHeight(m.tableHeight())
		m.table.SetWidth(m.width)
		return
	}

	home, _ := os.UserHomeDir()
	names := make([]string, len(m.repos))
	paths := make([]string, len(m.repos))
	statuses := make([]string, len(m.repos))
	for i, repo := range m.repos {
		names[i] = repo.DisplayName
		p := repo.LocalPath
		if home != "" {
			p = strings.Replace(p, home, "~", 1)
		}
		paths[i] = p
		if repo.Id != "" && repo.Id == m.deletingRepoID {
			statuses[i] = renderRowPendingStatus(m.spinner, "deleting")
		}
	}

	cols := []responsiveColumn{
		{col: cursorColumn, priority: 0, minWidth: 1},
		{col: table.Column{Title: "NAME", Width: maxColWidth("NAME", names, 30) + tableColumnSep}, priority: 0, minWidth: 1},
		{col: table.Column{Title: "PATH", Width: maxColWidth("PATH", paths, 60) + tableColumnSep}, priority: 2, minWidth: 1},
		// The status column is unlabelled: it only ever holds the transient
		// "deleting" spinner, so a permanent STATUS heading advertises a column
		// that is blank in the steady state. Width still sizes against the
		// header text so the reserved space (and the fit priority) is unchanged.
		{col: table.Column{Title: "", Width: maxColWidth("STATUS", statuses, 10) + tableColumnSep}, priority: 1, minWidth: 1},
	}
	fitted := fitColumnsIndexed(cols, fitAvailWidth(m.width, 1))
	fittedCols := fittedColumns(fitted)

	cursor := m.table.Cursor()
	rows := make([]table.Row, len(m.repos))
	for i := range m.repos {
		indicator := ""
		if i == cursor {
			indicator = cursorChevron
		}
		rows[i] = projectRow(fitted, table.Row{indicator, names[i], paths[i], statuses[i]})
	}
	setTableContent(&m.table, fittedCols, rows)
	m.table.SetWidth(columnsWidth(fittedCols))
	m.table.SetHeight(m.tableHeight())
	m.table.SetCursor(cursor)
}

func (m RepoListModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.buildTable()
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		if m.deletingRepoID != "" {
			m.buildTable()
		}
		return m, cmd

	case repoListLoadedMsg:
		m.loading = false
		m.repos = msg.repos
		m.err = msg.err
		slices.SortFunc(m.repos, func(a, b *pb.Repo) int {
			return strings.Compare(strings.ToLower(a.DisplayName), strings.ToLower(b.DisplayName))
		})
		m.buildTable()
		if m.highlightRepoID != "" {
			for i, repo := range m.repos {
				if repo.Id == m.highlightRepoID {
					m.table.SetCursor(i)
					updateCursorColumn(&m.table)
					break
				}
			}
			m.highlightRepoID = ""
		}
		return m, nil

	case repoRemovedMsg:
		if msg.id == m.deletingRepoID {
			m.deletingRepoID = ""
		}
		m.confirming = false
		if msg.err != nil {
			m.err = msg.err
			m.buildTable()
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
		case "esc":
			m.cancel = true
			return m, nil
		case "a":
			return m, func() tea.Msg { return switchViewMsg{view: ViewRepoAdd} }
		case "d":
			if len(m.repos) > 0 && m.deletingRepoID == "" {
				m.confirming = true
			}
			return m, nil
		case "enter":
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
		m.confirming = false
		m.deletingRepoID = repo.Id
		m.buildTable()
		return m, func() tea.Msg {
			err := m.client.RemoveRepo(m.ctx, repo.Id)
			return repoRemovedMsg{id: repo.Id, err: err}
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
	actionLines := 1
	if !m.confirming {
		actionLines = actionBarLineCount(m.width,
			[]string{"[enter] settings", "[d]elete"},
			[]string{"[a]dd"},
			[]string{"[esc] back"},
		)
	}
	return clampedTableHeight(len(m.repos), m.height, bannerOverhead+1+actionBarPadY+actionLines) // gap + actionbar padding + actionbar
}

func (m RepoListModel) View() tea.View {
	if m.err != nil {
		return tea.NewView(
			renderError(rpcErrorMessage(m.err), m.width) + "\n" +
				styleActionBar.Render("[esc] back"),
		)
	}

	if m.loading {
		return tea.NewView(lipgloss.NewStyle().Padding(0, 2).Render("Loading repositories..."))
	}

	var b strings.Builder

	if len(m.repos) == 0 {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(
			"All of the repositories you are working on will be listed here.\n\n" +
				"Press 'a' to add your first repository.",
		))
		b.WriteString("\n")
		b.WriteString(actionBarWidth(m.width, []string{"[a]dd"}, []string{"[esc] back"}))
		return tea.NewView(b.String())
	}

	b.WriteString(lipgloss.NewStyle().Padding(0, 1).Render(m.table.View()))
	b.WriteString("\n")

	if m.confirming {
		b.WriteString("\n")
		repo := m.repos[m.table.Cursor()]
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorDanger).Render(
			fmt.Sprintf("Delete %q?", repo.DisplayName)))
		b.WriteString("\n")
		b.WriteString(styleActionBar.Render("[y/enter] confirm  [n/esc] cancel"))
	} else {
		b.WriteString(actionBarWidth(m.width,
			[]string{"[enter] settings", "[d]elete"},
			[]string{"[a]dd"},
			[]string{"[esc] back"},
		))
	}

	return tea.NewView(b.String())
}
