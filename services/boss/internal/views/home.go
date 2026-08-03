package views

import (
	"context"
	"sync/atomic"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/boss/internal/client"
	"github.com/recurser/boss/internal/upgrade"
	"github.com/recurser/bossalib/config"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

const (
	pollInterval        = 2 * time.Second
	upgradeCheckTimeout = 5 * time.Second
	upgradeCacheTTL     = upgrade.CacheTTL

	// pollFailureThreshold is how many consecutive failed session polls must
	// occur before the TUI shows the "Cannot connect to daemon" screen. At a 2s
	// poll interval this tolerates ~6s of transient unreachability (e.g. the
	// daemon momentarily busy) without flashing the error view.
	pollFailureThreshold = 3
)

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
	// nextSessionPollID identifies outgoing session-list requests. Results from
	// an older request are ignored once a newer result has been applied, so a
	// slow poll cannot reset question-edge notification state.
	nextSessionPollID   uint64
	latestSessionPollID uint64
	generation          uint64

	// Navigation
	highlightSessionID string // session to auto-highlight after returning from chat picker

	// focused tracks whether boss's terminal currently has focus, updated from
	// tea.FocusMsg/BlurMsg. Assumed true at startup (the user just launched it).
	focused bool
	// pendingAttentionSessionID is the session boss most recently notified about
	// while unfocused. When the terminal regains focus (e.g. the user clicked the
	// OS notification), Home jumps into this session's chat if it is still
	// waiting, then clears it. Empty when there is nothing pending. See BOS-459.
	pendingAttentionSessionID string

	// mergedOptimisticID is set when the user returns from a successful merge
	// in the chat picker. While set, the matching session's DisplayStatus
	// is rendered as MERGED even if the daemon still reports PASSING — the
	// PR-merged webhook lands asynchronously, so without this override the
	// status column would flicker back to "passing" until the next poll.
	// Cleared once the daemon reports a terminal state (MERGED/CLOSED).
	mergedOptimisticID string

	// archivingOverrideIDs contains sessions whose status is optimistically
	// rendered as "archiving". Successful archives remain here until their row
	// leaves a later session poll; failed archives are removed immediately.
	archivingOverrideIDs map[string]struct{}

	// archiveInFlightIDs is the subset of archivingOverrideIDs whose archive RPC
	// has not resolved. It only re-seeds a re-entered ChatPickerModel while the
	// RPC is still active, avoiding a picker stuck waiting on a resolved archive.
	archiveInFlightIDs map[string]struct{}

	// Auth
	authMgr       *auth.Manager // nil means auth not configured
	loggedIn      bool
	loggedInEmail string
	// needsRelogin / reloginReason mirror the auth poll's retained
	// re-login state (BOS-659): credentials are still stored but cannot be
	// used. Both are cleared by the next clean poll, so a successful login
	// removes the warning without any extra bookkeeping.
	needsRelogin         bool
	reloginReason        string
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
	upgradeAvailable  bool
	upgradeCurrent    string
	upgradeLatest     string
	upgradeURL        string
	upgradeChecking   bool
	upgradeError      string
	upgradeDone       bool
	restartPrompt     bool
	upgrading         bool
	restarting        bool
	daemonRemediation string

	// logoutError holds the rendered text of a failed logout. Cleared by the
	// next successful one. Never carries token material: Manager.Logout's
	// errors wrap the credential-lock and keychain errors only.
	logoutError string
}

// handleLogout records the outcome of the confirmed logout so a failure is
// visible. The credential record is mutated behind a cross-process lock the
// daemon can be holding, so this really does fail; a discarded error left the
// board apparently unchanged and still signed in.
func (h HomeModel) handleLogout(msg logoutMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		h.logoutError = msg.err.Error()
		return h, nil
	}
	h.logoutError = ""
	return h, nil
}

// NewHomeModel creates a HomeModel wired to the daemon client.
func NewHomeModel(c client.BossClient, ctx context.Context, authMgr *auth.Manager) HomeModel {
	now := time.Now
	return HomeModel{
		client:               c,
		ctx:                  ctx,
		authMgr:              authMgr,
		spinner:              newStatusSpinner(),
		loading:              true,
		focused:              true,
		nextSessionPollID:    1,
		generation:           atomic.AddUint64(&homeGenerationSequence, 1),
		table:                newBossTable(nil, nil, 0),
		upgradeChecking:      true,
		settings:             config.DefaultSettings(),
		startedAt:            now(),
		now:                  now,
		archivingOverrideIDs: make(map[string]struct{}),
		archiveInFlightIDs:   make(map[string]struct{}),
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

// DaemonStatuses returns the per-session daemon heartbeat statuses, keyed by
// session ID. Used by the top-level App to attach diagnostic context to a
// bug report.
func (h HomeModel) DaemonStatuses() map[string]string { return h.daemonStatuses }

func (h HomeModel) Init() tea.Cmd {
	cmds := []tea.Cmd{fetchSessions(h.client, h.ctx, h.generation, h.nextSessionPollID), fetchRepoCount(h.client, h.ctx), tickCmd(), h.spinner.Tick, checkUpgradeCmd(h.ctx)}
	if h.authMgr != nil {
		cmds = append(cmds, fetchAuthStatus(h.authMgr))
	}
	return tea.Batch(cmds...)
}

// Update dispatches each message kind to its handler. Every handler keeps the
// value receiver Update itself has: it mutates its own copy of the model and
// returns it, exactly as the inline switch arms did. Helpers that must mutate
// through to the returned model (e.g. notifyNewQuestions and restoreTableCursor,
// reached below handleSessionList) take a pointer receiver and are called on the
// addressable local copy, which is the same aliasing the inline code had.
func (h HomeModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		return h.handleWindowSize(msg)
	case tea.FocusMsg:
		return h.handleFocus()
	case tea.BlurMsg:
		return h.handleBlur()
	case repoCountMsg:
		return h.handleRepoCount(msg)
	case authStatusMsg:
		return h.handleAuthStatus(msg)
	case cloudAccessMsg:
		return h.handleCloudAccess(msg)
	case homeCloudCheckoutMsg:
		return h.handleCloudCheckout(msg)
	case upgradeCheckMsg:
		return h.handleUpgradeCheck(msg)
	case upgradeRunMsg:
		return h.handleUpgradeRun(msg)
	case daemonRestartMsg:
		return h.handleDaemonRestart(msg)
	case logoutMsg:
		return h.handleLogout(msg)
	case sessionListMsg:
		return h.handleSessionList(msg)
	case spinner.TickMsg:
		return h.handleSpinnerTick(msg)
	case tickMsg:
		return h.handleTick()
	case tea.KeyMsg:
		return h.handleKey(msg)
	}

	return h, nil
}

func (h HomeModel) handleWindowSize(msg tea.WindowSizeMsg) (tea.Model, tea.Cmd) {
	h.width = msg.Width
	h.height = msg.Height
	h.table.SetHeight(h.tableHeight())
	h.table.SetWidth(msg.Width)
	// Re-fit the columns to the new width now rather than waiting for the next
	// session poll, which is up to a poll interval away — a resize that is not
	// reflected until then leaves the board laid out for the old terminal.
	// buildTableRows overwrites the height and width set above with the fitted
	// values; the two lines stay because it returns early in the empty state,
	// where they are the only sizing that happens (BOS-572).
	h.buildTableRows()
	return h, nil
}

func (h HomeModel) handleFocus() (tea.Model, tea.Cmd) {
	h.focused = true
	// The terminal just regained focus — likely because the user clicked the
	// OS notification for a waiting chat. Open that session's chat if it is
	// still waiting, then clear the pending marker so we only jump once.
	target := h.pendingAttentionSessionID
	h.pendingAttentionSessionID = ""
	if target != "" {
		if sess := sessionByID(h.sessions, target); sessionNeedsAttention(sess) {
			return h, func() tea.Msg {
				return switchViewMsg{view: ViewChatPicker, sessionID: target}
			}
		}
	}
	return h, nil
}

func (h HomeModel) handleBlur() (tea.Model, tea.Cmd) {
	h.focused = false
	return h, nil
}

func (h HomeModel) handleRepoCount(msg repoCountMsg) (tea.Model, tea.Cmd) {
	if msg.err == nil {
		h.repoCount = msg.count
		h.latchValueDeliveredIfNeeded()
	}
	return h, nil
}

func (h HomeModel) handleAuthStatus(msg authStatusMsg) (tea.Model, tea.Cmd) {
	h.loggedIn = msg.loggedIn
	h.loggedInEmail = msg.email
	h.needsRelogin = msg.needsRelogin
	h.reloginReason = msg.reloginReason
	// Auth state can add or remove the re-login warning below the table. The
	// table rows are unchanged, so recalculate its cached height here rather
	// than waiting for the next session poll.
	h.table.SetHeight(h.tableHeight())
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
}

func (h HomeModel) handleSpinnerTick(msg spinner.TickMsg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	h.spinner, cmd = h.spinner.Update(msg)
	// Rebuild rows to animate spinner frames.
	if len(h.sessions) > 0 {
		h.buildTableRows()
	}
	return h, cmd
}

func (h HomeModel) handleTick() (tea.Model, tea.Cmd) {
	// Re-poll auth status alongside sessions so the menu label
	// (e.g. [l]ogout vs [l]ogin) and the re-login warning catch up
	// after the daemon flags the stored credentials on a terminal
	// refresh failure. Without this, the TUI keeps showing the
	// pre-expiry label until the user navigates away and back.
	h.nextSessionPollID++
	cmds := []tea.Cmd{fetchSessions(h.client, h.ctx, h.generation, h.nextSessionPollID), tickCmd()}
	if h.authMgr != nil {
		cmds = append(cmds, fetchAuthStatus(h.authMgr))
	}
	if h.loggedIn && h.cloudAccess != nil && (cloudAccessPending(h.cloudStatus) || h.cloudCheckoutPolling) {
		h.cloudChecking = true
		cmds = append(cmds, refreshCloudAccessStatus(h.cloudAccess, h.ctx))
	}
	return h, tea.Batch(cmds...)
}

// View selects one of Home's four screens. Only the loading branch is wrapped
// in withUpgradeStatus — the daemon-error screen deliberately shows nothing
// else, and the empty-state and session-table renderers place the upgrade
// status themselves, at a position that depends on the rest of their content.
func (h HomeModel) View() tea.View {
	if h.err != nil {
		return tea.NewView(h.renderDaemonError())
	}

	if h.loading {
		return tea.NewView(h.withUpgradeStatus(h.renderLoading()))
	}

	if len(h.sessions) == 0 {
		return tea.NewView(h.renderEmptyState())
	}

	return tea.NewView(h.renderSessionTable())
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
		return authStatusMsg{
			loggedIn:      status.LoggedIn,
			email:         status.Email,
			needsRelogin:  status.NeedsRelogin,
			reloginReason: status.ReloginReason,
		}
	}
}
