package tui

import (
	"context"
	"maps"
	"net/url"

	"github.com/recurser/boss/api/tui/attach"
	"github.com/recurser/boss/internal/attachprocess"

	"github.com/recurser/boss/api/tui/chatpicker"

	"github.com/recurser/boss/api/tui/newsession"

	"github.com/recurser/boss/api/tui/settings"

	"github.com/recurser/boss/api/tui/repos"

	"github.com/recurser/boss/api/tui/accounts"

	"github.com/recurser/boss/api/tui/ui"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/bossalib/kit/config"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// handleSwitchView builds the destination view's model and returns its Init.
//
// Arms whose construction is more than a few lines delegate to an enter<View>
// method; trivial arms stay inline.
//
// The enter<View> arms bind the cmd to a local before returning rather than
// writing `return a, a.enterX()`. That matters: enterX has a POINTER receiver
// and mutates the `a` being returned, and the Go spec orders function calls
// left-to-right but leaves the order of a call relative to the evaluation of
// other operands in the same return statement unspecified. gc happens to read
// `a` after the call, so both forms work today — binding first makes the
// dependency explicit instead of resting on that.
//
// This is NOT yet a package-wide convention: the pre-existing
// `return a, a.switchToHome()` and `return a, a.switchToReturn(...)` sites in
// app_delegate.go and app_handlers.go are the identical shape and were left
// alone, since sweeping them is out of scope for a behaviour-preserving split.
// They are correct today for the same reason these were. Do not read the rule
// above as already holding everywhere.
func (a App) handleSwitchView(msg ui.SwitchViewMsg) (tea.Model, tea.Cmd) {
	a.activeView = msg.View
	switch msg.View { //nolint:exhaustive // ViewBugReport is pushed via ctrl+g, not switchViewMsg
	case ui.ViewNewSession:
		if msg.SessionID != "" {
			cmd := a.enterSessionSetup(msg)
			return a, cmd
		}
		cmd := a.enterNewSession()
		return a, cmd
	case ui.ViewChatPicker:
		cmd := a.enterChatPicker(msg)
		return a, cmd
	case ui.ViewRepoAdd:
		cmd := a.enterRepoAdd(msg)
		return a, cmd
	case ui.ViewRepoList:
		a.repoList = repos.NewRepoListModel(a.client, a.ctx)
		a.repoList.SetReturnView(msg.ReturnView)
		a.repoList.SetSize(a.width, a.height)
		return a, a.repoList.Init()
	case ui.ViewRepoSettings:
		cmd := a.enterRepoSettings(msg)
		return a, cmd
	case ui.ViewSessionSettings:
		a.sessionSettings = settings.NewSessionSettingsModel(a.client, a.ctx, msg.SessionID)
		a.sessionSettings.SetWidth(a.width)
		return a, a.sessionSettings.Init()
	case ui.ViewTrash:
		a.trash = NewTrashModel(a.client, a.ctx)
		a.trash.SetTelemetry(a.telemetry)
		a.trash.SetReturnView(msg.ReturnView)
		a.trash.SetSize(a.width, a.height)
		return a, a.trash.Init()
	case ui.ViewSettings:
		cmd := a.enterSettings()
		return a, cmd
	case ui.ViewGeneralSettings:
		a.generalSettings = settings.NewGeneralSettingsModel(a.client, a.ctx, settingsMCPTokenManager{})
		a.generalSettings.SetWidth(a.width)
		return a, a.generalSettings.Init()
	case ui.ViewAttach:
		a.attach = attach.NewModel(a.client, a.ctx, a.ptyManager, msg.SessionID, msg.ResumeID, attachprocess.OS{})
		a.attach.SetTelemetry(a.telemetry)
		a.attach.SetOverrideAgent(msg.AgentName)
		a.attach.SetArmedNote(msg.ArmedNote)
		return a, a.attach.Init()
	case ui.ViewLogin:
		cmd := a.enterLogin()
		return a, cmd
	case ui.ViewHome:
		a.home = a.newHomeModel()
		return a, a.home.Init()
	case ui.ViewCron:
		a.cronList = NewCronListModel(a.client, a.ctx)
		a.cronList.SetTelemetry(a.telemetry)
		a.cronList.SetReturnView(msg.ReturnView)
		a.cronList.SetSize(a.width, a.height)
		return a, a.cronList.Init()
	case ui.ViewCronForm:
		a.cronForm = NewCronFormModel(a.client, a.ctx)
		a.cronForm.SetTelemetry(a.telemetry)
		a.cronForm.SetSize(a.width, a.height)
		return a, a.cronForm.Init()
	case ui.ViewAgentAccounts:
		a.accountsList = accounts.NewAgentAccountsListModel(a.client, a.ctx)
		a.accountsList.SetTelemetry(a.telemetry)
		a.accountsList.SetReturnView(msg.ReturnView)
		a.accountsList.SetSize(a.width, a.height)
		return a, a.accountsList.Init()
	case ui.ViewAgentAccountEdit:
		a.accountEdit = accounts.NewAgentAccountEditModel(a.client, a.ctx, msg.Account)
		a.accountEdit.SetTelemetry(a.telemetry)
		a.accountEdit.SetReturnView(msg.ReturnView)
		a.accountEdit.SetSize(a.width, a.height)
		return a, a.accountEdit.Init()
	case ui.ViewAgentAccountRegister:
		a.accountRegister = accounts.NewAgentAccountRegisterModel(a.client, a.ctx)
		a.accountRegister.SetTelemetry(a.telemetry)
		a.accountRegister.SetReturnView(msg.ReturnView)
		a.accountRegister.SetSize(a.width, a.height)
		if msg.ReauthAccountID != "" {
			// In-place reauthentication skips the provider chooser: the account
			// already names its provider, and offering a choice here would let
			// an operator overwrite a codex credential with a claude one.
			var cmd tea.Cmd
			a.accountRegister, cmd = a.accountRegister.BeginReauth(msg.ReauthAccountID)
			return a, cmd
		}
		return a, a.accountRegister.Init()
	}
	return a, nil
}

func (a *App) enterNewSession() tea.Cmd {
	a.newSession = a.newSessionModel()
	a.newSession.SetSize(a.width, a.height)
	// Seeded alongside width, like every other height-owning view.
	// Without it newSession.Height() stays 0 until the user happens to
	// resize while already in this view, which makes its three
	// tableHeight() calls take clampedTableHeight's "terminal height
	// unknown" branch — uncapped — so reserveHeight (app_view.go)
	// silently does nothing here (BOS-506).

	return a.newSession.Init()
}

func (a *App) enterSessionSetup(msg ui.SwitchViewMsg) tea.Cmd {
	var session *pb.Session
	for _, candidate := range a.home.Sessions {
		if candidate.GetId() == msg.SessionID {
			session = candidate
			break
		}
	}
	if session == nil {
		session = &pb.Session{Id: msg.SessionID}
	}
	a.newSession = newsession.NewSetupModel(a.client, a.ctx, session)
	a.newSession.SetSize(a.width, a.height)
	return a.newSession.Init()
}

func (a *App) enterChatPicker(msg ui.SwitchViewMsg) tea.Cmd {
	a.chatPicker = a.newChatPickerModel(msg.SessionID, msg.ChatID)
	// Only seed the picker's key-swallowing archiving flag when the
	// archive is still actually in flight. After a successful archive the
	// override lingers on the still-present row for rendering, but no
	// chatpicker.ArchiveResultMsg is outstanding for a freshly created picker, so
	// seeding archiving=true here would leave it stuck on "Archiving
	// session…" swallowing every key but Esc.
	if a.home.ArchiveInFlight(msg.SessionID) {
		a.chatPicker.SetArchiving(true)
	}
	if merge, ok := a.home.MergeInFlight(msg.SessionID); ok {
		a.chatPicker.SetAttentionAction(merge)
	}
	return a.chatPicker.Init()
}

func (a *App) enterRepoAdd(msg ui.SwitchViewMsg) tea.Cmd {
	a.repoAdd = a.newRepoAddModel()
	a.repoAdd.SetSize(a.width, a.height)
	if msg.FirstRepo {
		// Adding the first repo from the home empty state: return to the
		// home empty state on cancel rather than the repo list.
		a.repoAdd.SetReturnHomeOnCancel(true)
	}
	return a.repoAdd.Init()
}

func (a *App) enterRepoSettings(msg ui.SwitchViewMsg) tea.Cmd {
	a.repoSettings = repos.NewRepoSettingsModel(a.client, a.ctx, msg.SessionID)
	a.repoSettings.SetGitHubActions(repoGitHubActions{})
	if githubAppClient, ok := a.cloudAccess.(repos.GitHubAppClient); ok {
		a.repoSettings.SetGitHubAppInstall(githubAppClient)
	}
	// The organization mapping lives in bosso, so the field only exists for a
	// user who can actually reach it. a.home.LoggedIn is the cached auth state
	// the rest of the app already routes on — reading it costs nothing, whereas
	// asking the auth manager here would put a keyring read on a keystroke.
	//
	// A local-only or signed-out user therefore never grows an organization
	// field at all, rather than growing a row that can only ever report "not
	// logged in". Sign-in is the only condition applied here; a signed-in user
	// who belongs to no organization still gets the field, for the reason
	// organizationRowVisible spells out.
	if a.home.LoggedIn {
		if orgClient, ok := a.cloudAccess.(repos.RepoOrganizationClient); ok {
			a.repoSettings.SetRepoOrganizationClient(orgClient)
		}
	}
	a.repoSettings.SetWidth(a.width)
	return a.repoSettings.Init()
}

func (a *App) enterSettings() tea.Cmd {
	selectedKey := a.settings.SelectedKey()
	a.settings = settings.NewModel(a.ctx, a.auth)
	if a.cloudAccess != nil {
		a.settings.SetCloudAccess(a.cloudAccess, accountSettingsReturnURL(a.checkoutReturnURL))
	}
	a.settings.RestoreSection(selectedKey)
	a.settings.SetWidth(a.width)
	return a.settings.Init()
}

func (a *App) enterLogin() tea.Cmd {
	a.login = NewLoginModel(a.auth, a.client, a.ctx)
	a.login.SetAfterAuth(a.afterAuth)
	a.login.SetAuthChangeQueue(a.authChanges)
	a.login.SetTelemetry(a.telemetry)
	if a.cloudAccess != nil {
		a.login.SetCloudSubscription(a.cloudAccess, a.checkoutReturnURL, a.checkoutCancelURL)
		a.login.SetSubscriptionURL(a.subscriptionURL)
	}
	a.login.SetWidth(a.width)
	return a.login.Init()
}

func (a App) newSessionModel() newsession.Model {
	m := newsession.NewModel(a.client, a.ctx)
	m.SetTelemetry(a.telemetry)
	settings, err := config.Load()
	if err == nil {
		m.SetAgentSettings(settings)
	}
	m.SetAgentSelectionHandler(saveDefaultAgent)
	return m
}

// newChatPickerModel builds a chat picker with the App-level wiring every entry
// point needs: telemetry, terminal size, and the agent-picker default plus the
// handler that persists a confirmed pick. Centralised because the picker is
// re-created from four places (enter, and the returns from session settings,
// trash restore and detach) and a site that forgot the agent wiring would
// silently reintroduce the stuck-default bug on that one path.
func (a App) newChatPickerModel(sessionID, highlightAgentSessionID string) chatpicker.Model {
	m := chatpicker.NewModel(a.client, a.ctx, sessionID, highlightAgentSessionID, chatpicker.ExternalActions{ChatTitle: agentChatTitle, SupportsNewTab: hasNewTabSupport(), OpenNewTab: openInNewTab, OpenURL: func(url string) error { return openURLFunc(url) }})
	m.SetTelemetry(a.telemetry)
	// A settings read that fails leaves preferredAgent empty, which falls back
	// to the session's own agent — the pre-fix behaviour, not a crash.
	if settings, err := config.Load(); err == nil {
		m.SetPreferredAgent(settings.DefaultAgent)
		m.SetShowOpenPorts(settings.DisplayOpenPorts)
	}
	m.SetAgentSelectionHandler(saveDefaultAgent)
	m.SetSize(a.width, a.height)
	return m
}

func (a App) newRepoAddModel() repos.RepoAddModel {
	m := repos.NewRepoAddModel(a.client, a.ctx)
	m.SetGitHubActions(repoGitHubActions{})
	if githubAppClient, ok := a.cloudAccess.(repos.GitHubAppClient); ok {
		m.SetGitHubAppInstall(a.auth, githubAppClient)
	}
	return m
}

func (a *App) switchToHome() tea.Cmd {
	highlightID := a.home.SelectedSessionID()
	a.activeView = ui.ViewHome
	a.home = a.newHomeModel()
	a.home.HighlightSessionID = highlightID
	return a.home.Init()
}

// switchToReturn routes back to the view a sub-view was opened from. ViewHome
// (the zero value) uses the full switchToHome rebuild; ViewSettings re-enters
// Settings via the normal init path. Any other value falls back to Home.
func (a *App) switchToReturn(v ui.View) tea.Cmd {
	switch v {
	case ui.ViewSettings:
		return func() tea.Msg { return ui.SwitchViewMsg{View: ui.ViewSettings} }
	case ui.ViewAgentAccounts:
		// Rebuild the accounts list (its Init re-fetches via ListAgentAccounts) so an
		// edit made in the account-edit form is reflected on return, and route
		// the rebuilt list's own esc back to Settings (its only entry point).
		return func() tea.Msg { return ui.SwitchViewMsg{View: ui.ViewAgentAccounts, ReturnView: ui.ViewSettings} }
	default:
		return a.switchToHome()
	}
}

func (a *App) newHomeModel() HomeModel {
	home := NewHomeModel(a.client, a.ctx, a.auth, a.homeRuntime)
	home.Filter.SeedApplied(a.home.Filter.Query())
	home.AuthChanges = a.authChanges
	home.StartedAt = a.startedAt
	home.SetSettings(a.userSettings)
	home.SetCloudSubscription(a.cloudAccess, a.checkoutReturnURL, a.checkoutCancelURL)
	home.Width = a.width
	home.Height = a.height
	// Preserve the prior poll snapshot so an existing question remains
	// acknowledged after Home is rebuilt. Copy the slice because the replacement
	// model owns future poll updates.
	home.Sessions = append([]*pb.Session(nil), a.home.Sessions...)
	// The parks the board already showed travel with that snapshot, or a park
	// revealed by dismissing an alert would notify again. Sharing the map is
	// safe: rememberShownParks builds a fresh one on every poll.
	home.ShownParks = a.home.ShownParks
	home.ToldAttention = a.home.ToldAttention
	// Preserve all optimistic archive state across rebuilds without sharing map
	// storage with the prior model. Both lingering success overrides and active
	// RPCs must survive until the next poll or result reconciles each session.
	home.ArchivingOverrideIDs = cloneSessionIDSet(a.home.ArchivingOverrideIDs)
	home.ArchiveInFlightIDs = cloneSessionIDSet(a.home.ArchiveInFlightIDs)
	home.MergesInFlight = maps.Clone(a.home.MergesInFlight)
	if a.home.StatusUnseen {
		home.Status, home.StatusKind = a.home.Status, a.home.StatusKind
	}
	// Same reason for the reorder override (BOS-1231): home.Sessions above
	// already carries the optimistically swapped order, so dropping the
	// override that defends it would let the next poll snap the row back to its
	// pre-move position and then jump again when the write lands. The in-flight
	// count and the queue behind it travel with it — an outstanding reply must
	// not arrive against a model that believes nothing is in flight, and a
	// queued chord the board is already showing must still be sent. Copy the
	// slices rather than sharing them, as with the archive sets.
	home.MoveOverrideOrder = append([]string(nil), a.home.MoveOverrideOrder...)
	home.MoveInFlight = a.home.MoveInFlight
	home.MovePending = append([]queuedMove(nil), a.home.MovePending...)
	// A logout command can still be waiting on the credential lock while the
	// user visits another view. Preserve its state so rebuilding Home cannot
	// expose a second logout action or accept a stale auth-status result.
	home.LogoutPending = a.home.LogoutPending
	home.AuthStatusGeneration = a.home.AuthStatusGeneration
	home.LogoutError = a.home.LogoutError
	home.LoggedIn = a.home.LoggedIn
	home.LoggedInEmail = a.home.LoggedInEmail
	// Preserve retained re-login state so a Home rebuild cannot blank the
	// warning or resurrect the guest cloud promo while auth remains paused.
	home.NeedsRelogin = a.home.NeedsRelogin
	home.ReloginReason = a.home.ReloginReason
	// Preserve focus state and any pending question so a rebuild between the
	// notification and the user's click doesn't drop the auto-open (BOS-459).
	home.Focused = a.home.Focused
	home.PendingAttentionSessionID = a.home.PendingAttentionSessionID
	// Carry the supervision probe and its last answer (BOS-1520), or the
	// unsupervised-daemon line flickers off every time the user returns to
	// Home until the rebuilt model's own probe lands.
	home.SetDaemonSupervisionProbe(a.supervisionProbe)
	home.Supervision = a.home.Supervision
	home.SupervisionSince = a.home.SupervisionSince
	home.SupervisionSincePID = a.home.SupervisionSincePID
	return home
}

// resumeTickCmd returns a tickCmd for views whose status refresh depends on a
// self-perpetuating tick chain. The bug-report modal swallows tickMsg while
// it is active, so the chain needs restarting when the modal dismisses back
// to one of these views. Returns nil for views that don't use ticks.
func resumeTickCmd(v ui.View) tea.Cmd {
	switch v { //nolint:exhaustive // only tick-driven views participate
	case ui.ViewHome, ui.ViewChatPicker:
		return tickCmd()
	}
	return nil
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

var openGitHubAppInstallURL = openURLFunc

type repoGitHubActions struct{}

func (repoGitHubActions) OpenURL(url string) error { return openGitHubAppInstallURL(url) }
func (repoGitHubActions) EnrichInstallURL(ctx context.Context, url, origin string) string {
	return enrichGitHubAppInstallURL(ctx, url, origin)
}
