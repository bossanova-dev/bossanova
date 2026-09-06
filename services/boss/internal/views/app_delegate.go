package views

import (
	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/accountflow"
	"github.com/recurser/boss/internal/auth"
)

// updateSub routes msg to a sub-model stored on the App, writing the updated
// model back through dst. It replaces ~18 copies of the same type-assert dance.
//
// The assert is deliberately skip-on-mismatch, exactly like the hand-written
// arms it replaces: a sub-model whose Update returns some other tea.Model
// leaves dst untouched. It must never panic and never force-assign.
func updateSub[T tea.Model](dst *T, msg tea.Msg) tea.Cmd {
	updated, cmd := (*dst).Update(msg)
	if m, ok := updated.(T); ok {
		*dst = m
	}
	return cmd
}

// delegateToActiveView routes msg to whichever sub-model is currently active.
//
// This switch deliberately carries NO default arm and NO exhaustive waiver.
// `.golangci.yml` enables the exhaustive linter with
// default-signifies-exhaustive, so either one would silently disable the guard.
// As written, adding a new View constant without wiring its arm here fails
// `make lint` — that compile-time gate is the reason this stayed a switch
// rather than becoming a map registry (BOS-529).
//
// The waiver directive is spelled out nowhere in this file on purpose: an audit
// of the package's waivers is a grep for that directive, and a mention inside
// this very doc comment would make the one file that must have none look like it
// has one.
func (a App) delegateToActiveView(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch a.activeView {
	case ViewOnboarding:
		return a.updateOnboarding(msg)
	case ViewHome:
		return a.updateHome(msg)
	case ViewNewSession:
		return a.updateNewSession(msg)
	case ViewChatPicker:
		return a.updateChatPicker(msg)
	case ViewRepoAdd:
		return a.updateRepoAdd(msg)
	case ViewRepoList:
		return a.updateRepoList(msg)
	case ViewRepoSettings:
		return a.updateRepoSettings(msg)
	case ViewSessionSettings:
		return a.updateSessionSettings(msg)
	case ViewTrash:
		return a.updateTrash(msg)
	case ViewSettings:
		return a.updateSettings(msg)
	case ViewGeneralSettings:
		return a.updateGeneralSettings(msg)
	case ViewAttach:
		return a.updateAttach(msg)
	case ViewLogin:
		return a.updateLogin(msg)
	case ViewBugReport:
		return a.updateBugReport(msg)
	case ViewCron:
		return a.updateCron(msg)
	case ViewCronForm:
		return a.updateCronForm(msg)
	case ViewAccounts:
		return a.updateAccounts(msg)
	case ViewAccountEdit:
		return a.updateAccountEdit(msg)
	case ViewAccountRegister:
		return a.updateAccountRegister(msg)
	}

	return a, nil
}

func (a App) updateOnboarding(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := updateSub(&a.onboarding, msg)
	if a.onboarding.Done() || a.onboarding.Cancelled() {
		return a, a.switchToHome()
	}
	return a, cmd
}

func (a App) updateHome(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := updateSub(&a.home, msg)
	// Propagate any settings the home model persisted (e.g. the
	// BossCloudValueDeliveredAt latch) back to the App, so later
	// newHomeModel() recreations re-seed from the latched value instead of
	// the stale startup snapshot. Without this, the latch would re-stamp on
	// every return-to-home (moving the "never moves" timestamp) and the
	// promo would revert once has_active_chat went false.
	a.userSettings = a.home.settings
	return a, cmd
}

func (a App) updateNewSession(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := updateSub(&a.newSession, msg)
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
}

func (a App) updateChatPicker(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := updateSub(&a.chatPicker, msg)
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
		return a, a.home.Init()
	}
	return a, cmd
}

func (a App) updateRepoAdd(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := updateSub(&a.repoAdd, msg)
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
}

func (a App) updateRepoList(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := updateSub(&a.repoList, msg)
	if a.repoList.Cancelled() {
		return a, a.switchToReturn(a.repoList.returnView)
	}
	return a, cmd
}

func (a App) updateRepoSettings(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := updateSub(&a.repoSettings, msg)
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
}

func (a App) updateSessionSettings(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := updateSub(&a.sessionSettings, msg)
	if a.sessionSettings.Cancelled() || a.sessionSettings.Done() {
		// Return to chat picker, highlighting the chat we came from.
		var highlightID string
		if cursor := a.chatPicker.table.Cursor(); cursor >= 0 && cursor < len(a.chatPicker.chats) {
			highlightID = a.chatPicker.chats[cursor].AgentSessionId
		}
		a.chatPicker = a.newChatPickerModel(a.sessionSettings.sessionID, highlightID)
		a.activeView = ViewChatPicker
		return a, a.chatPicker.Init()
	}
	return a, cmd
}

func (a App) updateTrash(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := updateSub(&a.trash, msg)
	if sessionID := a.trash.RestoredSessionID(); sessionID != "" {
		a.chatPicker = a.newChatPickerModel(sessionID, "")
		a.activeView = ViewChatPicker
		return a, a.chatPicker.Init()
	}
	if a.trash.Cancelled() {
		return a, a.switchToReturn(a.trash.returnView)
	}
	return a, cmd
}

func (a App) updateSettings(msg tea.Msg) (tea.Model, tea.Cmd) {
	// The hub is a menu of sections and holds no settings of its own —
	// the General list below owns the settings propagation (BOS-511).
	cmd := updateSub(&a.settings, msg)
	if a.settings.Cancelled() {
		return a, a.switchToHome()
	}
	return a, cmd
}

func (a App) updateGeneralSettings(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Deliberately NOT updateSub: the settings propagation below lives inside
	// the successful-assert branch, so it must not run when the sub-model's
	// Update returns some other tea.Model and a.generalSettings is left stale.
	updated, cmd := a.generalSettings.Update(msg)
	if m, ok := updated.(GeneralSettingsModel); ok {
		a.generalSettings = m
		if m.err == nil {
			a.userSettings = m.settings
			a.home.SetSettings(m.settings)
		}
	}
	if a.generalSettings.Cancelled() {
		// The General list's parent is always the Settings hub, so it needs
		// no returnView slot (BOS-511).
		return a, func() tea.Msg { return switchViewMsg{view: ViewSettings} }
	}
	return a, cmd
}

func (a App) updateAttach(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := updateSub(&a.attach, msg)
	if a.attach.Detached() {
		sessionID := a.attach.SessionID()
		agentSessionID := a.attach.AgentSessionID()
		a.chatPicker = a.newChatPickerModel(sessionID, agentSessionID)
		a.activeView = ViewChatPicker
		// Batch the attach cleanup cmd (e.g. orphan delete) with the chat picker init.
		return a, tea.Batch(cmd, a.chatPicker.Init())
	}
	return a, cmd
}

func (a App) updateLogin(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := updateSub(&a.login, msg)
	if a.login.Cancelled() || a.login.Done() {
		a.clearRetainedReloginAfterVerifiedLogin()
		return a, a.switchToHome()
	}
	return a, cmd
}

func (a *App) clearRetainedReloginAfterVerifiedLogin() {
	if a.login.verification.Outcome != auth.LoginVerified {
		return
	}
	// A successful login immediately supersedes the retained re-login warning.
	// Clear it before Home is rebuilt; the follow-up auth poll will refresh the
	// signed-in snapshot. Bump the generation so stale auth-status commands from
	// the pre-login Home cannot restore the retained warning after this point.
	a.home.needsRelogin = false
	a.home.reloginReason = ""
	a.home.authStatusGeneration++
}

func (a App) updateBugReport(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := updateSub(&a.bugReport, msg)
	if a.bugReport.Cancelled() || a.bugReport.Done() {
		// Restore the prior view without recreating it so existing state
		// (table cursor, loaded data, spinners) is preserved. The prior
		// view's self-perpetuating tick chain was swallowed by the modal,
		// so restart it when the restored view depends on tickMsg.
		a.activeView = a.bugReport.PreviousView()
		return a, resumeTickCmd(a.activeView)
	}
	return a, cmd
}

func (a App) updateCron(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := updateSub(&a.cronList, msg)
	if a.cronList.Cancelled() {
		return a, a.switchToReturn(a.cronList.returnView)
	}
	// cronFormOpenMsg is emitted by CronListModel as a Cmd; when it
	// arrives here as a message we open the cron form.
	if ofm, ok := msg.(cronFormOpenMsg); ok {
		a.activeView = ViewCronForm
		a.cronForm = NewCronFormModel(a.client, a.ctx)
		a.cronForm.SetTelemetry(a.telemetry)
		a.cronForm.job = ofm.job
		a.cronForm.width = a.width
		a.cronForm.height = a.height
		return a, a.cronForm.Init()
	}
	return a, cmd
}

func (a App) updateCronForm(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := updateSub(&a.cronForm, msg)
	if a.cronForm.Cancelled() {
		// Return to cron list without refreshing (user cancelled).
		returnView := a.cronList.returnView
		a.activeView = ViewCron
		a.cronList = NewCronListModel(a.client, a.ctx)
		a.cronList.SetTelemetry(a.telemetry)
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
		a.cronList.SetTelemetry(a.telemetry)
		a.cronList.returnView = returnView
		a.cronList.width = a.width
		a.cronList.height = a.height
		// Keep the just-saved (edited or created) job highlighted once the
		// recreated list loads its jobs. Empty jobID falls back to row 0.
		a.cronList.highlightJobID = doneMsg.jobID
		return a, a.cronList.Init()
	}
	return a, cmd
}

func (a App) updateAccounts(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := updateSub(&a.accountsList, msg)
	if a.accountsList.Cancelled() {
		return a, a.switchToReturn(a.accountsList.returnView)
	}
	// Re-stamp the carried reauthentication verdict once the rebuilt list has
	// finished loading. The toast is a 5-second transient whose clock starts
	// when it is set, and it is set at the moment the register view pops —
	// BEFORE ListAccounts is even dialled, while View() is still in its loading
	// branch, which returns early and renders no status at all. Without this the
	// only surface carrying the daemon's post-save verification verdict can
	// spend its whole window behind a spinner and expire unseen. Stamping it
	// again here starts the clock when the operator can actually read it.
	//
	// A FAILED load consumes nothing. AccountsListModel.View returns its
	// full-screen error before it reaches the status line, so a verdict stamped
	// onto a failed load is written to a surface that renders none of it, and
	// clearing the marker there would discard the daemon's verification verdict
	// permanently — the operator would be left with a dial error and no idea
	// whether the credential they just replaced actually works. Holding the
	// marker instead costs nothing: the accounts list reloads on [r] and on
	// re-entry, and the first load that succeeds re-stamps the verdict onto a
	// screen that can show it. It is not held forever either — a fresh
	// AccountRegisterModel is built on every entry to ViewAccountRegister
	// (handleSwitchView), so an unread verdict dies with the next flow.
	if loaded, ok := msg.(accountsLoadedMsg); ok && loaded.err == nil && a.accountRegister.reauthAccountID != "" {
		if detail := a.accountRegister.flowVerdictLine(); detail != "" {
			a.accountsList.setStatus(detail, !accountflow.ReauthLineIsVerified(detail))
		}
		// One carry per reauthentication: clearing the marker keeps a later
		// visit to the accounts list from re-toasting a verdict the operator
		// has already read.
		a.accountRegister.reauthAccountID = ""
	}
	return a, cmd
}

func (a App) updateAccountEdit(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := updateSub(&a.accountEdit, msg)
	if a.accountEdit.Cancelled() {
		return a, a.switchToReturn(a.accountEdit.returnView)
	}
	return a, cmd
}

func (a App) updateAccountRegister(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := updateSub(&a.accountRegister, msg)
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
		a.accountsList.SetTelemetry(a.telemetry)
		a.accountsList.returnView = ViewSettings
		a.accountsList.width = a.width
		a.accountsList.height = a.height
		// An add is self-evidencing: the new row appears. A reauthentication is
		// not — it replaces a secret on a row that was already there, and this
		// pop is immediate, so the flow's own closing verdict (which is the only
		// place the daemon's post-save verification is reported) would otherwise
		// never reach a screen. Carry it onto the list the operator lands on
		// (BOS-1142).
		//
		// The tier is DERIVED from the verdict, never assumed: the flow's
		// closing line either asserts the new credential verified or says
		// "verification couldn't run", and reporting the second in the info tier
		// would dress an unverified credential as a clean result — the exact
		// fail-open this change exists to close. updateAccounts re-stamps this
		// once the list has loaded so the transient window is not spent behind
		// the loading spinner.
		if a.accountRegister.reauthAccountID != "" {
			if detail := a.accountRegister.flowVerdictLine(); detail != "" {
				a.accountsList.setStatus(detail, !accountflow.ReauthLineIsVerified(detail))
			}
		}
		return a, a.accountsList.Init()
	}
	return a, cmd
}
