package views

// Characterization tests for App.Update's routing contract (BOS-529).
//
// The four fall-through tests were written against the pre-split monolith and
// still hold verbatim against the decomposed dispatcher — that invariance is the
// point, and is why they were kept rather than rewritten. The inverse-direction
// and heartbeat-payload tests at the bottom were added afterwards, during review
// of the split.
//
// App.Update is a `switch msg := msg.(type)` whose arms do NOT all return.
// Fifteen deliberately fall out of the type switch and continue into
// delegateToActiveView (app_delegate.go), so the active sub-model ALSO sees the
// message:
//
//  1. tea.WindowSizeMsg   — handleWindowSize seeds the sub-model width/heights,
//     then delegates.
//  2. tea.KeyMsg          — handleGlobalKey reports handled==true only for
//     ctrl+c and the bug-report shortcuts; every other key delegates, and
//     ctrl+g while already on ViewBugReport `break`s out
//     of handleGlobalKey's inner `switch msg.String()`
//     to a handled==false, which delegates too.
//  3. archiveResultMsg    — handleArchiveResult reconciles Home's optimistic
//     archive state, then delegates so the chatpicker
//     (if still active) can clear its own archiving flag.
//  4. sessionListMsg      — handleSessionList reports handled==true ONLY for a
//     stale home generation, or for a successful poll
//     while on ViewHome; every other combination
//     delegates.
//  5. mergeResultMsg      — handleMergeResult captures session_merged at the
//     root, where every result lands whether or not the
//     operator escaped out of the picker (BOS-683),
//     then delegates so a still-active picker sets its
//     own merged/statusMsg fields.
//  6. switchAccountResultMsg — handleSwitchAccountResult captures
//     account_switched at the root for the same
//     reason, then delegates so a still-active picker
//     sets switchNotice and reloads its chat list.
//  7. accountStatusUpdatedMsg — handleAccountStatusResult captures
//     account_enabled/account_disabled at the root,
//     because the accounts list accepts Esc while the
//     flip is in flight and App routes away (BOS-683),
//     then delegates so a still-active list clears its
//     per-row disabling flag and sets its status toast.
//  8. accountRemovedMsg    — handleAccountRemoveResult captures
//     account_removed at the root for the same reason,
//     then delegates so whichever of the list and the
//     edit screen is active handles the result as
//     before. Rooting it is ALSO what keeps the event
//     single: both of those views handle this message.
//  9-15. accountEditSavedMsg, cronJobDeletedMsg, cronJobUpdatedMsg,
//     cronRunNowMsg, sessionRestoredMsg, sessionDeletedMsg and
//     deleteProgressMsg — the account edit, cron list and trash views have the
//     same unguarded Esc, so their outcome captures are rooted for the same
//     reason and delegate for the same reason: each still-active view clears a
//     pending flag or surfaces an error that only its own Update writes.
//     TestAppUpdateRootedActionCapturesFallThroughToActiveView pins all seven
//     in one table.
//
// The other FIVE arms always return and the active view must NOT see their
// message: toastExpireMsg, heartbeatTickMsg, repoAddCompletedMsg, switchViewMsg
// and startSubscriptionFlowMsg. Only the first two are pinned below — see the
// coverage note on TestAppUpdateAppOnlyMsgsDoNotReachActiveView.
//
// Nothing in the type system, `go vet`, or the exhaustive linter notices when a
// non-returning arm gains a `return a, nil`, nor when an always-returning one
// loses it: the function still compiles and most App-level assertions still
// pass, while the active sub-model silently stops (or starts) receiving the
// message. Post-split, the same hazard lives in a handler's SIGNATURE — a
// `(tea.Cmd, bool)` handler that always reports true, or a plain mutator whose
// caller returns anyway.
//
// The routing tests here therefore assert on state that ONLY the delegated
// sub-model's own Update can produce — never on App-level state the arm itself
// writes — so each one fails if its arm's routing is flipped. Each test
// documents the mutant it kills.
//
// Three groups, in order:
//
//   - fifteen fall-through witnesses, one per non-returning arm (the last
//     seven share one table-driven test);
//   - TestAppUpdateAppOnlyMsgsDoNotReachActiveView, the inverse direction, so
//     the guard is not one-sided (partial — see its own coverage note);
//   - TestAppUpdateHeartbeatRearmsItsOwnTicker, which is the exception to the
//     rule above: it asserts on the tea.Cmd the arm itself returns, because that
//     batch is the payload no sub-model ever sees.

import (
	"errors"
	"strings"
	"testing"

	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// TestAppUpdateWindowSizeMsgFallsThroughToActiveView pins fall-through arm #1.
//
// The WindowSizeMsg arm assigns `a.<view>.width/height` on ~25 sub-models but
// never touches their tables. Only the sub-model's own Update calls
// table.SetWidth, so a non-zero table width is proof the message was delegated
// rather than swallowed. The chat picker is the sharpest witness: it sizes its
// table to `msg.Width - chatPickerBlockPadding*2`, a value App.Update never
// computes, so the assertion cannot be satisfied by App-level bookkeeping.
//
// Kills: `return a, nil` appended to the tea.WindowSizeMsg arm — the sub-model
// tables keep their constructed width of 0 and every case below fails.
func TestAppUpdateWindowSizeMsgFallsThroughToActiveView(t *testing.T) {
	const (
		width  = 120
		height = 40
	)

	tests := []struct {
		name           string
		view           View
		tableOf        func(App) table.Model
		wantTableWidth int
	}{
		{
			name:           "home",
			view:           ViewHome,
			tableOf:        func(a App) table.Model { return a.home.table },
			wantTableWidth: width,
		},
		{
			name:           "chat picker",
			view:           ViewChatPicker,
			tableOf:        func(a App) table.Model { return a.chatPicker.table },
			wantTableWidth: width - chatPickerBlockPadding*2,
		},
		{
			name:           "trash",
			view:           ViewTrash,
			tableOf:        func(a App) table.Model { return a.trash.table },
			wantTableWidth: width,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := NewApp(nil, nil)
			a.chatPicker = NewChatPickerModel(nil, a.ctx, "session-1", "")
			a.trash = NewTrashModel(nil, a.ctx)
			a.activeView = tt.view

			if got := tt.tableOf(a).Width(); got == tt.wantTableWidth {
				t.Fatalf("precondition: %s table already %d wide before the resize; the "+
					"assertion below would pass without any delegation", tt.name, got)
			}

			model, _ := a.Update(tea.WindowSizeMsg{Width: width, Height: height})
			got := model.(App)

			if got.width != width || got.height != height {
				t.Fatalf("App width/height = %d/%d, want %d/%d (the WindowSizeMsg arm itself must still run)",
					got.width, got.height, width, height)
			}
			if w := tt.tableOf(got).Width(); w != tt.wantTableWidth {
				t.Fatalf("%s table width = %d, want %d.\n"+
					"App.Update's tea.WindowSizeMsg arm must NOT return: after seeding the "+
					"sub-model width/height fields it has to fall through to the per-view "+
					"delegation so the active view's own Update resizes its table. Only the "+
					"sub-model sets the table width; an early return here leaves every list "+
					"view rendering at its pre-resize table width until the next unrelated "+
					"message happens to reach it.", tt.name, w, tt.wantTableWidth)
			}
		})
	}
}

// TestAppUpdateOrdinaryKeyFallsThroughToActiveView pins fall-through arm #2 for
// an ordinary (non-ctrl+c, non-bug-report) key.
//
// The tea.KeyMsg arm returns for exactly three chords; everything else must reach
// the active view. The repo list forwards unclaimed keys to its bubbles table,
// so "j" moving the table cursor is proof the key was delegated — App.Update
// never moves a sub-model cursor on a keypress.
//
// Kills: `return a, nil` appended to the tea.KeyMsg arm, and handleGlobalKey
// reporting handled==true unconditionally — the cursor stays on row 0, i.e. the
// whole TUI stops responding to every key except ctrl+c and the bug-report shortcuts.
func TestAppUpdateOrdinaryKeyFallsThroughToActiveView(t *testing.T) {
	a := appWithRepoList(t, []string{"repo-a", "repo-b", "repo-c"}, 0)
	a.activeView = ViewRepoList

	model, _ := a.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	got := model.(App)

	if got.activeView != ViewRepoList {
		t.Fatalf("activeView = %v, want %v (an ordinary key must not re-route the app)",
			got.activeView, ViewRepoList)
	}
	if cursor := got.repoList.table.Cursor(); cursor != 1 {
		t.Fatalf("repo list cursor = %d, want 1.\n"+
			"App.Update's tea.KeyMsg arm must NOT return for ordinary keys: only ctrl+c "+
			"(quit) and the bug-report shortcuts are handled at the App level, and every other "+
			"key has to fall through to the per-view delegation. An early return here makes "+
			"the entire TUI unresponsive to navigation, typing, and view-local shortcuts.",
			cursor)
	}
}

// TestAppUpdateCtrlGOnBugReportFallsThroughToBugReport pins fall-through arm #2
// for its subtlest case: ctrl+g while ALREADY on ViewBugReport takes a `break`,
// not a `return`, so the chord is delegated to the bug-report model like any
// other key.
//
// The bug report's success phase dismisses on any key, which makes the
// delegation observable: only BugReportModel.Update sets done, and App reacts to
// Done() by restoring the previous view.
//
// Kills: replacing that `break` with `return a, nil` — done stays false and the
// modal stays up. A reader "simplifying" the break into a return is the most
// likely way this regresses.
func TestAppUpdateCtrlGOnBugReportFallsThroughToBugReport(t *testing.T) {
	ctrlG := tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl}
	if ctrlG.String() != "ctrl+g" {
		t.Fatalf("ctrlG.String() = %q, want %q", ctrlG.String(), "ctrl+g")
	}

	a := NewApp(nil, nil)
	a.bugReport = NewBugReportModel(nil, a.ctx, nil, ViewTrash, nil, nil)
	a.bugReport.phase = bugReportPhaseSuccess
	a.activeView = ViewBugReport

	model, _ := a.Update(ctrlG)
	got := model.(App)

	if !got.bugReport.done {
		t.Fatal("bug report not dismissed by ctrl+g.\n" +
			"App.Update's ctrl+g case `break`s when the bug report is already active — a " +
			"break, not a return, so execution continues to the per-view delegation and the " +
			"bug-report model handles the chord itself. Turning that break into an early " +
			"return would make ctrl+g the one key the modal never sees.")
	}
	if got.activeView != ViewTrash {
		t.Fatalf("activeView = %v, want %v (Done() must restore the previous view after the "+
			"delegated key dismissed the modal)", got.activeView, ViewTrash)
	}
}

// TestAppUpdateMergeResultMsgFallsThroughToChatPicker pins fall-through arm #5.
//
// The mergeResultMsg arm exists only to capture session_merged at the root
// (BOS-683), so nothing App-level would notice if it started returning — but the
// picker would stop showing the merge outcome. The success case is the one with
// a durable witness: only ChatPickerModel.handleMergeResult sets merged.
//
// Kills: `return a, nil` appended to the mergeResultMsg arm — the picker stays
// merging=true forever, swallowing every key but Esc.
func TestAppUpdateMergeResultMsgFallsThroughToChatPicker(t *testing.T) {
	const sessionID = "session-1"

	a := NewApp(nil, nil)
	a.chatPicker = NewChatPickerModel(nil, a.ctx, sessionID, "")
	a.chatPicker.merging = true
	a.activeView = ViewChatPicker

	model, _ := a.Update(mergeResultMsg{sessionID: sessionID})
	got := model.(App)

	if !got.chatPicker.Merged() {
		t.Fatal("chat picker never saw mergeResultMsg. App.Update's mergeResultMsg arm must " +
			"NOT return: it captures telemetry and then falls through to the per-view " +
			"delegation so the picker records the merge and offers archive-in-place.")
	}
	if got.chatPicker.merging {
		t.Fatal("chat picker still merging after the merge resolved; the message never " +
			"reached the sub-model, leaving it swallowing every key but Esc")
	}
}

// TestAppUpdateSwitchAccountResultMsgFallsThroughToChatPicker pins fall-through
// arm #6. Same shape as the merge arm: the App level only captures
// account_switched, and the picker's switchNotice is the witness that the
// message still reached it.
//
// Kills: `return a, nil` appended to the switchAccountResultMsg arm — the picker
// stays switching=true with no notice, swallowing every key but Esc.
func TestAppUpdateSwitchAccountResultMsgFallsThroughToChatPicker(t *testing.T) {
	a := NewApp(nil, nil)
	a.chatPicker = NewChatPickerModel(nil, a.ctx, "session-1", "")
	a.chatPicker.switching = true
	a.activeView = ViewChatPicker

	model, _ := a.Update(switchAccountResultMsg{err: errors.New("account cooling")})
	got := model.(App)

	if got.chatPicker.switching {
		t.Fatal("chat picker still switching; App.Update's switchAccountResultMsg arm must " +
			"NOT return — it captures telemetry and then falls through to the per-view delegation")
	}
	if !strings.Contains(got.chatPicker.switchNotice, "account cooling") {
		t.Fatalf("chat picker switchNotice = %q, want the daemon's error. Only "+
			"ChatPickerModel.handleSwitchAccountResult writes this field, so its absence "+
			"means the message never reached the sub-model.", got.chatPicker.switchNotice)
	}
}

// TestAppUpdateArchiveResultMsgFallsThroughToChatPicker pins fall-through arm #3.
//
// archiveResultMsg is reconciled at the App level so an ESC-then-result still
// clears Home's optimistic override, and the long comment on that arm states
// outright that it does NOT return early: the chatpicker, when still active,
// must also process the message and clear its own archiving flag. A failed
// archive is the case that keeps the picker on screen (a successful one sets
// archived and routes Home), so it is the case that can observe the leftover.
//
// Kills: `return a, nil` appended to the archiveResultMsg arm — the picker stays
// archiving=true forever, swallowing every key but Esc, with no status message.
func TestAppUpdateArchiveResultMsgFallsThroughToChatPicker(t *testing.T) {
	const sessionID = "session-1"

	a := NewApp(nil, nil)
	a.home.markArchiving(sessionID)
	a.chatPicker = NewChatPickerModel(nil, a.ctx, sessionID, "")
	a.chatPicker.archiving = true
	a.activeView = ViewChatPicker

	model, _ := a.Update(archiveResultMsg{sessionID: sessionID, err: errors.New("archive exploded")})
	got := model.(App)

	if got.home.isArchiving(sessionID) {
		t.Fatal("home still reports the session as archiving; the App-level reconciliation " +
			"in the archiveResultMsg arm must still run")
	}
	if got.chatPicker.archiving {
		t.Fatal("chat picker still archiving after the archive failed.\n" +
			"App.Update's archiveResultMsg arm must NOT return: it reconciles Home's " +
			"optimistic archive state and then falls through to the per-view delegation so " +
			"the still-active chatpicker clears its own archiving flag. An early return " +
			"leaves the picker stuck on \"Archiving session…\", swallowing every key but Esc.")
	}
	if !strings.Contains(got.chatPicker.statusMsg, "Couldn't archive session") {
		t.Fatalf("chat picker statusMsg = %q, want it to report the archive failure. Only "+
			"ChatPickerModel.handleArchiveResult writes this field, so its absence means the "+
			"message never reached the sub-model.", got.chatPicker.statusMsg)
	}
}

// TestAppUpdateAccountStatusUpdatedMsgFallsThroughToAccountsList pins
// fall-through arm #7.
//
// handleAccountStatusResult captures account_enabled/account_disabled at the
// root so the event survives the operator escaping out mid-flip, but the
// accounts list must STILL see the message when it is active: only its own
// handler clears the per-row `disabling` pending and writes the status toast.
// A failed flip is the case that keeps the list on screen.
//
// Kills: `return a, nil` appended to the accountStatusUpdatedMsg arm — the row
// spins on "Disabling…" forever and [space] stays wedged, because the in-flight
// guard in updateNormal reads that same map.
func TestAppUpdateAccountStatusUpdatedMsgFallsThroughToAccountsList(t *testing.T) {
	const accountID = "acct-1"

	a := NewApp(nil, nil)
	a.accountsList = NewAccountsListModel(nil, a.ctx)
	a.accountsList.disabling[accountID] = true
	a.activeView = ViewAccounts

	model, _ := a.Update(accountStatusUpdatedMsg{
		id:     accountID,
		status: accountStatusDisabled,
		err:    errors.New("status flip exploded"),
	})
	got := model.(App)

	if got.accountsList.disabling[accountID] {
		t.Fatal("accounts list still reports the row as disabling.\n" +
			"App.Update's accountStatusUpdatedMsg arm must NOT return: it captures the " +
			"telemetry action at the root and then falls through to the per-view delegation " +
			"so a still-active list clears its own per-row pending. An early return leaves " +
			"the row stuck on \"Disabling…\" and wedges [space], which reads that same map.")
	}
	if !strings.Contains(got.accountsList.status, "status flip exploded") {
		t.Fatalf("accounts list status = %q, want it to report the failure. Only "+
			"AccountsListModel's accountStatusUpdatedMsg handler writes this field, so its "+
			"absence means the message never reached the sub-model.", got.accountsList.status)
	}
}

// TestAppUpdateAccountRemovedMsgFallsThroughToAccountsList pins fall-through
// arm #8, on the same principle as arm #7: rooting the capture must not stop
// the still-active view from clearing its own `removing` pending.
//
// Kills: `return a, nil` appended to the accountRemovedMsg arm — the row stays
// on "Removing…" and [d]/[space] stay wedged behind the same in-flight guard.
func TestAppUpdateAccountRemovedMsgFallsThroughToAccountsList(t *testing.T) {
	const accountID = "acct-1"

	a := NewApp(nil, nil)
	a.accountsList = NewAccountsListModel(nil, a.ctx)
	a.accountsList.removing[accountID] = true
	a.activeView = ViewAccounts

	model, _ := a.Update(accountRemovedMsg{id: accountID, err: errors.New("remove exploded")})
	got := model.(App)

	if got.accountsList.removing[accountID] {
		t.Fatal("accounts list still reports the row as removing; the accountRemovedMsg arm " +
			"must fall through to the per-view delegation, not return")
	}
	if !strings.Contains(got.accountsList.status, "remove exploded") {
		t.Fatalf("accounts list status = %q, want it to report the failure. Only "+
			"AccountsListModel's accountRemovedMsg handler writes this field, so its absence "+
			"means the message never reached the sub-model.", got.accountsList.status)
	}
}

// TestAppUpdateRootedActionCapturesFallThroughToActiveView pins fall-through
// arms #9 through #15 — the account-edit save, the three cron list outcomes,
// and the three trash outcomes.
//
// All seven were moved to the App root for the same reason as arms #7/#8: each
// originating view accepts Esc while its RPC is in flight, so the capture has
// to happen where every result lands. But rooting the capture must not stop the
// still-active view from processing the result — every one of these clears a
// pending flag or surfaces an error that ONLY the sub-model's own Update
// writes, so each case asserts on exactly that.
//
// Kills: `return a, nil` appended to any of the seven arms — the matching view
// keeps a stuck spinner or silently swallows its failure.
func TestAppUpdateRootedActionCapturesFallThroughToActiveView(t *testing.T) {
	boom := errors.New("rpc exploded")

	tests := []struct {
		name  string
		build func(App) App
		msg   tea.Msg
		check func(*testing.T, App)
	}{
		{
			name: "account edit save",
			build: func(a App) App {
				a.accountEdit = NewAccountEditModel(nil, a.ctx, nil)
				a.activeView = ViewAccountEdit
				return a
			},
			msg: accountEditSavedMsg{statusFlip: accountStatusDisabled, err: boom},
			check: func(t *testing.T, a App) {
				if a.accountEdit.err == nil {
					t.Fatal("account edit never recorded the save failure; only AccountEditModel's " +
						"accountEditSavedMsg handler writes err, so the message never reached it")
				}
			},
		},
		{
			name: "cron delete",
			build: func(a App) App {
				a.cronList = NewCronListModel(nil, a.ctx)
				a.cronList.deleting["job-1"] = true
				a.activeView = ViewCron
				return a
			},
			msg: cronJobDeletedMsg{id: "job-1", err: boom},
			check: func(t *testing.T, a App) {
				if a.cronList.deleting["job-1"] {
					t.Fatal("cron row still marked deleting; the arm must fall through so the list " +
						"clears its own per-row pending")
				}
			},
		},
		{
			name: "cron update",
			build: func(a App) App {
				a.cronList = NewCronListModel(nil, a.ctx)
				a.activeView = ViewCron
				return a
			},
			msg: cronJobUpdatedMsg{job: &pb.CronJob{Id: "job-1"}, err: boom},
			check: func(t *testing.T, a App) {
				if !strings.Contains(a.cronList.status, "rpc exploded") {
					t.Fatalf("cron list status = %q, want the update failure; only CronListModel "+
						"writes it", a.cronList.status)
				}
			},
		},
		{
			name: "cron run now",
			build: func(a App) App {
				a.cronList = NewCronListModel(nil, a.ctx)
				a.cronList.running["job-1"] = true
				a.activeView = ViewCron
				return a
			},
			msg: cronRunNowMsg{id: "job-1", err: boom},
			check: func(t *testing.T, a App) {
				if a.cronList.running["job-1"] {
					t.Fatal("cron row still marked running; the arm must fall through so the list " +
						"clears its own per-row spinner")
				}
			},
		},
		{
			name: "session restored",
			build: func(a App) App {
				a.trash = NewTrashModel(nil, a.ctx)
				a.trash.restoring = true
				a.activeView = ViewTrash
				return a
			},
			msg: sessionRestoredMsg{id: "sess-1", err: boom},
			check: func(t *testing.T, a App) {
				if a.trash.restoring {
					t.Fatal("trash still marked restoring; the arm must fall through so the view " +
						"clears its own in-flight flag")
				}
			},
		},
		{
			name: "session deleted",
			build: func(a App) App {
				a.trash = NewTrashModel(nil, a.ctx)
				a.trash.deleting = true
				a.activeView = ViewTrash
				return a
			},
			msg: sessionDeletedMsg{id: "sess-1", err: boom},
			check: func(t *testing.T, a App) {
				if a.trash.deleting {
					t.Fatal("trash still marked deleting; the arm must fall through so the view " +
						"clears its own in-flight flag")
				}
			},
		},
		{
			name: "delete-all batch progress",
			build: func(a App) App {
				a.trash = NewTrashModel(nil, a.ctx)
				a.trash.deletingAll = true
				a.activeView = ViewTrash
				return a
			},
			msg: deleteProgressMsg{id: "sess-1", err: boom},
			check: func(t *testing.T, a App) {
				if a.trash.deletingAll {
					t.Fatal("trash still draining the batch after a step failed; the arm must fall " +
						"through so the view stops the batch on the first failure")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := tt.build(NewApp(nil, nil))

			model, _ := a.Update(tt.msg)

			tt.check(t, model.(App))
		})
	}
}

// TestAppUpdateBugReportChordSuppressedDuringInFlightWork pins the other half
// of the escapable-outcome fix.
//
// ctrl+g / ctrl+b are app-global: handleGlobalKey runs BEFORE delegation and
// reports handled==true, so a view's own input guard cannot block them. Letting
// the chord swap activeView mid-operation means the eventual cronFormSavedMsg /
// flowDoneMsg is delivered only to the bug-report model, which drops it — the
// telemetry action is lost AND the retained view is left stuck (submitting
// forever, or a registration waiting on a result that already came and went).
//
// Rooting those captures would rescue the event but not the stuck view, so the
// chord is suppressed for the duration instead — the same rule Home already
// applies while upgrading/restarting.
//
// Kills: deleting either guard — activeView flips to ViewBugReport and the
// in-flight view is abandoned mid-RPC.
func TestAppUpdateBugReportChordSuppressedDuringInFlightWork(t *testing.T) {
	for _, tt := range []struct {
		name       string
		build      func(App) App
		wantView   View
		suppressed bool
	}{
		{
			name: "cron form submitting",
			build: func(a App) App {
				a.cronForm = CronFormModel{ctx: a.ctx, submitting: true}
				a.activeView = ViewCronForm
				return a
			},
			wantView:   ViewCronForm,
			suppressed: true,
		},
		{
			name: "cron form idle still opens the reporter",
			build: func(a App) App {
				a.cronForm = CronFormModel{ctx: a.ctx}
				a.activeView = ViewCronForm
				return a
			},
			wantView:   ViewBugReport,
			suppressed: false,
		},
		{
			name: "account register flow running",
			build: func(a App) App {
				a.accountRegister = NewAccountRegisterModel(nil, a.ctx)
				a.accountRegister.state = registerStateProgress
				a.activeView = ViewAccountRegister
				return a
			},
			wantView:   ViewAccountRegister,
			suppressed: true,
		},
		{
			name: "account register awaiting a prompt is still in flight",
			build: func(a App) App {
				a.accountRegister = NewAccountRegisterModel(nil, a.ctx)
				a.accountRegister.state = registerStateAwaitSecret
				a.activeView = ViewAccountRegister
				return a
			},
			wantView:   ViewAccountRegister,
			suppressed: true,
		},
		{
			name: "account register provider chooser is not in flight",
			build: func(a App) App {
				a.accountRegister = NewAccountRegisterModel(nil, a.ctx)
				a.accountRegister.state = registerStateProvider
				a.activeView = ViewAccountRegister
				return a
			},
			wantView:   ViewBugReport,
			suppressed: false,
		},
		{
			name: "account register error screen is not in flight",
			build: func(a App) App {
				a.accountRegister = NewAccountRegisterModel(nil, a.ctx)
				a.accountRegister.state = registerStateError
				a.activeView = ViewAccountRegister
				return a
			},
			wantView:   ViewBugReport,
			suppressed: false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a := tt.build(NewApp(nil, nil))

			model, _ := a.Update(tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl})
			got := model.(App)

			if got.activeView != tt.wantView {
				if tt.suppressed {
					t.Fatalf("activeView = %v, want %v: the bug-report chord must be suppressed "+
						"while this view owns an in-flight operation, or its result is delivered "+
						"to the modal and the view is left stuck", got.activeView, tt.wantView)
				}
				t.Fatalf("activeView = %v, want %v: the chord must still work when nothing is "+
					"in flight — the guard has to be narrow, not a blanket block",
					got.activeView, tt.wantView)
			}
		})
	}
}

// TestAppUpdateSessionListMsgFallsThroughToActiveView pins fall-through arm #4.
//
// The sessionListMsg arm returns early in exactly two situations: a stale home
// generation, and a SUCCESSFUL poll while ViewHome is active (which it handles
// inline, toast included). Every other combination — a failed poll, or any poll
// arriving while another view is active — has to fall through to the per-view
// delegation.
//
// Both surviving branches are covered:
//
//   - err != nil on ViewHome: HomeModel's own poll-failure debounce increments
//     pollFailures, which App.Update never touches.
//   - any poll on a non-Home view: CronListModel forwards unclaimed messages to
//     its table and re-runs updateCursorColumn, so a stale chevron is repaired.
//     No sub-model other than Home claims sessionListMsg, so this generic
//     forwarding is what makes the delegation observable off Home.
//
// Kills: `return a, nil` appended to the sessionListMsg arm (or hoisting its
// early return out of the `activeView == ViewHome && err == nil` guard) — the
// daemon-down error screen would never appear, and a poll landing on any other
// view would be dropped instead of delegated.
func TestAppUpdateSessionListMsgFallsThroughToActiveView(t *testing.T) {
	t.Run("failed poll on home reaches the home model", func(t *testing.T) {
		a := NewApp(nil, nil)
		a.activeView = ViewHome

		model, _ := a.Update(sessionListMsg{err: errors.New("daemon unreachable")})
		got := model.(App)

		if got.home.loading {
			t.Fatal("home still loading; the failed poll never reached HomeModel.Update")
		}
		if got.home.pollFailures != 1 {
			t.Fatalf("home pollFailures = %d, want 1.\n"+
				"App.Update's sessionListMsg arm returns early only for a stale generation or "+
				"a SUCCESSFUL poll on ViewHome; a failed poll must fall through to the per-view "+
				"delegation so Home can run its poll-failure debounce. An early return here "+
				"means the \"Cannot connect to daemon\" screen never appears, however long the "+
				"daemon stays down.", got.home.pollFailures)
		}
	})

	t.Run("poll on a non-home view reaches that view", func(t *testing.T) {
		a := NewApp(nil, nil)
		cronList := NewCronListModel(nil, a.ctx)
		cronList.table.SetColumns([]table.Column{{Title: "", Width: 2}, {Title: "NAME", Width: 10}})
		// Row 1 is selected but carries no chevron: only a sub-model Update pass
		// through updateCursorColumn repairs that.
		cronList.table.SetRows([]table.Row{{"", "job-a"}, {"", "job-b"}})
		cronList.table.SetCursor(1)
		a.cronList = cronList
		a.activeView = ViewCron

		model, _ := a.Update(sessionListMsg{})
		got := model.(App)

		if got.activeView != ViewCron {
			t.Fatalf("activeView = %v, want %v (a session poll must not re-route the app)",
				got.activeView, ViewCron)
		}
		rows := got.cronList.table.Rows()
		if len(rows) != 2 {
			t.Fatalf("cron list rows = %d, want 2", len(rows))
		}
		if rows[1][0] != cursorChevron {
			t.Fatalf("selected cron row indicator = %q, want %q.\n"+
				"App.Update's sessionListMsg arm handles the poll inline only while ViewHome "+
				"is active; a poll arriving on any other view must fall through to the "+
				"per-view delegation so that view still gets its Update pass. An early return "+
				"silently drops every message of this type for every non-Home view.",
				rows[1][0], cursorChevron)
		}
	})
}

// TestAppUpdateAppOnlyMsgsDoNotReachActiveView pins the INVERSE direction: the
// arms that always return must keep the message away from the active view.
//
// The four fall-through tests above are one-sided — they all fail when an arm
// stops delegating, and none fails when an arm STARTS delegating. Turning
// `return a.handleToastExpire(msg)` into a fall-through, or giving
// handleToastExpire/handleHeartbeat the plain-mutator shape that App.Update
// falls through on, is the same class of invisible routing change in the
// opposite direction.
//
// The witness is the same cron-list chevron the sessionListMsg test uses, which
// makes this test's negative assertion non-vacuous: the positive control below
// drives a sessionListMsg through the identical App and PROVES the chevron
// appears when delegation happens. Without that control, "chevron absent" would
// also be satisfied by a broken fixture.
//
// Coverage note. Of the five always-returns arms plus the two handled==true
// branches, this test covers the four that do not re-route: toastExpireMsg,
// heartbeatTickMsg, handleGlobalKey's ctrl+c, and handleSessionList's
// stale-generation rejection. startSubscriptionFlowMsg is pinned elsewhere —
// TestAppHomeSubscribeKeyShowsSubscriptionWaitingView asserts its returned
// two-element tea.BatchMsg, which a fall-through cannot produce.
//
// That leaves three genuinely unpinned in this direction: repoAddCompletedMsg,
// switchViewMsg, and handleGlobalKey's ctrl+g. Those three — and only those —
// re-route activeView, so the cron list stops being the active view and the
// chevron says nothing. Closing them needs an instrument that survives a
// re-route; a follow-up, not an oversight.
//
// Kills: any change that lets toastExpireMsg or heartbeatTickMsg fall through to
// delegateToActiveView.
func TestAppUpdateAppOnlyMsgsDoNotReachActiveView(t *testing.T) {
	// appOnCron returns an App parked on ViewCron whose selected row carries no
	// chevron. Only a CronListModel.Update pass repairs that, so the chevron is a
	// direct witness for "this message reached the active view".
	appOnCron := func() App {
		a := NewApp(nil, nil)
		cronList := NewCronListModel(nil, a.ctx)
		cronList.table.SetColumns([]table.Column{{Title: "", Width: 2}, {Title: "NAME", Width: 10}})
		cronList.table.SetRows([]table.Row{{"", "job-a"}, {"", "job-b"}})
		cronList.table.SetCursor(1)
		a.cronList = cronList
		a.activeView = ViewCron
		return a
	}

	selectedIndicator := func(t *testing.T, a App) string {
		t.Helper()
		rows := a.cronList.table.Rows()
		if len(rows) != 2 {
			t.Fatalf("cron list rows = %d, want 2", len(rows))
		}
		return rows[1][0]
	}

	// Positive control: a message that DOES delegate repairs the chevron. This is
	// what makes the negative assertions below meaningful rather than vacuous.
	t.Run("positive control: a delegating msg reaches the cron list", func(t *testing.T) {
		model, _ := appOnCron().Update(sessionListMsg{})
		if got := selectedIndicator(t, model.(App)); got != cursorChevron {
			t.Fatalf("selected row indicator = %q, want %q — the control message must "+
				"reach the active view, otherwise the negative cases below prove nothing",
				got, cursorChevron)
		}
	})

	tests := []struct {
		name       string
		setup      func(*App)
		msg        tea.Msg
		why        string
		alsoAssert func(*testing.T, App)
	}{
		{
			name: "toastExpireMsg",
			msg:  toastExpireMsg{},
			why: "toast expiry is the toast's own bookkeeping; handleToastExpire is " +
				"shaped as always-returns precisely so it cannot leak to the active view",
		},
		{
			name: "heartbeatTickMsg",
			msg:  heartbeatTickMsg{},
			why: "the heartbeat arm returns a fresh ticker plus the status-shipping cmd " +
				"and nothing else; delegating it would hand every view a message it has " +
				"no handler for, on a timer",
		},
		{
			// handleGlobalKey's ctrl+c branch is a handled==true return that does
			// NOT re-route, so the chevron witness reaches it.
			name: "ctrl+c",
			msg:  tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl},
			why: "ctrl+c quits; forwarding it would also hand the chord to the active " +
				"view, which may bind it to something of its own on the way out",
			alsoAssert: func(t *testing.T, got App) {
				t.Helper()
				if !got.quitting {
					t.Fatal("quitting = false; the ctrl+c branch itself must still run")
				}
			},
		},
		{
			// handleSessionList's stale-generation rejection is the other
			// handled==true return that does not re-route.
			name: "sessionListMsg from a replaced HomeModel",
			setup: func(a *App) {
				a.home.generation = 7
			},
			msg: sessionListMsg{homeGeneration: 99},
			why: "a poll from a superseded HomeModel is rejected outright so it cannot " +
				"mutate App rotation state or Home's question rising-edge state; letting " +
				"it fall through would hand every view a result the app already disowned",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := appOnCron()
			if tt.setup != nil {
				tt.setup(&a)
			}
			before := selectedIndicator(t, a)
			if before == cursorChevron {
				t.Fatalf("precondition: the fixture already has a chevron, so the "+
					"assertion below cannot detect delegation (%s)", tt.name)
			}

			model, _ := a.Update(tt.msg)
			got := model.(App)

			if got.activeView != ViewCron {
				t.Fatalf("activeView = %v, want %v", got.activeView, ViewCron)
			}
			if indicator := selectedIndicator(t, got); indicator == cursorChevron {
				t.Fatalf("selected row indicator = %q, want it left as %q.\n"+
					"%s reached the active view, but its App.Update arm must consume it: %s",
					indicator, before, tt.name, tt.why)
			}
			if tt.alsoAssert != nil {
				tt.alsoAssert(t, got)
			}
		})
	}
}

// TestAppUpdateHeartbeatRearmsItsOwnTicker pins the PAYLOAD of the heartbeat
// arm, which the routing tests above cannot see.
//
// The arm is self-perpetuating: it ships this client's per-chat statuses to
// bossd AND re-arms the ticker, so the returned batch is the only thing keeping
// the loop alive. Returning nil (or dropping either half) stops every heartbeat
// for the rest of the process lifetime, and other clients — the web UI, a second
// TUI — silently stop seeing this client's working/idle/question state. Nothing
// else in the package noticed: a nil return left the whole views suite green
// before this test existed.
//
// The batch is resolved but its sub-commands are NOT invoked: tea.Batch's own
// message is produced immediately, whereas heartbeatTickCmd wraps tea.Tick and
// would block for the full interval.
//
// Kills: `return nil` from handleHeartbeat, and dropping either sub-command from
// its batch.
func TestAppUpdateHeartbeatRearmsItsOwnTicker(t *testing.T) {
	a := NewApp(nil, nil)
	a.activeView = ViewHome

	_, cmd := a.Update(heartbeatTickMsg{})
	if cmd == nil {
		t.Fatal("heartbeat returned a nil cmd.\n" +
			"The heartbeatTickMsg arm must return both the status-shipping cmd and a " +
			"re-armed ticker. A nil return silently ends the heartbeat loop for the " +
			"rest of the process, so other clients stop seeing this one's chat states.")
	}

	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatalf("heartbeat cmd produced %T, want tea.BatchMsg — the arm batches the "+
			"status send with the ticker re-arm", cmd())
	}
	if len(batch) != 2 {
		t.Fatalf("heartbeat batch has %d cmds, want 2 (send statuses + re-arm the "+
			"ticker). Dropping the re-arm is the silent half: statuses ship once and "+
			"then never again.", len(batch))
	}
}
