// Package views implements the Bubbletea TUI for the boss CLI.
package views

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/boss/internal/client"
	bosspty "github.com/recurser/boss/internal/pty"
	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/telemetry"
)

// App is the root Bubbletea model that manages view routing and shared state.
type App struct {
	client            client.BossClient
	authChanges       *authChangeQueue
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
	generalSettings   GeneralSettingsModel
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
	authChanges := newAuthChangeQueue()
	home.authChanges = authChanges
	app := App{
		client:       c,
		authChanges:  authChanges,
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

// Update is the root dispatcher: app-global message handling, then delegation
// to the active view.
//
// The three arm shapes below are load-bearing, and the difference between them
// is the whole reason this function is easy to break:
//
//   - Arms that `return` handle the message completely; the active view never
//     sees it. Their handlers return a value Update can return directly, so the
//     shape makes the consumption visible at the call site.
//   - Arms that call a `(tea.Cmd, bool)` handler return only when the handler
//     reports handled==true. A false means the message is app-relevant AND
//     view-relevant, so it must go on to delegateToActiveView.
//   - Arms that call a handler returning NOTHING ALWAYS fall through — such a
//     handler only does bookkeeping (a pointer receiver, mutating App state) or
//     observes (a value receiver, e.g. a telemetry capture), never consumption.
//     These FOURTEEN are the ONLY handlers with that shape; keeping it that way
//     is what lets a reader infer routing from the signature. They are: window
//     size; the picker's merge / switch-account / archive results; and the
//     long-running action results whose originating view accepts Esc while the
//     RPC is in flight — accounts load (manual refresh only), account status,
//     account remove, account-edit save, cron delete / update / run-now,
//     session restore / delete / delete-batch progress. Every one of that
//     second group is a telemetry capture rooted here precisely so escaping
//     mid-RPC cannot lose the event (BOS-683).
//
// Turning a fall-through arm into an unconditional return — or an always-returns
// arm into a fall-through — is a silent routing regression, not a compile error.
//
// Sixteen arms fall through in total — the fourteen never-returns ones above
// plus the two `(tea.Cmd, bool)` ones when they report handled==false.
// app_fallthrough_test.go pins the fall-through direction for every one of them,
// and it pins
// the consumption direction for the four that do not re-route (toastExpireMsg,
// heartbeatTickMsg, ctrl+c, and the stale-generation rejection);
// startSubscriptionFlowMsg is pinned by its own test in app_test.go. Three
// remain uncovered in the consumption direction — repoAddCompletedMsg,
// switchViewMsg and ctrl+g — because each re-routes activeView, which destroys
// the witness those tests rely on. Flipping one of those three to fall through
// is currently invisible.
func (a App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Ctrl+X is an alias for Esc wherever the TUI treats Esc as "back one
	// level" (BOS-660). Rewriting the message here — before the toast, the
	// global chords and the active view — is what lets every existing Esc
	// branch inherit the alias without gaining a duplicate "ctrl+x" case.
	//
	// Narrowed to key presses at the call site so the alias reads as key
	// routing rather than a per-message hook; aliasCtrlXToEsc takes a pointer
	// receiver so this is a readability choice, not a load-bearing perf guard.
	if key, isKey := msg.(tea.KeyPressMsg); isKey {
		msg = a.aliasCtrlXToEsc(key)
	}

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
		return a.handleToastExpire(msg)
	case sessionListMsg:
		if cmd, handled := a.handleSessionList(msg); handled {
			return a, cmd
		}
	case tea.WindowSizeMsg:
		a.handleWindowSize(msg)
	case logoutMsg:
		// Logout actions originate in Home but can finish while a modal is
		// active. Route the result to the retained Home model so a failed
		// logout clears logoutPending and remains visible after returning.
		return a.updateHome(msg)
	case tea.KeyMsg:
		if cmd, handled := a.handleGlobalKey(msg); handled {
			return a, cmd
		}
	case heartbeatTickMsg:
		return a, a.handleHeartbeat()
	case repoAddCompletedMsg:
		return a.handleRepoAddCompleted(msg)
	case mergeResultMsg:
		a.handleMergeResult(msg)
	case switchAccountResultMsg:
		a.handleSwitchAccountResult(msg)
	case archiveResultMsg:
		a.handleArchiveResult(msg)
	case accountsLoadedMsg:
		a.handleAccountsLoadedResult(msg)
	case accountStatusUpdatedMsg:
		a.handleAccountStatusResult(msg)
	case accountRemovedMsg:
		a.handleAccountRemoveResult(msg)
	case accountEditSavedMsg:
		a.handleAccountEditSavedResult(msg)
	case cronJobDeletedMsg:
		a.handleCronJobDeletedResult(msg)
	case cronJobUpdatedMsg:
		a.handleCronJobUpdatedResult(msg)
	case cronRunNowMsg:
		a.handleCronRunNowResult(msg)
	case sessionRestoredMsg:
		a.handleSessionRestoredResult(msg)
	case sessionDeletedMsg:
		a.handleSessionDeletedResult(msg)
	case deleteProgressMsg:
		a.handleDeleteProgressResult(msg)
	case switchViewMsg:
		return a.handleSwitchView(msg)
	case startSubscriptionFlowMsg:
		return a.handleStartSubscriptionFlow()
	}

	// Mouse coordinates arrive in absolute screen space. The active view composes
	// its layout without knowing about the banner/toast chrome View prepends, so
	// translate into its content space here — App is the only place that knows
	// the chrome's height (BOS-512).
	//
	// Gated on the message type because Go evaluates arguments eagerly:
	// chromeHeight renders the banner (a per-character gradient) and the toast,
	// and every spinner tick, poll result and keypress would pay for it.
	if _, isMouse := msg.(tea.MouseMsg); isMouse {
		msg = translateMouseY(msg, a.chromeHeight())
	}

	return a.delegateToActiveView(msg)
}

// aliasCtrlXToEsc rewrites a bare ctrl+x key press into an Esc key press when
// the active view is a non-text back/cancel screen, and returns key untouched
// otherwise (BOS-660).
//
// The chord is matched on its full String() so only the exact `ctrl+x` is
// aliased: cmd+x, ctrl+shift+x and every other decorated variant fall through
// to the active view unchanged, as does ctrl+c (handled by handleGlobalKey).
//
// Presses only. A ctrl+x tea.KeyReleaseMsg would reach the views unaliased, but
// the program enables no keyboard enhancements, so releases are never delivered
// and no view handles them.
//
// Pointer receiver purely to avoid copying the ~20-sub-model App on the key
// path: this reads App and mutates nothing, and is NOT one of the Update-arm
// handler shapes the doc comment above describes.
func (a *App) aliasCtrlXToEsc(key tea.KeyPressMsg) tea.Msg {
	if key.String() != "ctrl+x" {
		return key
	}
	if !a.backKeyAliasEligible() {
		return key
	}
	return tea.KeyPressMsg{Code: tea.KeyEsc}
}

// backKeyAliasEligible reports whether the App's current state is a non-text
// back/cancel screen, i.e. one where Esc means "go back one level" and nothing
// on screen can legitimately consume a raw ctrl+x.
//
// The switch is EXHAUSTIVE over View on purpose — no `default`, so the
// `exhaustive` linter (enabled in .golangci.yml) turns a newly added View into
// a build-gate failure rather than a silent opt-in. Fail-open was the obvious
// shape and it is the wrong one: a view that quietly becomes eligible starts
// cancelling out from under the cursor with no compile error, no lint error and
// no failing test, because app_ctrl_x_test.go can only cover the views that
// exist today. Adding a text input to a listed view still needs its arm
// widened; that part the tests do guard.
//
// Every text-entry arm delegates to a textEntryActive() method on the model
// that owns the state, rather than re-encoding that model's "not editing"
// sentinel here. Two views are excluded outright instead (ViewAttach and
// ViewOnboarding): those are view-wide exclusions, not text-entry ones.
func (a *App) backKeyAliasEligible() bool {
	switch a.activeView {
	case ViewAttach:
		// Forward ctrl+x unchanged so tmux — not this model — can own it: the
		// detach binding lives in tmux's own root key table, and once the PTY is
		// attached the chord is consumed there without ever reaching App.Update.
		// On the pre-exec launch screen nothing is listening for it, so leaving
		// it unaliased makes it intentionally inert rather than a second way to
		// trigger attach's Esc branch (which bails out of the launch screen).
		return false
	case ViewOnboarding:
		// Onboarding's Esc quits the program outright rather than going back a
		// level, and `x` is a live binding on that screen — so a brushed Ctrl
		// must not turn it into an exit.
		return false
	case ViewNewSession:
		return !a.newSession.textEntryActive()
	case ViewTrash:
		return !a.trash.textEntryActive()
	case ViewRepoAdd:
		return !a.repoAdd.textEntryActive()
	case ViewCronForm:
		return !a.cronForm.textEntryActive()
	case ViewBugReport:
		return !a.bugReport.textEntryActive()
	case ViewRepoSettings:
		return !a.repoSettings.textEntryActive()
	case ViewSessionSettings:
		return !a.sessionSettings.textEntryActive()
	case ViewGeneralSettings:
		return !a.generalSettings.textEntryActive()
	case ViewAccountEdit:
		return !a.accountEdit.textEntryActive()
	case ViewAccountRegister:
		return !a.accountRegister.textEntryActive()
	case ViewHome, ViewChatPicker, ViewRepoList, ViewSettings, ViewLogin,
		ViewCron, ViewAccounts:
		// Pure list/hub screens: no text input, and Esc already means "back one
		// level" on each of them.
		return true
	}
	// Unreachable while the switch stays exhaustive; fail closed if it ever
	// stops being.
	return false
}

func (a App) View() tea.View {
	if a.quitting {
		return tea.NewView("")
	}

	// Reserve the rotation toast's lines out of the height each view sizes its
	// table against, so a visible toast never pushes the action bar off-screen.
	// Value receiver: this shrinks only the frame being rendered (BOS-506).
	a.reserveHeight(a.toast.Height(a.width))

	v := a.renderActiveView()

	// Prepend the banner to every screen except during tea.Exec (AttachModel
	// returns empty content while Claude Code owns the terminal).
	if v.Content != "" {
		v.Content = a.chromePrefix() + "\n" + v.Content
	}

	v.AltScreen = true
	// Enable terminal focus reporting so Home can auto-open a chat that started
	// waiting while boss was in the background, the moment the user clicks its
	// OS notification and the terminal regains focus (BOS-459).
	v.ReportFocus = true
	return v
}
