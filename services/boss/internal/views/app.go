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
//   - Arms that call a plain pointer-receiver mutator (window size, archive
//     result) ALWAYS fall through — the mutation is bookkeeping, not
//     consumption. These two are the ONLY handlers with that shape; keeping it
//     that way is what lets a reader infer routing from the signature.
//
// Turning a fall-through arm into an unconditional return — or an always-returns
// arm into a fall-through — is a silent routing regression, not a compile error.
//
// app_fallthrough_test.go pins the fall-through direction for all four arms, and
// the consumption direction for the four that do not re-route (toastExpireMsg,
// heartbeatTickMsg, ctrl+c, and the stale-generation rejection);
// startSubscriptionFlowMsg is pinned by its own test in app_test.go. Three
// remain uncovered in the consumption direction — repoAddCompletedMsg,
// switchViewMsg and ctrl+b — because each re-routes activeView, which destroys
// the witness those tests rely on. Flipping one of those three to fall through
// is currently invisible.
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
		return a.handleToastExpire(msg)
	case sessionListMsg:
		if cmd, handled := a.handleSessionList(msg); handled {
			return a, cmd
		}
	case tea.WindowSizeMsg:
		a.handleWindowSize(msg)
	case tea.KeyMsg:
		if cmd, handled := a.handleGlobalKey(msg); handled {
			return a, cmd
		}
	case heartbeatTickMsg:
		return a, a.handleHeartbeat()
	case repoAddCompletedMsg:
		return a.handleRepoAddCompleted(msg)
	case archiveResultMsg:
		a.handleArchiveResult(msg)
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
