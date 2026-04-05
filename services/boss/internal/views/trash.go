package views

import (
	"context"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// --- Trash View ---

// trashListMsg carries archived sessions for the trash view.
type trashListMsg struct {
	sessions []*pb.Session
	err      error
}

// sessionRestoredMsg carries the result of restoring an archived session.
type sessionRestoredMsg struct {
	id  string
	err error
}

// sessionDeletedMsg carries the result of permanently deleting a session.
type sessionDeletedMsg struct {
	id  string
	err error
}

// allSessionsDeletedMsg carries the result of emptying the entire trash.
type allSessionsDeletedMsg struct {
	err error
}

// TrashModel displays archived sessions with restore/delete functionality.
type TrashModel struct {
	client  client.BossClient
	ctx     context.Context
	spinner spinner.Model

	sessions []*pb.Session
	table    table.Model
	err      error
	cancel   bool
	loading  bool

	// Delete confirmation / in-progress states
	confirming    bool
	confirmingAll bool
	deleting      bool
	deletingAll   bool
	restoring     bool

	// Layout
	width  int
	height int
}

// NewTrashModel creates a TrashModel.
func NewTrashModel(c client.BossClient, ctx context.Context) TrashModel {
	return TrashModel{
		client:  c,
		ctx:     ctx,
		spinner: newStatusSpinner(),
		loading: true,
		table:   newBossTable(nil, nil, 0),
	}
}

func (m TrashModel) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, m.fetchArchived())
}

func (m TrashModel) fetchArchived() tea.Cmd {
	return func() tea.Msg {
		sessions, err := m.client.ListSessions(m.ctx, &pb.ListSessionsRequest{IncludeArchived: true})
		if err != nil {
			return trashListMsg{err: err}
		}
		// Filter to only archived sessions.
		var archived []*pb.Session
		for _, s := range sessions {
			if s.ArchivedAt != nil {
				archived = append(archived, s)
			}
		}
		return trashListMsg{sessions: archived}
	}
}

func (m *TrashModel) buildTable() {
	if len(m.sessions) == 0 {
		return
	}

	repos := make([]string, len(m.sessions))
	names := make([]string, len(m.sessions))
	prLabels := make([]string, len(m.sessions))
	prs := make([]string, len(m.sessions))
	archiveds := make([]string, len(m.sessions))
	for i, sess := range m.sessions {
		repos[i] = sess.RepoDisplayName
		if sess.Title != "" {
			names[i] = sess.Title
		} else {
			names[i] = sess.BranchName
		}
		if sess.PrNumber != nil {
			prLabels[i] = fmt.Sprintf("#%d", *sess.PrNumber)
			prs[i] = renderPRLink(sess)
		} else {
			prLabels[i] = "-"
			prs[i] = "-"
		}
		if sess.ArchivedAt != nil {
			archiveds[i] = RelativeTime(sess.ArchivedAt.AsTime())
		} else {
			archiveds[i] = "-"
		}
	}

	cols := []table.Column{
		cursorColumn,
		{Title: "REPO", Width: maxColWidth("REPO", repos, 20) + tableColumnSep},
		{Title: "NAME", Width: maxColWidth("NAME", names, 60) + tableColumnSep},
		{Title: "PR", Width: maxColWidth("PR", prLabels, 8) + tableColumnSep},
		{Title: "ARCHIVED", Width: maxColWidth("ARCHIVED", archiveds, 12) + tableColumnSep},
	}

	cursor := m.table.Cursor()
	rows := make([]table.Row, len(m.sessions))
	for i := range m.sessions {
		indicator := ""
		if i == cursor {
			indicator = cursorChevron
		}
		rows[i] = table.Row{indicator, repos[i], names[i], prs[i], archiveds[i]}
	}
	m.table.SetColumns(cols)
	m.table.SetRows(rows)
	m.table.SetWidth(columnsWidth(cols))
	m.table.SetHeight(m.tableHeight())
	m.table.SetCursor(cursor)
}

func (m *TrashModel) removeSession(id string) {
	for i, s := range m.sessions {
		if s.Id == id {
			m.sessions = append(m.sessions[:i], m.sessions[i+1:]...)
			break
		}
	}
	m.buildTable()
	// Clamp cursor after rebuild.
	if m.table.Cursor() >= len(m.sessions) && len(m.sessions) > 0 {
		m.table.SetCursor(len(m.sessions) - 1)
	}
}

func (m TrashModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.table.SetHeight(m.tableHeight())
		m.table.SetWidth(msg.Width)
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case trashListMsg:
		m.loading = false
		m.sessions = msg.sessions
		m.err = msg.err
		m.buildTable()
		return m, nil

	case sessionRestoredMsg:
		m.restoring = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.removeSession(msg.id)
		return m, nil

	case sessionDeletedMsg:
		m.confirming = false
		m.deleting = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.removeSession(msg.id)
		return m, nil

	case allSessionsDeletedMsg:
		m.confirming = false
		m.confirmingAll = false
		m.deleting = false
		m.deletingAll = false
		m.restoring = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.sessions = nil
		m.buildTable()
		return m, nil

	case tea.KeyMsg:
		if m.confirming || m.confirmingAll {
			return m.updateDeleteConfirm(msg)
		}

		switch msg.String() {
		case "esc":
			m.cancel = true
			return m, nil
		case "r":
			if len(m.sessions) > 0 && !m.deletingAll {
				m.restoring = true
				sess := m.sessions[m.table.Cursor()]
				return m, func() tea.Msg {
					_, err := m.client.ResurrectSession(m.ctx, sess.Id)
					return sessionRestoredMsg{id: sess.Id, err: err}
				}
			}
			return m, nil
		case "d":
			if len(m.sessions) > 0 && !m.deletingAll {
				m.confirming = true
				m.table.SetHeight(m.tableHeight())
			}
			return m, nil
		case "a":
			if len(m.sessions) > 0 && !m.deletingAll {
				m.confirmingAll = true
				m.table.SetHeight(m.tableHeight())
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

func (m TrashModel) updateDeleteConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "enter":
		if m.confirmingAll {
			m.confirmingAll = false
			m.deletingAll = true
			m.table.SetHeight(m.tableHeight())
			return m, func() tea.Msg {
				_, err := m.client.EmptyTrash(m.ctx, &pb.EmptyTrashRequest{})
				return allSessionsDeletedMsg{err: err}
			}
		}
		m.confirming = false
		m.deleting = true
		m.table.SetHeight(m.tableHeight())
		sess := m.sessions[m.table.Cursor()]
		return m, func() tea.Msg {
			err := m.client.RemoveSession(m.ctx, sess.Id)
			return sessionDeletedMsg{id: sess.Id, err: err}
		}
	case "n", "esc":
		m.confirming = false
		m.confirmingAll = false
		m.table.SetHeight(m.tableHeight())
	}
	return m, nil
}

// Cancelled returns true if the user exited the trash view.
func (m TrashModel) Cancelled() bool { return m.cancel }

// tableHeight returns the height to pass to table.SetHeight.
func (m TrashModel) tableHeight() int {
	overhead := bannerOverhead + 1 + actionBarPadY + 1 // gap + actionbar padding + actionbar
	if m.confirming || m.confirmingAll {
		overhead += 3 // confirmation prompt + surrounding blank lines
	}
	return clampedTableHeight(len(m.sessions), m.height, overhead)
}

func (m TrashModel) View() tea.View {
	if m.err != nil {
		return tea.NewView(
			renderError(fmt.Sprintf("Error: %v", m.err), m.width) + "\n" +
				styleActionBar.Render("[esc] back"),
		)
	}

	if m.loading {
		return tea.NewView(lipgloss.NewStyle().Padding(1, 2).Render("Loading archived sessions..."))
	}

	var b strings.Builder

	if len(m.sessions) == 0 {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("Trash is empty."))
		b.WriteString("\n")
		b.WriteString(actionBar([]string{"[esc] back"}))
		return tea.NewView(b.String())
	}

	b.WriteString(lipgloss.NewStyle().Padding(0, 1).Render(m.table.View()))
	b.WriteString("\n")

	if m.deleting {
		b.WriteString(lipgloss.NewStyle().Padding(actionBarPadY, 2).Foreground(colorDanger).Render(
			m.spinner.View() + "Deleting..."))
	} else if m.deletingAll {
		b.WriteString(lipgloss.NewStyle().Padding(actionBarPadY, 2).Foreground(colorDanger).Render(
			m.spinner.View() + "Deleting all..."))
	} else if m.restoring {
		b.WriteString(lipgloss.NewStyle().Padding(actionBarPadY, 2).Foreground(colorDanger).Render(
			m.spinner.View() + "Restoring..."))
	} else if m.confirmingAll {
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorDanger).Render(
			fmt.Sprintf("Permanently delete all %d archived sessions?", len(m.sessions))))
		b.WriteString("\n")
		b.WriteString(styleActionBar.Render("[y/enter] confirm  [n/esc] cancel"))
	} else if m.confirming {
		b.WriteString("\n")
		sess := m.sessions[m.table.Cursor()]
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorDanger).Render(
			fmt.Sprintf("Permanently delete %q?", sess.Title)))
		b.WriteString("\n")
		b.WriteString(styleActionBar.Render("[y/enter] confirm  [n/esc] cancel"))
	} else {
		b.WriteString(actionBar(
			[]string{"[d]elete", "[a] delete all", "[r]estore"},
			[]string{"[esc] back"},
		))
	}

	return tea.NewView(b.String())
}
