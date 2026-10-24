package tui

import (
	"context"

	"github.com/recurser/boss/api/tui/chatpicker"

	"github.com/recurser/boss/api/tui/repos"

	"github.com/recurser/boss/api/tui/accounts"

	"github.com/recurser/boss/api/tui/ui"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/bossalib/config"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// The handlers below come in three shapes, matching the three kinds of arm the
// original App.Update type switch had. The shape IS the contract — see the
// commentary on App.Update — so every handler must be readable as exactly one of
// them, with no exceptions for brevity:
//
//   - always-returns  → returns (tea.Model, tea.Cmd) or tea.Cmd; Update returns
//     it. handleToastExpire, handleHeartbeat, handleRepoAddCompleted,
//     handleStartSubscriptionFlow — plus handleSwitchView, which has this shape
//     but lives in app_routing.go next to the enter<View> methods it calls.
//   - may-fall-through → returns (tea.Cmd, bool); handled==false means "keep going
//     to delegateToActiveView". Pointer receiver, so partial state changes made
//     before the fall-through decision survive into the delegation.
//     handleSessionList, handleGlobalKey.
//   - never-returns   → returns nothing at all; Update always falls through.
//     A pointer receiver means it mutates App (handleWindowSize,
//     handleArchiveResult); a value receiver means it only observes — today,
//     emits telemetry (handleMergeResult, handleSwitchAccountResult). Those
//     four and ONLY those four. Giving any other handler this shape would make
//     the signature lie about the routing.

// handleToastExpire retires the rotation toast. Always returns: the expiry is
// the toast's own bookkeeping and no sub-model has any use for it.
//
// Shaped as always-returns (not a plain mutator) so it cannot be mistaken for
// one of the never-returns handlers in the taxonomy above.
func (a App) handleToastExpire(msg ui.ToastExpireMsg) (tea.Model, tea.Cmd) {
	a.toast, _ = a.toast.Update(msg)
	return a, nil
}

// handleSessionList reconciles App-level rotation state from a session poll.
//
// It reports handled==false for the fall-through case, where the poll must go
// on to reach the active view's own Update. Only the successful-poll-on-Home
// branch mutates anything, so both the stale-generation rejection and the
// fall-through leave the App untouched.
func (a *App) handleSessionList(msg sessionListMsg) (tea.Cmd, bool) {
	// A recreated HomeModel starts poll IDs at one again. Reject results
	// from the replaced model before they can mutate App-level rotation
	// state or Home's question rising-edge state.
	if msg.HomeGeneration != 0 && msg.HomeGeneration != a.home.Generation {
		return nil, true
	}
	// Surface a non-blocking toast when a new automatic rotation lands.
	// Seeded silently on first observation so a fresh TUI doesn't replay
	// history. Session rotation state only flows into the Home model, so
	// forward the message onward for its normal handling and batch the
	// toast command.
	if a.activeView == ui.ViewHome && msg.Err == nil {
		seen, toasts := detectNewRotationEvents(a.rotationSeen, msg.Sessions)
		a.rotationSeen = seen
		var toastCmd tea.Cmd
		if len(toasts) > 0 {
			a.toast, toastCmd = a.toast.Show(toasts[0])
		}
		cmd := updateSub(&a.home, msg)
		a.userSettings = a.home.Settings
		return tea.Batch(cmd, toastCmd), true
	}
	return nil, false
}

// handleWindowSize seeds the new terminal size onto the sub-models that read it.
// It never returns early: the resize must also reach the active view's own
// Update.
//
// The field list is deliberately NOT "every sub-model, both dimensions" — it is
// the set copied verbatim from the pre-BOS-529 arm. `attach` is absent
// entirely (it sizes itself from its own Update, as
// TestReserveHeightCoversEveryHeightOwningSubModel documents when it seeds
// attach.height by hand), and several views take width only. Widening this list
// to look "consistent" is therefore a behaviour change, not a cleanup.
func (a *App) handleWindowSize(msg tea.WindowSizeMsg) {
	a.width = msg.Width
	a.height = msg.Height
	a.home.Width = msg.Width
	a.home.Height = msg.Height
	a.newSession.SetSize(msg.Width, msg.Height)
	a.repoAdd.SetSize(msg.Width, msg.Height)
	a.repoList.SetSize(msg.Width, msg.Height)
	a.repoSettings.SetWidth(msg.Width)
	a.trash.SetSize(msg.Width, msg.Height)
	a.chatPicker.SetSize(msg.Width, msg.Height)
	a.settings.SetWidth(msg.Width)
	a.generalSettings.SetWidth(msg.Width)
	a.sessionSettings.SetWidth(msg.Width)
	a.login.SetWidth(msg.Width)
	a.bugReport.SetWidth(msg.Width)
	a.cronList.SetSize(msg.Width, msg.Height)
	a.cronForm.SetSize(msg.Width, msg.Height)
	a.accountsList.SetSize(msg.Width, msg.Height)
	a.accountEdit.SetSize(msg.Width, msg.Height)
	a.accountRegister.SetSize(msg.Width, msg.Height)
	a.onboarding.SetWidth(msg.Width)
}

// handleGlobalKey handles the app-global chords. Every other key — and either
// bug-report shortcut while the report is already open — reports handled==false
// so the key still reaches the active view.
func (a *App) handleGlobalKey(msg tea.KeyMsg) (tea.Cmd, bool) {
	switch msg.String() {
	case "ctrl+c":
		a.quitting = true
		return tea.Quit, true
	case "ctrl+g", "ctrl+b": // ctrl+b remains a deprecated alias for one release.
		if a.activeView == ui.ViewBugReport {
			break
		}
		if a.activeView == ui.ViewHome && (a.home.Upgrading || a.home.Restarting) {
			return nil, true
		}
		// Same rule as the Home guard above, for the two views that own an
		// uninterruptible in-flight operation (BOS-683).
		//
		// This chord is app-global: it is handled here, BEFORE delegation, and
		// reports handled==true, so a view's own input guard cannot block it.
		// Swapping activeView mid-operation means the eventual cronFormSavedMsg
		// / flowDoneMsg is delivered only to the bug-report model, which drops
		// it — losing the telemetry action AND leaving the retained view stuck
		// (submitting forever, or a registration waiting on a result that
		// already came and went). Suppressing the chord for the duration is the
		// cheaper half of the fix: rooting the capture would rescue the event
		// but not the stuck view.
		if a.activeView == ui.ViewCronForm && a.cronForm.Submitting() {
			return nil, true
		}
		if a.activeView == ui.ViewAgentAccountRegister && a.accountRegister.FlowRunning() {
			return nil, true
		}
		a.bugReport = NewBugReportModel(a.client, a.ctx, a.auth, a.activeView, a.currentSession(), a.currentDaemonStatuses())
		a.bugReport.SetWidth(a.width)
		a.activeView = ui.ViewBugReport
		return a.bugReport.Init(), true
	}
	return nil, false
}

// handleHeartbeat snapshots the local PTY manager and ships per-chat statuses
// to bossd so other clients (web UI, second TUI instance) see this client's
// "working/idle/question" state. Re-arms the ticker so the loop is
// self-perpetuating. Always returns.
func (a App) handleHeartbeat() tea.Cmd {
	return tea.Batch(
		sendHeartbeatsCmd(a.ctx, a.client, a.ptyManager),
		heartbeatTickCmd(),
	)
}

// handleRepoAddCompleted routes away from the add-repo wizard once the repo
// list has been re-fetched. Always returns.
func (a App) handleRepoAddCompleted(msg ui.RepoAddCompletedMsg) (tea.Model, tea.Cmd) {
	a.repoAddCompleting = false
	returnView := a.repoList.ReturnView()
	// A freshly-added repo configures its merge-strategy / automation /
	// integration options inline on the add wizard, so the completed add
	// returns to where it started: Home when there is at most one repo, else
	// the repo list with the new repo highlighted.
	if msg.Err == nil && len(msg.Repos) <= 1 {
		if returnView == ui.ViewSettings {
			return a, a.switchToReturn(returnView)
		}
		return a, a.switchToHome()
	}
	highlightID := msg.HighlightID
	if highlightID == "" {
		cursor := a.repoList.Table().Cursor()
		if cursor >= 0 && cursor < len(a.repoList.Repos()) {
			highlightID = a.repoList.Repos()[cursor].Id
		}
	}
	a.repoList = repos.NewRepoListModel(a.client, a.ctx)
	a.repoList.SetReturnView(returnView)
	a.repoList.SetHighlightRepoID(highlightID)
	a.repoList.SetSize(a.width, a.height)
	a.activeView = ui.ViewRepoList
	return a, a.repoList.Init()
}

// The five handlers below capture long-running session and account actions —
// the chat picker's merge/archive/account-switch, and the accounts list's
// status flip and removal. All five are captured HERE rather than in the
// originating view's own result handler, and they capture UNCONDITIONALLY, for
// one reason:
//
// App.Update is the tea root, so it sees each of these messages exactly once,
// no matter which view is active — while the originating view sees one only
// while it is still the active view and (for merge/archive) its orphan guard
// passes. The picker's key gate swallows every key but Esc during an in-flight
// merge, archive or account switch, precisely so the operator can navigate away
// and let the RPC finish in the background, so "the view is gone by the time
// the result lands" is a designed flow, not an edge case. The accounts list
// reaches the same state by a blunter route: its [esc] has no in-flight guard
// at all, so App.updateAccounts routes away on the very next keystroke and the
// list's own handler never runs again. Capturing at the root is therefore
// exactly-once by construction: no cross-file condition-and-its-negation
// invariant to keep in sync, and no action that silently reports nothing
// because the operator walked away.
//
// Removal is also handled by BOTH the accounts list and the account edit screen.
// Rooting the capture retires the "only one of the two is ever active, so
// exactly one fires" argument those two used to share — an invariant that was
// true right up until neither was active.
//
// All five deliberately do NOT consume the message: App.Update falls out of its
// type switch and on into delegateToActiveView (not a Go `fallthrough`), so a
// still-active view handles the result exactly as it did before.

// handleMergeResult captures session_merged. Observes only; see the note above.
func (a App) handleMergeResult(msg chatpicker.MergeResultMsg) {
	captureTUIAction(a.ctx, a.telemetry, tuiFeatureSession, tuiActionSessionMerged, tuiActionStatus(msg.Err))
}

// handleSwitchAccountResult captures account_switched. Observes only; see the
// note above. The feature is accounts, not session: this is an account action
// that happens to be reachable from the chat picker.
func (a App) handleSwitchAccountResult(msg chatpicker.SwitchAgentAccountResultMsg) {
	captureTUIAction(a.ctx, a.telemetry, tuiFeatureAccounts, tuiActionAccountSwitched, tuiActionStatus(msg.Err))
}

// handleArchiveResult captures session_archived and reconciles the home
// archiving override when the archive RPC resolves.
//
// The reconciliation half also has to run at the app level so an ESC-then-result
// still clears Home's optimistic state even when the chatpicker is no longer the
// active view; see the note above for why the capture half lives here too.
func (a *App) handleArchiveResult(msg chatpicker.ArchiveResultMsg) {
	captureTUIAction(a.ctx, a.telemetry, tuiFeatureSession, tuiActionSessionArchived, tuiActionStatus(msg.Err))
	if a.home.IsArchiving(msg.SessionID) {
		// The archive RPC resolved. On failure, drop the override entirely so
		// the real status is shown again. On success the override is retained
		// for rendering until the row leaves the list, but the archive is no
		// longer in flight, so re-entering the session must not seed a stuck
		// archiving picker.
		a.home.ResolveArchive(msg.SessionID, msg.Err, msg.Deferred)
		if msg.Err != nil || msg.Deferred {
			// The table rows cache rendered status text. Rebuild immediately so
			// a failed archive stops showing its optimistic label before the next
			// spinner tick or session poll, and a deferred one renders from the
			// daemon's archive_pending rather than the local override.
			a.home.BuildTableRows()
		}
	}
}

// handleAttentionActionResult settles a merge the operator walked away from
// while Home is off screen. The retained Home drops the row's "merging" and
// keeps the outcome for the operator's return. A merge whose outcome is not
// known yet reads merging on Home until a poll settles it, wherever it was
// started. An active Home settles its own result.
func (a *App) handleAttentionActionResult(msg ui.AttentionActionResultMsg) {
	if a.activeView == ui.ViewHome {
		return
	}
	if _, ok := a.home.MergeInFlight(msg.SessionID); !ok && !msg.Unsettled {
		return
	}
	a.home.ShowActionResult(msg)
	a.home.StatusUnseen = true
}

// handleAccountStatusResult captures account_enabled / account_disabled.
// Observes only; see the note above. [space] is a toggle, so this one message
// carries both directions: msg.status is the status the flip requested.
func (a App) handleAccountStatusResult(msg accounts.AgentAccountStatusUpdatedMsg) {
	captureTUIAction(a.ctx, a.telemetry, tuiFeatureAccounts, accountStatusAction(msg.Status), tuiActionStatus(msg.Err))
}

// handleAccountRemoveResult captures account_removed. Observes only; see the
// note above. Both the accounts list and the account edit screen handle this
// message, so rooting the capture is also what keeps it from firing twice.
func (a App) handleAccountRemoveResult(msg accounts.AgentAccountRemovedMsg) {
	captureTUIAction(a.ctx, a.telemetry, tuiFeatureAccounts, tuiActionAccountRemoved, tuiActionStatus(msg.Err))
}

// handleAccountsLoadedResult captures account_refreshed. Observes only; see the
// note above — the accounts list's Esc is unguarded during a [r] refresh too.
//
// msg.refresh is the request provenance the message carries: only a manual
// refresh is a user action worth an event, and keying on it (rather than the
// list's shared refreshing flag) is what stops an unrelated load that merely
// landed first from being reported as one.
func (a App) handleAccountsLoadedResult(msg accounts.AgentAccountsLoadedMsg) {
	if !msg.Refresh {
		return
	}
	captureTUIAction(a.ctx, a.telemetry, tuiFeatureAccounts, tuiActionAccountRefreshed, tuiActionStatus(msg.Err))
}

// handleAccountEditSavedResult captures the account edit screen's status flip.
// Observes only; see the note above. Its Esc is unguarded too, so
// App.updateAccountEdit routes away and the save result would otherwise reach
// only the return view.
//
// Only a status flip has an action in the bounded enum; a label or priority
// save is not one of the instrumented actions and emits nothing.
func (a App) handleAccountEditSavedResult(msg accounts.AgentAccountEditSavedMsg) {
	if msg.StatusFlip == "" {
		return
	}
	captureTUIAction(a.ctx, a.telemetry, tuiFeatureAccounts, accountStatusAction(msg.StatusFlip), tuiActionStatus(msg.Err))
}

// handleCronJobDeletedResult captures cron_job_deleted. Observes only; see the
// note above — the cron list's Esc is unguarded while the RPC is in flight.
func (a App) handleCronJobDeletedResult(msg cronJobDeletedMsg) {
	captureTUIAction(a.ctx, a.telemetry, tuiFeatureCron, tuiActionCronJobDeleted, tuiActionStatus(msg.Err))
}

// handleCronJobUpdatedResult captures cron_job_updated for the list's [space]
// enabled-state toggle. Observes only; see the note above.
func (a App) handleCronJobUpdatedResult(msg cronJobUpdatedMsg) {
	captureTUIAction(a.ctx, a.telemetry, tuiFeatureCron, tuiActionCronJobUpdated, tuiActionStatus(msg.Err))
}

// handleCronRunNowResult captures cron_job_run_now. Observes only; see the note
// above.
//
// status reports whether the run-now RPC itself succeeded. A gate skip
// (skippedReason) is a successful RPC that declined to fire; the daemon's own
// cron_job_fired event carries skip_reason, and tui_action's bounded property
// set deliberately does not duplicate it.
func (a App) handleCronRunNowResult(msg cronRunNowMsg) {
	captureTUIAction(a.ctx, a.telemetry, tuiFeatureCron, tuiActionCronJobRunNow, tuiActionStatus(msg.Err))
}

// handleSessionRestoredResult captures session_resurrected. Observes only; see
// the note above — the trash view hides its action bar during a restore but
// still lets Esc set cancel, so App.updateTrash routes away mid-RPC.
func (a App) handleSessionRestoredResult(msg sessionRestoredMsg) {
	captureTUIAction(a.ctx, a.telemetry, tuiFeatureSession, tuiActionSessionResurrected, tuiActionStatus(msg.Err))
}

// handleSessionDeletedResult captures session_removed for a single confirmed
// deletion. Observes only; see the note above.
func (a App) handleSessionDeletedResult(msg sessionDeletedMsg) {
	captureTUIAction(a.ctx, a.telemetry, tuiFeatureSession, tuiActionSessionRemoved, tuiActionStatus(msg.Err))
}

// handleDeleteProgressResult captures session_removed for one step of the
// delete-all batch. Observes only; see the note above.
//
// The batch drains one session at a time, so one removal is one session_removed
// — a batch of N emits N events, and a batch that fails on the Kth emits K-1
// successes plus one error before stopping.
//
// Rooting this capture fixes the attribution of the step that is in flight when
// the operator escapes. It does NOT keep the batch running: only TrashModel's
// own handler chains the next deleteNext(), so a batch abandoned mid-drain
// still stalls. That is pre-existing behaviour, untouched here.
func (a App) handleDeleteProgressResult(msg deleteProgressMsg) {
	captureTUIAction(a.ctx, a.telemetry, tuiFeatureSession, tuiActionSessionRemoved, tuiActionStatus(msg.Err))
}

// handleStartSubscriptionFlow jumps straight into the login view's cloud
// subscription attempt. Always returns.
func (a App) handleStartSubscriptionFlow() (tea.Model, tea.Cmd) {
	a.login = NewLoginModel(a.auth, a.client, a.ctx)
	a.login.SetAfterAuth(a.afterAuth)
	a.login.SetAuthChangeQueue(a.authChanges)
	a.login.SetTelemetry(a.telemetry)
	if a.cloudAccess != nil {
		a.login.SetCloudSubscription(a.cloudAccess, a.checkoutReturnURL, a.checkoutCancelURL)
		a.login.SetSubscriptionURL(a.subscriptionURL)
	}
	a.login.SetWidth(a.width)
	a.activeView = ui.ViewLogin
	updated, cmd := a.login.StartSubscription()
	a.login = updated
	return a, tea.Batch(cmd, a.login.SpinnerTick())
}

// currentSession returns the session associated with the active view, or nil
// if none. Only views that expose a *pb.Session participate.
func (a App) currentSession() *pb.Session {
	if a.activeView == ui.ViewChatPicker {
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
	case ui.ViewChatPicker:
		return a.chatPicker.DaemonStatuses()
	case ui.ViewHome:
		return a.home.DaemonStatuses()
	}
	return nil
}

func fetchReposAfterRepoAdd(c repos.ListClient, ctx context.Context, highlightID string) tea.Cmd {
	return func() tea.Msg {
		repos, err := c.ListRepos(ctx)
		return ui.RepoAddCompletedMsg{Repos: repos, Err: err, HighlightID: highlightID}
	}
}

func saveDefaultAgent(name string) error {
	settings, err := config.Load()
	if err != nil {
		return err
	}
	settings.DefaultAgent = name
	return config.Save(settings)
}
