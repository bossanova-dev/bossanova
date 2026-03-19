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
	confirming bool
	deleting   bool
	restoring  bool

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

	ids := make([]string, len(m.sessions))
	repos := make([]string, len(m.sessions))
	branches := make([]string, len(m.sessions))
	archiveds := make([]string, len(m.sessions))
	for i, sess := range m.sessions {
		id := sess.Id
		if len(id) > shortIDLen {
			id = id[:shortIDLen]
		}
		ids[i] = id
		repos[i] = sess.RepoDisplayName
		branches[i] = strings.TrimPrefix(sess.BranchName, "boss/")
		if sess.ArchivedAt != nil {
			archiveds[i] = relativeTime(sess.ArchivedAt.AsTime())
		} else {
			archiveds[i] = "-"
		}
	}

	cols := []table.Column{
		cursorColumn,
		{Title: "ID", Width: maxColWidth("ID", ids, shortIDLen) + tableColumnSep},
		{Title: "REPO", Width: maxColWidth("REPO", repos, 20) + tableColumnSep},
		{Title: "BRANCH", Width: maxColWidth("BRANCH", branches, 60) + tableColumnSep},
		{Title: "ARCHIVED", Width: maxColWidth("ARCHIVED", archiveds, 12) + tableColumnSep},
	}

	cursor := m.table.Cursor()
	rows := make([]table.Row, len(m.sessions))
	for i := range m.sessions {
		indicator := ""
		if i == cursor {
			indicator = cursorChevron
		}
		rows[i] = table.Row{indicator, ids[i], repos[i], branches[i], archiveds[i]}
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

	case tea.KeyMsg:
		if m.confirming {
			return m.updateDeleteConfirm(msg)
		}

		switch msg.String() {
		case "esc", "q":
			m.cancel = true
			return m, nil
		case "r":
			if len(m.sessions) > 0 {
				m.restoring = true
				sess := m.sessions[m.table.Cursor()]
				return m, func() tea.Msg {
					_, err := m.client.ResurrectSession(m.ctx, sess.Id)
					return sessionRestoredMsg{id: sess.Id, err: err}
				}
			}
			return m, nil
		case "d":
			if len(m.sessions) > 0 {
				m.confirming = true
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
		m.confirming = false
		m.deleting = true
		sess := m.sessions[m.table.Cursor()]
		return m, func() tea.Msg {
			err := m.client.RemoveSession(m.ctx, sess.Id)
			return sessionDeletedMsg{id: sess.Id, err: err}
		}
	case "n", "esc":
		m.confirming = false
	}
	return m, nil
}

// Cancelled returns true if the user exited the trash view.
func (m TrashModel) Cancelled() bool { return m.cancel }

// tableHeight returns the height to pass to table.SetHeight.
func (m TrashModel) tableHeight() int {
	return clampedTableHeight(len(m.sessions), m.height, 4) // title + blank + blank + action bar
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
	b.WriteString(styleTitle.Render("Trash"))
	b.WriteString("\n\n")

	if len(m.sessions) == 0 {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("Trash is empty."))
		b.WriteString("\n")
		b.WriteString(styleActionBar.Render("[esc] back"))
		return tea.NewView(b.String())
	}

	b.WriteString(lipgloss.NewStyle().Padding(0, 1).Render(m.table.View()))
	b.WriteString("\n")

	if m.deleting {
		b.WriteString(lipgloss.NewStyle().Padding(actionBarPadY, 2).Foreground(colorDanger).Render(
			m.spinner.View() + "Deleting..."))
	} else if m.restoring {
		b.WriteString(lipgloss.NewStyle().Padding(actionBarPadY, 2).Foreground(colorDanger).Render(
			m.spinner.View() + "Restoring..."))
	} else if m.confirming {
		b.WriteString("\n")
		sess := m.sessions[m.table.Cursor()]
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorDanger).Render(
			fmt.Sprintf("Permanently delete %q?", sess.Title)))
		b.WriteString("\n")
		b.WriteString(styleActionBar.Render("[y/enter] confirm  [n/esc] cancel"))
	} else {
		b.WriteString(styleActionBar.Render("[d]elete  [r]estore  [esc] back"))
	}

	return tea.NewView(b.String())
}
