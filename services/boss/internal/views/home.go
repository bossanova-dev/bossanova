package views

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/internal/client"
	bosspty "github.com/recurser/boss/internal/pty"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

const pollInterval = 2 * time.Second

// sessionListMsg carries the result of a ListSessions RPC call,
// along with daemon-side heartbeat statuses for cross-instance display.
type sessionListMsg struct {
	sessions       []*pb.Session
	daemonStatuses map[string]string // session_id → status string
	err            error
}

// repoCountMsg carries the number of registered repos.
type repoCountMsg struct {
	count int
	err   error
}

// sessionArchivedMsg carries the result of archiving a session.
type sessionArchivedMsg struct {
	id  string
	err error
}

// autoEnterResolvedMsg carries the result of checking if a session has exactly
// one active Claude process (for direct attach) or needs the chat picker.
type autoEnterResolvedMsg struct {
	sessionID string
	claudeID  string // non-empty if exactly one active chat found
}

// HomeModel is the main dashboard view showing active sessions.
type HomeModel struct {
	client         client.BossClient
	ctx            context.Context
	manager        *bosspty.Manager
	spinner        spinner.Model
	sessions       []*pb.Session
	daemonStatuses map[string]string // session_id → status string from daemon heartbeats
	table          table.Model
	err            error
	loading        bool
	width          int
	height         int
	repoCount      int // number of registered repos (for empty state guidance)

	// Navigation
	highlightSessionID string // session to auto-highlight after returning from chat picker

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
	return tea.Batch(fetchSessions(h.client, h.ctx), fetchRepoCount(h.client, h.ctx), tickCmd(), h.spinner.Tick)
}

// renderAttentionIndicator returns a colored "!" for sessions needing attention,
// or an empty string otherwise.
func renderAttentionIndicator(sess *pb.Session) string {
	if sess.AttentionStatus == nil || !sess.AttentionStatus.NeedsAttention {
		return ""
	}
	switch sess.AttentionStatus.Reason {
	case pb.AttentionReason_ATTENTION_REASON_BLOCKED_MAX_ATTEMPTS:
		return styleStatusDanger.Render("!")
	case pb.AttentionReason_ATTENTION_REASON_MERGE_CONFLICT_UNRESOLVABLE:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#FF8C00")).Render("!")
	default:
		return styleStatusWarning.Render("!")
	}
}

// sessionNeedsAttention returns true if the session has a non-nil AttentionStatus
// with NeedsAttention set.
func sessionNeedsAttention(sess *pb.Session) bool {
	return sess.AttentionStatus != nil && sess.AttentionStatus.NeedsAttention
}

// sortSessionsByAttention sorts sessions so needs-attention sessions appear first,
// preserving relative order within each group.
func sortSessionsByAttention(sessions []*pb.Session) {
	sort.SliceStable(sessions, func(i, j int) bool {
		ai := sessionNeedsAttention(sessions[i])
		aj := sessionNeedsAttention(sessions[j])
		if ai != aj {
			return ai
		}
		return false
	})
}

// buildTableRows rebuilds the table columns and rows from h.sessions.
func (h *HomeModel) buildTableRows() {
	if len(h.sessions) == 0 {
		h.table.SetRows(nil)
		return
	}

	// Sort: needs-attention sessions float to top.
	sortSessionsByAttention(h.sessions)

	repos := make([]string, len(h.sessions))
	names := make([]string, len(h.sessions))
	prLabels := make([]string, len(h.sessions)) // visible text for width calc
	prs := make([]string, len(h.sessions))      // may contain OSC 8 hyperlinks
	for i, sess := range h.sessions {
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
	}

	cols := []table.Column{
		cursorColumn,
		{Title: " ", Width: 1},
		{Title: "REPO", Width: maxColWidth("REPO", repos, 20) + tableColumnSep},
		{Title: "NAME", Width: maxColWidth("NAME", names, 60) + tableColumnSep},
		{Title: "PR", Width: maxColWidth("PR", prLabels, 8) + tableColumnSep},
		{Title: "STATUS", Width: 16 + tableColumnSep},
	}

	mutedStrike := lipgloss.NewStyle().Foreground(colorMuted).Strikethrough(true)

	cursor := h.table.Cursor()
	rows := make([]table.Row, len(h.sessions))
	for i, sess := range h.sessions {
		local := h.manager.SessionStatus(sess.Id)
		daemon := h.daemonStatuses[sess.Id]
		claudeStatus := mergeStatus(local, daemon)
		statusStyled := renderPRDisplayStatus(sess, claudeStatus, h.spinner)

		attn := renderAttentionIndicator(sess)
		repo, name, pr := repos[i], names[i], prs[i]
		if sess.PrDisplayStatus == pb.PRDisplayStatus_PR_DISPLAY_STATUS_MERGED ||
			sess.PrDisplayStatus == pb.PRDisplayStatus_PR_DISPLAY_STATUS_CLOSED {
			repo = mutedStrike.Render(repos[i])
			name = mutedStrike.Render(names[i])
			pr = renderMutedPRLink(sess)
		}

		indicator := ""
		if i == cursor {
			indicator = cursorChevron
		}
		rows[i] = table.Row{indicator, attn, repo, name, pr, statusStyled}
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

	case repoCountMsg:
		if msg.err == nil {
			h.repoCount = msg.count
		}
		return h, nil

	case sessionListMsg:
		h.loading = false
		h.sessions = msg.sessions
		h.daemonStatuses = msg.daemonStatuses
		h.err = msg.err
		h.buildTableRows()
		if h.highlightSessionID != "" {
			for i, sess := range h.sessions {
				if sess.Id == h.highlightSessionID {
					h.table.SetCursor(i)
					updateCursorColumn(&h.table)
					break
				}
			}
			h.highlightSessionID = ""
		} else if h.table.Cursor() >= len(h.sessions) && len(h.sessions) > 0 {
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

	case autoEnterResolvedMsg:
		if msg.claudeID != "" {
			return h, func() tea.Msg {
				return switchViewMsg{
					view:      ViewAttach,
					sessionID: msg.sessionID,
					resumeID:  msg.claudeID,
				}
			}
		}
		return h, func() tea.Msg {
			return switchViewMsg{view: ViewChatPicker, sessionID: msg.sessionID}
		}

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
		case "p":
			return h, func() tea.Msg { return switchViewMsg{view: ViewAutopilot} }
		case "h":
			if len(h.sessions) > 0 {
				sess := h.sessions[h.table.Cursor()]
				return h, func() tea.Msg {
					return switchViewMsg{view: ViewChatPicker, sessionID: sess.Id}
				}
			}
			return h, nil
		case "a":
			if len(h.sessions) > 0 {
				h.confirming = true
			}
			return h, nil
		case "enter":
			if len(h.sessions) > 0 {
				sess := h.sessions[h.table.Cursor()]
				return h, h.resolveAutoEnter(sess.Id)
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
		return "✔ merged"
	case pb.SessionState_SESSION_STATE_CLOSED:
		return "closed"
	default:
		return "unknown"
	}
}

// tableHeight returns the height to pass to table.SetHeight.
func (h HomeModel) tableHeight() int {
	overhead := bannerOverhead + 1 + actionBarPadY + 1 // banner+newline + gap + actionbar padding + actionbar
	return clampedTableHeight(len(h.sessions), h.height, overhead)
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
		var content string
		if h.repoCount == 0 {
			// No repos configured - show welcome message with setup instructions
			content = lipgloss.NewStyle().Padding(0, 2).Render(
				"Welcome to Bossanova!\n\n"+
					"Add your first repo to get started:\n\n"+
					"  boss repo add /path/to/your/repo\n\n"+
					"Then create a session:\n\n"+
					"  Press 'n' to create a new session\n\n"+
					"Docs: https://github.com/bossanova-dev/bossanova",
			) + "\n" +
				actionBar([]string{"[n]ew session", "[r]epos", "[s]ettings"}, []string{"[q]uit"})
		} else {
			// Repos exist but no sessions - show simplified guidance
			content = lipgloss.NewStyle().Padding(0, 2).Render(
				"No active sessions.\n\n"+
					"Press 'n' to create a new session, or 'p' for autopilot.",
			) + "\n" +
				actionBar([]string{"[n]ew session", "[p]ilot", "[r]epos", "[s]ettings", "[t]rash"}, []string{"[q]uit"})
		}
		return tea.NewView(content)
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
	} else {
		// Show attention summary for selected session if it needs attention.
		if cursor := h.table.Cursor(); cursor < len(h.sessions) {
			if sess := h.sessions[cursor]; sessionNeedsAttention(sess) {
				b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorWarning).Render(
					"⚠ " + sess.AttentionStatus.Summary))
				b.WriteString("\n")
			}
		}
		b.WriteString(actionBar(
			[]string{"[enter] select", "[h]istory", "[a]rchive"},
			[]string{"[n]ew", "[p]ilot", "[r]epos", "[s]ettings", "[t]rash"},
			[]string{"[q]uit"},
		))
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

// resolveAutoEnter checks if a session has exactly one active Claude chat.
// If so, the user can skip the chat picker and attach directly.
func (h HomeModel) resolveAutoEnter(sessionID string) tea.Cmd {
	return func() tea.Msg {
		chats, err := h.client.ListChats(h.ctx, sessionID)
		if err != nil || len(chats) == 0 {
			return autoEnterResolvedMsg{sessionID: sessionID}
		}
		statuses, _ := parseChatStatuses(h.client, h.ctx, sessionID)

		// Count chats that are working or idle (either locally or via daemon).
		var activeID string
		activeCount := 0
		for _, chat := range chats {
			local := bosspty.StatusStopped
			if h.manager != nil {
				local = h.manager.ProcessStatus(chat.ClaudeId)
			}
			daemon := statuses[chat.ClaudeId]
			merged := mergeStatus(local, daemon)
			if merged == bosspty.StatusWorking || merged == bosspty.StatusIdle || merged == bosspty.StatusQuestion {
				activeID = chat.ClaudeId
				activeCount++
			}
		}
		if activeCount == 1 {
			return autoEnterResolvedMsg{sessionID: sessionID, claudeID: activeID}
		}
		return autoEnterResolvedMsg{sessionID: sessionID}
	}
}

func fetchSessions(c client.BossClient, ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		sessions, err := c.ListSessions(ctx, &pb.ListSessionsRequest{})
		if err != nil {
			return sessionListMsg{err: err}
		}

		// Fetch daemon-side heartbeat statuses for cross-instance display.
		var daemonStatuses map[string]string
		if len(sessions) > 0 {
			ids := make([]string, len(sessions))
			for i, s := range sessions {
				ids[i] = s.Id
			}
			entries, sErr := c.GetSessionStatuses(ctx, ids)
			if sErr == nil {
				daemonStatuses = make(map[string]string, len(entries))
				for _, e := range entries {
					daemonStatuses[e.SessionId] = chatStatusString(e.Status)
				}
			}
		}

		return sessionListMsg{sessions: sessions, daemonStatuses: daemonStatuses}
	}
}

func fetchRepoCount(c client.BossClient, ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		repos, err := c.ListRepos(ctx)
		if err != nil {
			return repoCountMsg{err: err}
		}
		return repoCountMsg{count: len(repos)}
	}
}
