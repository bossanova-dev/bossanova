// Package views implements the Bubbletea TUI for the boss CLI.
package views

import (
	"context"
	"net/url"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/boss/internal/client"
	bosspty "github.com/recurser/boss/internal/pty"
	"github.com/recurser/bossalib/config"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/telemetry"
)

// View identifies which screen is currently active.
type View int

const (
	ViewHome View = iota
	ViewNewSession
	ViewAttach
	ViewChatPicker
	ViewRepoAdd
	ViewRepoList
	ViewRepoSettings
	ViewTrash
	ViewSettings
	ViewSessionSettings
	ViewLogin
	ViewBugReport
	ViewCron
	ViewCronForm
	ViewOnboarding
	// ViewAccounts is appended at the END of the const block so existing iota
	// values do not shift (BOS-265).
	ViewAccounts
	// ViewAccountEdit is the per-account inline-edit form (label/status/priority)
	// reached from the accounts list (BOS-266). Appended after ViewAccounts to
	// preserve existing iota values.
	ViewAccountEdit
	// ViewAccountRegister is the native add-account flow for Claude and Codex
	// (BOS-267), launched by [a] from the accounts list. Appended at the END of
	// the const block so existing iota values do not shift.
	ViewAccountRegister
)

// App is the root Bubbletea model that manages view routing and shared state.
type App struct {
	client            client.BossClient
	auth              *auth.Manager
	telemetry         telemetry.Client
	afterAuth         loginCompleteHook
	cloudAccess       CloudAccessClient
	checkoutReturnURL string
	checkoutCancelURL string
	subscriptionURL   string
	userSettings      config.Settings
	startedAt         time.Time
	ctx               context.Context
	ptyManager        *bosspty.Manager
	activeView        View
	home              HomeModel
	newSession        NewSessionModel
	chatPicker        ChatPickerModel
	repoAdd           RepoAddModel
	repoAddCompleting bool
	repoList          RepoListModel
	repoSettings      RepoSettingsModel
	sessionSettings   SessionSettingsModel
	trash             TrashModel
	settings          SettingsModel
	attach            AttachModel
	login             LoginModel
	bugReport         BugReportModel
	cronList          CronListModel
	cronForm          CronFormModel
	accountsList      AccountsListModel
	accountEdit       AccountEditModel
	accountRegister   AccountRegisterModel
	onboarding        OnboardingModel
	// toast is a single-slot, non-blocking notice line rendered under the
	// banner (currently: automatic account rotations, BOS-176).
	toast toastModel
	// rotationSeen tracks the newest rotation-event id per session so a new
	// rotation decision can be detected across refreshes and surfaced as a
	// toast. nil until the first session list is observed (seed pass).
	rotationSeen map[string]string
	width        int
	height       int
	quitting     bool
}

// WithTelemetry installs a telemetry client for action-level view events.
func (a *App) WithTelemetry(client telemetry.Client) *App {
	a.telemetry = client
	return a
}

func (a *App) WithLoginCompleteHook(hook func(context.Context)) *App {
	a.afterAuth = hook
	return a
}

func (a *App) WithCloudAccessClient(c CloudAccessClient) *App {
	a.cloudAccess = c
	a.home.SetCloudSubscription(c, a.checkoutReturnURL, a.checkoutCancelURL)
	return a
}

// WithCheckoutURLs configures the CLI-tagged Stripe checkout return/cancel URLs
// that the login model passes into the cloud subscription flow.
func (a *App) WithCheckoutURLs(returnURL, cancelURL string) *App {
	a.checkoutReturnURL = returnURL
	a.checkoutCancelURL = cancelURL
	a.home.SetCloudSubscription(a.cloudAccess, returnURL, cancelURL)
	return a
}

// WithSubscriptionURL configures the CLI-tagged web subscription landing page.
func (a *App) WithSubscriptionURL(rawURL string) *App {
	a.subscriptionURL = rawURL
	return a
}

func (a *App) WithSettings(settings config.Settings) *App {
	a.userSettings = settings
	a.home.SetSettings(settings)
	return a
}

// NewApp creates a new App wired to the daemon client.
//
// Provider setup runs before the daemon is auto-started so plugin settings
// are current by the time bossd reads them.
func NewApp(c client.BossClient, authMgr *auth.Manager) App {
	ctx := context.Background()
	settings := config.DefaultSettings()
	home := NewHomeModel(c, ctx, authMgr)
	app := App{
		client:       c,
		auth:         authMgr,
		ctx:          ctx,
		ptyManager:   bosspty.NewManager(),
		activeView:   ViewHome,
		home:         home,
		userSettings: settings,
		// startedAt is the process-lifetime session epoch for the guest cloud
		// offer fatigue timer. It is captured once here and copied into every
		// Home model so navigating back to Home does not reset the clock.
		startedAt: home.startedAt,
	}
	app.home.SetSettings(settings)
	return app
}

// SetInitialView overrides the default initial view before running the program.
func (a *App) SetInitialView(v View) {
	a.activeView = v
	switch v {
	case ViewNewSession:
		a.newSession = a.newSessionModel()
	case ViewRepoAdd:
		a.repoAdd = a.newRepoAddModel()
	case ViewRepoList:
		a.repoList = NewRepoListModel(a.client, a.ctx)
	default:
	}
}

// SetAttachSession sets the session ID to attach to. Must be called after SetInitialView(ViewAttach).
func (a *App) SetAttachSession(sessionID, resumeID string) {
	a.attach = NewAttachModel(a.client, a.ctx, a.ptyManager, sessionID, resumeID)
	a.attach.SetTelemetry(a.telemetry)
}

// SetInitialAgent overrides the agent for new sessions created via the
// NewSession view. Empty means "use Settings.DefaultAgent" (the daemon
// falls back automatically).
func (a *App) SetInitialAgent(name string) {
	a.newSession.SetInitialAgent(name)
}

// SetInitialAccount overrides the account for new sessions created via the
// NewSession view. Empty means "system default" (the daemon applies its
// default-account policy).
func (a *App) SetInitialAccount(account string) {
	a.newSession.SetInitialAccount(account)
}

func (a App) newSessionModel() NewSessionModel {
	m := NewNewSessionModel(a.client, a.ctx)
	m.SetTelemetry(a.telemetry)
	settings, err := config.Load()
	if err == nil {
		m.SetAgentSettings(settings)
	}
	m.SetAgentSelectionHandler(saveDefaultAgent)
	return m
}

func saveDefaultAgent(name string) error {
	settings, err := config.Load()
	if err != nil {
		return err
	}
	settings.DefaultAgent = name
	return config.Save(settings)
}

func (a App) Init() tea.Cmd {
	var viewCmd tea.Cmd
	switch a.activeView {
	case ViewNewSession:
		viewCmd = a.newSession.Init()
	case ViewChatPicker:
		viewCmd = a.chatPicker.Init()
	case ViewRepoAdd:
		viewCmd = a.repoAdd.Init()
	case ViewRepoList:
		viewCmd = a.repoList.Init()
	case ViewAttach:
		viewCmd = a.attach.Init()
	case ViewOnboarding:
		viewCmd = a.onboarding.Init()
	default:
		viewCmd = a.home.Init()
	}
	return tea.Batch(viewCmd, heartbeatTickCmd())
}

// switchViewMsg requests the app to switch to a different view.
type switchViewMsg struct {
	view       View
	sessionID  string      // used for ViewAttach and ViewChatPicker
	resumeID   string      // Claude Code session UUID to resume (ViewAttach only)
	agentName  string      // optional per-chat agent override (ViewAttach only); empty = inherit session
	returnView View        // optional view to return to when the target view is cancelled
	firstRepo  bool        // true when add-repo was opened from the zero-repo home empty state (return home on cancel)
	account    *pb.Account // selected account carrier for ViewAccountEdit (BOS-266)
}

type repoAddCompletedMsg struct {
	repos       []*pb.Repo
	err         error
	highlightID string
}

func (a App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Route keys through the rotation toast first. It only consumes Esc while a
	// toast is visible, letting the operator dismiss the notice without also
	// triggering the active view's back action.
	if _, ok := msg.(tea.KeyPressMsg); ok {
		var consumed bool
		if a.toast, consumed = a.toast.Update(msg); consumed {
			return a, nil
		}
	}

	switch msg := msg.(type) {
	case toastExpireMsg:
		a.toast, _ = a.toast.Update(msg)
		return a, nil
	case sessionListMsg:
		// Surface a non-blocking toast when a new automatic rotation lands.
		// Seeded silently on first observation so a fresh TUI doesn't replay
		// history. Session rotation state only flows into the Home model, so
		// forward the message onward for its normal handling and batch the
		// toast command.
		if a.activeView == ViewHome && msg.err == nil {
			seen, toasts := detectNewRotationEvents(a.rotationSeen, msg.sessions)
			a.rotationSeen = seen
			var toastCmd tea.Cmd
			if len(toasts) > 0 {
				a.toast, toastCmd = a.toast.Show(toasts[0])
			}
			updated, cmd := a.home.Update(msg)
			if m, ok := updated.(HomeModel); ok {
				a.home = m
			}
			a.userSettings = a.home.settings
			return a, tea.Batch(cmd, toastCmd)
		}
	case tea.WindowSizeMsg:
		a.width = msg.Width
		a.height = msg.Height
		a.home.width = msg.Width
		a.home.height = msg.Height
		a.newSession.width = msg.Width
		a.repoAdd.width = msg.Width
		a.repoList.width = msg.Width
		a.repoList.height = msg.Height
		a.repoSettings.width = msg.Width
		a.trash.width = msg.Width
		a.trash.height = msg.Height
		a.chatPicker.width = msg.Width
		a.chatPicker.height = msg.Height
		a.settings.width = msg.Width
		a.sessionSettings.width = msg.Width
		a.login.width = msg.Width
		a.bugReport.width = msg.Width
		a.cronList.width = msg.Width
		a.cronList.height = msg.Height
		a.cronForm.width = msg.Width
		a.cronForm.height = msg.Height
		a.accountsList.width = msg.Width
		a.accountsList.height = msg.Height
		a.accountEdit.width = msg.Width
		a.accountEdit.height = msg.Height
		a.accountRegister.width = msg.Width
		a.accountRegister.height = msg.Height
		a.onboarding.width = msg.Width

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			a.quitting = true
			return a, tea.Quit
		case "ctrl+b":
			if a.activeView == ViewBugReport {
				break
			}
			if a.activeView == ViewHome && (a.home.upgrading || a.home.restarting) {
				return a, nil
			}
			a.bugReport = NewBugReportModel(a.client, a.ctx, a.auth, a.activeView, a.currentSession(), a.currentDaemonStatuses())
			a.bugReport.width = a.width
			a.activeView = ViewBugReport
			return a, a.bugReport.Init()
		}

	case heartbeatTickMsg:
		// Snapshot the local PTY manager and ship per-chat statuses to bossd
		// so other clients (web UI, second TUI instance) see this client's
		// "working/idle/question" state. Re-arm the ticker so the loop is
		// self-perpetuating.
		return a, tea.Batch(
			sendHeartbeatsCmd(a.ctx, a.client, a.ptyManager),
			heartbeatTickCmd(),
		)

	case repoAddCompletedMsg:
		a.repoAddCompleting = false
		returnView := a.repoList.returnView
		// A freshly-added repo configures its merge-strategy / automation /
		// integration options inline on the add wizard, so the completed add
		// returns to where it started: Home when there is at most one repo, else
		// the repo list with the new repo highlighted.
		if msg.err == nil && len(msg.repos) <= 1 {
			if returnView == ViewSettings {
				return a, a.switchToReturn(returnView)
			}
			return a, a.switchToHome()
		}
		highlightID := msg.highlightID
		if highlightID == "" {
			cursor := a.repoList.table.Cursor()
			if cursor >= 0 && cursor < len(a.repoList.repos) {
				highlightID = a.repoList.repos[cursor].Id
			}
		}
		a.repoList = NewRepoListModel(a.client, a.ctx)
		a.repoList.returnView = returnView
		a.repoList.highlightRepoID = highlightID
		a.repoList.width = a.width
		a.repoList.height = a.height
		a.activeView = ViewRepoList
		return a, a.repoList.Init()

	case archiveResultMsg:
		// Reconcile the home archiving override when the archive RPC resolves.
		// Runs at the app level (not per-view) so an ESC-then-result still
		// reconciles the optimistic state even when the chatpicker is no longer
		// the active view. This case does NOT return
		// early: after updating app state, execution continues past this type
		// switch to the per-view dispatch below (not a Go `fallthrough`), so
		// the chatpicker (if still active) also processes the message and flips
		// its own archiving=false / statusMsg field.
		if a.home.isArchiving(msg.sessionID) {
			// The archive RPC resolved. On failure, drop the override entirely so
			// the real status is shown again. On success the override is retained
			// for rendering until the row leaves the list, but the archive is no
			// longer in flight, so re-entering the session must not seed a stuck
			// archiving picker.
			a.home.resolveArchive(msg.sessionID, msg.err)
			if msg.err != nil {
				// The table rows cache rendered status text. Rebuild immediately so
				// a failed archive stops showing its optimistic label before the next
				// spinner tick or session poll.
				a.home.buildTableRows()
			}
		}

	case mergeResultMsg:
		// Reconcile the home merging override when the merge RPC resolves. Runs at
		// the app level (not per-view, and without returning early) so an
		// ESC-then-result still reconciles the optimistic state even when the
		// chatpicker is no longer the active view — execution continues past this
		// type switch to the per-view dispatch below so the chatpicker (if still
		// active) also flips its own merging=false / merged fields.
		if a.home.isMerging(msg.sessionID) {
			a.home.resolveMerge(msg.sessionID)
			if msg.err != nil {
				// Failed merge: rebuild the cached row so it drops the optimistic
				// "merging" label and returns to its real status immediately, rather
				// than waiting for the next spinner tick or session poll.
				a.home.buildTableRows()
			} else {
				// Successful merge: hand off to the merged optimistic override so the
				// row flips straight from blue "merging" to "✓ merged" without a
				// "passing" flicker while the PR-merged webhook lands asynchronously.
				a.home.mergedOptimisticID = msg.sessionID
			}
		}

	case switchViewMsg:
		a.activeView = msg.view
		switch msg.view { //nolint:exhaustive // ViewBugReport is pushed via ctrl+b, not switchViewMsg
		case ViewNewSession:
			a.newSession = a.newSessionModel()
			a.newSession.width = a.width
			return a, a.newSession.Init()
		case ViewChatPicker:
			a.chatPicker = NewChatPickerModel(a.client, a.ctx, msg.sessionID, "")
			a.chatPicker.SetTelemetry(a.telemetry)
			a.chatPicker.width = a.width
			a.chatPicker.height = a.height
			// Only seed the picker's key-swallowing archiving flag when the
			// archive is still actually in flight. After a successful archive the
			// override lingers on the still-present row for rendering, but no
			// archiveResultMsg is outstanding for a freshly created picker, so
			// seeding archiving=true here would leave it stuck on "Archiving
			// session..." swallowing every key but Esc.
			if a.home.archiveInFlight(msg.sessionID) {
				a.chatPicker.archiving = true
			}
			// Same in-flight guard for merging: only seed merging=true (which shows
			// the "Merging PR..." banner and hides [m]erge) while the merge RPC is
			// still outstanding. After a successful merge the override lingers on the
			// row for rendering but no mergeResultMsg is outstanding, so seeding here
			// would leave the picker stuck.
			if a.home.mergeInFlight(msg.sessionID) {
				a.chatPicker.merging = true
			}
			return a, a.chatPicker.Init()
		case ViewRepoAdd:
			a.repoAdd = a.newRepoAddModel()
			a.repoAdd.width = a.width
			if msg.firstRepo {
				// Adding the first repo from the home empty state: return to the
				// home empty state on cancel rather than the repo list.
				a.repoAdd.returnHomeOnCancel = true
			}
			return a, a.repoAdd.Init()
		case ViewRepoList:
			a.repoList = NewRepoListModel(a.client, a.ctx)
			a.repoList.returnView = msg.returnView
			a.repoList.width = a.width
			a.repoList.height = a.height
			return a, a.repoList.Init()
		case ViewRepoSettings:
			a.repoSettings = NewRepoSettingsModel(a.client, a.ctx, msg.sessionID)
			if githubAppClient, ok := a.cloudAccess.(GitHubAppClient); ok {
				a.repoSettings.SetGitHubAppInstall(githubAppClient)
			}
			a.repoSettings.width = a.width
			return a, a.repoSettings.Init()
		case ViewSessionSettings:
			a.sessionSettings = NewSessionSettingsModel(a.client, a.ctx, msg.sessionID)
			a.sessionSettings.width = a.width
			return a, a.sessionSettings.Init()
		case ViewTrash:
			a.trash = NewTrashModel(a.client, a.ctx)
			a.trash.returnView = msg.returnView
			a.trash.width = a.width
			a.trash.height = a.height
			return a, a.trash.Init()
		case ViewSettings:
			a.settings = NewSettingsModel(a.client, a.ctx, a.auth)
			if a.cloudAccess != nil {
				a.settings.SetCloudAccess(a.cloudAccess, accountSettingsReturnURL(a.checkoutReturnURL))
			}
			a.settings.width = a.width
			return a, a.settings.Init()
		case ViewAttach:
			a.attach = NewAttachModel(a.client, a.ctx, a.ptyManager, msg.sessionID, msg.resumeID)
			a.attach.SetTelemetry(a.telemetry)
			a.attach.SetOverrideAgent(msg.agentName)
			return a, a.attach.Init()
		case ViewLogin:
			a.login = NewLoginModel(a.auth, a.client, a.ctx)
			a.login.SetAfterAuth(a.afterAuth)
			if a.cloudAccess != nil {
				a.login.SetCloudSubscription(a.cloudAccess, a.checkoutReturnURL, a.checkoutCancelURL)
				a.login.SetSubscriptionURL(a.subscriptionURL)
			}
			a.login.width = a.width
			return a, a.login.Init()
		case ViewHome:
			a.home = a.newHomeModel()
			return a, a.home.Init()
		case ViewCron:
			a.cronList = NewCronListModel(a.client, a.ctx)
			a.cronList.returnView = msg.returnView
			a.cronList.width = a.width
			a.cronList.height = a.height
			return a, a.cronList.Init()
		case ViewCronForm:
			a.cronForm = NewCronFormModel(a.client, a.ctx)
			a.cronForm.width = a.width
			a.cronForm.height = a.height
			return a, a.cronForm.Init()
		case ViewAccounts:
			a.accountsList = NewAccountsListModel(a.client, a.ctx)
			a.accountsList.returnView = msg.returnView
			a.accountsList.width = a.width
			a.accountsList.height = a.height
			return a, a.accountsList.Init()
		case ViewAccountEdit:
			a.accountEdit = NewAccountEditModel(a.client, a.ctx, msg.account)
			a.accountEdit.returnView = msg.returnView
			a.accountEdit.width = a.width
			a.accountEdit.height = a.height
			return a, a.accountEdit.Init()
		case ViewAccountRegister:
			a.accountRegister = NewAccountRegisterModel(a.client, a.ctx)
			a.accountRegister.returnView = msg.returnView
			a.accountRegister.width = a.width
			a.accountRegister.height = a.height
			return a, a.accountRegister.Init()
		}
		return a, nil
	case startSubscriptionFlowMsg:
		a.login = NewLoginModel(a.auth, a.client, a.ctx)
		a.login.SetAfterAuth(a.afterAuth)
		if a.cloudAccess != nil {
			a.login.SetCloudSubscription(a.cloudAccess, a.checkoutReturnURL, a.checkoutCancelURL)
			a.login.SetSubscriptionURL(a.subscriptionURL)
		}
		a.login.width = a.width
		a.activeView = ViewLogin
		updated, cmd := a.login.startSubscriptionAttempt()
		a.login = updated
		return a, tea.Batch(cmd, a.login.spinner.Tick)
	}

	switch a.activeView {
	case ViewOnboarding:
		updated, cmd := a.onboarding.Update(msg)
		if m, ok := updated.(OnboardingModel); ok {
			a.onboarding = m
		}
		if a.onboarding.Done() || a.onboarding.Cancelled() {
			return a, a.switchToHome()
		}
		return a, cmd
	case ViewHome:
		updated, cmd := a.home.Update(msg)
		if m, ok := updated.(HomeModel); ok {
			a.home = m
		}
		// Propagate any settings the home model persisted (e.g. the
		// BossCloudValueDeliveredAt latch) back to the App, so later
		// newHomeModel() recreations re-seed from the latched value instead of
		// the stale startup snapshot. Without this, the latch would re-stamp on
		// every return-to-home (moving the "never moves" timestamp) and the
		// promo would revert once has_active_chat went false.
		a.userSettings = a.home.settings
		return a, cmd
	case ViewNewSession:
		updated, cmd := a.newSession.Update(msg)
		if m, ok := updated.(NewSessionModel); ok {
			a.newSession = m
		}
		if a.newSession.Cancelled() {
			return a, a.switchToHome()
		}
		if a.newSession.Done() {
			sess := a.newSession.CreatedSession()
			if sess != nil {
				// New sessions launch directly into chat by design. The
				// home-list Enter key routes via ViewChatPicker, but new-
				// session creation must NOT — the user has just configured
				// the session and expects to start chatting immediately.
				a.attach = NewAttachModel(a.client, a.ctx, a.ptyManager, sess.Id, "")
				a.attach.SetTelemetry(a.telemetry)
				a.activeView = ViewAttach
				return a, a.attach.Init()
			}
			return a, a.switchToHome()
		}
		return a, cmd
	case ViewChatPicker:
		updated, cmd := a.chatPicker.Update(msg)
		if m, ok := updated.(ChatPickerModel); ok {
			a.chatPicker = m
		}
		// A successful merge keeps the user on the session-detail view so they can
		// archive in place; only cancel/archive return to the session list.
		if a.chatPicker.Cancelled() || a.chatPicker.Archived() {
			sessionID := a.chatPicker.sessionID
			merged := a.chatPicker.Merged()
			// On archive the session disappears from the list, so highlight the
			// session that fills its place (the next one down, or the previous
			// one if it was last) to keep the cursor where it was instead of
			// jumping back to the top. Cancel keeps highlighting the
			// session itself since it remains in the list. Computed against the
			// pre-archive list still held by a.home before it is rebuilt.
			highlightID := sessionID
			if a.chatPicker.Archived() {
				highlightID = a.home.neighborSessionID(sessionID)
			}
			a.activeView = ViewHome
			a.home = a.newHomeModel()
			a.home.highlightSessionID = highlightID
			if merged {
				a.home.mergedOptimisticID = sessionID
			}
			if archivingID := a.chatPicker.ArchivingSessionID(); archivingID != "" {
				a.home.markArchiving(archivingID)
			}
			// A mid-merge Esc is Cancelled() with merging still true, so
			// MergingSessionID() is non-empty and the optimistic blue "merging"
			// status carries onto the home row. A post-success return has
			// merging=false (handled by mergedOptimisticID above) so this no-ops.
			if mergingID := a.chatPicker.MergingSessionID(); mergingID != "" {
				a.home.markMerging(mergingID)
			}
			return a, a.home.Init()
		}
		return a, cmd
	case ViewRepoAdd:
		updated, cmd := a.repoAdd.Update(msg)
		if m, ok := updated.(RepoAddModel); ok {
			a.repoAdd = m
		}
		if a.repoAdd.Done() {
			if a.repoAddCompleting {
				return a, cmd
			}
			highlightID := ""
			if a.repoAdd.createdRepo != nil {
				highlightID = a.repoAdd.createdRepo.Id
			}
			a.repoAddCompleting = true
			return a, tea.Batch(cmd, fetchReposAfterRepoAdd(a.client, a.ctx, highlightID))
		}
		if a.repoAdd.Cancelled() {
			if a.repoAdd.returnHomeOnCancel {
				return a, a.switchToHome()
			}
			returnView := a.repoList.returnView
			var highlightID string
			if cursor := a.repoList.table.Cursor(); cursor >= 0 && cursor < len(a.repoList.repos) {
				highlightID = a.repoList.repos[cursor].Id
			}
			a.repoList = NewRepoListModel(a.client, a.ctx)
			a.repoList.returnView = returnView
			a.repoList.highlightRepoID = highlightID
			a.repoList.width = a.width
			a.repoList.height = a.height
			a.activeView = ViewRepoList
			return a, a.repoList.Init()
		}
		return a, cmd
	case ViewRepoList:
		updated, cmd := a.repoList.Update(msg)
		if m, ok := updated.(RepoListModel); ok {
			a.repoList = m
		}
		if a.repoList.Cancelled() {
			return a, a.switchToReturn(a.repoList.returnView)
		}
		return a, cmd
	case ViewRepoSettings:
		updated, cmd := a.repoSettings.Update(msg)
		if m, ok := updated.(RepoSettingsModel); ok {
			a.repoSettings = m
		}
		if a.repoSettings.Cancelled() || a.repoSettings.Done() {
			// Return to repo list, highlighting the repo we came from.
			returnView := a.repoList.returnView
			a.repoList = NewRepoListModel(a.client, a.ctx)
			a.repoList.returnView = returnView
			a.repoList.highlightRepoID = a.repoSettings.repoID
			a.repoList.width = a.width
			a.repoList.height = a.height
			a.activeView = ViewRepoList
			return a, a.repoList.Init()
		}
		return a, cmd
	case ViewSessionSettings:
		updated, cmd := a.sessionSettings.Update(msg)
		if m, ok := updated.(SessionSettingsModel); ok {
			a.sessionSettings = m
		}
		if a.sessionSettings.Cancelled() || a.sessionSettings.Done() {
			// Return to chat picker, highlighting the chat we came from.
			var highlightID string
			if cursor := a.chatPicker.table.Cursor(); cursor >= 0 && cursor < len(a.chatPicker.chats) {
				highlightID = a.chatPicker.chats[cursor].AgentSessionId
			}
			a.chatPicker = NewChatPickerModel(a.client, a.ctx, a.sessionSettings.sessionID, highlightID)
			a.chatPicker.SetTelemetry(a.telemetry)
			a.chatPicker.width = a.width
			a.chatPicker.height = a.height
			a.activeView = ViewChatPicker
			return a, a.chatPicker.Init()
		}
		return a, cmd
	case ViewTrash:
		updated, cmd := a.trash.Update(msg)
		if m, ok := updated.(TrashModel); ok {
			a.trash = m
		}
		if sessionID := a.trash.RestoredSessionID(); sessionID != "" {
			a.chatPicker = NewChatPickerModel(a.client, a.ctx, sessionID, "")
			a.chatPicker.SetTelemetry(a.telemetry)
			a.chatPicker.width = a.width
			a.chatPicker.height = a.height
			a.activeView = ViewChatPicker
			return a, a.chatPicker.Init()
		}
		if a.trash.Cancelled() {
			return a, a.switchToReturn(a.trash.returnView)
		}
		return a, cmd
	case ViewSettings:
		updated, cmd := a.settings.Update(msg)
		if m, ok := updated.(SettingsModel); ok {
			a.settings = m
		}
		if a.settings.Cancelled() {
			return a, a.switchToHome()
		}
		return a, cmd
	case ViewAttach:
		updated, cmd := a.attach.Update(msg)
		if m, ok := updated.(AttachModel); ok {
			a.attach = m
		}
		if a.attach.Detached() {
			sessionID := a.attach.SessionID()
			agentSessionID := a.attach.AgentSessionID()
			a.chatPicker = NewChatPickerModel(a.client, a.ctx, sessionID, agentSessionID)
			a.chatPicker.SetTelemetry(a.telemetry)
			a.chatPicker.width = a.width
			a.chatPicker.height = a.height
			a.activeView = ViewChatPicker
			// Batch the attach cleanup cmd (e.g. orphan delete) with the chat picker init.
			return a, tea.Batch(cmd, a.chatPicker.Init())
		}
		return a, cmd
	case ViewLogin:
		updated, cmd := a.login.Update(msg)
		if m, ok := updated.(LoginModel); ok {
			a.login = m
		}
		if a.login.Cancelled() || a.login.Done() {
			return a, a.switchToHome()
		}
		return a, cmd
	case ViewBugReport:
		updated, cmd := a.bugReport.Update(msg)
		if m, ok := updated.(BugReportModel); ok {
			a.bugReport = m
		}
		if a.bugReport.Cancelled() || a.bugReport.Done() {
			// Restore the prior view without recreating it so existing state
			// (table cursor, loaded data, spinners) is preserved. The prior
			// view's self-perpetuating tick chain was swallowed by the modal,
			// so restart it when the restored view depends on tickMsg.
			a.activeView = a.bugReport.PreviousView()
			return a, resumeTickCmd(a.activeView)
		}
		return a, cmd
	case ViewCron:
		updated, cmd := a.cronList.Update(msg)
		if m, ok := updated.(CronListModel); ok {
			a.cronList = m
		}
		if a.cronList.Cancelled() {
			return a, a.switchToReturn(a.cronList.returnView)
		}
		// cronFormOpenMsg is emitted by CronListModel as a Cmd; when it
		// arrives here as a message we open the cron form.
		if ofm, ok := msg.(cronFormOpenMsg); ok {
			a.activeView = ViewCronForm
			a.cronForm = NewCronFormModel(a.client, a.ctx)
			a.cronForm.job = ofm.job
			a.cronForm.width = a.width
			a.cronForm.height = a.height
			return a, a.cronForm.Init()
		}
		return a, cmd
	case ViewCronForm:
		updated, cmd := a.cronForm.Update(msg)
		if m, ok := updated.(CronFormModel); ok {
			a.cronForm = m
		}
		if a.cronForm.Cancelled() {
			// Return to cron list without refreshing (user cancelled).
			returnView := a.cronList.returnView
			a.activeView = ViewCron
			a.cronList = NewCronListModel(a.client, a.ctx)
			a.cronList.returnView = returnView
			a.cronList.width = a.width
			a.cronList.height = a.height
			return a, a.cronList.Init()
		}
		// cronFormDoneMsg is emitted by CronFormModel as a Cmd; when it
		// arrives here as a message we return to the cron list and refresh.
		if doneMsg, ok := msg.(cronFormDoneMsg); ok {
			returnView := a.cronList.returnView
			a.activeView = ViewCron
			a.cronList = NewCronListModel(a.client, a.ctx)
			a.cronList.returnView = returnView
			a.cronList.width = a.width
			a.cronList.height = a.height
			// Keep the just-saved (edited or created) job highlighted once the
			// recreated list loads its jobs. Empty jobID falls back to row 0.
			a.cronList.highlightJobID = doneMsg.jobID
			return a, a.cronList.Init()
		}
		return a, cmd
	case ViewAccounts:
		updated, cmd := a.accountsList.Update(msg)
		if m, ok := updated.(AccountsListModel); ok {
			a.accountsList = m
		}
		if a.accountsList.Cancelled() {
			return a, a.switchToReturn(a.accountsList.returnView)
		}
		return a, cmd
	case ViewAccountEdit:
		updated, cmd := a.accountEdit.Update(msg)
		if m, ok := updated.(AccountEditModel); ok {
			a.accountEdit = m
		}
		if a.accountEdit.Cancelled() {
			return a, a.switchToReturn(a.accountEdit.returnView)
		}
		return a, cmd
	case ViewAccountRegister:
		updated, cmd := a.accountRegister.Update(msg)
		if m, ok := updated.(AccountRegisterModel); ok {
			a.accountRegister = m
		}
		if a.accountRegister.Cancelled() {
			return a, a.switchToReturn(a.accountRegister.returnView)
		}
		if a.accountRegister.Done() {
			// Registration succeeded: return to a rebuilt accounts list (its Init
			// re-fetches via ListAccounts) so the new account appears, routing its
			// own esc back to Settings (mirrors the ViewCronForm→refreshed-ViewCron
			// return). ViewAccounts' only entry point is Settings.
			a.activeView = ViewAccounts
			a.accountsList = NewAccountsListModel(a.client, a.ctx)
			a.accountsList.returnView = ViewSettings
			a.accountsList.width = a.width
			a.accountsList.height = a.height
			return a, a.accountsList.Init()
		}
		return a, cmd
	}

	return a, nil
}

// currentSession returns the session associated with the active view, or nil
// if none. Only views that expose a *pb.Session participate.
func (a App) currentSession() *pb.Session {
	if a.activeView == ViewChatPicker {
		return a.chatPicker.Session()
	}
	return nil
}

// currentDaemonStatuses returns the daemon heartbeat statuses tracked by the
// active view, or nil if the view doesn't track any. Keys differ by view —
// Home is session-id keyed, ChatPicker is claude-id keyed — which is fine
// for diagnostic triage.
func (a App) currentDaemonStatuses() map[string]string {
	switch a.activeView { //nolint:exhaustive // only views that track statuses participate
	case ViewChatPicker:
		return a.chatPicker.DaemonStatuses()
	case ViewHome:
		return a.home.DaemonStatuses()
	}
	return nil
}

// resumeTickCmd returns a tickCmd for views whose status refresh depends on a
// self-perpetuating tick chain. The bug-report modal swallows tickMsg while
// it is active, so the chain needs restarting when the modal dismisses back
// to one of these views. Returns nil for views that don't use ticks.
func resumeTickCmd(v View) tea.Cmd {
	switch v { //nolint:exhaustive // only tick-driven views participate
	case ViewHome, ViewChatPicker:
		return tickCmd()
	}
	return nil
}

func fetchReposAfterRepoAdd(c client.BossClient, ctx context.Context, highlightID string) tea.Cmd {
	return func() tea.Msg {
		repos, err := c.ListRepos(ctx)
		return repoAddCompletedMsg{repos: repos, err: err, highlightID: highlightID}
	}
}

func (a App) newRepoAddModel() RepoAddModel {
	m := NewRepoAddModel(a.client, a.ctx)
	if githubAppClient, ok := a.cloudAccess.(GitHubAppClient); ok {
		m.SetGitHubAppInstall(a.auth, githubAppClient)
	}
	return m
}

func (a *App) switchToHome() tea.Cmd {
	highlightID := a.home.selectedSessionID()
	a.activeView = ViewHome
	a.home = a.newHomeModel()
	a.home.highlightSessionID = highlightID
	return a.home.Init()
}

// switchToReturn routes back to the view a sub-view was opened from. ViewHome
// (the zero value) uses the full switchToHome rebuild; ViewSettings re-enters
// Settings via the normal init path. Any other value falls back to Home.
func (a *App) switchToReturn(v View) tea.Cmd {
	switch v {
	case ViewSettings:
		return func() tea.Msg { return switchViewMsg{view: ViewSettings} }
	case ViewAccounts:
		// Rebuild the accounts list (its Init re-fetches via ListAccounts) so an
		// edit made in the account-edit form is reflected on return, and route
		// the rebuilt list's own esc back to Settings (its only entry point).
		return func() tea.Msg { return switchViewMsg{view: ViewAccounts, returnView: ViewSettings} }
	default:
		return a.switchToHome()
	}
}

func (a *App) newHomeModel() HomeModel {
	home := NewHomeModel(a.client, a.ctx, a.auth)
	home.startedAt = a.startedAt
	home.SetSettings(a.userSettings)
	home.SetCloudSubscription(a.cloudAccess, a.checkoutReturnURL, a.checkoutCancelURL)
	home.width = a.width
	home.height = a.height
	// Preserve all optimistic archive state across rebuilds without sharing map
	// storage with the prior model. Both lingering success overrides and active
	// RPCs must survive until the next poll or result reconciles each session.
	home.archivingOverrideIDs = cloneSessionIDSet(a.home.archivingOverrideIDs)
	home.archiveInFlightIDs = cloneSessionIDSet(a.home.archiveInFlightIDs)
	// Preserve optimistic merge state across rebuilds too — both the lingering
	// success/failure overrides and any active RPC must survive until the next
	// poll or mergeResultMsg reconciles each session.
	home.mergingOverrideIDs = cloneSessionIDSet(a.home.mergingOverrideIDs)
	home.mergeInFlightIDs = cloneSessionIDSet(a.home.mergeInFlightIDs)
	return home
}

func accountSettingsReturnURL(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	u.Path = "/settings/account"
	u.RawPath = ""
	u.Fragment = ""
	return u.String()
}

func (a App) View() tea.View {
	if a.quitting {
		return tea.NewView("")
	}

	var v tea.View
	switch a.activeView {
	case ViewOnboarding:
		v = a.onboarding.View()
	case ViewHome:
		v = a.home.View()
	case ViewNewSession:
		v = a.newSession.View()
	case ViewChatPicker:
		v = a.chatPicker.View()
	case ViewRepoAdd:
		v = a.repoAdd.View()
	case ViewRepoList:
		v = a.repoList.View()
	case ViewRepoSettings:
		v = a.repoSettings.View()
	case ViewSessionSettings:
		v = a.sessionSettings.View()
	case ViewTrash:
		v = a.trash.View()
	case ViewSettings:
		v = a.settings.View()
	case ViewAttach:
		v = a.attach.View()
	case ViewLogin:
		v = a.login.View()
	case ViewBugReport:
		v = a.bugReport.View()
	case ViewCron:
		v = a.cronList.View()
	case ViewCronForm:
		v = a.cronForm.View()
	case ViewAccounts:
		v = a.accountsList.View()
	case ViewAccountEdit:
		v = a.accountEdit.View()
	case ViewAccountRegister:
		v = a.accountRegister.View()
	default:
		v = tea.NewView("Unknown view")
	}

	// Prepend the banner to every screen except during tea.Exec (AttachModel
	// returns empty content while Claude Code owns the terminal).
	if v.Content != "" {
		var opts bannerOpts
		switch a.activeView { //nolint:exhaustive // only override for specific views
		case ViewChatPicker:
			opts.session = a.chatPicker.session
			opts.spinner = a.chatPicker.spinner
			opts.archiving = a.chatPicker.archiving
			opts.merging = a.chatPicker.merging
		case ViewRepoSettings:
			opts.repo = a.repoSettings.repo
		case ViewSessionSettings:
			opts.session = a.sessionSettings.session
		case ViewRepoList:
			opts.line1 = "Repositories"
		case ViewTrash:
			opts.line1 = "Archived Sessions"
		case ViewSettings:
			opts.line1 = "Settings"
		case ViewNewSession:
			opts.line1 = "New Session"
		case ViewRepoAdd:
			opts.line1 = "Add a repository"
		case ViewLogin:
			opts.line1 = "Login"
		case ViewBugReport:
			opts.line1 = "Report a bug"
		case ViewCron:
			opts.line1 = "Scheduled Jobs"
		case ViewAccounts:
			opts.line1 = "Accounts"
		case ViewAccountEdit:
			opts.line1 = "Edit Account"
		case ViewAccountRegister:
			opts.line1 = "Add Account"
		case ViewOnboarding:
			opts.line1 = "Welcome to Bossanova"
		}
		content := renderBanner(a.activeView, opts)
		if toastLine := a.toast.View(a.width); toastLine != "" {
			content += "\n" + toastLine
		}
		v.Content = content + "\n" + v.Content
	}

	v.AltScreen = true
	return v
}
