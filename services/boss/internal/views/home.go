package views

import (
	"context"
	"fmt"
	"image/color"
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/internal/client"
	bosspty "github.com/recurser/boss/internal/pty"
	"github.com/recurser/bossalib/buildinfo"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

const pollInterval = 2 * time.Second

// Shared layout constants for consistent TUI styling.
const (
	shortIDLen    = 7 // characters shown for truncated UUIDs
	colGap        = 2 // spaces between table columns
	actionBarPadY = 1 // blank lines above action bar
)

// colSep is the string used to separate table columns.
var colSep = strings.Repeat(" ", colGap)

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
	cursor   int
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
	}
}

func (h HomeModel) Init() tea.Cmd {
	return tea.Batch(fetchSessions(h.client, h.ctx), tickCmd(), h.spinner.Tick)
}

func (h HomeModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case sessionListMsg:
		h.loading = false
		h.sessions = msg.sessions
		h.err = msg.err
		if h.cursor >= len(h.sessions) && len(h.sessions) > 0 {
			h.cursor = len(h.sessions) - 1
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
		if h.cursor >= len(h.sessions) && len(h.sessions) > 0 {
			h.cursor = len(h.sessions) - 1
		}
		return h, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		h.spinner, cmd = h.spinner.Update(msg)
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
		case "up", "k":
			if h.cursor > 0 {
				h.cursor--
			}
			return h, nil
		case "down", "j":
			if h.cursor < len(h.sessions)-1 {
				h.cursor++
			}
			return h, nil
		case "enter":
			if len(h.sessions) > 0 {
				sess := h.sessions[h.cursor]
				return h, func() tea.Msg {
					return switchViewMsg{view: ViewChatPicker, sessionID: sess.Id}
				}
			}
			return h, nil
		}
	}

	return h, nil
}

func (h HomeModel) updateArchiveConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "enter":
		h.confirming = false
		h.archiving = true
		sess := h.sessions[h.cursor]
		return h, func() tea.Msg {
			_, err := h.client.ArchiveSession(h.ctx, sess.Id)
			return sessionArchivedMsg{id: sess.Id, err: err}
		}
	case "n", "esc":
		h.confirming = false
	}
	return h, nil
}

// State color scheme matching the TS implementation.
var (
	colorGreen  = lipgloss.Color("#04B575")
	colorYellow = lipgloss.Color("#DBBD70")
	colorRed    = lipgloss.Color("#FF6347")
	colorCyan   = lipgloss.Color("#00CED1")
	colorGray   = lipgloss.Color("#626262")

	styleTitle     = lipgloss.NewStyle().Bold(true).Padding(0, 2)
	styleSelected  = lipgloss.NewStyle().Bold(true)
	styleActionBar = lipgloss.NewStyle().Faint(true).Padding(actionBarPadY, 2)
	styleError     = lipgloss.NewStyle().Foreground(colorRed).Padding(1, 2)
	styleSubtle    = lipgloss.NewStyle().Faint(true)
)

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
		return "pass", colorGreen
	case pb.ChecksOverall_CHECKS_OVERALL_FAILED:
		return "fail", colorRed
	case pb.ChecksOverall_CHECKS_OVERALL_PENDING:
		return "...", colorYellow
	default:
		return "-", colorGray
	}
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
			renderBanner() + "\n" +
				lipgloss.NewStyle().Padding(0, 2).Render("Loading sessions..."),
		)
	}

	if len(h.sessions) == 0 {
		return tea.NewView(
			renderBanner() + "\n" +
				lipgloss.NewStyle().Padding(0, 2).Render("No active sessions.") + "\n" +
				styleActionBar.Render("[n]ew session  [r]epos  [s]ettings  [t] open trash  [q]uit"),
		)
	}

	var b strings.Builder

	b.WriteString(renderBanner())
	b.WriteString("\n")

	// Compute dynamic column widths from data.
	maxRepo := len("REPO")
	maxBranch := len("BRANCH")
	for _, sess := range h.sessions {
		if rl := len(sess.RepoDisplayName); rl > maxRepo {
			maxRepo = rl
		}
		if bl := len(strings.TrimPrefix(sess.BranchName, "boss/")); bl > maxBranch {
			maxBranch = bl
		}
	}
	if maxRepo > 20 {
		maxRepo = 20
	}
	if maxBranch > 60 {
		maxBranch = 60
	}

	// Table header.
	header := fmt.Sprintf("  %-*s"+colSep+"%-*s"+colSep+"%-*s"+colSep+"%-5s"+colSep+"%-4s"+colSep+"%s",
		shortIDLen, "ID", maxRepo, "REPO", maxBranch, "BRANCH", "PR", "CI", "STATUS")
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Faint(true).Render(header))
	b.WriteString("\n")

	// Session rows.
	for i, sess := range h.sessions {
		selected := i == h.cursor

		status := h.manager.SessionStatus(sess.Id)
		repoName := truncate(sess.RepoDisplayName, maxRepo)
		branchDisplay := strings.TrimPrefix(sess.BranchName, "boss/")
		branch := truncate(branchDisplay, maxBranch)
		pr := "-"
		if sess.PrNumber != nil {
			pr = fmt.Sprintf("#%d", *sess.PrNumber)
		}
		ciLabel, ciColor := checksLabelAndColor(sess.LastCheckState)

		stateStyled := renderStatus(status, h.spinner)

		cursor := "  "
		if selected {
			cursor = "> "
		}

		shortID := sess.Id
		if len(shortID) > shortIDLen {
			shortID = shortID[:shortIDLen]
		}

		// Pad raw text to column width before styling, so ANSI codes don't
		// break column alignment. Use lipgloss Width to pad after styling,
		// since Render() can trim trailing whitespace.
		prPadded := fmt.Sprintf("%-5s", pr)
		ciStyled := lipgloss.NewStyle().Foreground(ciColor).Width(4).Render(ciLabel)

		// Build text-only prefix (cursor through branch) — this part gets
		// bold styling when selected. Styled columns (CI, status) are appended
		// independently so their ANSI codes don't inflate the padding.
		textPrefix := fmt.Sprintf("%s%-*s"+colSep+"%-*s"+colSep+"%-*s"+colSep+"%s",
			cursor, shortIDLen, shortID, maxRepo, repoName, maxBranch, branch, prPadded)

		var row string
		if selected {
			row = styleSelected.Render(textPrefix) + colSep + ciStyled + colSep + stateStyled
		} else {
			row = textPrefix + colSep + ciStyled + colSep + stateStyled
		}

		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(row))
		b.WriteString("\n")
	}

	if h.archiving {
		b.WriteString(lipgloss.NewStyle().Padding(actionBarPadY, 2).Foreground(colorRed).Render(
			h.spinner.View() + "Archiving..."))
	} else if h.confirming {
		b.WriteString("\n")
		sess := h.sessions[h.cursor]
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorRed).Render(
			fmt.Sprintf("Archive %q?", sess.Title)))
		b.WriteString("\n")
		b.WriteString(styleActionBar.Render("[y/enter] confirm  [n/esc] cancel"))
	} else {
		b.WriteString(styleActionBar.Render("[n]ew session  [enter] select  [a]rchive  [r]epos  [s]ettings  [t] open trash  [q]uit"))
	}

	return tea.NewView(b.String())
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return s[:max-3] + "..."
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
