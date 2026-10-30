package home

import (
	"context"
	"sync/atomic"
	"time"

	"charm.land/bubbles/v2/spinner"
	"github.com/recurser/boss/api/tui/ui"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/auth"
	upgrade "github.com/recurser/boss/internal/homeprocess"
	"github.com/recurser/bossalib/kit/config"

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

// Model is the main dashboard view showing active sessions.
type Model struct {
	Client            Client
	runtime           Runtime
	Ctx               context.Context
	Spinner           spinner.Model
	Sessions          []*pb.Session
	HeartbeatStatuses map[string]string // session_id → status string from daemon heartbeats
	// daemonWaitingReasons holds session_id → the daemon's reason for a
	// waiting aggregate status, e.g. "awaiting checks_passed_ready on
	// acme/widget#123" (SessionStatusEntry.waiting_reason, BOS-668). Kept
	// alongside daemonStatuses rather than on the session because the Session
	// proto carries only the composite label, not why. Empty/absent for every
	// session that is not parked.
	DaemonWaitingReasons map[string]string
	// daemonPhases carries the skill-reported working phase from status polling.
	DaemonPhases map[string]string
	// sessionReadFailures holds the organizations whose sessions the last
	// successful poll could not read (BOS-1151). A cloud session read fans out
	// across every organization the caller belongs to and returns what it could
	// get, so this is what makes the resulting list's incompleteness visible
	// instead of silently showing a short list. Always empty against a local
	// daemon, which reads one place and either answers or fails outright.
	SessionReadFailures []*pb.OrganizationSessionReadFailure
	Table               ui.Table
	layout              *homeTableLayout
	selectedRow         int
	warningCells        map[int]string
	Err                 error
	// status is the line under the board. Write it only through setStatus:
	// statusKind decides its colour and how long it stays, and statusSeq stops
	// an older outcome's timer from clearing it.
	Status     string
	StatusKind homeStatusKind
	statusSeq  uint64
	Loading    bool
	Width      int
	Height     int
	RepoCount  int // number of registered repos (for empty state guidance)

	// pollFailures counts consecutive failed session polls. The "Cannot
	// connect to daemon" screen is only shown once it reaches
	// pollFailureThreshold, so a single transient blip (e.g. the daemon briefly
	// busy) keeps the last-good session list on screen instead of flashing the
	// error view every 2s. Reset to 0 on any successful poll.
	PollFailures int
	// nextSessionPollID identifies outgoing session-list requests. Results from
	// an older request are ignored once a newer result has been applied, so a
	// slow poll cannot reset question-edge notification state.
	NextSessionPollID   uint64
	latestSessionPollID uint64
	Generation          uint64

	// Navigation
	HighlightSessionID string // session to auto-highlight after returning from chat picker

	// focused tracks whether boss's terminal currently has focus, updated from
	// tea.FocusMsg/BlurMsg. Assumed true at startup (the user just launched it).
	Focused bool
	// pendingAttentionSessionID is the session boss most recently notified about
	// while unfocused. When the terminal regains focus (e.g. the user clicked the
	// OS notification), Home jumps into this session's chat if it is still
	// waiting, then clears it. Empty when there is nothing pending. See BOS-459.
	PendingAttentionSessionID string
	// shownParks is the verify park each session's row last showed, by session
	// id (rememberShownParks). It lets a cleared fix alert that reveals an
	// already-shown park stay silent.
	ShownParks map[string]string
	// toldAttention is the last Needs you attention each session's row showed,
	// by session id (rememberToldAttention); nil until the first poll. It keeps
	// an attention the operator already knows about from being announced again.
	ToldAttention map[string]string

	// mergedOptimisticID is set when the user returns from a successful merge
	// in the chat picker. While set, the matching session's DisplayStatus
	// is rendered as MERGED even if the daemon still reports PASSING — the
	// PR-merged webhook lands asynchronously, so without this override the
	// status column would flicker back to "passing" until the next poll.
	// Cleared once the daemon reports a terminal state (MERGED/CLOSED).
	MergedOptimisticID string

	// archivingOverrideIDs contains sessions whose status is optimistically
	// rendered as "archiving". Successful archives remain here until their row
	// leaves a later session poll; failed archives are removed immediately.
	ArchivingOverrideIDs map[string]struct{}

	// archiveInFlightIDs is the subset of archivingOverrideIDs whose archive RPC
	// has not resolved. It only re-seeds a re-entered chatpicker.Model while the
	// RPC is still active, avoiding a picker stuck waiting on a resolved archive.
	ArchiveInFlightIDs map[string]struct{}

	// mergesInFlight holds the confirmed [m]erges that run on without the
	// operator, by session id. Their rows read "merging" until they settle.
	MergesInFlight map[string]ui.AttentionActionState

	// statusUnseen marks a status set while Home was off screen, so the
	// rebuilt Home the operator returns to still shows it.
	StatusUnseen bool

	// moveOverrideOrder is the session-id order the alt+up / alt+down chords
	// imposed locally (BOS-1231), held so the list responds to the keystroke at
	// once instead of at the next poll tick. The poll reassigns h.sessions
	// wholesale from the daemon, so without this the row would snap back for up
	// to a poll interval and then jump again when the write landed.
	//
	// Modelled on archivingOverrideIDs: an override kept only until the
	// server's own answer supersedes it. applyMoveOverride drops it on the
	// FIRST poll that arrives with no move work outstanding, whatever order
	// that poll carries — the daemon's move is multi-position in general (see
	// db.ComputeListRankMove), so waiting for the daemon to agree with this
	// adjacent swap would mean never dropping it. It permutes only the ids it
	// names, so a session the daemon added since keeps the position the daemon
	// gave it.
	MoveOverrideOrder []string

	// moveInFlight counts unresolved MoveSession RPCs. While it is non-zero a
	// poll that still carries the pre-move order must not be allowed to clear
	// moveOverrideOrder, or the row flashes back to where it started between
	// the keystroke and the write landing. Requests are serialized, so it only
	// ever holds 0 or 1; it stays a count so an extra reply cannot drive it
	// negative.
	MoveInFlight int

	// movePending holds the chords pressed while a MoveSession RPC was already
	// outstanding, in keypress order. Each press would otherwise return its own
	// tea.Cmd, and bubbletea runs commands concurrently: two fast chords could
	// reach the daemon in either order, making the first a boundary no-op and
	// leaving a persisted order that contradicts what was typed. The board
	// still moves on every press — only the RPC waits.
	MovePending []QueuedMove

	// Auth
	authMgr       *auth.Manager // nil means auth not configured
	AuthChanges   *AuthChangeQueue
	LoggedIn      bool
	LoggedInEmail string
	// authStatusGeneration invalidates a poll that started before a successful
	// logout, so it cannot restore the deleted credential's presentation.
	AuthStatusGeneration uint64
	// needsRelogin / reloginReason mirror the auth poll's retained
	// re-login state (BOS-659): credentials are still stored but cannot be
	// used. Both are cleared by the next clean poll, so a successful login
	// removes the warning without any extra bookkeeping.
	NeedsRelogin    bool
	ReloginReason   string
	AttentionAction ui.AttentionActionState
	Confirm         ui.ConfirmPrompt
	// rename is the inline title editor opened by the hidden [r] shortcut
	// (BOS-837). Like confirm it stands in for the action bar while active and
	// swallows the keys the board would otherwise act on.
	Rename               ui.RenamePrompt
	Filter               ui.ListFilter
	CloudAccess          CloudAccessClient
	CloudStatus          *pb.CloudAccessStatus
	cloudErr             error
	cloudChecking        bool
	checkoutReturnURL    string
	checkoutCancelURL    string
	cloudCheckoutStatus  string
	cloudCheckoutPolling bool
	Settings             config.Settings
	StartedAt            time.Time
	lastSessionPollAt    time.Time
	Now                  func() time.Time

	// Upgrade banner / restart prompt
	upgradeAvailable  bool
	upgradeCurrent    string
	upgradeLatest     string
	upgradeURL        string
	upgradeChecking   bool
	upgradeError      string
	upgradeDone       bool
	restartPrompt     bool
	Upgrading         bool
	Restarting        bool
	daemonRemediation string

	// Daemon supervision health line (BOS-1520). supervisionProbe is nil when
	// the TUI is not driving this machine's daemon. supervision is the last
	// result; supervisionProbing/supervisionProbedAt throttle the tick's
	// re-probe, and supervisionProbeID discards a superseded result.
	supervisionProbe    DaemonSupervisionProbe
	Supervision         DaemonSupervision
	SupervisionSince    time.Time
	SupervisionSincePID int
	SupervisionProbing  bool
	supervisionProbedAt time.Time
	SupervisionProbeID  uint64

	// Ticket notices (home_ticket_notices.go): merged PRs whose ticket bossd
	// could not move. ticketNoticesDismissed hides a dismissed note even from a
	// read that was in flight when it was dismissed.
	ticketNotices          []*pb.Note
	ticketNoticesDismissed map[string]struct{}
	ticketNoticesFetching  bool
	ticketNoticesFetchedAt time.Time

	// logoutError holds the rendered text of a failed logout. Cleared by the
	// next successful one. Never carries token material: Manager.Logout's
	// errors wrap the credential-lock and keychain errors only.
	LogoutError string
	// logoutPending prevents a second confirmation from starting another
	// keychain delete before the first confirmed logout reports its result.
	LogoutPending bool
}

// handleLogout records the outcome of the confirmed logout so a failure is
// visible. The credential record is mutated behind a cross-process lock the
// daemon can be holding, so this really does fail; a discarded error left the
// board apparently unchanged and still signed in.
func (h Model) handleLogout(msg LogoutMsg) (tea.Model, tea.Cmd) {
	h.LogoutPending = false
	if msg.Err != nil {
		h.LogoutError = msg.Err.Error()
		// A failed logout adds a status block below the session table. Refresh
		// the cached table height immediately instead of waiting for a poll.
		h.Table.SetHeight(h.TableHeight())
		return h, nil
	}
	h.LogoutError = ""
	h.LoggedIn = false
	h.LoggedInEmail = ""
	h.NeedsRelogin = false
	h.ReloginReason = ""
	h.AuthStatusGeneration++
	// The successful result removes the error block and changes the action bar.
	// Keep the cached table height in sync with the new footer immediately.
	h.Table.SetHeight(h.TableHeight())
	return h, msg.notify
}

// NewModel creates a Model wired to the daemon client.
func NewModel(c Client, ctx context.Context, authMgr *auth.Manager, services ...Runtime) Model {
	now := time.Now
	startedAt := now()
	var runtime Runtime
	if len(services) > 0 && services[0] != nil {
		runtime = services[0]
	}
	return Model{
		runtime:              runtime,
		Client:               c,
		Ctx:                  ctx,
		authMgr:              authMgr,
		Spinner:              ui.NewStatusSpinner(),
		Filter:               ui.NewListFilter(),
		Loading:              true,
		Focused:              true,
		NextSessionPollID:    1,
		Generation:           atomic.AddUint64(&homeGenerationSequence, 1),
		Table:                ui.NewBossTable(nil, nil, 0),
		upgradeChecking:      true,
		Settings:             config.DefaultSettings(),
		StartedAt:            startedAt,
		lastSessionPollAt:    startedAt,
		Now:                  now,
		ArchivingOverrideIDs: make(map[string]struct{}),
		ArchiveInFlightIDs:   make(map[string]struct{}),
		// Init, which every path that builds a Home calls next, issues the
		// first read; the tick must not start a second one alongside it.
		ticketNoticesFetching:  c != nil,
		ticketNoticesFetchedAt: now(),
	}
}

func (h *Model) SetSettings(settings config.Settings) {
	h.Settings = settings
}

func (h Model) currentTime() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// DaemonStatuses returns the per-session daemon heartbeat statuses, keyed by
// session ID. Used by the top-level App to attach diagnostic context to a
// bug report.
func (h Model) DaemonStatuses() map[string]string { return h.HeartbeatStatuses }

// sessionWaitingReason returns the daemon-supplied waiting reason for a
// session, or "" when it is not parked on an external event. The single
// accessor every auxiliary-row helper goes through, so the row that
// buildTableRows emits and the count sessionSubRowCount reports cannot disagree
// (BOS-474's stranded-cursor failure mode).
func (h Model) sessionWaitingReason(sess *pb.Session) string {
	if sess == nil {
		return ""
	}
	return h.DaemonWaitingReasons[sess.GetId()]
}

// hasEndpointRow applies the port display preference to the endpoint predicate.
func (h Model) hasEndpointRow(sess *pb.Session) bool {
	return h.Settings.DisplayOpenPorts && ui.SessionHasEndpointRow(sess)
}

// subRowCount binds auxiliary row accounting to this model's settings and waiting reasons.
func (h Model) SubRowCount(sess *pb.Session) int {
	return ui.SessionSubRowCount(sess, h.sessionWaitingReason(sess), h.Settings.DisplayOpenPorts)
}

func (h Model) Init() tea.Cmd {
	cmds := []tea.Cmd{FetchSessions(h.Client, h.Ctx, h.Generation, h.NextSessionPollID, h.Settings.DisplayOpenPorts), fetchRepoCount(h.Client, h.Ctx), TickCmd(), h.Spinner.Tick, checkUpgradeCmd(h.Ctx, h.runtime)}
	if h.authMgr != nil {
		cmds = append(cmds, fetchAuthStatus(h.authMgr, h.AuthStatusGeneration))
	}
	if probe := h.DaemonSupervisionProbeCmd(); probe != nil {
		cmds = append(cmds, probe)
	}
	if notices := fetchTicketNotices(h.Client, h.Ctx, h.Generation); notices != nil {
		cmds = append(cmds, notices)
	}
	if expire := h.statusExpiry(); expire != nil {
		cmds = append(cmds, expire)
	}
	return tea.Batch(cmds...)
}

// Update dispatches each message kind to its handler. Every handler keeps the
// value receiver Update itself has: it mutates its own copy of the model and
// returns it, exactly as the inline switch arms did. Helpers that must mutate
// through to the returned model (e.g. notifyNewQuestions and restoreTableCursor,
// reached below handleSessionList) take a pointer receiver and are called on the
// addressable local copy, which is the same aliasing the inline code had.
func (h Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		return h.HandleWindowSize(msg)
	case tea.FocusMsg:
		return h.handleFocus()
	case tea.BlurMsg:
		return h.handleBlur()
	case RepoCountMsg:
		return h.handleRepoCount(msg)
	case AuthStatusMsg:
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
	case ticketNoticesMsg:
		return h.handleTicketNotices(msg)
	case ticketNoticesDismissedMsg:
		return h.handleTicketNoticesDismissed(msg)
	case DaemonSupervisionMsg:
		return h.handleDaemonSupervision(msg)
	case LogoutMsg:
		return h.handleLogout(msg)
	case SessionListMsg:
		return h.HandleSessionList(msg)
	case spinner.TickMsg:
		return h.handleSpinnerTick(msg)
	case TickMsg:
		return h.handleTick()
	case ui.AttentionActionResultMsg:
		if h.AttentionAction.SessionID == msg.SessionID && h.AttentionAction.Action == msg.Action {
			h.AttentionAction = ui.AttentionActionState{}
		}
		expire := h.ShowActionResult(msg)
		h.Table.SetHeight(h.TableHeight())
		if msg.Action == pb.AttentionAction_ATTENTION_ACTION_MERGE && msg.Err == nil {
			// Re-read now so the merged row leaves NEEDS YOU without waiting
			// for the next poll.
			h.NextSessionPollID++
			return h, tea.Batch(expire, FetchSessions(h.Client, h.Ctx, h.Generation, h.NextSessionPollID, h.Settings.DisplayOpenPorts))
		}
		return h, expire
	case StatusExpiredMsg:
		return h.handleStatusExpired(msg)
	case SessionRenamedMsg:
		return h.handleSessionRenamed(msg)
	case sessionMovedMsg:
		return h.handleSessionMoved(msg)
	// Bracketed paste is not a KeyMsg, so it must be forwarded explicitly or a
	// pasted title never reaches the rename input. Placed ahead of the KeyMsg
	// arm because a tea.PasteMsg would otherwise fall through to the default
	// return and be dropped. Inert unless the rename prompt is open.
	case tea.PasteMsg:
		if !h.Rename.Active() {
			if h.Filter.Active() {
				return h.updateFilter(msg)
			}
			return h, nil
		}
		var cmd tea.Cmd
		h.Rename, cmd = h.Rename.Update(msg)
		return h, cmd
	case tea.KeyMsg:
		return h.HandleKey(msg)
	}

	return h, nil
}

func (h Model) HandleWindowSize(msg tea.WindowSizeMsg) (tea.Model, tea.Cmd) {
	h.Width = msg.Width
	h.Height = msg.Height
	h.Table.SetHeight(h.TableHeight())
	h.Table.SetWidth(msg.Width)
	// Re-fit the columns to the new width now rather than waiting for the next
	// session poll, which is up to a poll interval away — a resize that is not
	// reflected until then leaves the board laid out for the old terminal.
	// buildTableRows overwrites the height and width set above with the fitted
	// values; the two lines stay because it returns early in the empty state,
	// where they are the only sizing that happens (BOS-572).
	h.BuildTableRows()
	return h, nil
}

func (h Model) handleFocus() (tea.Model, tea.Cmd) {
	if h.Focused {
		return h, nil
	}
	h.Focused = true
	// The terminal just regained focus — likely because the user clicked the
	// OS notification for a waiting chat. Open that session's chat if it is
	// still waiting, then clear the pending marker so we only jump once.
	target := h.PendingAttentionSessionID
	h.PendingAttentionSessionID = ""
	if target != "" {
		if sess := sessionByID(h.Sessions, target); sessionNeedsAttention(sess) {
			return h, func() tea.Msg {
				return ui.SwitchViewMsg{View: ui.ViewChatPicker, SessionID: target}
			}
		}
	}
	cmds := append(h.pollCmds(), h.Spinner.Tick)
	return h, tea.Batch(cmds...)
}

func (h Model) handleBlur() (tea.Model, tea.Cmd) {
	h.Focused = false
	return h, nil
}

func (h Model) handleRepoCount(msg RepoCountMsg) (tea.Model, tea.Cmd) {
	if msg.Err == nil {
		h.RepoCount = msg.Count
		h.latchValueDeliveredIfNeeded()
	}
	return h, nil
}

func (h Model) handleAuthStatus(msg AuthStatusMsg) (tea.Model, tea.Cmd) {
	if msg.Generation != h.AuthStatusGeneration {
		return h, nil
	}
	h.LoggedIn = msg.LoggedIn
	h.LoggedInEmail = msg.Email
	h.NeedsRelogin = msg.NeedsRelogin
	h.ReloginReason = msg.ReloginReason
	// Auth state can add or remove the re-login warning below the table. The
	// table rows are unchanged, so recalculate its cached height here rather
	// than waiting for the next session poll.
	h.Table.SetHeight(h.TableHeight())
	if !h.LoggedIn {
		h.CloudStatus = nil
		h.cloudErr = nil
		h.cloudCheckoutStatus = ""
		h.cloudCheckoutPolling = false
		h.cloudChecking = false
		return h, nil
	}
	if h.CloudAccess != nil && h.CloudStatus == nil && !h.cloudChecking {
		h.cloudChecking = true
		return h, fetchCloudAccessStatus(h.CloudAccess, h.Ctx)
	}
	return h, nil
}

func (h Model) handleSpinnerTick(msg spinner.TickMsg) (tea.Model, tea.Cmd) {
	if !h.Focused {
		return h, nil
	}
	var cmd tea.Cmd
	h.Spinner, cmd = h.Spinner.Update(msg)
	h.refreshSpinnerRows()
	return h, cmd
}

func (h Model) handleTick() (tea.Model, tea.Cmd) {
	// Time-limited footer lines can expire without a probe or session update.
	h.Table.SetHeight(h.TableHeight())
	cmds := []tea.Cmd{TickCmd()}
	if ui.PollDue(h.Focused, h.lastSessionPollAt, h.currentTime()) {
		cmds = append(cmds, h.pollCmds()...)
	}
	return h, tea.Batch(cmds...)
}

func (h *Model) pollCmds() []tea.Cmd {
	h.lastSessionPollAt = h.currentTime()
	// Re-poll auth status alongside sessions so the menu label
	// (e.g. [l]ogout vs [l]ogin) and the re-login warning catch up
	// after the daemon flags the stored credentials on a terminal
	// refresh failure. Without this, the TUI keeps showing the
	// pre-expiry label until the user navigates away and back.
	h.NextSessionPollID++
	cmds := []tea.Cmd{FetchSessions(h.Client, h.Ctx, h.Generation, h.NextSessionPollID, h.Settings.DisplayOpenPorts)}
	if h.authMgr != nil {
		cmds = append(cmds, fetchAuthStatus(h.authMgr, h.AuthStatusGeneration))
	}
	if h.LoggedIn && h.CloudAccess != nil && (cloudAccessPending(h.CloudStatus) || h.cloudCheckoutPolling) {
		h.cloudChecking = true
		cmds = append(cmds, refreshCloudAccessStatus(h.CloudAccess, h.Ctx))
	}
	if probe := h.tickDaemonSupervisionProbe(); probe != nil {
		cmds = append(cmds, probe)
	}
	if notices := h.tickTicketNotices(); notices != nil {
		cmds = append(cmds, notices)
	}
	return cmds
}

// View selects one of Home's four screens. Only the loading branch is wrapped
// in withUpgradeStatus — the daemon-error screen deliberately shows nothing
// else, and the empty-state and session-table renderers place the upgrade
// status themselves, at a position that depends on the rest of their content.
func (h Model) View() tea.View {
	if h.Err != nil {
		return tea.NewView(h.renderDaemonError())
	}

	if h.Loading {
		return tea.NewView(h.withUpgradeStatus(h.renderLoading()))
	}

	if len(h.Sessions) == 0 {
		return tea.NewView(h.RenderEmptyState())
	}

	return tea.NewView(h.RenderSessionTable())
}

// textEntryActive reports whether Home is capturing free text, so App can stop
// aliasing ctrl+x to the back action while a title is being typed
// (app.go's backKeyAliasEligible). Mirrors chatpicker.Model.textEntryActive.
func (h Model) TextEntryActive() bool {
	return h.Rename.Active() || h.Filter.Active()
}

func fetchRepoCount(c Client, ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		repos, err := c.ListRepos(ctx)
		if err != nil {
			return RepoCountMsg{Err: err}
		}
		return RepoCountMsg{Count: len(repos)}
	}
}

func fetchAuthStatus(mgr *auth.Manager, generation uint64) tea.Cmd {
	return func() tea.Msg {
		status := mgr.Status()
		return AuthStatusMsg{
			Generation:    generation,
			LoggedIn:      status.LoggedIn,
			Email:         status.Email,
			NeedsRelogin:  status.NeedsRelogin,
			ReloginReason: status.ReloginReason,
		}
	}
}
