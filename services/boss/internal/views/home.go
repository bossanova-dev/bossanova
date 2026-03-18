package views

import (
	"context"
	"fmt"
	"image/color"
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/internal/client"
	bosspty "github.com/recurser/boss/internal/pty"
	"github.com/recurser/bossalib/buildinfo"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

const pollInterval = 2 * time.Second

// sessionListMsg carries the result of a ListSessions RPC call.
type sessionListMsg struct {
	sessions []*pb.Session
	err      error
}

// sessionArchivedMsg carries the result of archiving a session.
type sessionArchivedMsg struct {
	id  string
	err error
}

// HomeModel is the main dashboard view showing active sessions.
type HomeModel struct {
	client   client.BossClient
	ctx      context.Context
	manager  *bosspty.Manager
	spinner  spinner.Model
	sessions []*pb.Session
	table    table.Model
	err      error
	loading  bool
	width    int
	height   int

	// Archive confirmation / in-progress
	confirming bool
	archiving  bool
}

// NewHomeModel creates a HomeModel wired to the daemon client.
func NewHomeModel(c client.BossClient, ctx context.Context, manager *bosspty.Manager) HomeModel {
	return HomeModel{
		client:  c,
		ctx:     ctx,
		manager: manager,
		spinner: newStatusSpinner(),
		loading: true,
		table:   newBossTable(nil, nil, 0),
	}
}

func (h HomeModel) Init() tea.Cmd {
	return tea.Batch(fetchSessions(h.client, h.ctx), tickCmd(), h.spinner.Tick)
}

// buildTableRows rebuilds the table columns and rows from h.sessions.
func (h *HomeModel) buildTableRows() {
	if len(h.sessions) == 0 {
		h.table.SetRows(nil)
		return
	}

	repos := make([]string, len(h.sessions))
	branches := make([]string, len(h.sessions))
	prs := make([]string, len(h.sessions))
	for i, sess := range h.sessions {
		repos[i] = sess.RepoDisplayName
		branches[i] = strings.TrimPrefix(sess.BranchName, "boss/")
		if sess.PrNumber != nil {
			prs[i] = fmt.Sprintf("#%d", *sess.PrNumber)
		} else {
			prs[i] = "-"
		}
	}

	cols := []table.Column{
		cursorColumn,
		{Title: "REPO", Width: maxColWidth("REPO", repos, 20)},
		{Title: "BRANCH", Width: maxColWidth("BRANCH", branches, 60)},
		{Title: "PR", Width: maxColWidth("PR", prs, 8)},
		{Title: "CI", Width: 7},
		{Title: "STATUS", Width: 14},
	}

	cursor := h.table.Cursor()
	rows := make([]table.Row, len(h.sessions))
	for i, sess := range h.sessions {
		ciLabel, ciColor := checksLabelAndColor(sess.LastCheckState)
		ciStyled := lipgloss.NewStyle().Foreground(ciColor).Render(ciLabel)

		status := h.manager.SessionStatus(sess.Id)
		statusStyled := renderStatus(status, h.spinner)

		indicator := ""
		if i == cursor {
			indicator = cursorChevron
		}
		rows[i] = table.Row{indicator, repos[i], branches[i], prs[i], ciStyled, statusStyled}
	}

	h.table.SetColumns(cols)
	h.table.SetRows(rows)
	h.table.SetWidth(columnsWidth(cols))
	h.table.SetHeight(h.tableHeight())
	h.table.SetCursor(cursor)
}

func (h HomeModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		h.width = msg.Width
		h.height = msg.Height
		h.table.SetHeight(h.tableHeight())
		h.table.SetWidth(msg.Width)
		return h, nil

	case sessionListMsg:
		h.loading = false
		h.sessions = msg.sessions
		h.err = msg.err
		h.buildTableRows()
		if h.table.Cursor() >= len(h.sessions) && len(h.sessions) > 0 {
			h.table.SetCursor(len(h.sessions) - 1)
		}
		return h, nil

	case sessionArchivedMsg:
		h.confirming = false
		h.archiving = false
		if msg.err != nil {
			h.err = msg.err
			return h, nil
		}
		// Remove from list and adjust cursor.
		for i, s := range h.sessions {
			if s.Id == msg.id {
				h.sessions = append(h.sessions[:i], h.sessions[i+1:]...)
				break
			}
		}
		h.buildTableRows()
		if h.table.Cursor() >= len(h.sessions) && len(h.sessions) > 0 {
			h.table.SetCursor(len(h.sessions) - 1)
		}
		return h, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		h.spinner, cmd = h.spinner.Update(msg)
		// Rebuild rows to animate spinner frames.
		if len(h.sessions) > 0 {
			h.buildTableRows()
		}
		return h, cmd

	case tickMsg:
		return h, tea.Batch(fetchSessions(h.client, h.ctx), tickCmd())

	case tea.KeyMsg:
		if h.confirming {
			return h.updateArchiveConfirm(msg)
		}

		switch msg.String() {
		case "q":
			return h, tea.Quit
		case "n":
			return h, func() tea.Msg { return switchViewMsg{view: ViewNewSession} }
		case "r":
			return h, func() tea.Msg { return switchViewMsg{view: ViewRepoList} }
		case "s":
			return h, func() tea.Msg { return switchViewMsg{view: ViewSettings} }
		case "t":
			return h, func() tea.Msg { return switchViewMsg{view: ViewTrash} }
		case "a":
			if len(h.sessions) > 0 {
				h.confirming = true
			}
			return h, nil
		case "enter":
			if len(h.sessions) > 0 {
				sess := h.sessions[h.table.Cursor()]
				return h, func() tea.Msg {
					return switchViewMsg{view: ViewChatPicker, sessionID: sess.Id}
				}
			}
			return h, nil
		}

		// Forward navigation keys to the table.
		var cmd tea.Cmd
		h.table, cmd = h.table.Update(msg)
		updateCursorColumn(&h.table)
		return h, cmd
	}

	return h, nil
}

func (h HomeModel) updateArchiveConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "enter":
		h.confirming = false
		h.archiving = true
		sess := h.sessions[h.table.Cursor()]
		return h, func() tea.Msg {
			_, err := h.client.ArchiveSession(h.ctx, sess.Id)
			return sessionArchivedMsg{id: sess.Id, err: err}
		}
	case "n", "esc":
		h.confirming = false
	}
	return h, nil
}

// renderError renders an error message that wraps to the given terminal width.
// If width is 0 (unknown), it falls back to no width constraint.
func renderError(msg string, width int) string {
	s := styleError
	if width > 0 {
		// Account for padding (2 chars each side).
		s = s.Width(width - 4)
	}
	return s.Render(msg)
}

// bannerGradient defines a horizontal color gradient for the B icon (dawn palette).
var bannerGradient = []color.Color{
	lipgloss.Color("#00C6FF"),
	lipgloss.Color("#00AAFF"),
	lipgloss.Color("#008EFF"),
	lipgloss.Color("#0072FF"),
}

// bannerHeight is the number of lines rendered by renderBanner (including padding).
// Banner has padding(1,1,1,1) = 1 top + 2 content + 1 bottom = 4 lines.
const bannerHeight = 4

func renderBanner() string {
	cwd, _ := os.Getwd()
	if home, err := os.UserHomeDir(); err == nil {
		cwd = strings.Replace(cwd, home, "~", 1)
	}

	// Logo chars per row, matching `npx oh-my-logo "B" dawn --filled --block-font tiny`.
	row1 := []string{" ", "█", "▄", "▄"}
	row2 := []string{" ", "█", "▄", "█"}

	colorize := func(chars []string) string {
		var b strings.Builder
		for i, ch := range chars {
			b.WriteString(lipgloss.NewStyle().Foreground(bannerGradient[i]).Render(ch))
		}
		return b.String()
	}

	banner := colorize(row1) + "  Bossanova v" + buildinfo.Version + "\n" +
		colorize(row2) + "  " + styleSubtle.Render(cwd)

	return lipgloss.NewStyle().Padding(1, 1, 1, 1).Render(banner)
}

// StateLabel returns a short human-readable label for a session state.
func StateLabel(state pb.SessionState) string {
	switch state {
	case pb.SessionState_SESSION_STATE_CREATING_WORKTREE:
		return "creating"
	case pb.SessionState_SESSION_STATE_STARTING_CLAUDE:
		return "starting"
	case pb.SessionState_SESSION_STATE_PUSHING_BRANCH:
		return "pushing"
	case pb.SessionState_SESSION_STATE_OPENING_DRAFT_PR:
		return "opening PR"
	case pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN:
		return "implementing"
	case pb.SessionState_SESSION_STATE_AWAITING_CHECKS:
		return "checks"
	case pb.SessionState_SESSION_STATE_FIXING_CHECKS:
		return "fixing"
	case pb.SessionState_SESSION_STATE_GREEN_DRAFT:
		return "green"
	case pb.SessionState_SESSION_STATE_READY_FOR_REVIEW:
		return "review"
	case pb.SessionState_SESSION_STATE_BLOCKED:
		return "blocked"
	case pb.SessionState_SESSION_STATE_MERGED:
		return "merged"
	case pb.SessionState_SESSION_STATE_CLOSED:
		return "closed"
	default:
		return "unknown"
	}
}

// ChecksLabel returns a plain text label for CI status (for non-TUI output).
func ChecksLabel(state pb.ChecksOverall) string {
	switch state {
	case pb.ChecksOverall_CHECKS_OVERALL_PASSED:
		return "pass"
	case pb.ChecksOverall_CHECKS_OVERALL_FAILED:
		return "fail"
	case pb.ChecksOverall_CHECKS_OVERALL_PENDING:
		return "pending"
	default:
		return "-"
	}
}

// checksLabelAndColor returns the raw label and color for a CI status.
func checksLabelAndColor(state pb.ChecksOverall) (string, color.Color) {
	switch state {
	case pb.ChecksOverall_CHECKS_OVERALL_PASSED:
		return "pass", colorSuccess
	case pb.ChecksOverall_CHECKS_OVERALL_FAILED:
		return "fail", colorDanger
	case pb.ChecksOverall_CHECKS_OVERALL_PENDING:
		return "...", colorWarning
	default:
		return "-", colorMuted
	}
}

// tableHeight returns the height to pass to table.SetHeight.
// The table renders header + (h-1) data rows, so h = available vertical space.
// Capped at len(sessions)+1 so the table doesn't expand beyond its content.
func (h HomeModel) tableHeight() int {
	// Only as tall as needed: header + all rows.
	needed := len(h.sessions) + 1
	if h.height <= 0 {
		return needed
	}
	overhead := bannerHeight + 1 + 1 + actionBarPadY + 1 // banner + gap + gap + actionbar padding + actionbar
	avail := h.height - overhead
	if avail < 1 {
		avail = 1
	}
	if needed < avail {
		return needed
	}
	return avail
}

func (h HomeModel) View() tea.View {
	if h.err != nil {
		return tea.NewView(
			renderError(fmt.Sprintf("Cannot connect to daemon: %v", h.err), h.width) +
				"\n" +
				lipgloss.NewStyle().Padding(0, 2).Render("Start the daemon with: bossd") +
				"\n" +
				styleActionBar.Render("Press q to quit."),
		)
	}

	if h.loading {
		return tea.NewView(
			lipgloss.NewStyle().Padding(0, 2).Render("Loading sessions..."),
		)
	}

	if len(h.sessions) == 0 {
		return tea.NewView(
			lipgloss.NewStyle().Padding(0, 2).Render("No active sessions.") + "\n" +
				styleActionBar.Render("[n]ew session  [r]epos  [s]ettings  [t] open trash  [q]uit"),
		)
	}

	var b strings.Builder

	if len(h.sessions) > 0 {
		b.WriteString(lipgloss.NewStyle().Padding(0, 1).Render(h.table.View()))
		b.WriteString("\n")
	}

	if h.archiving {
		b.WriteString(lipgloss.NewStyle().Padding(actionBarPadY, 2).Foreground(colorDanger).Render(
			h.spinner.View() + "Archiving..."))
	} else if h.confirming {
		b.WriteString("\n")
		sess := h.sessions[h.table.Cursor()]
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorDanger).Render(
			fmt.Sprintf("Archive %q?", sess.Title)))
		b.WriteString("\n")
		b.WriteString(styleActionBar.Render("[y/enter] confirm  [n/esc] cancel"))
	} else if len(h.sessions) > 0 {
		b.WriteString(styleActionBar.Render("[enter] select  [n]ew session  [a]rchive  [r]epos  [s]ettings  [t] open trash  [q]uit"))
	} else {
		b.WriteString(styleActionBar.Render("[n]ew session  [r]epos  [s]ettings  [t] open trash  [q]uit"))
	}

	return tea.NewView(b.String())
}

// tickMsg signals a polling refresh.
type tickMsg struct{}

func tickCmd() tea.Cmd {
	return tea.Tick(pollInterval, func(time.Time) tea.Msg {
		return tickMsg{}
	})
}

func fetchSessions(c client.BossClient, ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		sessions, err := c.ListSessions(ctx, &pb.ListSessionsRequest{})
		return sessionListMsg{sessions: sessions, err: err}
	}
}
