package views

import (
	"context"
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/telemetry"
)

// errTUIAction is the failure injected into every outcome message below so the
// error branch of each handler is exercised with the same value.
var errTUIAction = errors.New("boom")

// tuiActionCase drives one instrumented view: send builds the model with the
// supplied telemetry client and feeds it the action's outcome message carrying
// err, returning the model Update produced.
type tuiActionCase struct {
	name    string
	feature tuiFeature
	action  tuiAction
	send    func(t *testing.T, client telemetry.Client, err error) (tea.Model, tea.Cmd)
}

// tuiActionCases is the single source of truth for the instrumented actions.
// Every entry is exercised for both success and error by
// TestTelemetryTUIActionPerView, and for nil-client safety by
// TestTelemetryTUIActionNilClientIsInert.
func tuiActionCases() []tuiActionCase {
	return []tuiActionCase{
		{
			name:    "account_added",
			feature: tuiFeatureAccounts,
			action:  tuiActionAccountAdded,
			send: func(_ *testing.T, client telemetry.Client, err error) (tea.Model, tea.Cmd) {
				m := NewAccountRegisterModel(nil, context.Background())
				m.SetTelemetry(client)
				return m.Update(flowDoneMsg{err: err})
			},
		},
		{
			// Captured at the App root, so driven through a real App; see
			// newAppForAccountsTelemetry.
			name:    "account_removed",
			feature: tuiFeatureAccounts,
			action:  tuiActionAccountRemoved,
			send: func(_ *testing.T, client telemetry.Client, err error) (tea.Model, tea.Cmd) {
				a := newAppForAccountsTelemetry(client, ViewAccounts)
				return a.Update(accountRemovedMsg{id: "acct-1", err: err})
			},
		},
		{
			name:    "account_disabled",
			feature: tuiFeatureAccounts,
			action:  tuiActionAccountDisabled,
			send: func(_ *testing.T, client telemetry.Client, err error) (tea.Model, tea.Cmd) {
				a := newAppForAccountsTelemetry(client, ViewAccounts)
				return a.Update(accountStatusUpdatedMsg{id: "acct-1", status: accountStatusDisabled, err: err})
			},
		},
		{
			// [space] is a toggle, so the same message carries an enable. An
			// event named account_disabled must not fire for one.
			name:    "account_enabled",
			feature: tuiFeatureAccounts,
			action:  tuiActionAccountEnabled,
			send: func(_ *testing.T, client telemetry.Client, err error) (tea.Model, tea.Cmd) {
				a := newAppForAccountsTelemetry(client, ViewAccounts)
				return a.Update(accountStatusUpdatedMsg{id: "acct-1", status: accountStatusActive, err: err})
			},
		},
		{
			// The edit screen flips the same status through the same RPC, so
			// it must report the same action rather than staying silent.
			name:    "account_disabled_from_edit_screen",
			feature: tuiFeatureAccounts,
			action:  tuiActionAccountDisabled,
			send: func(_ *testing.T, client telemetry.Client, err error) (tea.Model, tea.Cmd) {
				a := newAppForAccountsTelemetry(client, ViewAccountEdit)
				return a.Update(accountEditSavedMsg{statusFlip: accountStatusDisabled, err: err})
			},
		},
		{
			name:    "account_enabled_from_edit_screen",
			feature: tuiFeatureAccounts,
			action:  tuiActionAccountEnabled,
			send: func(_ *testing.T, client telemetry.Client, err error) (tea.Model, tea.Cmd) {
				a := newAppForAccountsTelemetry(client, ViewAccountEdit)
				return a.Update(accountEditSavedMsg{statusFlip: accountStatusActive, err: err})
			},
		},
		{
			// The edit screen's [d] runs the same removeAccountCmd as the list.
			// Both now delegate the capture to the root, so the event must fire
			// with the edit screen active too — and, per
			// TestTelemetryAccountRemovedIsCapturedOnce, exactly once.
			name:    "account_removed_from_edit_screen",
			feature: tuiFeatureAccounts,
			action:  tuiActionAccountRemoved,
			send: func(_ *testing.T, client telemetry.Client, err error) (tea.Model, tea.Cmd) {
				a := newAppForAccountsTelemetry(client, ViewAccountEdit)
				return a.Update(accountRemovedMsg{id: "acct-1", err: err})
			},
		},
		{
			name:    "account_refreshed",
			feature: tuiFeatureAccounts,
			action:  tuiActionAccountRefreshed,
			send: func(_ *testing.T, client telemetry.Client, err error) (tea.Model, tea.Cmd) {
				// Captured at the App root like every other escapable action;
				// provenance still rides on the message, so the initial-load
				// and crossed-response cases below stay silent.
				a := newAppForAccountsTelemetry(client, ViewAccounts)
				a.accountsList.refreshing = true
				return a.Update(accountsLoadedMsg{err: err, refresh: true})
			},
		},
		{
			name:    "account_switched",
			feature: tuiFeatureAccounts,
			action:  tuiActionAccountSwitched,
			send: func(_ *testing.T, client telemetry.Client, err error) (tea.Model, tea.Cmd) {
				a := newAppForPickerTelemetry(client, ViewChatPicker)
				return a.Update(switchAccountResultMsg{err: err})
			},
		},
		{
			name:    "cron_job_created",
			feature: tuiFeatureCron,
			action:  tuiActionCronJobCreated,
			send: func(_ *testing.T, client telemetry.Client, err error) (tea.Model, tea.Cmd) {
				m := CronFormModel{ctx: context.Background()} // nil job = create mode
				m.SetTelemetry(client)
				return m.Update(cronFormSavedMsg{job: &pb.CronJob{Id: "job-1"}, err: err})
			},
		},
		{
			name:    "cron_job_updated_from_form",
			feature: tuiFeatureCron,
			action:  tuiActionCronJobUpdated,
			send: func(_ *testing.T, client telemetry.Client, err error) (tea.Model, tea.Cmd) {
				m := CronFormModel{ctx: context.Background(), job: &pb.CronJob{Id: "job-1"}} // edit mode
				m.SetTelemetry(client)
				return m.Update(cronFormSavedMsg{job: &pb.CronJob{Id: "job-1"}, err: err})
			},
		},
		{
			name:    "cron_job_updated_from_list",
			feature: tuiFeatureCron,
			action:  tuiActionCronJobUpdated,
			send: func(_ *testing.T, client telemetry.Client, err error) (tea.Model, tea.Cmd) {
				a := newAppForCronTelemetry(client, ViewCron)
				return a.Update(cronJobUpdatedMsg{job: &pb.CronJob{Id: "job-1"}, err: err})
			},
		},
		{
			name:    "cron_job_deleted",
			feature: tuiFeatureCron,
			action:  tuiActionCronJobDeleted,
			send: func(_ *testing.T, client telemetry.Client, err error) (tea.Model, tea.Cmd) {
				a := newAppForCronTelemetry(client, ViewCron)
				return a.Update(cronJobDeletedMsg{id: "job-1", err: err})
			},
		},
		{
			name:    "cron_job_run_now",
			feature: tuiFeatureCron,
			action:  tuiActionCronJobRunNow,
			send: func(_ *testing.T, client telemetry.Client, err error) (tea.Model, tea.Cmd) {
				a := newAppForCronTelemetry(client, ViewCron)
				return a.Update(cronRunNowMsg{id: "job-1", err: err})
			},
		},
		{
			name:    "session_merged",
			feature: tuiFeatureSession,
			action:  tuiActionSessionMerged,
			send: func(_ *testing.T, client telemetry.Client, err error) (tea.Model, tea.Cmd) {
				a := newAppForPickerTelemetry(client, ViewChatPicker)
				return a.Update(mergeResultMsg{sessionID: pickerTelemetrySessionID, err: err})
			},
		},
		{
			name:    "session_archived",
			feature: tuiFeatureSession,
			action:  tuiActionSessionArchived,
			send: func(_ *testing.T, client telemetry.Client, err error) (tea.Model, tea.Cmd) {
				a := newAppForPickerTelemetry(client, ViewChatPicker)
				return a.Update(archiveResultMsg{sessionID: pickerTelemetrySessionID, err: err})
			},
		},
		{
			name:    "session_removed",
			feature: tuiFeatureSession,
			action:  tuiActionSessionRemoved,
			send: func(_ *testing.T, client telemetry.Client, err error) (tea.Model, tea.Cmd) {
				a := newAppForTrashTelemetry(client, ViewTrash)
				return a.Update(sessionDeletedMsg{id: "sess-1", err: err})
			},
		},
		{
			name:    "session_removed_in_delete_all_batch",
			feature: tuiFeatureSession,
			action:  tuiActionSessionRemoved,
			send: func(_ *testing.T, client telemetry.Client, err error) (tea.Model, tea.Cmd) {
				a := newAppForTrashTelemetry(client, ViewTrash)
				return a.Update(deleteProgressMsg{id: "sess-1", err: err})
			},
		},
		{
			name:    "session_resurrected",
			feature: tuiFeatureSession,
			action:  tuiActionSessionResurrected,
			send: func(_ *testing.T, client telemetry.Client, err error) (tea.Model, tea.Cmd) {
				a := newAppForTrashTelemetry(client, ViewTrash)
				return a.Update(sessionRestoredMsg{id: "sess-1", err: err})
			},
		},
	}
}

// TestTelemetryTUIActionPerView asserts that every instrumented action emits
// exactly one tui_action with the right feature/action, that a successful
// outcome reports status "success", and that a failed outcome reports
// status "error".
func TestTelemetryTUIActionPerView(t *testing.T) {
	for _, tc := range tuiActionCases() {
		for _, outcome := range []struct {
			name   string
			err    error
			status tuiStatus
		}{
			{name: "success", err: nil, status: tuiStatusSuccess},
			{name: "error", err: errTUIAction, status: tuiStatusError},
		} {
			t.Run(tc.name+"/"+outcome.name, func(t *testing.T) {
				enableViewTelemetryForTest(t)
				rec := &fakeTelemetry{}

				tc.send(t, rec, outcome.err)

				if len(rec.events) != 1 {
					t.Fatalf("events = %d, want exactly 1", len(rec.events))
				}
				if rec.events[0] != telemetry.EventTUIAction {
					t.Fatalf("event = %q, want %q", rec.events[0], telemetry.EventTUIAction)
				}
				props := rec.props[0]
				if got := props["feature"]; got != string(tc.feature) {
					t.Errorf("feature = %v, want %q", got, tc.feature)
				}
				if got := props["action"]; got != string(tc.action) {
					t.Errorf("action = %v, want %q", got, tc.action)
				}
				if got := props["status"]; got != string(outcome.status) {
					t.Errorf("status = %v, want %q", got, outcome.status)
				}
				if got := props["source"]; got != "tui" {
					t.Errorf("source = %v, want tui", got)
				}
				assertNoSensitiveTelemetryProps(t, props)
				assertTUIActionPropertiesRegistered(t, props)
			})
		}
	}
}

// TestTelemetryTUIActionNilClientIsInert proves every touched view behaves
// identically with telemetry unset — the default in every other test's model
// construction. A missed nil guard would panic here.
func TestTelemetryTUIActionNilClientIsInert(t *testing.T) {
	for _, tc := range tuiActionCases() {
		t.Run(tc.name, func(t *testing.T) {
			enableViewTelemetryForTest(t)
			if model, _ := tc.send(t, nil, nil); model == nil {
				t.Fatal("Update returned a nil model with telemetry unset")
			}
			if model, _ := tc.send(t, nil, errTUIAction); model == nil {
				t.Fatal("Update returned a nil model with telemetry unset")
			}
		})
	}
}

// TestTelemetryTUIActionIntroducesNoCommand pins the acceptance criterion "no
// new tea.Cmd ... is introduced in Update": every capture is a bare statement on
// an already-async enqueue, so each instrumented handler must return the same
// command shape whether telemetry is installed or not. A capture that grew into
// a tea.Cmd would move work onto Bubble Tea's command runner and change the
// handler's contract with App.
func TestTelemetryTUIActionIntroducesNoCommand(t *testing.T) {
	for _, tc := range tuiActionCases() {
		for _, err := range []error{nil, errTUIAction} {
			t.Run(tc.name, func(t *testing.T) {
				enableViewTelemetryForTest(t)

				_, without := tc.send(t, nil, err)
				_, with := tc.send(t, &fakeTelemetry{}, err)

				if (without == nil) != (with == nil) {
					t.Fatalf("command shape changed with telemetry installed (nil without = %v, "+
						"nil with = %v); the capture must stay a bare statement, never a tea.Cmd",
						without == nil, with == nil)
				}
			})
		}
	}
}

// TestTelemetryTUIActionPropertiesAreRegistered is the property-hygiene gate:
// every key an instrumented view emits must survive the registry's own
// event-scoped filter, so a call site cannot introduce an unregistered key.
func TestTelemetryTUIActionPropertiesAreRegistered(t *testing.T) {
	for _, tc := range tuiActionCases() {
		t.Run(tc.name, func(t *testing.T) {
			enableViewTelemetryForTest(t)
			rec := &fakeTelemetry{}
			tc.send(t, rec, nil)
			if len(rec.props) != 1 {
				t.Fatalf("captures = %d, want 1", len(rec.props))
			}
			assertTUIActionPropertiesRegistered(t, rec.props[0])
		})
	}
}

// assertTUIActionPropertiesRegistered fails when an emitted key is not allowed
// for tui_action, naming the offending key.
func assertTUIActionPropertiesRegistered(t *testing.T, props map[string]any) {
	t.Helper()
	filtered := telemetry.FilterProperties(telemetry.EventTUIAction, props)
	for key := range props {
		if _, ok := filtered[key]; !ok {
			t.Errorf("emitted property %q is not registered for %s", key, telemetry.EventTUIAction)
		}
	}
}

// TestTelemetryTUIActionCancelledConfirmationEmitsNothing pins the
// capture-on-outcome rule for the destructive actions: the keypress that opens
// a confirmation, and the keystroke that dismisses it, must emit nothing. Only
// the result message does.
// Each prompt is armed with a real action (see the note on the confirm-key test
// below) and each subtest asserts the prompt was actually dismissed, so a
// prompt that was never armed cannot pass by doing nothing.
func TestTelemetryTUIActionCancelledConfirmationEmitsNothing(t *testing.T) {
	cancelKeys := []tea.KeyPressMsg{
		{Code: 'n', Text: "n"},
		{Code: tea.KeyEscape},
	}
	neverRun := func() tea.Msg {
		t.Fatal("the confirm action ran on a cancel keystroke")
		return nil
	}

	t.Run("accounts_remove", func(t *testing.T) {
		for _, key := range cancelKeys {
			enableViewTelemetryForTest(t)
			rec := &fakeTelemetry{}
			m := newAccountsListForTelemetry(rec)
			m.confirm = newConfirmPrompt("remove?", neverRun)
			m.confirmAccountID = "acct-1"
			m.confirmRemoving = true

			model, cmd := m.Update(key)

			if cmd != nil {
				t.Fatalf("key %v returned a command; a cancelled confirmation must fire no RPC", key)
			}
			if model.(AccountsListModel).confirm.active {
				t.Fatalf("key %v left the prompt open; the cancel branch never ran", key)
			}
			if len(rec.events) != 0 {
				t.Fatalf("cancelled confirmation emitted %d events, want 0: %v", len(rec.events), rec.props)
			}
		}
	})

	t.Run("cron_delete", func(t *testing.T) {
		for _, key := range cancelKeys {
			enableViewTelemetryForTest(t)
			rec := &fakeTelemetry{}
			m := newCronListForTelemetry(rec)
			m.jobs = []*pb.CronJob{{Id: "job-1"}}
			m.rebuildTable()
			m.confirm = newConfirmPrompt("delete?", neverRun)

			model, cmd := m.Update(key)

			if cmd != nil {
				t.Fatalf("key %v returned a command; a cancelled confirmation must fire no RPC", key)
			}
			if model.(CronListModel).confirm.active {
				t.Fatalf("key %v left the prompt open; the cancel branch never ran", key)
			}
			if len(rec.events) != 0 {
				t.Fatalf("cancelled confirmation emitted %d events, want 0: %v", len(rec.events), rec.props)
			}
		}
	})

	t.Run("trash_delete", func(t *testing.T) {
		for _, key := range cancelKeys {
			enableViewTelemetryForTest(t)
			rec := &fakeTelemetry{}
			m := newTrashForTelemetry(rec)
			m.confirm = newConfirmPrompt("delete?", neverRun)

			model, cmd := m.Update(key)

			if cmd != nil {
				t.Fatalf("key %v returned a command; a cancelled confirmation must fire no RPC", key)
			}
			if model.(TrashModel).confirm.active {
				t.Fatalf("key %v left the prompt open; the cancel branch never ran", key)
			}
			if len(rec.events) != 0 {
				t.Fatalf("cancelled confirmation emitted %d events, want 0: %v", len(rec.events), rec.props)
			}
		}
	})
}

// TestTelemetryTUIActionAddFlowCancelEmitsNothing covers the add-account flow's
// own cancellation shape: an operator teardown cancels the flow context, so the
// flow returns an error that is a cancellation rather than a failed add.
func TestTelemetryTUIActionAddFlowCancelEmitsNothing(t *testing.T) {
	enableViewTelemetryForTest(t)
	rec := &fakeTelemetry{}
	m := NewAccountRegisterModel(nil, context.Background())
	m.SetTelemetry(rec)
	m.cancelled = true

	m.Update(flowDoneMsg{err: context.Canceled})

	if len(rec.events) != 0 {
		t.Fatalf("cancelled add flow emitted %d events, want 0: %v", len(rec.events), rec.props)
	}
}

// TestTelemetryTUIActionInitialLoadEmitsNothing pins that only a *manual* usage
// refresh counts as an account_refreshed action. The initial Init load and the
// re-fetches that follow every account action deliver the same message with
// refreshing false, and must stay silent — otherwise every account action would
// emit a spurious second event.
func TestTelemetryTUIActionInitialLoadEmitsNothing(t *testing.T) {
	enableViewTelemetryForTest(t)
	rec := &fakeTelemetry{}
	m := newAccountsListForTelemetry(rec)
	m.refreshing = false

	m.Update(accountsLoadedMsg{accounts: []*pb.Account{{Id: "acct-1"}}})

	if len(rec.events) != 0 {
		t.Fatalf("initial load emitted %d events, want 0: %v", len(rec.events), rec.props)
	}
}

// TestTelemetryTUIActionPickerResultsEmitExactlyOnce is the exhaustive
// exactly-once proof for the three chat-picker actions captured at the App root.
//
// The picker's key gate (chatpicker_keys.go) swallows every key but Esc while a
// merge, archive or account switch is in flight, precisely so the operator can
// navigate away and let the RPC finish — so every one of the states below is
// reachable in production, not hypothetical:
//
//   - picker still active on the same session (the ordinary case);
//   - picker gone, Home active (the operator pressed Esc);
//   - picker active on a DIFFERENT session (Esc, then open another session);
//   - picker re-entered for the SAME session, so its in-flight latch is false.
//
// Capturing at App.Update — the tea root, which sees each message exactly once
// regardless of active view — makes all four one event by construction. Moving
// any capture back down into ChatPickerModel turns rows 2-3 into zero, and
// capturing in both places turns row 1 into two.
func TestTelemetryTUIActionPickerResultsEmitExactlyOnce(t *testing.T) {
	const otherSession = "sess-2"

	for _, state := range []struct {
		name       string
		view       View
		pickerSess string
		latched    bool
	}{
		{name: "picker_active_same_session", view: ViewChatPicker, pickerSess: pickerTelemetrySessionID, latched: true},
		{name: "operator_escaped_to_home", view: ViewHome, pickerSess: pickerTelemetrySessionID, latched: true},
		{name: "picker_active_other_session", view: ViewChatPicker, pickerSess: otherSession, latched: false},
		{name: "picker_reopened_latch_cleared", view: ViewChatPicker, pickerSess: pickerTelemetrySessionID, latched: false},
	} {
		for _, action := range []struct {
			name string
			want tuiAction
			msg  tea.Msg
		}{
			{name: "session_merged", want: tuiActionSessionMerged, msg: mergeResultMsg{sessionID: pickerTelemetrySessionID}},
			{name: "session_archived", want: tuiActionSessionArchived, msg: archiveResultMsg{sessionID: pickerTelemetrySessionID}},
			{name: "account_switched", want: tuiActionAccountSwitched, msg: switchAccountResultMsg{}},
		} {
			t.Run(state.name+"/"+action.name, func(t *testing.T) {
				enableViewTelemetryForTest(t)
				rec := &fakeTelemetry{}

				a := NewApp(nil, nil)
				a.telemetry = rec
				a.chatPicker = NewChatPickerModel(nil, a.ctx, state.pickerSess, "")
				a.chatPicker.SetTelemetry(rec)
				a.chatPicker.merging = state.latched
				a.chatPicker.archiving = state.latched
				a.chatPicker.switching = state.latched
				a.home.markArchiving(pickerTelemetrySessionID)
				a.activeView = state.view

				a.Update(action.msg)

				if len(rec.events) != 1 {
					t.Fatalf("events = %d, want exactly 1 — every picker result must be captured "+
						"once at the App root, never zero (operator walked away) and never twice "+
						"(App and picker both capturing): %v", len(rec.events), rec.props)
				}
				if got := rec.props[0]["action"]; got != string(action.want) {
					t.Errorf("action = %v, want %q", got, action.want)
				}
				assertTUIActionPropertiesRegistered(t, rec.props[0])
			})
		}
	}
}

// TestTelemetryTUIActionConfirmKeypressEmitsNothing is the other half of the
// capture-on-outcome rule, and the assertion that actually pins "exactly one":
// the CONFIRM keystroke only dispatches the RPC as a tea.Cmd, which these tests
// never run. An implementation that also captured at the keypress would satisfy
// every other test in this file while double-counting in production.
// The prompts below are built with newConfirmPrompt carrying a real action, not
// by setting confirm.active field-by-field: confirmPrompt.update returns a nil
// cmd for a nil action, and every confirmed branch is guarded on `cmd != nil`.
// A hand-built prompt would leave those branches — the natural place a
// keypress-time capture would be written — unreachable, and the test would pass
// vacuously. Each subtest asserts the branch actually ran (the per-row pending
// flip) so that can never silently regress.
func TestTelemetryTUIActionConfirmKeypressEmitsNothing(t *testing.T) {
	confirmKeys := []tea.KeyPressMsg{
		{Code: 'y', Text: "y"},
		{Code: tea.KeyEnter},
	}
	// A command that resolves to nothing: these tests never run it, and its only
	// job is to make confirmPrompt return a non-nil cmd.
	noopAction := func() tea.Msg { return nil }

	t.Run("accounts_remove", func(t *testing.T) {
		for _, key := range confirmKeys {
			enableViewTelemetryForTest(t)
			rec := &fakeTelemetry{}
			m := newAccountsListForTelemetry(rec)
			m.confirm = newConfirmPrompt("remove?", noopAction)
			m.confirmAccountID = "acct-1"
			m.confirmRemoving = true

			model, _ := m.Update(key)

			if !model.(AccountsListModel).removing["acct-1"] {
				t.Fatalf("key %v did not reach the confirmed branch; the assertion below would be vacuous", key)
			}
			if len(rec.events) != 0 {
				t.Fatalf("confirm keypress emitted %d events, want 0 (the outcome message is the only capture point): %v", len(rec.events), rec.props)
			}
		}
	})

	t.Run("cron_delete", func(t *testing.T) {
		for _, key := range confirmKeys {
			enableViewTelemetryForTest(t)
			rec := &fakeTelemetry{}
			m := newCronListForTelemetry(rec)
			// selectedJob() must resolve for the confirmed branch to run.
			m.jobs = []*pb.CronJob{{Id: "job-1"}}
			m.rebuildTable()
			m.confirm = newConfirmPrompt("delete?", noopAction)

			model, _ := m.Update(key)

			if !model.(CronListModel).deleting["job-1"] {
				t.Fatalf("key %v did not reach the confirmed branch; the assertion below would be vacuous", key)
			}
			if len(rec.events) != 0 {
				t.Fatalf("confirm keypress emitted %d events, want 0: %v", len(rec.events), rec.props)
			}
		}
	})

	t.Run("trash_delete", func(t *testing.T) {
		for _, key := range confirmKeys {
			enableViewTelemetryForTest(t)
			rec := &fakeTelemetry{}
			m := newTrashForTelemetry(rec)
			m.confirm = newConfirmPrompt("delete?", noopAction)

			model, _ := m.Update(key)

			if !model.(TrashModel).deleting {
				t.Fatalf("key %v did not reach the confirmed branch; the assertion below would be vacuous", key)
			}
			if len(rec.events) != 0 {
				t.Fatalf("confirm keypress emitted %d events, want 0: %v", len(rec.events), rec.props)
			}
		}
	})
}

// TestTelemetryTUIActionAccountEditNonStatusSaveEmitsNothing pins that only the
// status row's toggle is an instrumented action. A label or priority save comes
// back on the same accountEditSavedMsg and has no value in the bounded enum, so
// capturing it would invent an action name.
func TestTelemetryTUIActionAccountEditNonStatusSaveEmitsNothing(t *testing.T) {
	enableViewTelemetryForTest(t)
	rec := &fakeTelemetry{}
	m := newAccountEditForTelemetry(rec)

	m.Update(accountEditSavedMsg{account: &pb.Account{Id: "acct-1"}})

	if len(rec.events) != 0 {
		t.Fatalf("label/priority save emitted %d events, want 0: %v", len(rec.events), rec.props)
	}
}

// TestTelemetryTUIActionEventName pins the wire name PostHog insights key on.
// Renaming the constant is free; renaming the emitted string silently orphans
// every dashboard built on it.
func TestTelemetryTUIActionEventName(t *testing.T) {
	const want = "tui_action"
	if got := string(telemetry.EventTUIAction); got != want {
		t.Fatalf("event name = %q, want %q", got, want)
	}
	t.Logf("emitted event name is %q", want)
}

// TestTelemetryCreationEventsKeepTheirNames pins that this ticket did not fold
// the three pre-existing TUI creation events into tui_action; renaming them
// would break existing PostHog insights.
func TestTelemetryCreationEventsKeepTheirNames(t *testing.T) {
	for _, event := range []telemetry.Event{
		telemetry.EventSessionCreated,
		telemetry.EventChatCreated,
		telemetry.EventChatAttached,
	} {
		if !telemetry.IsAllowed(event) {
			t.Errorf("%q is no longer registered", event)
		}
	}
	if telemetry.EventSessionCreated != "session_created" {
		t.Errorf("session_created renamed to %q", telemetry.EventSessionCreated)
	}
	if telemetry.EventChatCreated != "chat_created" {
		t.Errorf("chat_created renamed to %q", telemetry.EventChatCreated)
	}
	if telemetry.EventChatAttached != "chat_attached" {
		t.Errorf("chat_attached renamed to %q", telemetry.EventChatAttached)
	}
}

// --- construction helpers ---
//
// Each mirrors its production constructor closely enough that the handler under
// test does not panic. Update's handlers only touch the daemon client inside
// returned tea.Cmd closures, which these tests never execute, so a nil client
// is safe (the idiom newCronListForUpdate already established).

// TestTelemetryAccountOutcomesSurviveNavigatingAway is the regression witness
// for every long-running action capture that lives at the App root.
//
// None of the originating views guard Esc while their RPC is in flight:
// AccountsListModel, AccountEditModel, CronListModel and TrashModel all set
// cancel unconditionally, and App.update<View> then routes away the moment it
// sees Cancelled(). A result landing after that is delegated only to the NEW
// active view, so a capture inside the originating view's own handler would
// never run and the completed action would be silently lost.
//
// Driving App.Update with an unrelated view active is exactly that post-escape
// state. Every one of these events must still fire.
func TestTelemetryAccountOutcomesSurviveNavigatingAway(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  tea.Msg
		want tuiAction
	}{
		{"disabled", accountStatusUpdatedMsg{id: "acct-1", status: accountStatusDisabled}, tuiActionAccountDisabled},
		{"enabled", accountStatusUpdatedMsg{id: "acct-1", status: accountStatusActive}, tuiActionAccountEnabled},
		{"removed", accountRemovedMsg{id: "acct-1"}, tuiActionAccountRemoved},
		// The same navigate-away exposure was found in the account edit, cron
		// list and trash views: each accepts Esc while its RPC is in flight.
		{"edit_status_flip", accountEditSavedMsg{statusFlip: accountStatusDisabled}, tuiActionAccountDisabled},
		{"cron_deleted", cronJobDeletedMsg{id: "job-1"}, tuiActionCronJobDeleted},
		{"cron_updated", cronJobUpdatedMsg{job: &pb.CronJob{Id: "job-1"}}, tuiActionCronJobUpdated},
		{"cron_run_now", cronRunNowMsg{id: "job-1"}, tuiActionCronJobRunNow},
		{"session_restored", sessionRestoredMsg{id: "sess-1"}, tuiActionSessionResurrected},
		{"session_deleted", sessionDeletedMsg{id: "sess-1"}, tuiActionSessionRemoved},
		{"delete_batch_step", deleteProgressMsg{id: "sess-1"}, tuiActionSessionRemoved},
		{"manual_refresh", accountsLoadedMsg{refresh: true}, tuiActionAccountRefreshed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			enableViewTelemetryForTest(t)
			rec := &fakeTelemetry{}
			// The operator escaped out to Settings while the RPC was still in
			// flight, so NO originating view is active any more.
			a := newAppForAccountsTelemetry(rec, ViewSettings)
			a.cronList = NewCronListModel(nil, a.ctx)
			a.cronList.SetTelemetry(rec)
			a.trash = NewTrashModel(nil, a.ctx)
			a.trash.SetTelemetry(rec)

			a.Update(tc.msg)

			if len(rec.events) != 1 {
				t.Fatalf("emitted %d events after navigating away, want 1; the capture "+
					"must not depend on the originating view still being active: %v", len(rec.events), rec.props)
			}
			if got := rec.props[0]["action"]; got != string(tc.want) {
				t.Errorf("action = %v, want %q", got, tc.want)
			}
		})
	}
}

// TestTelemetryAccountRemovedIsCapturedOnce pins the other side of rooting the
// removal capture: accountRemovedMsg is handled by BOTH the accounts list and
// the account edit screen, and the root arm deliberately falls through to
// delegateToActiveView. If either view's handler regained its own capture, the
// event would double-fire whenever that view is the active one.
func TestTelemetryAccountRemovedIsCapturedOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		view View
	}{
		{"accounts_list_active", ViewAccounts},
		{"account_edit_active", ViewAccountEdit},
		{"navigated_away", ViewSettings},
	} {
		t.Run(tc.name, func(t *testing.T) {
			enableViewTelemetryForTest(t)
			rec := &fakeTelemetry{}
			a := newAppForAccountsTelemetry(rec, tc.view)

			a.Update(accountRemovedMsg{id: "acct-1"})

			if len(rec.events) != 1 {
				t.Fatalf("account_removed emitted %d events with %s, want exactly 1: %v",
					len(rec.events), tc.name, rec.props)
			}
		})
	}
}

// TestTelemetryAccountRefreshedRequiresMessageProvenance is the regression
// witness for correlating account_refreshed with the request that caused it.
//
// accountsLoadedMsg is produced by three commands (Init's fetchAccounts, the
// post-action re-fetches, and refreshAccountsCmd). m.refreshing is shared model
// state, so keying the event off it let whichever load happened to land FIRST
// consume the flag: an unrelated result would be reported as account_refreshed
// carrying its own status, and the real refresh would emit nothing.
func TestTelemetryAccountRefreshedRequiresMessageProvenance(t *testing.T) {
	enableViewTelemetryForTest(t)
	rec := &fakeTelemetry{}
	a := newAppForAccountsTelemetry(rec, ViewAccounts)

	// Operator pressed [r] while the Init load was still outstanding.
	a.accountsList.refreshing = true

	// The Init load lands first. It is not the refresh, so it must emit
	// nothing — and must not consume the in-flight refresh either.
	model, _ := a.Update(accountsLoadedMsg{})
	a = model.(App)

	if len(rec.events) != 0 {
		t.Fatalf("an unrelated load emitted %d events, want 0; provenance must come "+
			"off the message, not m.refreshing: %v", len(rec.events), rec.props)
	}
	if !a.accountsList.refreshing {
		t.Fatal("an unrelated load cleared m.refreshing; the manual refresh is still in flight, " +
			"so its spinner and its pending event must both survive")
	}

	// Now the refresh itself returns, failing. It must be the one reported,
	// with ITS status rather than the earlier load's.
	model, _ = a.Update(accountsLoadedMsg{err: errTUIAction, refresh: true})
	a = model.(App)

	if len(rec.events) != 1 {
		t.Fatalf("the real refresh emitted %d events, want 1: %v", len(rec.events), rec.props)
	}
	if got := rec.props[0]["action"]; got != string(tuiActionAccountRefreshed) {
		t.Errorf("action = %v, want %q", got, tuiActionAccountRefreshed)
	}
	if got := rec.props[0]["status"]; got != string(tuiStatusError) {
		t.Errorf("status = %v, want %q; the refresh's own outcome must be reported", got, tuiStatusError)
	}
	if a.accountsList.refreshing {
		t.Error("m.refreshing survived its own refresh result; the spinner would never stop")
	}
}

// newAppForCronTelemetry builds a real App around the cron list, whose delete,
// enabled-toggle and run-now outcomes are captured at the App root.
func newAppForCronTelemetry(client telemetry.Client, view View) App {
	a := NewApp(nil, nil)
	a.telemetry = client
	a.cronList = NewCronListModel(nil, a.ctx)
	a.cronList.SetTelemetry(client)
	a.activeView = view
	return a
}

// newAppForTrashTelemetry builds a real App around the trash view, whose
// restore, delete and delete-all-batch outcomes are captured at the App root.
func newAppForTrashTelemetry(client telemetry.Client, view View) App {
	a := NewApp(nil, nil)
	a.telemetry = client
	a.trash = NewTrashModel(nil, a.ctx)
	a.trash.SetTelemetry(client)
	a.activeView = view
	return a
}

func newAccountsListForTelemetry(client telemetry.Client) AccountsListModel {
	m := NewAccountsListModel(nil, context.Background())
	m.SetTelemetry(client)
	return m
}

func newCronListForTelemetry(client telemetry.Client) CronListModel {
	m := NewCronListModel(nil, context.Background())
	m.SetTelemetry(client)
	return m
}

func newTrashForTelemetry(client telemetry.Client) TrashModel {
	m := NewTrashModel(nil, context.Background())
	m.SetTelemetry(client)
	return m
}

func newAccountEditForTelemetry(client telemetry.Client) AccountEditModel {
	m := NewAccountEditModel(nil, context.Background(), &pb.Account{Id: "acct-1"})
	m.SetTelemetry(client)
	return m
}

// newAppForAccountsTelemetry builds a real App around the accounts screens,
// because the account status flip and removal are captured at the App root
// rather than in the list's or edit screen's own handlers — driving
// AccountsListModel.Update directly would bypass the capture entirely.
//
// view is a parameter precisely because the root capture does not depend on it:
// TestTelemetryAccountOutcomesSurviveNavigatingAway passes a view that is
// neither accounts screen.
func newAppForAccountsTelemetry(client telemetry.Client, view View) App {
	a := NewApp(nil, nil)
	a.telemetry = client
	a.accountsList = NewAccountsListModel(nil, a.ctx)
	a.accountsList.SetTelemetry(client)
	a.accountEdit = NewAccountEditModel(nil, a.ctx, &pb.Account{Id: "acct-1"})
	a.accountEdit.SetTelemetry(client)
	a.activeView = view
	return a
}

// pickerTelemetrySessionID is the session every chat-picker telemetry case
// drives, so a message's sessionID and the picker's can be compared at a glance.
const pickerTelemetrySessionID = "sess-1"

// newAppForPickerTelemetry builds a real App around a chat picker, because the
// picker's three long-running actions are captured at the App root rather than
// in the picker's own handlers — driving ChatPickerModel.Update directly would
// exercise none of the capture.
func newAppForPickerTelemetry(client telemetry.Client, view View) App {
	a := NewApp(nil, nil)
	a.telemetry = client
	a.chatPicker = NewChatPickerModel(nil, a.ctx, pickerTelemetrySessionID, "")
	a.chatPicker.SetTelemetry(client)
	a.activeView = view
	return a
}

// TestTelemetryTUIActionSubViewsAreWiredByApp closes the gap the per-view cases
// above cannot: every one of them calls SetTelemetry by hand, so deleting the
// wiring in app_routing.go / app_delegate.go would leave the whole suite green
// while silently killing every event in production. This drives the real
// switchViewMsg routes and asserts App handed each sub-model its client.
//
// Covers the six switchViewMsg routes in app_routing.go; the four
// mid-delegation reconstructions this ticket added in app_delegate.go are
// covered by TestTelemetryTUIActionDelegateRoutesRewireTelemetry below. Between
// them, each of the ten SetTelemetry call sites BOS-683 introduced has a
// witness. The four that predate it (app_delegate.go's attach and chatpicker
// wiring) are not in scope here — only one of them, the ViewAttach rebuild, has
// a witness today, in app_test.go.
func TestTelemetryTUIActionSubViewsAreWiredByApp(t *testing.T) {
	rec := &fakeTelemetry{}

	for _, tc := range []struct {
		name  string
		view  View
		msg   switchViewMsg
		wired func(App) bool
	}{
		{name: "trash", view: ViewTrash, msg: switchViewMsg{view: ViewTrash},
			wired: func(a App) bool { return a.trash.telemetry != nil }},
		{name: "cron_list", view: ViewCron, msg: switchViewMsg{view: ViewCron},
			wired: func(a App) bool { return a.cronList.telemetry != nil }},
		{name: "cron_form", view: ViewCronForm, msg: switchViewMsg{view: ViewCronForm},
			wired: func(a App) bool { return a.cronForm.telemetry != nil }},
		{name: "accounts_list", view: ViewAccounts, msg: switchViewMsg{view: ViewAccounts},
			wired: func(a App) bool { return a.accountsList.telemetry != nil }},
		{name: "account_register", view: ViewAccountRegister, msg: switchViewMsg{view: ViewAccountRegister},
			wired: func(a App) bool { return a.accountRegister.telemetry != nil }},
		{name: "account_edit", view: ViewAccountEdit,
			msg:   switchViewMsg{view: ViewAccountEdit, account: &pb.Account{Id: "acct-1"}},
			wired: func(a App) bool { return a.accountEdit.telemetry != nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := NewApp(nil, nil)
			a.telemetry = rec

			model, _ := a.handleSwitchView(tc.msg)
			got := model.(App)

			if got.activeView != tc.view {
				t.Fatalf("activeView = %v, want %v — the route under test did not run", got.activeView, tc.view)
			}
			if !tc.wired(got) {
				t.Fatalf("App routed to %v without calling SetTelemetry on the sub-model. "+
					"Every capture in that view is now permanently dead, and no per-view test "+
					"notices because they all wire telemetry by hand.", tc.view)
			}
		})
	}
}

// TestTelemetryTUIActionDelegateRoutesRewireTelemetry covers the four
// SetTelemetry calls BOS-683 added where switchViewMsg never reaches:
// app_delegate.go rebuilds a sub-model mid-delegation, so a dropped wiring line
// there is invisible to the switchView table above. (The file's other four
// SetTelemetry sites are the pre-existing attach/chatpicker wiring, out of scope
// here.)
//
// The cron-form case is the sharp one. app_routing.go builds the form with job
// unset (create mode); updateCron's cronFormOpenMsg branch is the ONLY route
// that sets a.cronForm.job, and cronFormSaveAction keys cron_job_updated on
// exactly that field — so form-edit cron_job_updated is emittable only via the
// route this test drives, and nothing else would notice it going dead.
func TestTelemetryTUIActionDelegateRoutesRewireTelemetry(t *testing.T) {
	rec := &fakeTelemetry{}

	for _, tc := range []struct {
		name  string
		drive func(App) App
		wired func(App) bool
	}{
		{
			name: "cron_list_opens_cron_form_in_edit_mode",
			drive: func(a App) App {
				a.activeView = ViewCron
				a.cronList = NewCronListModel(a.client, a.ctx)
				model, _ := a.updateCron(cronFormOpenMsg{job: &pb.CronJob{Id: "job-1"}})
				return model.(App)
			},
			wired: func(a App) bool {
				// job non-nil is what makes cronFormSaveAction report an edit.
				return a.cronForm.telemetry != nil && a.cronForm.job != nil
			},
		},
		{
			name: "cron_form_save_returns_to_rebuilt_cron_list",
			drive: func(a App) App {
				a.activeView = ViewCronForm
				a.cronForm = NewCronFormModel(a.client, a.ctx)
				model, _ := a.updateCronForm(cronFormDoneMsg{jobID: "job-1"})
				return model.(App)
			},
			wired: func(a App) bool { return a.cronList.telemetry != nil },
		},
		{
			name: "cron_form_cancel_returns_to_rebuilt_cron_list",
			drive: func(a App) App {
				a.activeView = ViewCronForm
				a.cronForm = NewCronFormModel(a.client, a.ctx)
				a.cronForm.cancelled = true
				model, _ := a.updateCronForm(nil)
				return model.(App)
			},
			wired: func(a App) bool { return a.cronList.telemetry != nil },
		},
		{
			name: "account_add_returns_to_rebuilt_accounts_list",
			drive: func(a App) App {
				a.activeView = ViewAccountRegister
				a.accountRegister = NewAccountRegisterModel(nil, a.ctx)
				a.accountRegister.state = registerStateDone
				a.accountRegister.done = true
				model, _ := a.updateAccountRegister(nil)
				return model.(App)
			},
			wired: func(a App) bool { return a.accountsList.telemetry != nil },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := NewApp(nil, nil)
			a.telemetry = rec

			got := tc.drive(a)

			if !tc.wired(got) {
				t.Fatal("App rebuilt the sub-model mid-delegation without calling " +
					"SetTelemetry. Every capture in that view is now permanently dead, and " +
					"no per-view test notices because they all wire telemetry by hand.")
			}
		})
	}
}
