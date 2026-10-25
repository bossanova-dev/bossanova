package trash

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/recurser/boss/internal/domain/rpc"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/recurser/boss/api/tui/ui"
	"github.com/recurser/boss/internal/domain/display"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/telemetry"
)

// --- Trash View ---

// ListMsg carries archived sessions for the trash view.
type ListMsg struct {
	Sessions []*pb.Session
	Err      error
}

type restoreStreamIdentity struct{ _ byte }

// SessionRestoredMsg carries the TERMINAL result of restoring an archived
// session.
//
// setupError is the third outcome BOS-984 made expressible. err means the
// session did not come back (a worktree failure, or the RPC itself failing —
// including a deadline); setupError means it DID come back but its setup script
// failed, so the worktree is missing its dependencies. Both empty is a clean
// restore.
type SessionRestoredMsg struct {
	streamID   *restoreStreamIdentity
	ID         string
	Err        error
	SetupError string
}

// RestoreStreamOpenedMsg hands the freshly-opened resurrect stream back to the
// model so subsequent frames can be pumped from it.
type RestoreStreamOpenedMsg struct {
	streamID *restoreStreamIdentity
	ID       string
	Stream   rpc.ResurrectSessionStream
}

// RestoreProgressMsg carries one setup-output line from the resurrect stream.
// Without it the restore is silent for however long the repo's setup script
// runs — the exact experience BOS-984 replaced.
type RestoreProgressMsg struct {
	streamID *restoreStreamIdentity
	ID       string
	Line     string
}

// SessionDeletedMsg carries the result of permanently deleting a session.
type SessionDeletedMsg struct {
	ID  string
	Err error
}

// DeleteProgressMsg carries the result of deleting one session in a delete-all batch.
type DeleteProgressMsg struct {
	ID  string
	Err error
}

// Model displays archived sessions with restore/delete functionality.
type Model struct {
	client    Client
	ctx       context.Context
	telemetry telemetry.Client
	spinner   spinner.Model

	sessions []*pb.Session
	filter   ui.ListFilter

	filteredSessions []int
	table            table.Model
	err              error
	cancel           bool
	loading          bool
	returnView       ui.View

	// Delete confirmation / in-progress states
	confirm          ui.ConfirmPrompt
	confirmDeleteAll bool
	deleting         bool
	deletingAll      bool
	restoring        bool
	restoredID       string
	// restoreStream is the in-flight resurrect stream; restoreLine is the most
	// recent progress line it produced, and restoreWarning the non-fatal setup
	// failure a completed restore reported (BOS-984).
	restoreStreamID *restoreStreamIdentity
	restoreCancel   context.CancelFunc
	status          string
	restoreStream   rpc.ResurrectSessionStream
	restoreLine     string
	restoreWarning  string

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

// NewModel creates a Model.
func NewModel(c Client, ctx context.Context) Model {
	return Model{
		client:  c,
		ctx:     ctx,
		spinner: ui.NewStatusSpinner(),
		filter:  ui.NewListFilter(),
		loading: true,
		table:   ui.NewBossTable(nil, nil, 0),
	}
}

// SetTelemetry installs a telemetry client for completed trash actions.
func (m *Model) SetTelemetry(client telemetry.Client) {
	m.telemetry = client
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, m.fetchArchived())
}

func (m Model) fetchArchived() tea.Cmd {
	return func() tea.Msg {
		sessions, err := m.client.ListSessions(m.ctx, &pb.ListSessionsRequest{IncludeArchived: true}, rpc.SessionReadOptions{})
		if err != nil {
			return ListMsg{Err: err}
		}
		// Filter to only archived sessions.
		var archived []*pb.Session
		for _, s := range sessions {
			if s.ArchivedAt != nil {
				archived = append(archived, s)
			}
		}
		return ListMsg{Sessions: archived}
	}
}

func (m *Model) buildTable() {
	if len(m.sessions) == 0 {
		// Nothing left to filter; drop any applied query so the view does not
		// strand an invisible filter that swallows the first esc.
		m.filter.Reset()
		m.filteredSessions = nil
		m.filter.SetCounts(0, 0)
		m.table.SetRows(nil)
		m.table.SetHeight(m.tableHeight())
		m.table.SetWidth(m.width)
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

	cols := []ui.ResponsiveColumn{
		{Col: ui.CursorColumn, Priority: 0, MinWidth: 1},
		{Col: table.Column{Title: "REPO", Width: display.MaxColWidth("REPO", repos, 20) + ui.TableColumnSep}, Priority: 3, MinWidth: 1},
		{Col: table.Column{Title: "NAME", Width: display.MaxColWidth("NAME", names, 60) + ui.TableColumnSep}, Priority: 0, MinWidth: 1},
		{Col: table.Column{Title: "PR", Width: display.MaxColWidth("PR", prLabels, 8) + ui.TableColumnSep}, Priority: 2, MinWidth: 1},
		{Col: table.Column{Title: "ARCHIVED", Width: display.MaxColWidth("ARCHIVED", archiveds, 12) + ui.TableColumnSep}, Priority: 1, MinWidth: 1},
	}
	fitted := ui.FitColumnsIndexed(cols, ui.FitAvailWidth(m.width, 1))
	fittedCols := ui.FittedColumns(fitted)

	cursor := m.table.Cursor()
	if cursor >= len(m.filteredSessions) && len(m.filteredSessions) > 0 {
		cursor = len(m.filteredSessions) - 1
	}
	rows := make([]table.Row, len(m.filteredSessions))
	for i := range m.filteredSessions {
		indicator := ""
		if i == cursor {
			indicator = ui.CursorChevron
		}
		rows[i] = ui.ProjectRow(fitted, table.Row{indicator, repos[i], names[i], prs[i], archiveds[i]})
	}
	ui.SetTableContent(&m.table, fittedCols, rows)
	m.table.SetWidth(ui.ColumnsWidth(fittedCols))
	m.table.SetHeight(m.tableHeight())
	m.table.SetCursor(cursor)
}

func (m *Model) applyFilter() {
	m.filteredSessions = m.filteredSessions[:0]
	for i, sess := range m.sessions {
		if m.filter.Matches(m.trashFilterHaystack(sess)) {
			m.filteredSessions = append(m.filteredSessions, i)
		}
	}
	m.filter.SetCounts(len(m.filteredSessions), len(m.sessions))
}

func (m *Model) removeSession(id string) {
	for i, s := range m.sessions {
		if s.Id == id {
			m.sessions = append(m.sessions[:i], m.sessions[i+1:]...)
			break
		}
	}
	// buildTable already clamps the cursor into range and calls SetCursor.
	m.buildTable()
	ui.UpdateCursorColumn(&m.table)
}

func (m Model) selectedSession() *pb.Session {
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

func (m Model) filteredSessionIDs() []string {
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
func (m Model) allSessionIDs() []string {
	ids := make([]string, 0, len(m.sessions))
	for _, sess := range m.sessions {
		ids = append(ids, sess.Id)
	}
	return ids
}

// deleteNext returns a command that deletes the session at the head of the
// delete-all queue and reports the outcome as a DeleteProgressMsg. It returns
// nil when the queue is empty so the chain terminates.
func (m Model) deleteNext() tea.Cmd {
	if len(m.deleteQueue) == 0 {
		return nil
	}
	id := m.deleteQueue[0]
	c, ctx := m.client, m.ctx
	return func() tea.Msg {
		err := c.RemoveSession(ctx, id)
		return DeleteProgressMsg{ID: id, Err: err}
	}
}

func (m Model) trashFilterHaystack(sess *pb.Session) string {
	return strings.Join([]string{
		sess.RepoDisplayName,
		trashSessionName(sess),
		trashSessionPRLabel(sess),
		trashSessionArchived(sess),
	}, " ")
}

func trashSessionName(sess *pb.Session) string {
	name := sess.Title
	if name == "" {
		name = sess.BranchName
	}
	if sess.GetDisplaySettingUp() {
		name += " · restoring"
	}
	return name
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
	return ui.RenderPRLink(sess)
}

func trashSessionArchived(sess *pb.Session) string {
	if sess.ArchivedAt == nil {
		return "-"
	}
	return display.RelativeTime(sess.ArchivedAt.AsTime())
}

func (m *Model) updateTrashFilterInput(msg tea.Msg) tea.Cmd {
	cmd, changed := m.filter.Update(msg)
	if changed {
		m.buildTable()
		m.table.SetCursor(0)
		ui.UpdateCursorColumn(&m.table)
	}
	return cmd
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.buildTable()
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case ListMsg:
		m.loading = false
		m.sessions = msg.Sessions
		m.err = msg.Err
		m.buildTable()
		return m, nil

	case RestoreStreamOpenedMsg:
		if !m.restoring || msg.streamID != m.restoreStreamID {
			_ = msg.Stream.Close()
			return m, nil
		}
		m.restoreStream = msg.Stream
		return m, readNextRestoreMsg(msg.ID, msg.Stream, msg.streamID)

	case RestoreProgressMsg:
		// Ignore a frame that arrives after this restore stopped being the
		// model's business (the terminal frame already landed, or the stream
		// was dropped on a routed-away restore) rather than rendering a stale
		// line or pumping a nil stream.
		if !m.restoring || m.restoreStream == nil || msg.streamID != m.restoreStreamID {
			return m, nil
		}
		m.restoreLine = msg.Line
		return m, readNextRestoreMsg(msg.ID, m.restoreStream, msg.streamID)

	case SessionRestoredMsg:
		if !m.restoring || msg.streamID != m.restoreStreamID {
			return m, nil
		}
		m.detachRestore()
		m.restoreLine = ""
		m.restoreWarning = msg.SetupError
		// session_resurrected is captured at the app root
		// (App.handleSessionRestoredResult); see the note there.
		if msg.Err != nil {
			m.err = msg.Err
			return m, nil
		}
		m.restoredID = msg.ID
		m.removeSession(msg.ID)
		return m, nil

	case SessionDeletedMsg:
		m.deleting = false
		// session_removed is captured at the app root
		// (App.handleSessionDeletedResult); see the note there.
		if msg.Err != nil {
			m.err = msg.Err
			return m, nil
		}
		m.removeSession(msg.ID)
		return m, nil

	case DeleteProgressMsg:
		// session_removed for each batch step is captured at the app root
		// (App.handleDeleteProgressResult), including the per-step accounting.
		if msg.Err != nil {
			// Stop the batch on the first failure. Sessions deleted before this
			// one stay pruned (they were removed as their progress messages
			// arrived), matching the previous partial-failure semantics.
			m.err = msg.Err
			m.deletingAll = false
			m.deleteQueue = nil
			m.deleteTotal = 0
			m.deleteDone = 0
			m.buildTable()
			return m, nil
		}
		m.removeSession(msg.ID)
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
		if msg.String() == "esc" && m.restoring {
			m.detachRestore()
			m.cancel = true
			return m, nil
		}
		if m.err != nil {
			if msg.String() == "esc" {
				m.cancel = true
			}
			return m, nil
		}

		if m.confirm.Active {
			confirmed := msg.String() == "y" || msg.String() == "enter"
			deleteAll := m.confirmDeleteAll
			var cmd tea.Cmd
			m.confirm, cmd = m.confirm.Update(msg)
			if !m.confirm.Active {
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
					m.filter.Dismiss()
					m.buildTable()
				}
				return m, nil
			case "esc":
				m.filter.Reset()
				m.buildTable()
				m.table.SetCursor(0)
				ui.UpdateCursorColumn(&m.table)
				return m, nil
			case "up", "down", "ctrl+p", "ctrl+n", "ctrl+d", "ctrl+u":
				var cmd tea.Cmd
				m.table, cmd = m.table.Update(msg)
				ui.UpdateCursorColumn(&m.table)
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
				m.filter.Reset()
				m.buildTable()
				m.table.SetCursor(0)
				ui.UpdateCursorColumn(&m.table)
				return m, nil
			}
			m.cancel = true
			return m, nil
		case "r":
			if sess := m.selectedSession(); sess != nil && !m.deletingAll {
				m.detachRestore()
				m.restoring = true
				m.status = ""
				m.restoreStreamID = &restoreStreamIdentity{}
				streamID := m.restoreStreamID
				m.restoreLine = ""
				m.restoreWarning = ""
				m.restoreStream = nil
				c := m.client
				ctx, cancel := context.WithCancel(m.ctx)
				m.restoreCancel = cancel
				id := sess.Id
				return m, func() tea.Msg {
					stream, err := c.ResurrectSession(ctx, id)
					if err != nil {
						return SessionRestoredMsg{ID: id, Err: err, streamID: streamID}
					}
					return RestoreStreamOpenedMsg{ID: id, Stream: stream, streamID: streamID}
				}
			}
			return m, nil
		case "d":
			if sess := m.selectedSession(); sess != nil && !m.deletingAll {
				if sess.GetDisplaySettingUp() {
					m.status = "restore in progress"
					return m, nil
				}
				c := m.client
				ctx := m.ctx
				id := sess.Id
				name := trashSessionName(sess)
				m.confirmDeleteAll = false
				m.confirm = ui.NewConfirmPrompt(fmt.Sprintf("Permanently delete %q?", name), func() tea.Msg {
					err := c.RemoveSession(ctx, id)
					return SessionDeletedMsg{ID: id, Err: err}
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
					prompt = fmt.Sprintf("Permanently delete %d matching archived sessions?", len(ids))
				}
				m.pendingDeleteIDs = ids
				m.confirmDeleteAll = true
				m.confirm = ui.NewConfirmPrompt(prompt, nil)
				m.table.SetHeight(m.tableHeight())
			}
			return m, nil
		}

		// Forward navigation keys to the table.
		var cmd tea.Cmd
		m.table, cmd = m.table.Update(msg)
		ui.UpdateCursorColumn(&m.table)
		return m, cmd
	}

	return m, nil
}

func (m *Model) detachRestore() {
	if m.restoreCancel != nil {
		m.restoreCancel()
		m.restoreCancel = nil
	}
	m.restoreStreamID = nil
	m.restoreStream = nil
	m.restoring = false
}

// Cancelled returns true if the user exited the trash view.
func (m Model) Cancelled() bool { return m.cancel }

// textEntryActive reports whether the "/" filter input is focused, so App can
// leave ctrl+x alone rather than aliasing it onto Esc (BOS-660).
//
// Active() (focused), not Engaged(): a committed but blurred filter is a
// non-text state whose Esc clears the query, and that is behaviour ctrl+x
// should inherit.
//
// The m.err guard keeps the predicate honest while an error overlay is up. A
// restore/delete can fail after the user has already focused the filter, and
// View then REPLACES the filter with a full-screen error whose only route out
// is Esc. Reporting text entry there would leave ctrl+x unconverted for the
// Update arm above to swallow, stranding the user on a screen Esc escapes.
func (m Model) textEntryActive() bool { return m.err == nil && m.filter.Active() }

// RestoredSessionID returns the session restored by the latest successful restore.
func (m Model) RestoredSessionID() string { return m.restoredID }

// tableHeight returns the height to pass to table.SetHeight.
func (m Model) tableHeight() int {
	actionLines := 1
	if !m.deleting && !m.deletingAll && !m.restoring && !m.confirm.Active {
		if m.filter.Active() {
			actionLines = ui.ActionBarLineCount(m.width, m.filter.ActionBar())
		} else if len(m.filteredSessions) > 0 {
			primary := []string{"[d]elete", "[a] delete all", "[r]estore"}
			if m.filter.Applied() {
				actionLines = ui.ActionBarLineCount(m.width, primary, []string{"[/] edit search", "[esc] clear"})
			} else {
				actionLines = ui.ActionBarLineCount(m.width, append(primary, "[/] search"), []string{"[esc] back"})
			}
		}
	}
	overhead := ui.BannerOverhead + 1 + ui.ActionBarPadY + actionLines // gap + actionbar padding + actionbar
	overhead += m.filter.Height()
	if m.status != "" {
		overhead++
	}
	if m.confirm.Active {
		overhead += 3 // confirmation prompt + surrounding blank lines
	}
	return ui.ClampedTableHeight(len(m.filteredSessions), m.height, overhead)
}

func (m Model) View() tea.View {
	if m.err != nil {
		return tea.NewView(
			ui.RenderError(ui.RPCErrorMessage(m.err), m.width) + "\n" +
				ui.StyleActionBar.Render("[esc] back"),
		)
	}

	if m.loading {
		return tea.NewView(lipgloss.NewStyle().Padding(0, 2).Render("Loading archived sessions…"))
	}

	var b strings.Builder

	if len(m.sessions) == 0 {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("Trash is empty."))
		b.WriteString("\n")
		b.WriteString(ui.ActionBarWidth(m.width, []string{"[esc] back"}))
		return tea.NewView(b.String())
	}

	if len(m.filteredSessions) == 0 {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("No archived sessions match search."))
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

	// A restore that came back with a failed setup script is a SUCCESS with a
	// caveat, not an error — the session is out of the trash but its worktree
	// has no dependencies installed. Rendering it as a warning above the action
	// bar keeps it distinct from the error banner a failed restore raises
	// (BOS-984).
	if m.restoreWarning != "" && !m.restoring {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(ui.ColorWarning).Render(
			"Restored, but the repo setup script failed - dependencies may be missing: " + m.restoreWarning))
		b.WriteString("\n")
	}

	if m.deleting {
		b.WriteString(lipgloss.NewStyle().Padding(ui.ActionBarPadY, 2).Foreground(ui.ColorDanger).Render(
			m.spinner.View() + "Deleting…"))
	} else if m.deletingAll {
		// deleteDone+1 (capped at the total) reads as "currently deleting item
		// N of M" while the Nth RemoveSession is in flight.
		b.WriteString(lipgloss.NewStyle().Padding(ui.ActionBarPadY, 2).Foreground(ui.ColorDanger).Render(
			m.spinner.View() + fmt.Sprintf("Deleting %d/%d…", min(m.deleteDone+1, m.deleteTotal), m.deleteTotal)))
	} else if m.restoring {
		// The progress line is what distinguishes "still coming back" from a
		// hung restore. Before BOS-984 this spinner was all the user ever saw,
		// right up until the client deadline turned a healthy restore into
		// deadline_exceeded.
		label := "Restoring…"
		if m.restoreLine != "" {
			label = "Restoring… " + m.restoreLine
		}
		b.WriteString(lipgloss.NewStyle().Padding(ui.ActionBarPadY, 2).Foreground(ui.ColorDanger).Render(
			m.spinner.View() + truncateRestoreLabel(label, m.width)))
	} else if m.confirm.Active {
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(ui.ColorDanger).Render(
			m.confirm.Prompt))
		b.WriteString("\n")
		b.WriteString(ui.StyleActionBar.Render("[y/enter] confirm  [n/esc] cancel"))
	} else {
		if m.status != "" {
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(m.status))
			b.WriteString("\n")
		}
		b.WriteString(trashActionBar(m.width, m.filter, len(m.filteredSessions) > 0))
	}

	return tea.NewView(b.String())
}

func trashActionBar(width int, f ui.ListFilter, hasRows bool) string {
	if f.Active() {
		return ui.ActionBarWidth(width, f.ActionBar())
	}
	primary := []string{}
	if hasRows {
		primary = []string{"[d]elete", "[a] delete all", "[r]estore"}
	}
	if f.Applied() {
		return ui.ActionBarWidth(width, primary, []string{"[/] edit search", "[esc] clear"})
	}
	if hasRows {
		primary = append(primary, "[/] search")
		return ui.ActionBarWidth(width, primary, []string{"[esc] back"})
	}
	return ui.ActionBarWidth(width, []string{"[esc] back"})
}

// readNextRestoreMsg pumps one frame off the resurrect stream. SetupOutput is
// non-terminal (the caller schedules another read and the stream stays open);
// SessionResurrected and stream end are terminal.
//
// An unrecognised frame is skipped rather than failing the restore, so a newer
// daemon can add a frame type without breaking this binary — same forward
// compatibility rule readNextStreamMsg follows for create.
func readNextRestoreMsg(id string, stream rpc.ResurrectSessionStream, streamID *restoreStreamIdentity) tea.Cmd {
	return func() tea.Msg {
		for {
			if !stream.Receive() {
				_ = stream.Close()
				if err := stream.Err(); err != nil {
					return SessionRestoredMsg{ID: id, Err: err, streamID: streamID}
				}
				return SessionRestoredMsg{ID: id, Err: errRestoreStreamEndedEarly, streamID: streamID}
			}
			switch e := stream.Msg().Event.(type) {
			case *pb.ResurrectSessionResponse_SetupOutput:
				line := strings.TrimSpace(e.SetupOutput.GetText())
				if line == "" {
					// A blank line is real setup output, but rendering it would
					// blank the progress label for no information.
					continue
				}
				return RestoreProgressMsg{ID: id, Line: line, streamID: streamID}
			case *pb.ResurrectSessionResponse_SessionResurrected:
				_ = stream.Close()
				return SessionRestoredMsg{ID: id, SetupError: e.SessionResurrected.GetSetupError(), streamID: streamID}
			default:
				continue
			}
		}
	}
}

// errRestoreStreamEndedEarly is the restore counterpart of the create wizard's
// "stream ended unexpectedly": the daemon closed the stream without ever
// sending a terminal frame, so nothing can be said about the session's state.
var errRestoreStreamEndedEarly = errors.New("restore stream ended without a result")

// truncateRestoreLabel keeps a long setup-output line from wrapping the action
// row. The subtracted columns are the spinner plus the style's horizontal
// padding; a width at or below that chrome (an unmeasured view) leaves the
// label alone.
//
// ansi.Truncate, never a byte slice: setup output is arbitrary program output
// and routinely carries UTF-8 and SGR escapes, both of which byte-slicing
// splits into mojibake or a stranded escape sequence.
func truncateRestoreLabel(label string, width int) string {
	const chrome = 8
	if width <= chrome {
		return label
	}
	return ansi.Truncate(label, width-chrome, "…")
}

// Client is the daemon capability consumed by this screen.
type Client interface {
	ListSessions(ctx context.Context, req *pb.ListSessionsRequest, opts rpc.SessionReadOptions) ([]*pb.Session, error)
	RemoveSession(ctx context.Context, id string) error
	ResurrectSession(ctx context.Context, id string) (rpc.ResurrectSessionStream, error)
}

func (m *Model) SetSize(width, height int)  { m.width, m.height = width, height }
func (m *Model) SetReturnView(view ui.View) { m.returnView = view }
func (m Model) ReturnView() ui.View         { return m.returnView }
func (m Model) TextEntryActive() bool       { return m.textEntryActive() }
func (m *Model) Table() *table.Model        { return &m.table }
func (m Model) TableHeight() int            { return m.tableHeight() }

func (m Model) Width() int            { return m.width }
func (m Model) Height() int           { return m.height }
func (m *Model) SetHeight(height int) { m.height = height }

func (m Model) HasTelemetry() bool { return m.telemetry != nil }

func (m Model) Busy() bool { return m.restoring || m.deleting || m.deletingAll }

func (m Model) FilterQuery() string { return m.filter.Query() }
