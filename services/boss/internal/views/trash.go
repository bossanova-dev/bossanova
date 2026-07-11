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

// deleteProgressMsg carries the result of deleting one session in a delete-all batch.
type deleteProgressMsg struct {
	id  string
	err error
}

// TrashModel displays archived sessions with restore/delete functionality.
type TrashModel struct {
	client  client.BossClient
	ctx     context.Context
	spinner spinner.Model

	sessions []*pb.Session
	filter   listFilter

	filteredSessions []int
	table            table.Model
	err              error
	cancel           bool
	loading          bool
	returnView       View

	// Delete confirmation / in-progress states
	confirm          confirmPrompt
	confirmDeleteAll bool
	deleting         bool
	deletingAll      bool
	restoring        bool
	restoredID       string

	// Delete-all batch progress. pendingDeleteIDs holds the IDs resolved when
	// [a] is pressed (so a later filter change cannot alter the batch). Once
	// confirmed they move into deleteQueue and are drained one session at a
	// time so the view can render a live Deleting N/M count.
	pendingDeleteIDs []string
	deleteQueue      []string // session IDs still to delete in the active batch
	deleteTotal      int      // total sessions in the active batch (denominator)
	deleteDone       int      // sessions deleted so far (numerator)

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
		filter:  newListFilter(),
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
		// Nothing left to filter; drop any applied query so the view does not
		// strand an invisible filter that swallows the first esc.
		m.filter.Deactivate()
		m.filteredSessions = nil
		m.filter.SetCounts(0, 0)
		m.table.SetRows(nil)
		m.table.SetHeight(m.tableHeight())
		return
	}

	m.applyFilter()

	repos := make([]string, len(m.filteredSessions))
	names := make([]string, len(m.filteredSessions))
	prLabels := make([]string, len(m.filteredSessions))
	prs := make([]string, len(m.filteredSessions))
	archiveds := make([]string, len(m.filteredSessions))
	for i, sessionIndex := range m.filteredSessions {
		sess := m.sessions[sessionIndex]
		repos[i] = sess.RepoDisplayName
		names[i] = trashSessionName(sess)
		prLabels[i] = trashSessionPRLabel(sess)
		prs[i] = trashSessionPR(sess)
		archiveds[i] = trashSessionArchived(sess)
	}

	cols := []table.Column{
		cursorColumn,
		{Title: "REPO", Width: maxColWidth("REPO", repos, 20) + tableColumnSep},
		{Title: "NAME", Width: maxColWidth("NAME", names, 60) + tableColumnSep},
		{Title: "PR", Width: maxColWidth("PR", prLabels, 8) + tableColumnSep},
		{Title: "ARCHIVED", Width: maxColWidth("ARCHIVED", archiveds, 12) + tableColumnSep},
	}

	cursor := m.table.Cursor()
	if cursor >= len(m.filteredSessions) && len(m.filteredSessions) > 0 {
		cursor = len(m.filteredSessions) - 1
	}
	rows := make([]table.Row, len(m.filteredSessions))
	for i := range m.filteredSessions {
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

func (m *TrashModel) applyFilter() {
	m.filteredSessions = m.filteredSessions[:0]
	for i, sess := range m.sessions {
		if m.filter.Matches(m.trashFilterHaystack(sess)) {
			m.filteredSessions = append(m.filteredSessions, i)
		}
	}
	m.filter.SetCounts(len(m.filteredSessions), len(m.sessions))
}

func (m *TrashModel) removeSession(id string) {
	for i, s := range m.sessions {
		if s.Id == id {
			m.sessions = append(m.sessions[:i], m.sessions[i+1:]...)
			break
		}
	}
	// buildTable already clamps the cursor into range and calls SetCursor.
	m.buildTable()
	updateCursorColumn(&m.table)
}

func (m TrashModel) selectedSession() *pb.Session {
	cursor := m.table.Cursor()
	if cursor < 0 || cursor >= len(m.filteredSessions) {
		return nil
	}
	sessionIndex := m.filteredSessions[cursor]
	if sessionIndex < 0 || sessionIndex >= len(m.sessions) {
		return nil
	}
	return m.sessions[sessionIndex]
}

func (m TrashModel) filteredSessionIDs() []string {
	ids := make([]string, 0, len(m.filteredSessions))
	for _, sessionIndex := range m.filteredSessions {
		if sessionIndex >= 0 && sessionIndex < len(m.sessions) {
			ids = append(ids, m.sessions[sessionIndex].Id)
		}
	}
	return ids
}

// allSessionIDs returns the IDs of every archived session the view holds,
// mirroring filteredSessionIDs for the unfiltered delete-all batch.
func (m TrashModel) allSessionIDs() []string {
	ids := make([]string, 0, len(m.sessions))
	for _, sess := range m.sessions {
		ids = append(ids, sess.Id)
	}
	return ids
}

// deleteNext returns a command that deletes the session at the head of the
// delete-all queue and reports the outcome as a deleteProgressMsg. It returns
// nil when the queue is empty so the chain terminates.
func (m TrashModel) deleteNext() tea.Cmd {
	if len(m.deleteQueue) == 0 {
		return nil
	}
	id := m.deleteQueue[0]
	c, ctx := m.client, m.ctx
	return func() tea.Msg {
		err := c.RemoveSession(ctx, id)
		return deleteProgressMsg{id: id, err: err}
	}
}

func (m TrashModel) trashFilterHaystack(sess *pb.Session) string {
	return strings.Join([]string{
		sess.RepoDisplayName,
		trashSessionName(sess),
		trashSessionPRLabel(sess),
		trashSessionArchived(sess),
	}, " ")
}

func trashSessionName(sess *pb.Session) string {
	if sess.Title != "" {
		return sess.Title
	}
	return sess.BranchName
}

func trashSessionPRLabel(sess *pb.Session) string {
	if sess.PrNumber == nil {
		return "-"
	}
	return fmt.Sprintf("#%d", *sess.PrNumber)
}

func trashSessionPR(sess *pb.Session) string {
	if sess.PrNumber == nil {
		return "-"
	}
	return renderPRLink(sess)
}

func trashSessionArchived(sess *pb.Session) string {
	if sess.ArchivedAt == nil {
		return "-"
	}
	return RelativeTime(sess.ArchivedAt.AsTime())
}

func (m *TrashModel) updateTrashFilterInput(msg tea.Msg) tea.Cmd {
	cmd, changed := m.filter.Update(msg)
	if changed {
		m.buildTable()
		m.table.SetCursor(0)
		updateCursorColumn(&m.table)
	}
	return cmd
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
		m.restoredID = msg.id
		m.removeSession(msg.id)
		return m, nil

	case sessionDeletedMsg:
		m.deleting = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.removeSession(msg.id)
		return m, nil

	case deleteProgressMsg:
		if msg.err != nil {
			// Stop the batch on the first failure. Sessions deleted before this
			// one stay pruned (they were removed as their progress messages
			// arrived), matching the previous partial-failure semantics.
			m.err = msg.err
			m.deletingAll = false
			m.deleteQueue = nil
			m.deleteTotal = 0
			m.deleteDone = 0
			m.buildTable()
			return m, nil
		}
		m.removeSession(msg.id)
		m.deleteDone++
		if len(m.deleteQueue) > 0 {
			m.deleteQueue = m.deleteQueue[1:]
		}
		if len(m.deleteQueue) == 0 {
			m.deletingAll = false
			m.deleteTotal = 0
			m.deleteDone = 0
			return m, nil
		}
		return m, m.deleteNext()

	case tea.PasteMsg:
		if m.filter.Active() {
			cmd := m.updateTrashFilterInput(msg)
			return m, cmd
		}
		return m, nil

	case tea.KeyMsg:
		if m.err != nil {
			if msg.String() == "esc" {
				m.cancel = true
			}
			return m, nil
		}

		if m.confirm.active {
			confirmed := msg.String() == "y" || msg.String() == "enter"
			deleteAll := m.confirmDeleteAll
			var cmd tea.Cmd
			m.confirm, cmd = m.confirm.update(msg)
			if !m.confirm.active {
				m.confirmDeleteAll = false
			}
			if confirmed && deleteAll {
				// Drive delete-all client-side, one session at a time, so the
				// view can render a live Deleting N/M count. The batch was
				// captured when [a] was pressed.
				m.deletingAll = true
				m.deleteQueue = m.pendingDeleteIDs
				m.deleteTotal = len(m.deleteQueue)
				m.deleteDone = 0
				m.pendingDeleteIDs = nil
				m.table.SetHeight(m.tableHeight())
				return m, m.deleteNext()
			}
			if cmd != nil && confirmed {
				m.deleting = true
			}
			m.table.SetHeight(m.tableHeight())
			return m, cmd
		}

		if m.filter.Active() {
			switch msg.String() {
			case "enter":
				if !m.filter.Commit() {
					m.filter.Deactivate()
					m.buildTable()
				}
				return m, nil
			case "esc":
				m.filter.Deactivate()
				m.buildTable()
				m.table.SetCursor(0)
				updateCursorColumn(&m.table)
				return m, nil
			case "up", "down", "ctrl+p", "ctrl+n", "ctrl+d", "ctrl+u":
				var cmd tea.Cmd
				m.table, cmd = m.table.Update(msg)
				updateCursorColumn(&m.table)
				return m, cmd
			}
			cmd := m.updateTrashFilterInput(msg)
			return m, cmd
		}

		switch msg.String() {
		case "/":
			if len(m.sessions) == 0 {
				return m, nil
			}
			cmd := m.filter.Activate()
			m.buildTable()
			return m, cmd
		case "esc":
			if m.filter.Applied() {
				m.filter.Deactivate()
				m.buildTable()
				m.table.SetCursor(0)
				updateCursorColumn(&m.table)
				return m, nil
			}
			m.cancel = true
			return m, nil
		case "r":
			if sess := m.selectedSession(); sess != nil && !m.deletingAll {
				m.restoring = true
				return m, func() tea.Msg {
					_, err := m.client.ResurrectSession(m.ctx, sess.Id)
					return sessionRestoredMsg{id: sess.Id, err: err}
				}
			}
			return m, nil
		case "d":
			if sess := m.selectedSession(); sess != nil && !m.deletingAll {
				c := m.client
				ctx := m.ctx
				id := sess.Id
				name := trashSessionName(sess)
				m.confirmDeleteAll = false
				m.confirm = newConfirmPrompt(fmt.Sprintf("Permanently delete %q?", name), func() tea.Msg {
					err := c.RemoveSession(ctx, id)
					return sessionDeletedMsg{id: id, err: err}
				})
				m.table.SetHeight(m.tableHeight())
			}
			return m, nil
		case "a":
			if len(m.filteredSessions) > 0 && !m.deletingAll {
				// Resolve the batch now, at [a]-time, so a later filter change
				// cannot alter what the confirm deletes. The unfiltered path
				// deletes every archived session the view already holds via
				// RemoveSession (client-side, one at a time) rather than the
				// blocking EmptyTrash RPC, so the delete can report progress.
				ids := m.allSessionIDs()
				prompt := fmt.Sprintf("Permanently delete all %d archived sessions?", len(m.sessions))
				if m.filter.Engaged() {
					ids = m.filteredSessionIDs()
					prompt = fmt.Sprintf("Permanently delete %d filtered archived sessions?", len(ids))
				}
				m.pendingDeleteIDs = ids
				m.confirmDeleteAll = true
				m.confirm = newConfirmPrompt(prompt, nil)
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

// Cancelled returns true if the user exited the trash view.
func (m TrashModel) Cancelled() bool { return m.cancel }

// RestoredSessionID returns the session restored by the latest successful restore.
func (m TrashModel) RestoredSessionID() string { return m.restoredID }

// tableHeight returns the height to pass to table.SetHeight.
func (m TrashModel) tableHeight() int {
	overhead := bannerOverhead + 1 + actionBarPadY + 1 // gap + actionbar padding + actionbar
	overhead += m.filter.Height()
	if m.confirm.active {
		overhead += 3 // confirmation prompt + surrounding blank lines
	}
	return clampedTableHeight(len(m.filteredSessions), m.height, overhead)
}

func (m TrashModel) View() tea.View {
	if m.err != nil {
		return tea.NewView(
			renderError(fmt.Sprintf("Error: %v", m.err), m.width) + "\n" +
				styleActionBar.Render("[esc] back"),
		)
	}

	if m.loading {
		return tea.NewView(lipgloss.NewStyle().Padding(0, 2).Render("Loading archived sessions..."))
	}

	var b strings.Builder

	if len(m.sessions) == 0 {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("Trash is empty."))
		b.WriteString("\n")
		b.WriteString(actionBar([]string{"[esc] back"}))
		return tea.NewView(b.String())
	}

	if len(m.filteredSessions) == 0 {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("No archived sessions match filter."))
		b.WriteString("\n")
	} else {
		b.WriteString(lipgloss.NewStyle().Padding(0, 1).Render(m.table.View()))
		b.WriteString("\n")
	}
	// Table-backed filters render below their content, matching PR and Linear issue selectors.
	if m.filter.Engaged() {
		b.WriteString(m.filter.View())
		b.WriteString("\n")
	}

	if m.deleting {
		b.WriteString(lipgloss.NewStyle().Padding(actionBarPadY, 2).Foreground(colorDanger).Render(
			m.spinner.View() + "Deleting..."))
	} else if m.deletingAll {
		// deleteDone+1 (capped at the total) reads as "currently deleting item
		// N of M" while the Nth RemoveSession is in flight.
		b.WriteString(lipgloss.NewStyle().Padding(actionBarPadY, 2).Foreground(colorDanger).Render(
			m.spinner.View() + fmt.Sprintf("Deleting %d/%d…", min(m.deleteDone+1, m.deleteTotal), m.deleteTotal)))
	} else if m.restoring {
		b.WriteString(lipgloss.NewStyle().Padding(actionBarPadY, 2).Foreground(colorDanger).Render(
			m.spinner.View() + "Restoring..."))
	} else if m.confirm.active {
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorDanger).Render(
			m.confirm.prompt))
		b.WriteString("\n")
		b.WriteString(styleActionBar.Render("[y/enter] confirm  [n/esc] cancel"))
	} else {
		b.WriteString(trashActionBar(m.filter, len(m.filteredSessions) > 0))
	}

	return tea.NewView(b.String())
}

func trashActionBar(f listFilter, hasRows bool) string {
	if f.Active() {
		return actionBar(f.ActionBar())
	}
	primary := []string{}
	if hasRows {
		primary = []string{"[d]elete", "[a] delete all", "[r]estore"}
	}
	if f.Applied() {
		return actionBar(primary, []string{"[/] edit filter", "[esc] clear"})
	}
	if hasRows {
		primary = append(primary, "[/] filter")
		return actionBar(primary, []string{"[esc] back"})
	}
	return actionBar([]string{"[esc] back"})
}
