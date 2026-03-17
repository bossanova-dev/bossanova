package views

import (
	"context"
	"fmt"
	"image/color"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/internal/client"
	"github.com/recurser/bossalib/buildinfo"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

const pollInterval = 2 * time.Second

// sessionListMsg carries the result of a ListSessions RPC call.
type sessionListMsg struct {
	sessions []*pb.Session
	err      error
}

// HomeModel is the main dashboard view showing active sessions.
type HomeModel struct {
	client   client.BossClient
	ctx      context.Context
	sessions []*pb.Session
	cursor   int
	err      error
	loading  bool
	width    int
	height   int
}

// NewHomeModel creates a HomeModel wired to the daemon client.
func NewHomeModel(c client.BossClient, ctx context.Context) HomeModel {
	return HomeModel{
		client:  c,
		ctx:     ctx,
		loading: true,
	}
}

func (h HomeModel) Init() tea.Cmd {
	return tea.Batch(fetchSessions(h.client, h.ctx), tickCmd())
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

	case tickMsg:
		return h, tea.Batch(fetchSessions(h.client, h.ctx), tickCmd())

	case tea.KeyMsg:
		switch msg.String() {
		case "q":
			return h, tea.Quit
		case "n":
			return h, func() tea.Msg { return switchViewMsg{view: ViewNewSession} }
		case "r":
			return h, func() tea.Msg { return switchViewMsg{view: ViewRepoList} }
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

// State color scheme matching the TS implementation.
var (
	colorGreen  = lipgloss.Color("#04B575")
	colorYellow = lipgloss.Color("#DBBD70")
	colorRed    = lipgloss.Color("#FF6347")
	colorCyan   = lipgloss.Color("#00CED1")
	colorGray   = lipgloss.Color("#626262")
	colorOrange = lipgloss.Color("#F09837")

	styleTitle     = lipgloss.NewStyle().Bold(true).Padding(0, 2)
	styleSelected  = lipgloss.NewStyle().Bold(true)
	styleActionBar = lipgloss.NewStyle().Faint(true).Padding(1, 2)
	styleError     = lipgloss.NewStyle().Foreground(colorRed).Padding(1, 2)
	styleSubtle    = lipgloss.NewStyle().Faint(true)
)

func renderBanner() string {
	l := lipgloss.NewStyle().Foreground(colorOrange)

	cwd, _ := os.Getwd()
	if home, err := os.UserHomeDir(); err == nil {
		cwd = strings.Replace(cwd, home, "~", 1)
	}

	banner := l.Render(" ████") + "\n" +
		l.Render(" █   █") + "   Bossanova v" + buildinfo.Version + "\n" +
		l.Render(" ████") + "   " + styleSubtle.Render(cwd) + "\n" +
		l.Render(" █   █") + "\n" +
		l.Render(" ████")

	return lipgloss.NewStyle().Padding(1, 1, 1, 1).Render(banner)
}

func stateColor(state pb.SessionState) color.Color {
	switch state {
	case pb.SessionState_SESSION_STATE_MERGED,
		pb.SessionState_SESSION_STATE_GREEN_DRAFT,
		pb.SessionState_SESSION_STATE_READY_FOR_REVIEW:
		return colorGreen
	case pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
		pb.SessionState_SESSION_STATE_AWAITING_CHECKS:
		return colorYellow
	case pb.SessionState_SESSION_STATE_FIXING_CHECKS,
		pb.SessionState_SESSION_STATE_BLOCKED:
		return colorRed
	case pb.SessionState_SESSION_STATE_CREATING_WORKTREE,
		pb.SessionState_SESSION_STATE_STARTING_CLAUDE,
		pb.SessionState_SESSION_STATE_PUSHING_BRANCH,
		pb.SessionState_SESSION_STATE_OPENING_DRAFT_PR:
		return colorCyan
	default:
		return colorGray
	}
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

func checksIcon(state pb.ChecksOverall) string {
	switch state {
	case pb.ChecksOverall_CHECKS_OVERALL_PASSED:
		return lipgloss.NewStyle().Foreground(colorGreen).Render("pass")
	case pb.ChecksOverall_CHECKS_OVERALL_FAILED:
		return lipgloss.NewStyle().Foreground(colorRed).Render("fail")
	case pb.ChecksOverall_CHECKS_OVERALL_PENDING:
		return lipgloss.NewStyle().Foreground(colorYellow).Render("...")
	default:
		return styleSubtle.Render("-")
	}
}

func (h HomeModel) View() tea.View {
	if h.err != nil {
		return tea.NewView(
			styleError.Render(fmt.Sprintf("Cannot connect to daemon: %v", h.err)) +
				"\n" +
				lipgloss.NewStyle().Padding(0, 2).Render("Start the daemon with: bossd") +
				"\n\n" +
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
				styleActionBar.Render("[n]ew session  [r]epo  [q]uit"),
		)
	}

	var b strings.Builder

	b.WriteString(renderBanner())
	b.WriteString("\n")

	// Fixed columns: cursor(2) + padding(4) + ID(7) + STATE(14) + PR(5) + CI(4) + spacing(9) = 45
	const fixedCols = 45
	branchWidth := h.width - fixedCols
	if branchWidth < 16 {
		branchWidth = 16
	}

	// Table header.
	header := fmt.Sprintf("  %-7s  %-*s  %-14s  %-5s  %-4s",
		"ID", branchWidth, "BRANCH", "STATE", "PR", "CI")
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Faint(true).Render(header))
	b.WriteString("\n")

	// Session rows.
	for i, sess := range h.sessions {
		selected := i == h.cursor

		state := StateLabel(sess.State)
		branchDisplay := strings.TrimPrefix(sess.BranchName, "boss/")
		branch := truncate(branchDisplay, branchWidth)
		pr := "-"
		if sess.PrNumber != nil {
			pr = fmt.Sprintf("#%d", *sess.PrNumber)
		}
		ci := checksIcon(sess.LastCheckState)

		stateStyled := lipgloss.NewStyle().Foreground(stateColor(sess.State)).Render(fmt.Sprintf("%-14s", state))

		cursor := "  "
		if selected {
			cursor = "> "
		}

		shortID := sess.Id
		if len(shortID) > 7 {
			shortID = shortID[:7]
		}

		row := fmt.Sprintf("%s%-7s  %-*s  %s  %-5s  %s",
			cursor, shortID, branchWidth, branch, stateStyled, pr, ci)

		if selected {
			row = styleSelected.Render(row)
		}

		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(row))
		b.WriteString("\n")
	}

	b.WriteString(styleActionBar.Render("[n]ew session  [r]epo  [enter] chats  [q]uit"))

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
