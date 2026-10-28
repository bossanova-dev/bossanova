package repos

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/table"
	"github.com/recurser/boss/api/tui/ui"
	"github.com/recurser/boss/internal/domain/display"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// --- Repo List View ---

// missingWebhooksHint explains Repo.is_missing_webhooks in repo settings; the
// list marks the same repos with missingWebhooksLabel.
const (
	missingWebhooksHint  = "Polling only — no GitHub webhooks received (is the Bossanova app installed on this repo?)"
	missingWebhooksLabel = "no webhooks"
)

// RepoListLoadedMsg carries repos for the list view.
type RepoListLoadedMsg struct {
	Repos []*pb.Repo
	Err   error
}

// repoRemovedMsg carries the result of removing a repo.
type repoRemovedMsg struct {
	id  string
	err error
}

// RepoListModel displays registered repos with remove functionality.
type RepoListModel struct {
	client ListClient
	ctx    context.Context

	repos   []*pb.Repo
	table   ui.Table
	err     error
	cancel  bool
	loading bool

	// Remove confirmation
	confirming     bool
	deletingRepoID string
	spinner        spinner.Model

	// Navigation
	highlightRepoID string  // repo to auto-highlight after returning from settings
	returnView      ui.View // view to route back to on cancel

	// Layout
	width  int
	height int
}

// NewRepoListModel creates a RepoListModel.
func NewRepoListModel(c ListClient, ctx context.Context) RepoListModel {
	return RepoListModel{
		client:  c,
		ctx:     ctx,
		loading: true,
		spinner: ui.NewStatusSpinner(),
		table:   ui.NewBossTable(nil, nil, 0),
	}
}

func (m RepoListModel) Init() tea.Cmd {
	fetch := func() tea.Msg {
		repos, err := m.client.ListRepos(m.ctx)
		return RepoListLoadedMsg{Repos: repos, Err: err}
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
		switch {
		case repo.Id != "" && repo.Id == m.deletingRepoID:
			statuses[i] = ui.RenderRowPendingStatus(m.spinner, "deleting")
		case repo.GetIsMissingWebhooks():
			statuses[i] = ui.StyleSubtle.Render(missingWebhooksLabel)
		}
	}

	cols := []ui.ResponsiveColumn{
		{Col: ui.CursorColumn, Priority: 0, MinWidth: 1},
		{Col: table.Column{Title: "NAME", Width: display.MaxColWidth("NAME", names, 30) + ui.TableColumnSep}, Priority: 0, MinWidth: 1},
		{Col: table.Column{Title: "PATH", Width: display.MaxColWidth("PATH", paths, 60) + ui.TableColumnSep}, Priority: 2, MinWidth: 1},
		// The status column is unlabelled: it holds only the transient
		// "deleting" spinner and the rare "no webhooks" marker, so a permanent
		// STATUS heading advertises a column that is blank in the steady state.
		// Width still sizes against the header text so the reserved space (and
		// the fit priority) is unchanged.
		{Col: table.Column{Title: "", Width: display.MaxColWidth("STATUS", statuses, len(missingWebhooksLabel)) + ui.TableColumnSep}, Priority: 1, MinWidth: 1},
	}
	fitted := ui.FitColumnsIndexed(cols, ui.FitAvailWidth(m.width, 1))
	fittedCols := ui.FittedColumns(fitted)

	cursor := m.table.Cursor()
	rows := make([]table.Row, len(m.repos))
	for i := range m.repos {
		indicator := ""
		if i == cursor {
			indicator = ui.CursorChevron
		}
		rows[i] = ui.ProjectRow(fitted, table.Row{indicator, names[i], paths[i], statuses[i]})
	}
	ui.SetTableContent(&m.table, fittedCols, rows)
	m.table.SetWidth(ui.ColumnsWidth(fittedCols))
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

	case RepoListLoadedMsg:
		m.loading = false
		m.repos = msg.Repos
		m.err = msg.Err
		slices.SortFunc(m.repos, func(a, b *pb.Repo) int {
			return strings.Compare(strings.ToLower(a.DisplayName), strings.ToLower(b.DisplayName))
		})
		m.buildTable()
		if m.highlightRepoID != "" {
			for i, repo := range m.repos {
				if repo.Id == m.highlightRepoID {
					m.table.SetCursor(i)
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
			return m, func() tea.Msg { return ui.SwitchViewMsg{View: ui.ViewRepoAdd} }
		case "d":
			if len(m.repos) > 0 && m.deletingRepoID == "" {
				m.confirming = true
			}
			return m, nil
		case "enter":
			if len(m.repos) > 0 {
				repo := m.repos[m.table.Cursor()]
				return m, func() tea.Msg { return ui.SwitchViewMsg{View: ui.ViewRepoSettings, SessionID: repo.Id} }
			}
			return m, nil
		}

		// Forward navigation keys to the table.
		var cmd tea.Cmd
		m.table, cmd = m.table.Update(msg)
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
		actionLines = ui.ActionBarLineCount(m.width,
			[]string{"[enter] settings", "[d]elete"},
			[]string{"[a]dd"},
			[]string{"[esc] back"},
		)
	}
	return ui.ClampedTableHeight(len(m.repos), m.height, ui.BannerOverhead+1+ui.ActionBarPadY+actionLines) // gap + actionbar padding + actionbar
}

func (m RepoListModel) View() tea.View {
	if m.err != nil {
		return tea.NewView(
			ui.RenderError(ui.RPCErrorMessage(m.err), m.width) + "\n" +
				ui.StyleActionBar.Render("[esc] back"),
		)
	}

	if m.loading {
		return tea.NewView(lipgloss.NewStyle().Padding(0, 2).Render("Loading repositories…"))
	}

	var b strings.Builder

	if len(m.repos) == 0 {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(
			"All of the repositories you are working on will be listed here.\n\n" +
				"Press 'a' to add your first repository.",
		))
		b.WriteString("\n")
		b.WriteString(ui.ActionBarWidth(m.width, []string{"[a]dd"}, []string{"[esc] back"}))
		return tea.NewView(b.String())
	}

	b.WriteString(lipgloss.NewStyle().Padding(0, 1).Render(m.table.View()))
	b.WriteString("\n")

	if m.confirming {
		b.WriteString("\n")
		repo := m.repos[m.table.Cursor()]
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(ui.ColorDanger).Render(
			fmt.Sprintf("Delete %q?", repo.DisplayName)))
		b.WriteString("\n")
		b.WriteString(ui.StyleActionBar.Render("[y/enter] confirm  [n/esc] cancel"))
	} else {
		b.WriteString(ui.ActionBarWidth(m.width,
			[]string{"[enter] settings", "[d]elete"},
			[]string{"[a]dd"},
			[]string{"[esc] back"},
		))
	}

	return tea.NewView(b.String())
}
