package views

import (
	"context"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/client"
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
//   - never-returns   → plain pointer-receiver mutator returning nothing; Update
//     always falls through. handleWindowSize, handleArchiveResult — and ONLY
//     those two. Giving any other handler this shape would make the signature
//     lie about the routing.

// handleToastExpire retires the rotation toast. Always returns: the expiry is
// the toast's own bookkeeping and no sub-model has any use for it.
//
// Shaped as always-returns (not a plain mutator) so it cannot be mistaken for
// one of the two never-returns mutators in the taxonomy above.
func (a App) handleToastExpire(msg toastExpireMsg) (tea.Model, tea.Cmd) {
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
	if msg.homeGeneration != 0 && msg.homeGeneration != a.home.generation {
		return nil, true
	}
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
		cmd := updateSub(&a.home, msg)
		a.userSettings = a.home.settings
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
	a.home.width = msg.Width
	a.home.height = msg.Height
	a.newSession.width = msg.Width
	a.newSession.height = msg.Height
	a.repoAdd.width = msg.Width
	a.repoAdd.height = msg.Height
	a.repoList.width = msg.Width
	a.repoList.height = msg.Height
	a.repoSettings.width = msg.Width
	a.trash.width = msg.Width
	a.trash.height = msg.Height
	a.chatPicker.width = msg.Width
	a.chatPicker.height = msg.Height
	a.settings.width = msg.Width
	a.generalSettings.width = msg.Width
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
}

// handleGlobalKey handles the two app-global chords. Every other key — and
// ctrl+b while the bug report is already open — reports handled==false so the
// key still reaches the active view.
func (a *App) handleGlobalKey(msg tea.KeyMsg) (tea.Cmd, bool) {
	switch msg.String() {
	case "ctrl+c":
		a.quitting = true
		return tea.Quit, true
	case "ctrl+b":
		if a.activeView == ViewBugReport {
			break
		}
		if a.activeView == ViewHome && (a.home.upgrading || a.home.restarting) {
			return nil, true
		}
		a.bugReport = NewBugReportModel(a.client, a.ctx, a.auth, a.activeView, a.currentSession(), a.currentDaemonStatuses())
		a.bugReport.width = a.width
		a.activeView = ViewBugReport
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
func (a App) handleRepoAddCompleted(msg repoAddCompletedMsg) (tea.Model, tea.Cmd) {
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
}

// handleArchiveResult reconciles the home archiving override when the archive
// RPC resolves.
//
// Runs at the app level (not per-view) so an ESC-then-result still reconciles
// the optimistic state even when the chatpicker is no longer the active view.
//
// This handler deliberately does NOT consume the message: it is a plain mutator,
// so App.Update falls out of its type switch and on into delegateToActiveView
// (not a Go `fallthrough`), and the chatpicker — if still active — also
// processes the message and flips its own archiving=false / statusMsg field.
func (a *App) handleArchiveResult(msg archiveResultMsg) {
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
}

// handleStartSubscriptionFlow jumps straight into the login view's cloud
// subscription attempt. Always returns.
func (a App) handleStartSubscriptionFlow() (tea.Model, tea.Cmd) {
	a.login = NewLoginModel(a.auth, a.client, a.ctx)
	a.login.SetAfterAuth(a.afterAuth)
	a.login.SetAuthChangeQueue(a.authChanges)
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

func fetchReposAfterRepoAdd(c client.BossClient, ctx context.Context, highlightID string) tea.Cmd {
	return func() tea.Msg {
		repos, err := c.ListRepos(ctx)
		return repoAddCompletedMsg{repos: repos, err: err, highlightID: highlightID}
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
