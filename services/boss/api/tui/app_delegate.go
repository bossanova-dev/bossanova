package tui

import (
	"github.com/recurser/boss/api/tui/accounts"
	"github.com/recurser/boss/api/tui/attach"
	"github.com/recurser/boss/api/tui/repos"
	"github.com/recurser/boss/api/tui/settings"
	"github.com/recurser/boss/api/tui/ui"
	"github.com/recurser/boss/internal/attachprocess"

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
	case ui.ViewOnboarding:
		return a.updateOnboarding(msg)
	case ui.ViewHome:
		return a.updateHome(msg)
	case ui.ViewNewSession:
		return a.updateNewSession(msg)
	case ui.ViewChatPicker:
		return a.updateChatPicker(msg)
	case ui.ViewRepoAdd:
		return a.updateRepoAdd(msg)
	case ui.ViewRepoList:
		return a.updateRepoList(msg)
	case ui.ViewRepoSettings:
		return a.updateRepoSettings(msg)
	case ui.ViewSessionSettings:
		return a.updateSessionSettings(msg)
	case ui.ViewTrash:
		return a.updateTrash(msg)
	case ui.ViewSettings:
		return a.updateSettings(msg)
	case ui.ViewGeneralSettings:
		return a.updateGeneralSettings(msg)
	case ui.ViewAttach:
		return a.updateAttach(msg)
	case ui.ViewLogin:
		return a.updateLogin(msg)
	case ui.ViewBugReport:
		return a.updateBugReport(msg)
	case ui.ViewCron:
		return a.updateCron(msg)
	case ui.ViewCronForm:
		return a.updateCronForm(msg)
	case ui.ViewAgentAccounts:
		return a.updateAccounts(msg)
	case ui.ViewAgentAccountEdit:
		return a.updateAccountEdit(msg)
	case ui.ViewAgentAccountRegister:
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
	a.userSettings = a.home.Settings
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
			a.attach = attach.NewModel(a.client, a.ctx, a.ptyManager, sess.Id, "", attachprocess.OS{})
			a.attach.SetTelemetry(a.telemetry)
			a.activeView = ui.ViewAttach
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
		sessionID := a.chatPicker.SessionID()
		merged := a.chatPicker.Merged()
		// On archive the session disappears from the list, so highlight the
		// session that fills its place (the next one down, or the previous
		// one if it was last) to keep the cursor where it was instead of
		// jumping back to the top. Cancel keeps highlighting the
		// session itself since it remains in the list. Computed against the
		// pre-archive list still held by a.home before it is rebuilt.
		highlightID := sessionID
		if a.chatPicker.Archived() {
			highlightID = a.home.NeighborSessionID(sessionID)
		}
		a.activeView = ui.ViewHome
		a.home = a.newHomeModel()
		a.home.HighlightSessionID = highlightID
		if merged {
			a.home.MergedOptimisticID = sessionID
		}
		if archivingID := a.chatPicker.ArchivingSessionID(); archivingID != "" {
			a.home.MarkArchiving(archivingID)
		}
		if merge, ok := a.chatPicker.MergeInFlight(); ok {
			a.home.MarkMerging(merge)
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
		if a.repoAdd.CreatedRepo() != nil {
			highlightID = a.repoAdd.CreatedRepo().Id
		}
		a.repoAddCompleting = true
		return a, tea.Batch(cmd, fetchReposAfterRepoAdd(a.client, a.ctx, highlightID))
	}
	if a.repoAdd.Cancelled() {
		if a.repoAdd.ReturnHomeOnCancel() {
			return a, a.switchToHome()
		}
		returnView := a.repoList.ReturnView()
		var highlightID string
		if cursor := a.repoList.Table().Cursor(); cursor >= 0 && cursor < len(a.repoList.Repos()) {
			highlightID = a.repoList.Repos()[cursor].Id
		}
		a.repoList = repos.NewRepoListModel(a.client, a.ctx)
		a.repoList.SetReturnView(returnView)
		a.repoList.SetHighlightRepoID(highlightID)
		a.repoList.SetSize(a.width, a.height)
		a.activeView = ui.ViewRepoList
		return a, a.repoList.Init()
	}
	return a, cmd
}

func (a App) updateRepoList(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := updateSub(&a.repoList, msg)
	if a.repoList.Cancelled() {
		return a, a.switchToReturn(a.repoList.ReturnView())
	}
	return a, cmd
}

func (a App) updateRepoSettings(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := updateSub(&a.repoSettings, msg)
	if a.repoSettings.Cancelled() || a.repoSettings.Done() {
		// Return to repo list, highlighting the repo we came from.
		returnView := a.repoList.ReturnView()
		a.repoList = repos.NewRepoListModel(a.client, a.ctx)
		a.repoList.SetReturnView(returnView)
		a.repoList.SetHighlightRepoID(a.repoSettings.RepoID())
		a.repoList.SetSize(a.width, a.height)
		a.activeView = ui.ViewRepoList
		return a, a.repoList.Init()
	}
	return a, cmd
}

func (a App) updateSessionSettings(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := updateSub(&a.sessionSettings, msg)
	if a.sessionSettings.Cancelled() || a.sessionSettings.Done() {
		// Return to chat picker, highlighting the chat we came from.
		highlightID := a.chatPicker.SelectedChatID()
		a.chatPicker = a.newChatPickerModel(a.sessionSettings.SessionID(), highlightID)
		a.activeView = ui.ViewChatPicker
		return a, a.chatPicker.Init()
	}
	return a, cmd
}

func (a App) updateTrash(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := updateSub(&a.trash, msg)
	if sessionID := a.trash.RestoredSessionID(); sessionID != "" {
		a.chatPicker = a.newChatPickerModel(sessionID, "")
		a.activeView = ui.ViewChatPicker
		return a, a.chatPicker.Init()
	}
	if a.trash.Cancelled() {
		return a, a.switchToReturn(a.trash.ReturnView())
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
	if m, ok := updated.(settings.GeneralSettingsModel); ok {
		a.generalSettings = m
		if m.Err() == nil {
			a.userSettings = m.Settings()
			a.home.SetSettings(m.Settings())
		}
	}
	if a.generalSettings.Cancelled() {
		// The General list's parent is always the Settings hub, so it needs
		// no returnView slot (BOS-511).
		return a, func() tea.Msg { return ui.SwitchViewMsg{View: ui.ViewSettings} }
	}
	return a, cmd
}

func (a App) updateAttach(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := updateSub(&a.attach, msg)
	if a.attach.Detached() {
		sessionID := a.attach.SessionID()
		agentSessionID := a.attach.AgentSessionID()
		a.chatPicker = a.newChatPickerModel(sessionID, agentSessionID)
		a.activeView = ui.ViewChatPicker
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
	if a.login.Verification().Outcome != auth.LoginVerified {
		return
	}
	// A successful login immediately supersedes the retained re-login warning.
	// Clear it before Home is rebuilt; the follow-up auth poll will refresh the
	// signed-in snapshot. Bump the generation so stale auth-status commands from
	// the pre-login Home cannot restore the retained warning after this point.
	a.home.NeedsRelogin = false
	a.home.ReloginReason = ""
	a.home.AuthStatusGeneration++
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
		return a, a.switchToReturn(a.cronList.ReturnView())
	}
	// cronFormOpenMsg is emitted by CronListModel as a Cmd; when it
	// arrives here as a message we open the cron form.
	if ofm, ok := msg.(cronFormOpenMsg); ok {
		a.activeView = ui.ViewCronForm
		a.cronForm = NewCronFormModel(a.client, a.ctx)
		a.cronForm.SetTelemetry(a.telemetry)
		a.cronForm.SetJob(ofm.Job)
		a.cronForm.SetSize(a.width, a.height)
		return a, a.cronForm.Init()
	}
	return a, cmd
}

func (a App) updateCronForm(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := updateSub(&a.cronForm, msg)
	if a.cronForm.Cancelled() {
		// Return to cron list without refreshing (user cancelled).
		returnView := a.cronList.ReturnView()
		a.activeView = ui.ViewCron
		a.cronList = NewCronListModel(a.client, a.ctx)
		a.cronList.SetTelemetry(a.telemetry)
		a.cronList.SetReturnView(returnView)
		a.cronList.SetSize(a.width, a.height)
		return a, a.cronList.Init()
	}
	// cronFormDoneMsg is emitted by CronFormModel as a Cmd; when it
	// arrives here as a message we return to the cron list and refresh.
	if doneMsg, ok := msg.(cronFormDoneMsg); ok {
		returnView := a.cronList.ReturnView()
		a.activeView = ui.ViewCron
		a.cronList = NewCronListModel(a.client, a.ctx)
		a.cronList.SetTelemetry(a.telemetry)
		a.cronList.SetReturnView(returnView)
		a.cronList.SetSize(a.width, a.height)
		// Keep the just-saved (edited or created) job highlighted once the
		// recreated list loads its jobs. Empty jobID falls back to row 0.
		a.cronList.HighlightJob(doneMsg.JobID)
		return a, a.cronList.Init()
	}
	return a, cmd
}

func (a App) updateAccounts(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := updateSub(&a.accountsList, msg)
	if a.accountsList.Cancelled() {
		return a, a.switchToReturn(a.accountsList.ReturnView())
	}
	// Re-stamp the carried reauthentication verdict once the rebuilt list has
	// finished loading. The toast is a 5-second transient whose clock starts
	// when it is set, and it is set at the moment the register view pops —
	// BEFORE ListAgentAccounts is even dialled, while View() is still in its loading
	// branch, which returns early and renders no status at all. Without this the
	// only surface carrying the daemon's post-save verification verdict can
	// spend its whole window behind a spinner and expire unseen. Stamping it
	// again here starts the clock when the operator can actually read it.
	//
	// A FAILED load consumes nothing. accounts.AgentAccountsListModel.View returns its
	// full-screen error before it reaches the status line, so a verdict stamped
	// onto a failed load is written to a surface that renders none of it, and
	// clearing the marker there would discard the daemon's verification verdict
	// permanently — the operator would be left with a dial error and no idea
	// whether the credential they just replaced actually works. Holding the
	// marker instead costs nothing: the accounts list reloads on [r] and on
	// re-entry, and the first load that succeeds re-stamps the verdict onto a
	// screen that can show it. It is not held forever either — a fresh
	// accounts.AgentAccountRegisterModel is built on every entry to ViewAgentAccountRegister
	// (handleSwitchView), so an unread verdict dies with the next flow.
	if loaded, ok := msg.(accounts.AgentAccountsLoadedMsg); ok && loaded.Err == nil && a.accountRegister.ReauthAgentAccountID() != "" {
		if detail := a.accountRegister.FlowVerdictLine(); detail != "" {
			a.accountsList.SetStatus(detail, !accountflow.ReauthLineIsVerified(detail))
		}
		// One carry per reauthentication: clearing the marker keeps a later
		// visit to the accounts list from re-toasting a verdict the operator
		// has already read.
		a.accountRegister.ClearReauthAgentAccountID()
	}
	return a, cmd
}

func (a App) updateAccountEdit(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := updateSub(&a.accountEdit, msg)
	if a.accountEdit.Cancelled() {
		return a, a.switchToReturn(a.accountEdit.ReturnView())
	}
	return a, cmd
}

func (a App) updateAccountRegister(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := updateSub(&a.accountRegister, msg)
	if a.accountRegister.Cancelled() {
		return a, a.switchToReturn(a.accountRegister.ReturnView())
	}
	if a.accountRegister.Done() {
		// Registration succeeded: return to a rebuilt accounts list (its Init
		// re-fetches via ListAgentAccounts) so the new account appears, routing its
		// own esc back to Settings (mirrors the ViewCronForm→refreshed-ViewCron
		// return). ViewAgentAccounts' only entry point is Settings.
		a.activeView = ui.ViewAgentAccounts
		a.accountsList = accounts.NewAgentAccountsListModel(a.client, a.ctx)
		a.accountsList.SetTelemetry(a.telemetry)
		a.accountsList.SetReturnView(ui.ViewSettings)
		a.accountsList.SetSize(a.width, a.height)
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
		if a.accountRegister.ReauthAgentAccountID() != "" {
			if detail := a.accountRegister.FlowVerdictLine(); detail != "" {
				a.accountsList.SetStatus(detail, !accountflow.ReauthLineIsVerified(detail))
			}
		}
		return a, a.accountsList.Init()
	}
	return a, cmd
}
