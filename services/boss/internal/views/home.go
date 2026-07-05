package views

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/boss/internal/client"
	"github.com/recurser/boss/internal/daemon"
	"github.com/recurser/boss/internal/upgrade"
	"github.com/recurser/bossalib/buildinfo"
	"github.com/recurser/bossalib/config"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

const (
	pollInterval        = 2 * time.Second
	upgradeCheckTimeout = 5 * time.Second
	upgradeCacheTTL     = 24 * time.Hour

	// pollFailureThreshold is how many consecutive failed session polls must
	// occur before the TUI shows the "Cannot connect to daemon" screen. At a 2s
	// poll interval this tolerates ~6s of transient unreachability (e.g. the
	// daemon momentarily busy) without flashing the error view.
	pollFailureThreshold = 3
)

// upgradeNow is the clock source used by the upgrade-check goroutine. Tests
// override it to pin a fixed instant.
var upgradeNow = time.Now

// saveSettings persists config.Settings to disk. Tests stub this to avoid
// writing real config files.
var saveSettings = config.Save

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
	sessions       []*pb.Session
	daemonStatuses map[string]string // session_id → status string
	err            error
}

// repoCountMsg carries the number of registered repos.
type repoCountMsg struct {
	count int
	err   error
}

// authStatusMsg carries the result of checking auth status.
type authStatusMsg struct {
	loggedIn bool
	email    string
}

type CloudAccessClient interface {
	GetCloudAccessStatus(ctx context.Context) (*pb.CloudAccessStatus, error)
	CreateCheckoutSession(ctx context.Context, returnURL, cancelURL string) (string, error)
	CreateBillingPortalSession(ctx context.Context, returnURL string) (string, error)
	RefreshCloudEntitlements(ctx context.Context) (*pb.CloudAccessStatus, error)
}

type cloudAccessMsg struct {
	status *pb.CloudAccessStatus
	err    error
}

type homeCloudCheckoutMsg struct {
	url string
	err error
}

type startSubscriptionFlowMsg struct{}

type upgradeCheckMsg struct {
	current   string
	latest    string
	url       string
	available bool
	err       error
}

type upgradeRunMsg struct {
	output string
	err    error
}

type daemonRestartMsg struct {
	err error
}

type upgradeCheckFunc func(context.Context, string) (upgrade.CheckResult, error)

// HomeModel is the main dashboard view showing active sessions.
type HomeModel struct {
	client         client.BossClient
	ctx            context.Context
	spinner        spinner.Model
	sessions       []*pb.Session
	daemonStatuses map[string]string // session_id → status string from daemon heartbeats
	table          table.Model
	err            error
	status         string
	loading        bool
	width          int
	height         int
	repoCount      int // number of registered repos (for empty state guidance)

	// pollFailures counts consecutive failed session polls. The "Cannot
	// connect to daemon" screen is only shown once it reaches
	// pollFailureThreshold, so a single transient blip (e.g. the daemon briefly
	// busy) keeps the last-good session list on screen instead of flashing the
	// error view every 2s. Reset to 0 on any successful poll.
	pollFailures int

	// Navigation
	highlightSessionID string // session to auto-highlight after returning from chat picker

	// mergedOptimisticID is set when the user returns from a successful merge
	// in the chat picker. While set, the matching session's DisplayStatus
	// is rendered as MERGED even if the daemon still reports PASSING — the
	// PR-merged webhook lands asynchronously, so without this override the
	// status column would flicker back to "passing" until the next poll.
	// Cleared once the daemon reports a terminal state (MERGED/CLOSED).
	mergedOptimisticID string

	// archivingOptimisticID is set when the user presses ESC while an archive
	// is in flight. While set, the matching session's status is rendered as
	// "archiving" + spinner instead of the PR status, so the home list
	// reflects the in-progress archive across the chatpicker→home navigation.
	// Cleared once the session leaves the list (success) or archiveResultMsg
	// signals a failure.
	archivingOptimisticID string

	// archiveInFlight reports whether the archive behind archivingOptimisticID
	// is still actually outstanding (no archiveResultMsg seen yet). It gates
	// re-seeding a re-entered ChatPickerModel with archiving=true: on success
	// the override is deliberately retained for rendering until the row leaves
	// the list, but the archive is done, so pressing Enter on that stale row
	// must NOT open a picker stuck on "Archiving session..." waiting for a
	// result that will never arrive. Cleared whenever the archive resolves or
	// the override is dropped.
	archiveInFlight bool

	// Auth
	authMgr              *auth.Manager // nil means auth not configured
	loggedIn             bool
	loggedInEmail        string
	confirm              confirmPrompt
	cloudAccess          CloudAccessClient
	cloudStatus          *pb.CloudAccessStatus
	cloudErr             error
	cloudChecking        bool
	checkoutReturnURL    string
	checkoutCancelURL    string
	cloudCheckoutStatus  string
	cloudCheckoutPolling bool
	settings             config.Settings
	startedAt            time.Time
	now                  func() time.Time

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
	now := time.Now
	return HomeModel{
		client:          c,
		ctx:             ctx,
		authMgr:         authMgr,
		spinner:         newStatusSpinner(),
		loading:         true,
		table:           newBossTable(nil, nil, 0),
		upgradeChecking: true,
		settings:        config.DefaultSettings(),
		startedAt:       now(),
		now:             now,
	}
}

func (h *HomeModel) SetSettings(settings config.Settings) {
	h.settings = settings
}

func (h HomeModel) currentTime() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

// valueDelivered reports whether the user has received real value: a repo,
// a session, and a chat.
func valueDelivered(repoCount, sessionCount int, hasChat bool) bool {
	return repoCount > 0 && sessionCount > 0 && hasChat
}

// sessionsHaveChat reports whether any session has an active chat. has_active_chat
// (heartbeat-tracked) is the cleanest available "has started a chat" signal on the
// session list — no extra RPC.
func sessionsHaveChat(sessions []*pb.Session) bool {
	for _, s := range sessions {
		if s.GetHasActiveChat() {
			return true
		}
	}
	return false
}

// latchValueDeliveredIfNeeded sets the one-time BossCloudValueDeliveredAt milestone
// the first time the user has a repo + session + chat, and persists it. Set-once:
// the timestamp never moves, so the promo stays eligible even if the repo is later
// deleted. Persist failures are non-fatal — the latch retries on the next poll.
func (h *HomeModel) latchValueDeliveredIfNeeded() {
	if !valueDelivered(h.repoCount, len(h.sessions), sessionsHaveChat(h.sessions)) {
		return
	}
	updated, dirty := h.settings.EnsureBossCloudValueDeliveredAt(h.currentTime())
	if !dirty {
		return
	}
	if err := saveSettings(updated); err == nil {
		h.settings = updated
	}
	// On save error, leave h.settings unchanged so the latch retries next poll.
}

func (h *HomeModel) SetCloudAccessClient(c CloudAccessClient) {
	h.SetCloudSubscription(c, h.checkoutReturnURL, h.checkoutCancelURL)
}

func (h *HomeModel) SetCloudSubscription(c CloudAccessClient, returnURL, cancelURL string) {
	h.cloudAccess = c
	h.checkoutReturnURL = returnURL
	h.checkoutCancelURL = cancelURL
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

// sessionsContainID reports whether any session in the slice has the given id.
func sessionsContainID(sessions []*pb.Session, id string) bool {
	for _, s := range sessions {
		if s.Id == id {
			return true
		}
	}
	return false
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

// neighborSessionID returns the ID of the session that should receive the
// cursor once the session with removedID leaves the list (e.g. on archive). It
// prefers the next session down so the cursor stays at the same visual position
// after the list closes the gap; if the removed session was last, it falls back
// to the previous session. Returns "" when there is no suitable neighbor (the
// removed session was the only one, or is not in the current list).
func (h HomeModel) neighborSessionID(removedID string) string {
	idx := -1
	for i, sess := range h.sessions {
		if sess.Id == removedID {
			idx = i
			break
		}
	}
	if idx == -1 {
		return ""
	}
	if idx+1 < len(h.sessions) {
		return h.sessions[idx+1].Id
	}
	if idx-1 >= 0 {
		return h.sessions[idx-1].Id
	}
	return ""
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
	if h.archivingOptimisticID != "" && sess != nil && sess.Id == h.archivingOptimisticID {
		return renderArchivingStatus(h.spinner)
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
				// An active snooze for this version suppresses the upgrade
				// prompt regardless of the prior availability check.
				available = false
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

var runUpgradeCmd = func(executable string) tea.Cmd {
	return func() tea.Msg {
		cmd := exec.Command(executable, "upgrade", "--yes", "--no-restart")
		output, err := cmd.CombinedOutput()
		return upgradeRunMsg{output: string(output), err: err}
	}
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

// buildTableRows rebuilds the table columns and rows from h.sessions.
func (h *HomeModel) buildTableRows() {
	if len(h.sessions) == 0 {
		h.table.SetRows(nil)
		return
	}

	repos := make([]string, len(h.sessions))
	names := make([]string, len(h.sessions))       // plain text for width calc
	linkedNames := make([]string, len(h.sessions)) // may contain OSC 8 hyperlinks
	nameWidthLabels := make([]string, 0, len(h.sessions)*2)
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
		{Title: "PR", Width: maxColWidth("PR", prLabels, 8) + tableColumnSep},
		{Title: "STATUS", Width: 16 + tableColumnSep},
	}

	mutedStrike := lipgloss.NewStyle().Foreground(colorMuted).Strikethrough(true)

	cursor := h.table.Cursor()
	rows := make([]table.Row, 0, len(h.sessions)*2)
	for i, sess := range h.sessions {
		rowIndex := len(rows)
		statusStyled := h.renderSessionStatus(sess)

		attn := renderAttentionIndicator(sess)
		repo, name, pr := repos[i], linkedNames[i], prs[i]
		selected := rowIndex == cursor
		merged := sess.DisplayStatus == pb.DisplayStatus_DISPLAY_STATUS_MERGED ||
			sess.DisplayStatus == pb.DisplayStatus_DISPLAY_STATUS_CLOSED
		switch {
		case merged:
			repo = mutedStrike.Render(repos[i])
			// renderMutedTrackerLink styles the full title with raw ANSI and
			// wraps the tracker ID in OSC 8; do NOT wrap its output with
			// lipgloss — that strips the hyperlink envelope.
			name = renderMutedTrackerLink(sess, names[i])
			pr = renderMutedPRLink(sess)
		case selected:
			// The table applies the selected (blue) foreground to the whole
			// row, but a styled leading cell (the attention "!") emits a full
			// SGR reset that cancels it for the cells after it (repo/name/pr).
			// Re-assert the selection color on those cells with raw ANSI so it
			// survives regardless of the reset. See BOS-103.
			repo = renderSelectedText(repos[i])
			name = renderSelectedTrackerLink(sess, names[i])
			if sess.PrNumber != nil {
				pr = renderSelectedPRLink(sess)
			} else {
				pr = renderSelectedText(prs[i]) // the "-" placeholder, kept blue
			}
		}

		indicator := ""
		if rowIndex == cursor {
			indicator = cursorChevron
		}
		row := table.Row{indicator, attn, repo, name, pr, statusStyled}
		rows = append(rows, row)
		// A merged/closed row's warnings no longer need to alarm — dim them
		// (BOS-246). Part A clears the reason at the source, so this only paints
		// the brief window before the next poll reconciles the session.
		hintStyle := styleStatusDanger
		if merged {
			hintStyle = styleStatusDangerFaded
		}
		for _, hint := range sessionWarningHints(sess) {
			hintRow := table.Row{"", "", "", hintStyle.Render(hint), "", ""}
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
			h.latchValueDeliveredIfNeeded()
		}
		return h, nil

	case authStatusMsg:
		h.loggedIn = msg.loggedIn
		h.loggedInEmail = msg.email
		if !h.loggedIn {
			h.cloudStatus = nil
			h.cloudErr = nil
			h.cloudCheckoutStatus = ""
			h.cloudCheckoutPolling = false
			h.cloudChecking = false
			return h, nil
		}
		if h.cloudAccess != nil && h.cloudStatus == nil && !h.cloudChecking {
			h.cloudChecking = true
			return h, fetchCloudAccessStatus(h.cloudAccess, h.ctx)
		}
		return h, nil

	case cloudAccessMsg:
		h.cloudChecking = false
		h.cloudStatus = msg.status
		h.cloudErr = msg.err
		if subscriptionIsActive(msg.status) {
			h.cloudCheckoutPolling = false
			h.cloudCheckoutStatus = ""
		}
		if msg.status.GetState() == pb.CloudAccessState_CLOUD_ACCESS_STATE_BILLING_UNAVAILABLE {
			h.cloudCheckoutPolling = false
		}
		return h, nil

	case homeCloudCheckoutMsg:
		if msg.err != nil {
			if checkoutActivationPending(msg.err) {
				h.cloudStatus = &pb.CloudAccessStatus{
					State:   pb.CloudAccessState_CLOUD_ACCESS_STATE_PENDING_ENTITLEMENT_REFRESH,
					Message: "Your subscription is being activated.",
				}
				h.cloudErr = nil
				h.cloudCheckoutStatus = ""
				h.cloudCheckoutPolling = true
				h.cloudChecking = true
				return h, refreshCloudAccessStatus(h.cloudAccess, h.ctx)
			}
			if msg.url != "" {
				h.cloudCheckoutStatus = "Open this billing URL: " + msg.url
				h.cloudCheckoutPolling = true
				h.cloudChecking = true
				return h, refreshCloudAccessStatus(h.cloudAccess, h.ctx)
			} else {
				h.cloudCheckoutStatus = "Cloud checkout unavailable. Local sessions are still available."
				h.cloudCheckoutPolling = false
			}
			return h, nil
		}
		h.cloudCheckoutStatus = "Opened Bossanova Cloud subscription checkout."
		h.cloudCheckoutPolling = true
		h.cloudChecking = true
		return h, refreshCloudAccessStatus(h.cloudAccess, h.ctx)

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
			h.upgradeError = upgradeRunError(msg)
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
		if msg.err != nil {
			// Debounce transient poll failures: keep the last-good session list
			// on screen and only surface the "Cannot connect to daemon" view
			// after several consecutive failures. This stops the constant
			// flashing when the daemon is briefly unreachable (e.g. busy).
			h.pollFailures++
			if h.pollFailures >= pollFailureThreshold {
				h.err = msg.err
			}
			h.buildTableRows()
			return h, nil
		}
		// Successful poll: clear the failure streak and any error screen.
		h.pollFailures = 0
		h.err = nil
		h.sessions = msg.sessions
		h.latchValueDeliveredIfNeeded()
		h.daemonStatuses = msg.daemonStatuses
		h.applyMergedOptimisticOverride()
		if h.archivingOptimisticID != "" && !sessionsContainID(h.sessions, h.archivingOptimisticID) {
			h.archivingOptimisticID = ""
			h.archiveInFlight = false
		}
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
		if h.loggedIn && h.cloudAccess != nil && (cloudAccessPending(h.cloudStatus) || h.cloudCheckoutPolling) {
			h.cloudChecking = true
			cmds = append(cmds, refreshCloudAccessStatus(h.cloudAccess, h.ctx))
		}
		return h, tea.Batch(cmds...)

	case tea.KeyMsg:
		if h.confirm.active {
			confirmed := msg.String() == "y" || msg.String() == "enter"
			var cmd tea.Cmd
			h.confirm, cmd = h.confirm.update(msg)
			if confirmed && cmd != nil {
				h.loggedIn = false
				h.loggedInEmail = ""
			}
			return h, cmd
		}
		if h.restartPrompt {
			switch msg.String() {
			case "r", "enter":
				return h, restartDaemonCmd()
			case "esc":
				h.restartPrompt = false
				return h, nil
			}
		}
		if msg.String() == "u" && h.canOpenCloudSubscription() && !h.upgradeAvailable {
			return h, func() tea.Msg { return startSubscriptionFlowMsg{} }
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
			case "d":
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
		case "s":
			return h, func() tea.Msg { return switchViewMsg{view: ViewSettings} }
		case "l":
			if h.authMgr == nil {
				return h, nil
			}
			if h.loggedIn {
				authMgr := h.authMgr
				c := h.client
				ctx := h.ctx
				label := "Log out?"
				if h.loggedInEmail != "" {
					label = fmt.Sprintf("Log out %s?", h.loggedInEmail)
				}
				h.confirm = newConfirmPrompt(label, func() tea.Msg {
					if authMgr != nil {
						_ = authMgr.Logout()
					}
					_ = c.NotifyAuthChange(ctx, "logout")
					return nil
				})
				return h, nil
			}
			return h, func() tea.Msg { return switchViewMsg{view: ViewLogin} }
		case "enter":
			if h.repoCount == 0 {
				// New user with no repos: guide them into adding their first
				// repository. firstRepo=true makes the add wizard return to the
				// home empty state on cancel rather than the repo list.
				return h, func() tea.Msg { return switchViewMsg{view: ViewRepoAdd, firstRepo: true} }
			}
			if sess := h.selectedSession(); sess != nil {
				return h, func() tea.Msg {
					return switchViewMsg{view: ViewChatPicker, sessionID: sess.Id}
				}
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
		// Rebuild so the selection (blue) styling follows the new cursor; this
		// also re-runs updateCursorColumn internally to keep the chevron in
		// sync. updateCursorColumn alone would only move the chevron, leaving
		// repo/name/pr stale on error/attention rows (BOS-103).
		h.buildTableRows()
		return h, cmd
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
		lines = append(lines, lipgloss.NewStyle().Padding(0, 2).Foreground(colorWarning).Render("Upgrade installed. Quit boss after restart to use the new binary. [r]estart [esc] later"))
	case h.upgradeDone:
		lines = append(lines, lipgloss.NewStyle().Padding(0, 2).Render("Upgrade installed. Quit boss (q) and re-launch to use the new binary."))
	case h.upgradeAvailable:
		lines = append(lines, lipgloss.NewStyle().Padding(0, 2).Foreground(colorWarning).Render(fmt.Sprintf(
			"Upgrade available: boss %s -> %s. [u]pgrade [d]ismiss",
			h.upgradeCurrent,
			h.upgradeLatest,
		)))
	}
	return strings.Join(lines, "\n")
}

func upgradeRunError(msg upgradeRunMsg) string {
	output := strings.TrimSpace(msg.output)
	if output == "" {
		return msg.err.Error()
	}
	return msg.err.Error() + "\n" + output
}

func (h HomeModel) withUpgradeStatus(content string) string {
	status := h.upgradeStatusView()
	if status == "" {
		return content
	}
	if content == "" {
		return status
	}
	return content + "\n" + status
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
	case pb.SessionState_SESSION_STATE_ORPHANED:
		return "orphaned"
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

// cloudAction intentionally returns no action-bar entry. When the Cloud
// subscription is inactive, Home stays quiet: it shows a one-line reminder
// (see cloudGateLine) instead of a permanent [b]illing menu item. Re-entry to
// billing stays in the login/subscription flow.
func (h HomeModel) cloudAction() string {
	return ""
}

func (h HomeModel) canOpenCloudSubscription() bool {
	return h.loggedIn && h.cloudAccess != nil && (subscriptionNeedsCheckout(h.cloudStatus) || cloudAccessPending(h.cloudStatus))
}

func checkoutActivationPending(err error) bool {
	if err == nil || connect.CodeOf(err) != connect.CodeFailedPrecondition {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "being activated")
}

var cloudAccessSecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b((?:access_token|refresh_token|id_token|token|api[_-]?key|client[_-]?secret|secret|key)\s*=\s*)[^&\s,;]+`),
	regexp.MustCompile(`(?i)\b(authorization\s*:\s*bearer\s+)[^\s,;]+`),
}

// cloudBillingUnavailableLine is the user-facing copy shown when Bossanova
// Cloud reports its billing system as unavailable. It is shared between the
// home gate line and the subscription flow so the two cannot drift.
const cloudBillingUnavailableLine = "Cloud billing unavailable. Local sessions are still available."

// cloudAccessUnavailableFallbackLine is shown when the subscription flow reaches
// an unexpected, unrecoverable state with no specific error to report.
const cloudAccessUnavailableFallbackLine = "Bossanova Cloud is unavailable. Local sessions are still available."

func cloudAccessUnavailableLine(err error) string {
	return fmt.Sprintf(
		"Cloud access status unavailable: %s. Local sessions are still available.",
		cloudAccessErrorSummary(err),
	)
}

func cloudAccessErrorSummary(err error) string {
	if err == nil {
		return "unknown error"
	}
	if code := connect.CodeOf(err); code != connect.CodeUnknown {
		message := redactCloudAccessError(connectErrorMessage(err))
		return fmt.Sprintf("%s: %s", strings.ToLower(code.String()), message)
	}
	return redactCloudAccessError(err.Error())
}

func connectErrorMessage(err error) string {
	var connectErr *connect.Error
	if errors.As(err, &connectErr) {
		if message := connectErr.Message(); message != "" {
			return message
		}
	}
	return "unknown error"
}

func redactCloudAccessError(message string) string {
	for _, pattern := range cloudAccessSecretPatterns {
		message = pattern.ReplaceAllString(message, "$1[redacted]")
	}
	return message
}

func (h HomeModel) cloudGateLine() string {
	if !h.loggedIn || h.cloudAccess == nil {
		return ""
	}
	if h.cloudErr != nil {
		return lipgloss.NewStyle().Padding(0, 2).Foreground(colorWarning).Render(cloudAccessUnavailableLine(h.cloudErr))
	}
	if h.cloudStatus.GetState() == pb.CloudAccessState_CLOUD_ACCESS_STATE_BILLING_UNAVAILABLE {
		return lipgloss.NewStyle().Padding(0, 2).Foreground(colorWarning).Render(cloudBillingUnavailableLine)
	}
	if h.cloudChecking && h.cloudStatus == nil {
		return lipgloss.NewStyle().Padding(0, 2).Foreground(colorMuted).Render("Checking Bossanova Cloud access...")
	}
	switch h.cloudStatus.GetState() {
	case pb.CloudAccessState_CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION,
		pb.CloudAccessState_CLOUD_ACCESS_STATE_PAST_DUE,
		pb.CloudAccessState_CLOUD_ACCESS_STATE_CANCELED:
		if h.upgradeAvailable {
			return lipgloss.NewStyle().Padding(0, 2).Foreground(colorWarning).Render(
				"Bossanova Cloud requires a subscription. Local sessions are still available. Upgrade boss first, then subscribe.",
			)
		}
		return lipgloss.NewStyle().Padding(0, 2).Foreground(colorWarning).Render(
			"Bossanova Cloud requires a subscription. Local sessions are still available. Type u to s[u]bscribe.",
		)
	case pb.CloudAccessState_CLOUD_ACCESS_STATE_PENDING_ENTITLEMENT_REFRESH:
		if h.upgradeAvailable {
			return lipgloss.NewStyle().Padding(0, 2).Foreground(colorWarning).Render(
				"Bossanova Cloud setup has not completed yet. Upgrade boss first, then subscribe.",
			)
		}
		return lipgloss.NewStyle().Padding(0, 2).Foreground(colorWarning).Render(
			"Bossanova Cloud setup has not completed yet. Type u to s[u]bscribe.",
		)
	default:
		return ""
	}
}

func (h HomeModel) cloudCheckoutStatusLine() string {
	if !h.loggedIn || h.cloudAccess == nil || h.cloudCheckoutStatus == "" {
		return ""
	}
	return lipgloss.NewStyle().Padding(0, 2).Foreground(colorSuccess).Render(h.cloudCheckoutStatus)
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
		discovery := ""
		if cloudGuestOfferVisible(h.settings, h.currentTime(), h.startedAt, h.loggedIn, h.authMgr != nil) {
			discovery = cloudDiscoveryLine(h.loggedIn, h.authMgr != nil)
		}
		if h.repoCount == 0 {
			// No repos configured - guide the new user straight to adding their
			// first repository (the primary action), rather than sending them to
			// Settings. The [s]ettings key still works as a hidden escape hatch.
			nav := []string{"[enter] add repository"}
			// Suppress the [l]ogout action while the logout confirmation is
			// showing so the empty state doesn't display the control and its
			// confirmation prompt at once (mirrors the session-list view).
			if la := h.loginAction(); la != "" && !h.confirm.active {
				nav = append(nav, la)
			}
			if ca := h.cloudAction(); ca != "" {
				nav = append(nav, ca)
			}
			content = lipgloss.NewStyle().Padding(0, 2).Render(
				"Welcome to Bossanova!\n\n"+
					"To get started, you need to add a repository for us to work on together.\n\n"+
					lipgloss.NewStyle().Bold(true).Render("Press Enter to add your first repository"),
			) + "\n" +
				actionBar(nav, []string{"[q]uit"})
		} else {
			// Repos exist but no sessions - show simplified guidance
			left := []string{"[n]ew session"}
			nav := []string{"[s]ettings"}
			// Suppress the [l]ogout action while the logout confirmation is
			// showing so the empty state doesn't display the control and its
			// confirmation prompt at once (mirrors the session-list view).
			if la := h.loginAction(); la != "" && !h.confirm.active {
				nav = append(nav, la)
			}
			if ca := h.cloudAction(); ca != "" {
				nav = append(nav, ca)
			}
			content = lipgloss.NewStyle().Padding(0, 2).Render(
				"You have no active sessions.\n\n"+
					lipgloss.NewStyle().Bold(true).Render("Press 'n' to create a new session."),
			) + "\n" +
				actionBar(left, nav, []string{"[q]uit"})
		}
		if discovery != "" {
			content += discovery
		}
		if !h.confirm.active {
			if upgradeStatus := h.upgradeStatusView(); upgradeStatus != "" {
				content += "\n" + upgradeStatus
			}
		}
		if gate := h.cloudGateLine(); gate != "" {
			content += "\n" + gate
		}
		if checkoutStatus := h.cloudCheckoutStatusLine(); checkoutStatus != "" {
			content += "\n" + checkoutStatus
		}
		if h.confirm.active {
			content += "\n" + h.logoutConfirmationView()
			if upgradeStatus := h.upgradeStatusView(); upgradeStatus != "" {
				content += "\n" + upgradeStatus
			}
		}
		return tea.NewView(content)
	}

	var b strings.Builder

	if len(h.sessions) > 0 {
		b.WriteString(lipgloss.NewStyle().Padding(0, 1).Render(h.table.View()))
		b.WriteString("\n")
	}
	if h.status != "" {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorSuccess).Render(h.status))
		b.WriteString("\n")
	}

	if h.confirm.active {
		b.WriteString("\n")
		b.WriteString(h.logoutConfirmationView())
	} else {
		left := []string{"[n]ew session", "[enter] select"}
		nav := []string{"[s]ettings"}
		if la := h.loginAction(); la != "" {
			nav = append(nav, la)
		}
		if ca := h.cloudAction(); ca != "" {
			nav = append(nav, ca)
		}
		b.WriteString(actionBar(
			left,
			nav,
			[]string{"[q]uit"},
		))
		if cloudGuestOfferVisible(h.settings, h.currentTime(), h.startedAt, h.loggedIn, h.authMgr != nil) {
			if discovery := cloudDiscoveryLine(h.loggedIn, h.authMgr != nil); discovery != "" {
				b.WriteString(discovery)
			}
		}
		if upgradeStatus := h.upgradeStatusView(); upgradeStatus != "" {
			b.WriteString("\n")
			b.WriteString(upgradeStatus)
		}
		if gate := h.cloudGateLine(); gate != "" {
			b.WriteString("\n")
			b.WriteString(gate)
		}
		if checkoutStatus := h.cloudCheckoutStatusLine(); checkoutStatus != "" {
			b.WriteString("\n")
			b.WriteString(checkoutStatus)
		}
	}
	if h.confirm.active {
		if upgradeStatus := h.upgradeStatusView(); upgradeStatus != "" {
			b.WriteString("\n")
			b.WriteString(upgradeStatus)
		}
	}

	return tea.NewView(b.String())
}

func (h HomeModel) logoutConfirmationView() string {
	return lipgloss.NewStyle().Padding(0, 2).Foreground(colorDanger).Render(h.confirm.prompt) +
		"\n" +
		styleActionBar.Render("[y/enter] confirm  [n/esc] cancel")
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

		return sessionListMsg{
			sessions:       sessions,
			daemonStatuses: daemonStatuses,
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

func fetchCloudAccessStatus(c CloudAccessClient, ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		status, err := c.GetCloudAccessStatus(ctx)
		return cloudAccessMsg{status: status, err: err}
	}
}

func refreshCloudAccessStatus(c CloudAccessClient, ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		status, err := c.RefreshCloudEntitlements(ctx)
		return cloudAccessMsg{status: status, err: err}
	}
}

func cloudAccessPending(status *pb.CloudAccessStatus) bool {
	return status.GetState() == pb.CloudAccessState_CLOUD_ACCESS_STATE_PENDING_ENTITLEMENT_REFRESH
}
