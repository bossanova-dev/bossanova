package views

import (
	"net/url"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/bossalib/config"
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
func (a App) handleSwitchView(msg switchViewMsg) (tea.Model, tea.Cmd) {
	a.activeView = msg.view
	switch msg.view { //nolint:exhaustive // ViewBugReport is pushed via ctrl+b, not switchViewMsg
	case ViewNewSession:
		cmd := a.enterNewSession()
		return a, cmd
	case ViewChatPicker:
		cmd := a.enterChatPicker(msg)
		return a, cmd
	case ViewRepoAdd:
		cmd := a.enterRepoAdd(msg)
		return a, cmd
	case ViewRepoList:
		a.repoList = NewRepoListModel(a.client, a.ctx)
		a.repoList.returnView = msg.returnView
		a.repoList.width = a.width
		a.repoList.height = a.height
		return a, a.repoList.Init()
	case ViewRepoSettings:
		cmd := a.enterRepoSettings(msg)
		return a, cmd
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
		cmd := a.enterSettings()
		return a, cmd
	case ViewGeneralSettings:
		a.generalSettings = NewGeneralSettingsModel(a.client, a.ctx)
		a.generalSettings.width = a.width
		return a, a.generalSettings.Init()
	case ViewAttach:
		a.attach = NewAttachModel(a.client, a.ctx, a.ptyManager, msg.sessionID, msg.resumeID)
		a.attach.SetTelemetry(a.telemetry)
		a.attach.SetOverrideAgent(msg.agentName)
		return a, a.attach.Init()
	case ViewLogin:
		cmd := a.enterLogin()
		return a, cmd
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
}

func (a *App) enterNewSession() tea.Cmd {
	a.newSession = a.newSessionModel()
	a.newSession.width = a.width
	// Seeded alongside width, like every other height-owning view.
	// Without it newSession.height stays 0 until the user happens to
	// resize while already in this view, which makes its three
	// tableHeight() calls take clampedTableHeight's "terminal height
	// unknown" branch — uncapped — so reserveHeight (app_view.go)
	// silently does nothing here (BOS-506).
	a.newSession.height = a.height
	return a.newSession.Init()
}

func (a *App) enterChatPicker(msg switchViewMsg) tea.Cmd {
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
	return a.chatPicker.Init()
}

func (a *App) enterRepoAdd(msg switchViewMsg) tea.Cmd {
	a.repoAdd = a.newRepoAddModel()
	a.repoAdd.width = a.width
	a.repoAdd.height = a.height
	if msg.firstRepo {
		// Adding the first repo from the home empty state: return to the
		// home empty state on cancel rather than the repo list.
		a.repoAdd.returnHomeOnCancel = true
	}
	return a.repoAdd.Init()
}

func (a *App) enterRepoSettings(msg switchViewMsg) tea.Cmd {
	a.repoSettings = NewRepoSettingsModel(a.client, a.ctx, msg.sessionID)
	if githubAppClient, ok := a.cloudAccess.(GitHubAppClient); ok {
		a.repoSettings.SetGitHubAppInstall(githubAppClient)
	}
	a.repoSettings.width = a.width
	return a.repoSettings.Init()
}

func (a *App) enterSettings() tea.Cmd {
	selectedKey := a.settings.selectedKey()
	a.settings = NewSettingsModel(a.client, a.ctx, a.auth)
	if a.cloudAccess != nil {
		a.settings.SetCloudAccess(a.cloudAccess, accountSettingsReturnURL(a.checkoutReturnURL))
	}
	a.settings.restoreSection(selectedKey)
	a.settings.width = a.width
	return a.settings.Init()
}

func (a *App) enterLogin() tea.Cmd {
	a.login = NewLoginModel(a.auth, a.client, a.ctx)
	a.login.SetAfterAuth(a.afterAuth)
	if a.cloudAccess != nil {
		a.login.SetCloudSubscription(a.cloudAccess, a.checkoutReturnURL, a.checkoutCancelURL)
		a.login.SetSubscriptionURL(a.subscriptionURL)
	}
	a.login.width = a.width
	return a.login.Init()
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
	// Preserve the prior poll snapshot so an existing question remains
	// acknowledged after Home is rebuilt. Copy the slice because the replacement
	// model owns future poll updates.
	home.sessions = append([]*pb.Session(nil), a.home.sessions...)
	// Preserve all optimistic archive state across rebuilds without sharing map
	// storage with the prior model. Both lingering success overrides and active
	// RPCs must survive until the next poll or result reconciles each session.
	home.archivingOverrideIDs = cloneSessionIDSet(a.home.archivingOverrideIDs)
	home.archiveInFlightIDs = cloneSessionIDSet(a.home.archiveInFlightIDs)
	// Preserve focus state and any pending question so a rebuild between the
	// notification and the user's click doesn't drop the auto-open (BOS-459).
	home.focused = a.home.focused
	home.pendingAttentionSessionID = a.home.pendingAttentionSessionID
	return home
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
