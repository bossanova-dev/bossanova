package views

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/boss/internal/client"
	"github.com/recurser/boss/internal/daemon"
	"github.com/recurser/boss/internal/upgrade"
	"github.com/recurser/bossalib/buildinfo"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

const (
	pollInterval        = 2 * time.Second
	upgradeCheckTimeout = 5 * time.Second
	upgradeCacheTTL     = 24 * time.Hour
)

// upgradeNow is the clock source used by the upgrade-check goroutine. Tests
// override it to pin a fixed instant.
var upgradeNow = time.Now

// upgradeCachePath returns the on-disk path for the upgrade banner cache.
// Returns "" if the user's cache directory is unavailable; callers treat an
// empty path as "cache disabled" and skip both reads and writes.
var upgradeCachePath = func() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "bossanova", "upgrade-cache.json")
}

// sessionListMsg carries the result of a ListSessions RPC call,
// along with daemon-side heartbeat statuses for cross-instance display.
type sessionListMsg struct {
	sessions        []*pb.Session
	daemonStatuses  map[string]string // session_id → status string
	availableAgents []client.AgentInfo
	err             error
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

// authStatusMsg carries the result of checking auth status.
type authStatusMsg struct {
	loggedIn bool
	email    string
}

type upgradeCheckMsg struct {
	current   string
	latest    string
	url       string
	available bool
	err       error
}

type upgradeRunMsg struct {
	err error
}

type daemonRestartMsg struct {
	err error
}

type upgradeCheckFunc func(context.Context, string) (upgrade.CheckResult, error)

// HomeModel is the main dashboard view showing active sessions.
type HomeModel struct {
	client          client.BossClient
	ctx             context.Context
	spinner         spinner.Model
	sessions        []*pb.Session
	daemonStatuses  map[string]string // session_id → status string from daemon heartbeats
	availableAgents []client.AgentInfo
	table           table.Model
	err             error
	loading         bool
	width           int
	height          int
	repoCount       int // number of registered repos (for empty state guidance)

	// Navigation
	highlightSessionID string // session to auto-highlight after returning from chat picker

	// mergedOptimisticID is set when the user returns from a successful merge
	// in the chat picker. While set, the matching session's DisplayStatus
	// is rendered as MERGED even if the daemon still reports PASSING — the
	// PR-merged webhook lands asynchronously, so without this override the
	// status column would flicker back to "passing" until the next poll.
	// Cleared once the daemon reports a terminal state (MERGED/CLOSED).
	mergedOptimisticID string

	// Archive confirmation / in-progress
	confirming         bool
	archivingSessionID string

	// Auth
	authMgr          *auth.Manager // nil means auth not configured
	loggedIn         bool
	loggedInEmail    string
	logoutConfirming bool

	// Upgrade banner / restart prompt
	upgradeAvailable bool
	upgradeCurrent   string
	upgradeLatest    string
	upgradeURL       string
	upgradeChecking  bool
	upgradeError     string
	upgradeDone      bool
	restartPrompt    bool
}

// NewHomeModel creates a HomeModel wired to the daemon client.
func NewHomeModel(c client.BossClient, ctx context.Context, authMgr *auth.Manager) HomeModel {
	return HomeModel{
		client:          c,
		ctx:             ctx,
		authMgr:         authMgr,
		spinner:         newStatusSpinner(),
		loading:         true,
		table:           newBossTable(nil, nil, 0),
		upgradeChecking: true,
	}
}

// DaemonStatuses returns the per-session daemon heartbeat statuses, keyed by
// session ID. Used by the top-level App to attach diagnostic context to a
// bug report.
func (h HomeModel) DaemonStatuses() map[string]string { return h.daemonStatuses }

// applyMergedOptimisticOverride overrides the tracked session's display
// status to MERGED until the daemon webhook catches up. Clears the override
// once the server reports a terminal state.
func (h *HomeModel) applyMergedOptimisticOverride() {
	if h.mergedOptimisticID == "" {
		return
	}
	for _, sess := range h.sessions {
		if sess.Id != h.mergedOptimisticID {
			continue
		}
		switch sess.GetDisplayStatus() {
		case pb.DisplayStatus_DISPLAY_STATUS_MERGED,
			pb.DisplayStatus_DISPLAY_STATUS_CLOSED:
			h.mergedOptimisticID = ""
		default:
			sess.DisplayStatus = pb.DisplayStatus_DISPLAY_STATUS_MERGED
		}
		return
	}
}

// selectedSessionID returns the ID of the currently highlighted session,
// or "" if no session is selected.
func (h HomeModel) selectedSessionID() string {
	sess := h.selectedSession()
	if sess == nil {
		return ""
	}
	return sess.Id
}

func (h HomeModel) selectedSession() *pb.Session {
	idx, ok := h.sessionIndexForTableCursor(h.table.Cursor())
	if !ok {
		return nil
	}
	return h.sessions[idx]
}

func (h HomeModel) sessionIndexForTableCursor(cursor int) (int, bool) {
	if cursor < 0 {
		return 0, false
	}
	row := 0
	for i, sess := range h.sessions {
		if cursor == row {
			return i, true
		}
		row++
		for range sessionWarningHints(sess) {
			if cursor == row {
				return i, true
			}
			row++
		}
	}
	return 0, false
}

func (h HomeModel) primarySessionRows() []int {
	rows := make([]int, 0, len(h.sessions))
	row := 0
	for _, sess := range h.sessions {
		rows = append(rows, row)
		row++
		for range sessionWarningHints(sess) {
			row++
		}
	}
	return rows
}

func (h HomeModel) tableDataRowCount() int {
	rows := len(h.sessions)
	for _, sess := range h.sessions {
		rows += len(sessionWarningHints(sess))
	}
	return rows
}

func (h HomeModel) tableCursorForSessionIndex(sessionIndex int) int {
	row := 0
	for i, sess := range h.sessions {
		if i == sessionIndex {
			return row
		}
		row++
		for range sessionWarningHints(sess) {
			row++
		}
	}
	return -1
}

func (h HomeModel) renderSessionStatus(sess *pb.Session) string {
	if sess != nil && sess.Id != "" && sess.Id == h.archivingSessionID {
		return renderRowPendingStatus(h.spinner, "archiving")
	}
	return renderDisplayStatus(sess, h.spinner)
}

func (h *HomeModel) normalizeTableCursor(previousCursor int) {
	rows := h.primarySessionRows()
	if len(rows) == 0 {
		return
	}
	cursor := h.table.Cursor()
	for _, row := range rows {
		if cursor == row {
			return
		}
	}
	if cursor > previousCursor {
		for _, row := range rows {
			if row > cursor {
				h.table.SetCursor(row)
				return
			}
		}
	} else if cursor < previousCursor {
		for i := len(rows) - 1; i >= 0; i-- {
			if rows[i] < cursor {
				h.table.SetCursor(rows[i])
				return
			}
		}
	}
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i] <= cursor {
			h.table.SetCursor(rows[i])
			return
		}
	}
	h.table.SetCursor(rows[0])
}

func (h HomeModel) Init() tea.Cmd {
	cmds := []tea.Cmd{fetchSessions(h.client, h.ctx), fetchRepoCount(h.client, h.ctx), tickCmd(), h.spinner.Tick, checkUpgradeCmd(h.ctx)}
	if h.authMgr != nil {
		cmds = append(cmds, fetchAuthStatus(h.authMgr))
	}
	return tea.Batch(cmds...)
}

func checkUpgradeCmd(ctx context.Context) tea.Cmd {
	checker := upgrade.Checker{}
	return checkUpgradeCmdForVersion(ctx, buildinfo.Version, checker.Check)
}

func checkUpgradeCmdForVersion(ctx context.Context, current string, check upgradeCheckFunc) tea.Cmd {
	if ctx == nil {
		ctx = context.Background()
	}
	version, ok, dev := upgrade.NormalizeVersion(current)
	if !ok || dev {
		return func() tea.Msg {
			return upgradeCheckMsg{}
		}
	}
	if check == nil {
		checker := upgrade.Checker{}
		check = checker.Check
	}
	return func() tea.Msg {
		now := upgradeNow()
		cachePath := upgradeCachePath()
		if cachePath != "" {
			if entry, ok, err := upgrade.ReadFreshCache(cachePath, version, now, upgradeCacheTTL); err == nil && ok {
				return upgradeCheckMsg{
					current:   entry.CurrentVersion,
					latest:    entry.LatestVersion,
					url:       entry.ReleaseURL,
					available: upgrade.CompareStableVersions(entry.CurrentVersion, entry.LatestVersion) == upgrade.CompareOlder && !entry.Suppressed(now, entry.LatestVersion),
				}
			}
		}
		checkCtx, cancel := context.WithTimeout(ctx, upgradeCheckTimeout)
		defer cancel()
		result, err := check(checkCtx, version)
		available := result.Available
		if err == nil && cachePath != "" {
			entry := upgrade.CacheEntry{
				CheckedAt:      now,
				CurrentVersion: result.CurrentVersion,
				LatestVersion:  result.LatestVersion,
				ReleaseURL:     result.ReleaseURL,
			}
			// Preserve a prior snooze across the cache refresh — without
			// this the user's dismiss is silently discarded the moment
			// the cache TTL expires.
			if prior, ok, _ := upgrade.ReadCache(cachePath); ok && prior.SnoozedVersion == result.LatestVersion && !prior.SnoozedUntil.IsZero() && now.Before(prior.SnoozedUntil) {
				entry.SnoozedVersion = prior.SnoozedVersion
				entry.SnoozedUntil = prior.SnoozedUntil
				if available {
					available = false
				}
			}
			_ = upgrade.WriteCache(cachePath, entry)
		}
		return upgradeCheckMsg{
			current:   result.CurrentVersion,
			latest:    result.LatestVersion,
			url:       result.ReleaseURL,
			available: available,
			err:       err,
		}
	}
}

func runUpgradeCmd(executable string) tea.Cmd {
	return tea.ExecProcess(
		exec.Command(executable, "upgrade", "--yes", "--no-restart"),
		func(err error) tea.Msg { return upgradeRunMsg{err: err} },
	)
}

func restartDaemonCmd() tea.Cmd {
	return func() tea.Msg {
		return daemonRestartMsg{err: daemon.Restart()}
	}
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
		return ""
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
	names := make([]string, len(h.sessions))       // plain text for width calc
	linkedNames := make([]string, len(h.sessions)) // may contain OSC 8 hyperlinks
	nameWidthLabels := make([]string, 0, len(h.sessions)*2)
	agents := make([]string, len(h.sessions))
	showAgentColumn := shouldShowSessionAgentColumn(h.availableAgents, h.sessions)
	prLabels := make([]string, len(h.sessions)) // visible text for width calc
	prs := make([]string, len(h.sessions))      // may contain OSC 8 hyperlinks
	for i, sess := range h.sessions {
		repos[i] = sess.RepoDisplayName
		if sess.Title != "" {
			names[i] = sess.Title
		} else {
			names[i] = sess.BranchName
		}
		nameWidthLabels = append(nameWidthLabels, names[i])
		agents[i] = sess.GetAgentName()
		if agents[i] == "" {
			agents[i] = "-"
		}
		nameWidthLabels = append(nameWidthLabels, sessionWarningHints(sess)...)
		linkedNames[i] = renderTrackerLink(sess, names[i])
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
		{Title: "NAME", Width: maxColWidth("NAME", nameWidthLabels, 60) + tableColumnSep},
	}
	if showAgentColumn {
		cols = append(cols, table.Column{Title: "AGENT", Width: maxColWidth("AGENT", agents, 12) + tableColumnSep})
	}
	cols = append(cols,
		table.Column{Title: "PR", Width: maxColWidth("PR", prLabels, 8) + tableColumnSep},
		table.Column{Title: "STATUS", Width: 16 + tableColumnSep},
	)

	mutedStrike := lipgloss.NewStyle().Foreground(colorMuted).Strikethrough(true)

	cursor := h.table.Cursor()
	rows := make([]table.Row, 0, len(h.sessions)*2)
	for i, sess := range h.sessions {
		rowIndex := len(rows)
		statusStyled := h.renderSessionStatus(sess)

		attn := renderAttentionIndicator(sess)
		repo, name, pr := repos[i], linkedNames[i], prs[i]
		if sess.DisplayStatus == pb.DisplayStatus_DISPLAY_STATUS_MERGED ||
			sess.DisplayStatus == pb.DisplayStatus_DISPLAY_STATUS_CLOSED {
			repo = mutedStrike.Render(repos[i])
			// renderMutedTrackerLink styles the full title with raw ANSI and
			// wraps the tracker ID in OSC 8; do NOT wrap its output with
			// lipgloss — that strips the hyperlink envelope.
			name = renderMutedTrackerLink(sess, names[i])
			pr = renderMutedPRLink(sess)
		}

		indicator := ""
		if rowIndex == cursor {
			indicator = cursorChevron
		}
		row := table.Row{indicator, attn, repo, name}
		if showAgentColumn {
			row = append(row, agents[i])
		}
		row = append(row, pr, statusStyled)
		rows = append(rows, row)
		for _, hint := range sessionWarningHints(sess) {
			hintRow := table.Row{"", "", "", styleStatusMuted.Render(hint)}
			if showAgentColumn {
				hintRow = append(hintRow, "")
			}
			hintRow = append(hintRow, "", "")
			rows = append(rows, hintRow)
		}
	}

	h.table.SetColumns(cols)
	h.table.SetRows(rows)
	h.table.SetWidth(columnsWidth(cols))
	h.table.SetHeight(h.tableHeight())
	h.table.SetCursor(cursor)
	h.normalizeTableCursor(cursor)
	updateCursorColumn(&h.table)
}

func hasMultipleSessionAgents(sessions []*pb.Session) bool {
	seen := map[string]struct{}{}
	for _, sess := range sessions {
		agent := sess.GetAgentName()
		if agent == "" {
			continue
		}
		seen[agent] = struct{}{}
		if len(seen) > 1 {
			return true
		}
	}
	return false
}

func shouldShowSessionAgentColumn(availableAgents []client.AgentInfo, sessions []*pb.Session) bool {
	seen := map[string]struct{}{}
	for _, agent := range availableAgents {
		name := agent.Name
		if name == "" {
			continue
		}
		seen[name] = struct{}{}
		if len(seen) > 1 {
			return true
		}
	}
	return hasMultipleSessionAgents(sessions)
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

	case authStatusMsg:
		h.loggedIn = msg.loggedIn
		h.loggedInEmail = msg.email
		return h, nil

	case upgradeCheckMsg:
		h.upgradeChecking = false
		if msg.err != nil {
			return h, nil
		}
		h.upgradeAvailable = msg.available
		h.upgradeCurrent = msg.current
		h.upgradeLatest = msg.latest
		h.upgradeURL = msg.url
		return h, nil

	case upgradeRunMsg:
		if msg.err != nil {
			h.upgradeError = msg.err.Error()
			return h, nil
		}
		h.upgradeError = ""
		h.upgradeDone = true
		h.restartPrompt = true
		return h, nil

	case daemonRestartMsg:
		if msg.err != nil {
			h.upgradeError = msg.err.Error()
			return h, nil
		}
		h.upgradeError = ""
		h.restartPrompt = false
		h.upgradeAvailable = false
		return h, nil

	case sessionListMsg:
		h.loading = false
		h.sessions = msg.sessions
		h.daemonStatuses = msg.daemonStatuses
		h.availableAgents = msg.availableAgents
		h.err = msg.err
		h.applyMergedOptimisticOverride()
		h.buildTableRows()
		if h.highlightSessionID != "" {
			for i, sess := range h.sessions {
				if sess.Id == h.highlightSessionID {
					h.table.SetCursor(h.tableCursorForSessionIndex(i))
					updateCursorColumn(&h.table)
					break
				}
			}
			h.highlightSessionID = ""
		} else if len(h.sessions) > 0 {
			h.normalizeTableCursor(h.table.Cursor())
			updateCursorColumn(&h.table)
		}
		return h, nil

	case sessionArchivedMsg:
		h.confirming = false
		h.archivingSessionID = ""
		if msg.err != nil {
			h.err = msg.err
			h.buildTableRows()
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
		if len(h.sessions) > 0 {
			h.normalizeTableCursor(h.table.Cursor())
			updateCursorColumn(&h.table)
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
		// Re-poll auth status alongside sessions so the menu label
		// (e.g. [l]ogout vs [l]ogin) catches up after the daemon
		// clears the keychain on a terminal refresh failure
		// (invalid_grant). Without this, the TUI keeps showing the
		// pre-expiry label until the user navigates away and back.
		cmds := []tea.Cmd{fetchSessions(h.client, h.ctx), tickCmd()}
		if h.authMgr != nil {
			cmds = append(cmds, fetchAuthStatus(h.authMgr))
		}
		return h, tea.Batch(cmds...)

	case tea.KeyMsg:
		if h.logoutConfirming {
			return h.updateLogoutConfirm(msg)
		}
		if h.confirming {
			return h.updateArchiveConfirm(msg)
		}
		if h.restartPrompt {
			switch msg.String() {
			case "r", "enter":
				return h, restartDaemonCmd()
			case "esc", "n":
				h.restartPrompt = false
				return h, nil
			}
		}
		if h.upgradeAvailable {
			switch msg.String() {
			case "u":
				executable, err := os.Executable()
				if err != nil {
					h.upgradeError = err.Error()
					return h, nil
				}
				h.upgradeError = ""
				return h, runUpgradeCmd(executable)
			case "U":
				h.upgradeAvailable = false
				if path := upgradeCachePath(); path != "" {
					_ = upgrade.SnoozeUpgrade(
						path,
						h.upgradeCurrent,
						h.upgradeLatest,
						h.upgradeURL,
						upgradeNow(),
						upgrade.DefaultSnoozeDuration,
					)
				}
				return h, nil
			}
		}

		switch msg.String() {
		case "n":
			if h.repoCount == 0 {
				return h, nil
			}
			return h, func() tea.Msg { return switchViewMsg{view: ViewNewSession} }
		case "r":
			return h, func() tea.Msg { return switchViewMsg{view: ViewRepoList} }
		case "s":
			return h, func() tea.Msg { return switchViewMsg{view: ViewSettings} }
		case "t":
			return h, func() tea.Msg { return switchViewMsg{view: ViewTrash} }
		case "c":
			return h, func() tea.Msg { return switchViewMsg{view: ViewCron} }
		case "l":
			if h.authMgr == nil {
				return h, nil
			}
			if h.loggedIn {
				h.logoutConfirming = true
				return h, nil
			}
			return h, func() tea.Msg { return switchViewMsg{view: ViewLogin} }
		case "enter":
			if sess := h.selectedSession(); sess != nil {
				if sess.Id != "" && sess.Id == h.archivingSessionID {
					return h, nil
				}
				return h, func() tea.Msg {
					return switchViewMsg{view: ViewChatPicker, sessionID: sess.Id}
				}
			}
			return h, nil
		case "a":
			if len(h.sessions) > 0 && h.archivingSessionID == "" {
				h.confirming = true
			}
			return h, nil
		case "q":
			return h, tea.Quit
		}

		// Forward navigation keys to the table.
		var cmd tea.Cmd
		previousCursor := h.table.Cursor()
		h.table, cmd = h.table.Update(msg)
		h.normalizeTableCursor(previousCursor)
		updateCursorColumn(&h.table)
		return h, cmd
	}

	return h, nil
}

func (h HomeModel) updateArchiveConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "enter":
		h.confirming = false
		sess := h.selectedSession()
		if sess == nil || sess.Id == "" {
			return h, nil
		}
		sessionID := sess.Id
		h.archivingSessionID = sessionID
		h.buildTableRows()
		return h, func() tea.Msg {
			_, err := h.client.ArchiveSession(h.ctx, sessionID)
			return sessionArchivedMsg{id: sessionID, err: err}
		}
	case "n", "esc":
		h.confirming = false
	}
	return h, nil
}

func (h HomeModel) updateLogoutConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "enter":
		h.logoutConfirming = false
		if h.authMgr != nil {
			_ = h.authMgr.Logout()
		}
		h.loggedIn = false
		h.loggedInEmail = ""
		return h, func() tea.Msg {
			_ = h.client.NotifyAuthChange(h.ctx, "logout")
			return nil
		}
	case "n", "esc":
		h.logoutConfirming = false
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

func (h HomeModel) upgradeStatusView() string {
	var lines []string
	if h.upgradeError != "" {
		lines = append(lines, renderError("Upgrade: "+h.upgradeError, h.width))
	}
	switch {
	case h.restartPrompt:
		lines = append(lines, lipgloss.NewStyle().Padding(0, 2).Render("Upgrade installed. Quit boss after restart to use the new binary.  r restart  n later"))
	case h.upgradeDone:
		lines = append(lines, lipgloss.NewStyle().Padding(0, 2).Render("Upgrade installed. Quit boss (q) and re-launch to use the new binary."))
	case h.upgradeAvailable:
		lines = append(lines, lipgloss.NewStyle().Padding(0, 2).Render(fmt.Sprintf(
			"Upgrade available: boss %s -> %s  u upgrade  U dismiss",
			h.upgradeCurrent,
			h.upgradeLatest,
		)))
	}
	return strings.Join(lines, "\n")
}

func (h HomeModel) withUpgradeStatus(content string) string {
	status := h.upgradeStatusView()
	if status == "" {
		return content
	}
	if content == "" {
		return status
	}
	return status + "\n" + content
}

// StateLabel returns a short human-readable label for a session state.
func StateLabel(state pb.SessionState) string {
	switch state {
	case pb.SessionState_SESSION_STATE_CREATING_WORKTREE:
		return "creating"
	case pb.SessionState_SESSION_STATE_STARTING_AGENT:
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
		return "✓ merged"
	case pb.SessionState_SESSION_STATE_CLOSED:
		return "closed"
	default:
		return "unknown"
	}
}

// tableHeight returns the height to pass to table.SetHeight.
func (h HomeModel) tableHeight() int {
	overhead := bannerOverhead + 1 + actionBarPadY + 1 // banner+newline + gap + actionbar padding + actionbar
	return clampedTableHeight(h.tableDataRowCount(), h.height, overhead)
}

// loginAction returns the login/logout action label for the action bar,
// or "" if auth is not configured.
func (h HomeModel) loginAction() string {
	if h.authMgr == nil {
		return ""
	}
	if h.loggedIn {
		return "[l]ogout"
	}
	return "[l]ogin"
}

func (h HomeModel) View() tea.View {
	if h.err != nil {
		return tea.NewView(
			renderError(fmt.Sprintf("Cannot connect to daemon (%v)", h.err), h.width) +
				"\n" +
				lipgloss.NewStyle().Padding(0, 2).Render("Start the daemon with: bossd") +
				"\n" +
				styleActionBar.Render("Press q to quit."),
		)
	}

	if h.loading {
		return tea.NewView(
			h.withUpgradeStatus(lipgloss.NewStyle().Padding(0, 2).Render("Loading sessions...")),
		)
	}

	if len(h.sessions) == 0 {
		var content string
		if h.repoCount == 0 {
			// No repos configured - show welcome message with setup instructions
			actions := []string{"[r]epos", "[s]ettings"}
			if la := h.loginAction(); la != "" {
				actions = append(actions, la)
			}
			content = lipgloss.NewStyle().Padding(0, 2).Render(
				"Welcome to Bossanova!\n\n"+
					"To get started, you need to add a repository for us to work on together.\n\n"+
					lipgloss.NewStyle().Bold(true).Render("Press 'r' to open the repos menu."),
			) + "\n" +
				actionBar(actions, []string{"[q]uit"})
		} else {
			// Repos exist but no sessions - show simplified guidance
			actions := []string{"[n]ew session", "[r]epos", "[s]ettings", "[t]rash", "[c]ron"}
			if la := h.loginAction(); la != "" {
				actions = append(actions, la)
			}
			content = lipgloss.NewStyle().Padding(0, 2).Render(
				"You have no active sessions.\n\n"+
					lipgloss.NewStyle().Bold(true).Render("Press 'n' to create a new session."),
			) + "\n" +
				actionBar(actions, []string{"[q]uit"})
		}
		return tea.NewView(h.withUpgradeStatus(content))
	}

	var b strings.Builder

	if len(h.sessions) > 0 {
		b.WriteString(lipgloss.NewStyle().Padding(0, 1).Render(h.table.View()))
		b.WriteString("\n")
	}

	if h.confirming {
		b.WriteString("\n")
		if sess := h.selectedSession(); sess != nil {
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorDanger).Render(
				fmt.Sprintf("Archive %q?", sess.Title)))
		}
		b.WriteString("\n")
		b.WriteString(styleActionBar.Render("[y/enter] confirm  [n/esc] cancel"))
	} else if h.logoutConfirming {
		b.WriteString("\n")
		label := "Log out?"
		if h.loggedInEmail != "" {
			label = fmt.Sprintf("Log out %s?", h.loggedInEmail)
		}
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorDanger).Render(label))
		b.WriteString("\n")
		b.WriteString(styleActionBar.Render("[y/enter] confirm  [n/esc] cancel"))
	} else {
		navActions := []string{"[n]ew", "[r]epos", "[s]ettings", "[t]rash", "[c]ron"}
		if la := h.loginAction(); la != "" {
			navActions = append(navActions, la)
		}
		b.WriteString(actionBar(
			[]string{"[enter] select", "[a]rchive"},
			navActions,
			[]string{"[q]uit"},
		))
	}

	return tea.NewView(h.withUpgradeStatus(b.String()))
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

		availableAgents, _ := c.ListAgents(ctx)

		return sessionListMsg{
			sessions:        sessions,
			daemonStatuses:  daemonStatuses,
			availableAgents: availableAgents,
		}
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

func fetchAuthStatus(mgr *auth.Manager) tea.Cmd {
	return func() tea.Msg {
		status := mgr.Status()
		return authStatusMsg{loggedIn: status.LoggedIn, email: status.Email}
	}
}
