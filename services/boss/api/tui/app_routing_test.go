package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/recurser/boss/api/tui/home"
	"github.com/recurser/boss/api/tui/trash"
	"github.com/recurser/boss/internal/domain/rpc"

	"github.com/recurser/boss/api/tui/chatpicker"

	"github.com/recurser/boss/api/tui/settings"

	repospkg "github.com/recurser/boss/api/tui/repos"

	"github.com/recurser/boss/api/tui/accounts"
	"github.com/recurser/boss/api/tui/login"
	"github.com/recurser/boss/api/tui/ui"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/bossalib/displaystatus"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// appWithRepoList builds an App whose (pre-replacement) repoList holds the given
// repo IDs with the table cursor parked on cursorIdx. The repoAdd-completion and
// repoAdd-cancel handlers read this list to compute which repo to re-highlight.
func appWithRepoList(t *testing.T, ids []string, cursorIdx int) App {
	t.Helper()
	a := NewApp(nil, nil)
	rl := repospkg.NewRepoListModel(nil, a.ctx)
	repos := make([]*pb.Repo, len(ids))
	for i, id := range ids {
		repos[i] = &pb.Repo{Id: id}
	}
	v, _ := rl.Update(repospkg.RepoListLoadedMsg{Repos: repos})
	rl = v.(repospkg.RepoListModel)
	rl.Table().SetCursor(cursorIdx)
	a.repoList = rl
	return a
}

// TestAppRepoAddCompletedActiveView covers the routing guard at app.go:272
// (`msg.err == nil && len(msg.repos) <= 1`). With no error and at most one repo
// the app drops straight back to Home; otherwise it lands on the repo list.
//
// Kills CONDITIONALS_NEGATION on `msg.err == nil` (the error case must NOT go
// Home), CONDITIONALS_NEGATION on `len(msg.repos) <= 1`, and CONDITIONALS_BOUNDARY
// `<= 1` → `< 1` (exactly one repo is the boundary: it must still go Home).
func TestAppRepoAddCompletedActiveView(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		repoLen  int
		wantView ui.View
	}{
		{name: "no error zero repos goes home", err: nil, repoLen: 0, wantView: ui.ViewHome},
		{name: "no error one repo goes home", err: nil, repoLen: 1, wantView: ui.ViewHome},
		{name: "no error two repos shows list", err: nil, repoLen: 2, wantView: ui.ViewRepoList},
		{name: "error zero repos shows list", err: context.Canceled, repoLen: 0, wantView: ui.ViewRepoList},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := NewApp(nil, nil)
			a.activeView = ui.ViewRepoAdd
			a.repoAddCompleting = true

			repos := make([]*pb.Repo, tt.repoLen)
			for i := range repos {
				repos[i] = &pb.Repo{Id: "r"}
			}

			model, _ := a.Update(ui.RepoAddCompletedMsg{Repos: repos, Err: tt.err})
			got := model.(App)

			if got.activeView != tt.wantView {
				t.Fatalf("activeView = %v, want %v", got.activeView, tt.wantView)
			}
			if got.repoAddCompleting {
				t.Fatal("repoAddCompleting should be reset to false after completion")
			}
		})
	}
}

// TestAppRepoAddCompletedHighlight covers highlight selection on the non-Home
// branch (more than one repo, so routing falls through to the repo list where a
// highlight target is chosen).
//
//   - An explicit highlightID must win outright (kills the `== ""` negation:
//     a `!= ""` mutant would discard it and fall back to the cursor).
//   - With no explicit highlightID the cursor's repo is highlighted, for cursors
//     at index 0 and mid-range (kills the `>= 0` boundary `>` and both
//     negations on the cursor-range check).
func TestAppRepoAddCompletedHighlight(t *testing.T) {
	tests := []struct {
		name          string
		ids           []string
		cursor        int
		explicit      string
		wantHighlight string
	}{
		{name: "explicit highlight wins", ids: []string{"a0", "a1"}, cursor: 1, explicit: "chosen", wantHighlight: "chosen"},
		{name: "cursor mid range", ids: []string{"a0", "a1"}, cursor: 1, explicit: "", wantHighlight: "a1"},
		{name: "cursor at zero", ids: []string{"a0", "a1"}, cursor: 0, explicit: "", wantHighlight: "a0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := appWithRepoList(t, tt.ids, tt.cursor)
			a.activeView = ui.ViewRepoAdd

			// Two repos in the message + empty highlightID: falls through to the
			// cursor-based highlight path (non-Home, non-settings branch).
			msg := ui.RepoAddCompletedMsg{
				Repos:       []*pb.Repo{{Id: "x"}, {Id: "y"}},
				Err:         nil,
				HighlightID: tt.explicit,
			}
			model, _ := a.Update(msg)
			got := model.(App)

			if got.activeView != ui.ViewRepoList {
				t.Fatalf("activeView = %v, want %v", got.activeView, ui.ViewRepoList)
			}
			if got.repoList.HighlightRepoID() != tt.wantHighlight {
				t.Fatalf("highlightRepoID = %q, want %q", got.repoList.HighlightRepoID(), tt.wantHighlight)
			}
		})
	}
}

// TestAppRepoAddCancelHighlight covers app.go:440, the cursor-range guard that
// picks the repo to re-highlight when the user cancels the add-repo flow.
// Mirrors the completion-path highlight logic but reached via Cancelled().
func TestAppRepoAddCancelHighlight(t *testing.T) {
	tests := []struct {
		name          string
		ids           []string
		cursor        int
		wantHighlight string
	}{
		{name: "cursor mid range", ids: []string{"b0", "b1"}, cursor: 1, wantHighlight: "b1"},
		{name: "cursor at zero", ids: []string{"b0", "b1"}, cursor: 0, wantHighlight: "b0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := appWithRepoList(t, tt.ids, tt.cursor)
			a.activeView = ui.ViewRepoAdd
			ra := repospkg.NewRepoAddModel(nil, a.ctx)
			v, _ := ra.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
			ra = v.(repospkg.RepoAddModel)
			a.repoAdd = ra

			model, _ := a.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
			got := model.(App)

			if got.activeView != ui.ViewRepoList {
				t.Fatalf("activeView = %v, want %v", got.activeView, ui.ViewRepoList)
			}
			if got.repoList.HighlightRepoID() != tt.wantHighlight {
				t.Fatalf("highlightRepoID = %q, want %q", got.repoList.HighlightRepoID(), tt.wantHighlight)
			}
		})
	}
}

// TestAppSessionSettingsReturnHighlight covers app.go:477, the cursor-range
// guard that selects which chat to re-highlight when returning from session
// settings to the chat picker.
func TestAppSessionSettingsReturnHighlight(t *testing.T) {
	tests := []struct {
		name          string
		agentIDs      []string
		cursor        int
		wantHighlight string
	}{
		{name: "cursor mid range", agentIDs: []string{"c0", "c1"}, cursor: 1, wantHighlight: "c1"},
		{name: "cursor at zero", agentIDs: []string{"c0", "c1"}, cursor: 0, wantHighlight: "c0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := NewApp(nil, nil)
			cp := chatpicker.NewModel(nil, a.ctx, "sid", "", chatpicker.ExternalActions{ChatTitle: agentChatTitle, SupportsNewTab: hasNewTabSupport(), OpenNewTab: openInNewTab, OpenURL: func(url string) error { return openURLFunc(url) }})
			chats := make([]*pb.AgentChat, len(tt.agentIDs))
			for i, id := range tt.agentIDs {
				chats[i] = &pb.AgentChat{AgentSessionId: id}
			}
			cp, _ = updateModel(cp, chatpicker.ChatsListedMsg{Chats: chats})
			cp.Table().SetCursor(tt.cursor)
			a.chatPicker = cp

			a.activeView = ui.ViewSessionSettings
			ss := settings.NewSessionSettingsModel(nil, a.ctx, "sid")
			cancelled, _ := ss.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
			ss = cancelled.(settings.SessionSettingsModel)
			a.sessionSettings = ss

			model, _ := a.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
			got := model.(App)

			if got.activeView != ui.ViewChatPicker {
				t.Fatalf("activeView = %v, want %v", got.activeView, ui.ViewChatPicker)
			}
			if got.chatPicker.HighlightID() != tt.wantHighlight {
				t.Fatalf("chatPicker.highlightID = %q, want %q", got.chatPicker.HighlightID(), tt.wantHighlight)
			}
		})
	}
}

// TestAppTrashRestoreRoutesToChatPicker covers app.go:491
// (`a.trash.RestoredSessionID() != ""`). A restored session must route the app
// into the chat picker for that session. Kills the negation (`!=` → `==` would
// leave the app stuck in the trash view).
func TestAppTrashRestoreRoutesToChatPicker(t *testing.T) {
	t.Run("restored session opens chat picker", func(t *testing.T) {
		a := NewApp(nil, nil)
		a.activeView = ui.ViewTrash
		tr := NewTrashModel(&routingRestoreClient{}, a.ctx)
		tr, _ = updateModel(tr, trash.ListMsg{Sessions: []*pb.Session{{Id: "restored-session"}}})
		tr, open := updateModel(tr, tea.KeyPressMsg{Code: 'r'})
		tr, read := updateModel(tr, open())
		tr, _ = updateModel(tr, read())
		a.trash = tr

		model, _ := a.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
		got := model.(App)

		if got.activeView != ui.ViewChatPicker {
			t.Fatalf("activeView = %v, want %v", got.activeView, ui.ViewChatPicker)
		}
	})

	t.Run("no restored session stays in trash", func(t *testing.T) {
		a := NewApp(nil, nil)
		a.activeView = ui.ViewTrash
		a.trash = NewTrashModel(nil, a.ctx)

		model, _ := a.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
		got := model.(App)

		if got.activeView != ui.ViewTrash {
			t.Fatalf("activeView = %v, want %v (no restore, no cancel)", got.activeView, ui.ViewTrash)
		}
	})
}

// TestAppBugReportGlobalShortcuts covers the ctrl+g guard and its deprecated
// ctrl+b alias. From any non-bug-report view either opens the bug-report modal;
// while already in it, either falls through to the active modal.
//
// Kills the negation (`==` → `!=`): from Home, a `!=` mutant would take the
// break and never open the modal, leaving the app on Home.
func TestAppBugReportGlobalShortcuts(t *testing.T) {
	ctrlG := tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl}
	ctrlB := tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl}
	if ctrlG.String() != "ctrl+g" {
		t.Fatalf("ctrlG.String() = %q, want %q", ctrlG.String(), "ctrl+g")
	}

	for _, shortcut := range []struct {
		name string
		msg  tea.KeyPressMsg
	}{
		{name: "new shortcut", msg: ctrlG},
		{name: "deprecated alias", msg: ctrlB},
	} {
		t.Run(shortcut.name, func(t *testing.T) {
			for _, view := range allViewConstants(t) {
				t.Run(view.Name, func(t *testing.T) {
					a := NewApp(nil, nil)
					a.activeView = view.View

					_, handled := a.handleGlobalKey(shortcut.msg)
					if view.View == ui.ViewBugReport {
						if handled {
							t.Fatalf("%s while already open: handled = true, want false", shortcut.msg.String())
						}
						return
					}
					if !handled {
						t.Fatalf("%s from %s was not handled", shortcut.msg.String(), view.Name)
					}
					if a.activeView != ui.ViewBugReport {
						t.Fatalf("%s from %s: activeView = %v, want %v", shortcut.msg.String(), view.Name, a.activeView, ui.ViewBugReport)
					}
				})
			}
		})
	}
}

func TestAppBugReportGlobalShortcutsPreserveGuards(t *testing.T) {
	shortcuts := []tea.KeyPressMsg{
		{Code: 'g', Mod: tea.ModCtrl},
		{Code: 'b', Mod: tea.ModCtrl},
	}
	for _, shortcut := range shortcuts {
		t.Run(shortcut.String(), func(t *testing.T) {
			t.Run("already open falls through", func(t *testing.T) {
				a := NewApp(nil, nil)
				a.activeView = ui.ViewBugReport
				if _, handled := a.handleGlobalKey(shortcut); handled {
					t.Fatalf("%s while bug report is open: handled = true, want false", shortcut.String())
				}
			})

			for _, busy := range []struct {
				name       string
				upgrading  bool
				restarting bool
			}{
				{name: "upgrading", upgrading: true},
				{name: "restarting", restarting: true},
			} {
				t.Run(busy.name, func(t *testing.T) {
					a := NewApp(nil, nil)
					a.activeView = ui.ViewHome
					a.home.Upgrading = busy.upgrading
					a.home.Restarting = busy.restarting
					if _, handled := a.handleGlobalKey(shortcut); !handled {
						t.Fatalf("%s while Home is %s: handled = false, want true", shortcut.String(), busy.name)
					}
					if a.activeView != ui.ViewHome {
						t.Fatalf("%s while Home is %s: activeView = %v, want %v", shortcut.String(), busy.name, a.activeView, ui.ViewHome)
					}
				})
			}
		})
	}
}

func TestAppCtrlCStillQuitsAndUnrelatedGlobalKeyFallsThrough(t *testing.T) {
	a := NewApp(nil, nil)
	if _, handled := a.handleGlobalKey(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}); !handled || !a.quitting {
		t.Fatal("ctrl+c must remain the handled app-global quit shortcut")
	}

	a = NewApp(nil, nil)
	if _, handled := a.handleGlobalKey(tea.KeyPressMsg{Code: 'z', Mod: tea.ModCtrl}); handled {
		t.Fatal("unrelated ctrl+z must fall through to the active view")
	}
}

func TestAppRoutesLogoutResultToHomeWhileBugReportIsOpen(t *testing.T) {
	a := NewApp(nil, nil)
	a.activeView = ui.ViewBugReport
	a.home.LoggedIn = true
	a.home.LogoutPending = true

	model, _ := a.Update(logoutMsg{Err: errors.New("keychain refused the delete")})
	got := model.(App)

	if got.activeView != ui.ViewBugReport {
		t.Fatalf("activeView = %v, want ViewBugReport", got.activeView)
	}
	if got.home.LogoutPending {
		t.Fatal("logoutPending remained true after failed logout while modal was open")
	}
	if got.home.LogoutError != "keychain refused the delete" {
		t.Fatalf("logoutError = %q, want logout failure", got.home.LogoutError)
	}
}

// TestAppRoutesRenameFailureToHomeWhileTheChatPickerIsOpen covers the window
// commitRename opens: it closes the editor and returns before the RPC does, so
// the very next enter can switch to the chat picker while the write is still in
// flight. Without an explicit arm the reply reaches whichever view happens to
// be active, which drops it — and the status line is the ONLY report a failed
// rename ever gets, so the operator would be left believing a rename landed
// that never did.
//
// Falsification: delete the `case sessionRenamedMsg:` arm from App.Update
// (app.go:261) and this fails — home.Status stays empty. Performed once and the
// arm restored.
func TestAppRoutesRenameFailureToHomeWhileTheChatPickerIsOpen(t *testing.T) {
	a := NewApp(nil, nil)
	a.activeView = ui.ViewChatPicker

	model, _ := a.Update(sessionRenamedMsg{Err: errors.New("daemon unavailable")})
	got := model.(App)

	if got.activeView != ui.ViewChatPicker {
		t.Fatalf("activeView = %v, want ViewChatPicker; routing must not yank the operator back", got.activeView)
	}
	if !strings.Contains(got.home.Status, "Rename failed") {
		t.Fatalf("home.Status = %q, want the rename failure waiting on the board", got.home.Status)
	}
	if !got.home.StatusErr() {
		t.Fatal("the rename failure was not recorded as a failure")
	}
}

func TestAppPreservesDeferredLogoutFailureReturningFromSettings(t *testing.T) {
	a := NewApp(nil, nil)
	a.activeView = ui.ViewSettings
	a.home.LoggedIn = true
	a.home.LoggedInEmail = "dev@example.com"
	a.home.LogoutPending = true

	model, _ := a.Update(logoutMsg{Err: errors.New("keychain refused the delete")})
	got := model.(App)

	model, _ = got.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	got = model.(App)

	if got.activeView != ui.ViewHome {
		t.Fatalf("activeView = %v, want ViewHome", got.activeView)
	}
	if got.home.LogoutPending {
		t.Fatal("logoutPending remained true after failed logout")
	}
	if got.home.LogoutError != "keychain refused the delete" {
		t.Fatalf("logoutError = %q, want logout failure after returning from Settings", got.home.LogoutError)
	}
	if !got.home.LoggedIn || got.home.LoggedInEmail != "dev@example.com" {
		t.Fatalf("returning Home after a failed logout dropped signed-in state: loggedIn=%t email=%q", got.home.LoggedIn, got.home.LoggedInEmail)
	}
}

// TestAppCurrentSession covers app.go:589 (`a.activeView == ViewChatPicker`).
// Only the chat picker exposes a current session; every other view reports nil.
// Kills the negation (`==` → `!=` would return nil for the chat picker).
func TestAppCurrentSession(t *testing.T) {
	a := NewApp(nil, nil)
	want := &pb.Session{Id: "session-1"}
	a.chatPicker = pickerSessionFixture(want.Id, want)

	a.activeView = ui.ViewChatPicker
	if got := a.currentSession(); got != want {
		t.Fatalf("currentSession() in chat picker = %v, want %v", got, want)
	}

	a.activeView = ui.ViewHome
	if got := a.currentSession(); got != nil {
		t.Fatalf("currentSession() in home = %v, want nil", got)
	}
}

// TestAppChatPickerStaysOnMerge guards the critical regression: after a
// successful merge the app must NOT bounce back to the session list. The user
// stays on ViewChatPicker so they can archive in place.
func TestAppChatPickerStaysOnMerge(t *testing.T) {
	a := NewApp(nil, nil)
	a.activeView = ui.ViewChatPicker
	a.chatPicker = pickerSessionFixture("s1", &pb.Session{Id: "s1"})
	a.chatPicker, _ = updateModel(a.chatPicker, chatpicker.MergeResultMsg{SessionID: "s1"})

	model, _ := a.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	got := model.(App)

	if got.activeView != ui.ViewChatPicker {
		t.Fatalf("activeView = %v after merge, want ViewChatPicker (must stay on detail view)", got.activeView)
	}
}

// TestAppChatPickerCancelAfterMergeReturnsHome guards that cancel still
// returns to the session list even when the session was already merged,
// and that mergedOptimisticID is set on the returned home model.
func TestAppChatPickerCancelAfterMergeReturnsHome(t *testing.T) {
	a := NewApp(nil, nil)
	a.activeView = ui.ViewChatPicker
	a.chatPicker = pickerSessionFixture("s1", &pb.Session{Id: "s1"})
	a.chatPicker, _ = updateModel(a.chatPicker, chatpicker.MergeResultMsg{SessionID: "s1"})
	a.chatPicker, _ = updateModel(a.chatPicker, tea.KeyPressMsg{Code: tea.KeyEsc})

	model, _ := a.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	got := model.(App)

	if got.activeView != ui.ViewHome {
		t.Fatalf("activeView = %v after cancel+merge, want ViewHome", got.activeView)
	}
	if got.home.MergedOptimisticID != "s1" {
		t.Fatalf("mergedOptimisticID = %q, want %q (optimistic merge must carry through)", got.home.MergedOptimisticID, "s1")
	}
}

// TestAppChatPickerArchiveAfterMergeReturnsHome guards that archive still
// returns to the session list even when the session was already merged,
// and that mergedOptimisticID is set on the returned home model.
func TestAppChatPickerArchiveAfterMergeReturnsHome(t *testing.T) {
	a := NewApp(nil, nil)
	a.activeView = ui.ViewChatPicker
	a.chatPicker = pickerSessionFixture("s1", &pb.Session{Id: "s1"})
	a.chatPicker, _ = updateModel(a.chatPicker, chatpicker.MergeResultMsg{SessionID: "s1"})
	a.chatPicker, _ = updateModel(a.chatPicker, chatpicker.ArchiveResultMsg{SessionID: "s1"})

	model, _ := a.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	got := model.(App)

	if got.activeView != ui.ViewHome {
		t.Fatalf("activeView = %v after archive+merge, want ViewHome", got.activeView)
	}
	if got.home.MergedOptimisticID != "s1" {
		t.Fatalf("mergedOptimisticID = %q, want %q (optimistic merge must carry through)", got.home.MergedOptimisticID, "s1")
	}
}

// TestAppChatPickerRefreshDetectedArchiveReturnsHome guards the poll-detected
// archive path: no TUI-initiated archive RPC, just a refreshed session that
// already carries ArchivedAt (e.g. archive-after-merge fired server-side, or
// an external/manual archive). The chatpicker.RefreshMsg must flow through
// chatpicker.Model.Update (setting m.archived), and the app loop must then
// route back to ViewHome exactly as it does for a TUI-initiated archive.
func TestAppChatPickerRefreshDetectedArchiveReturnsHome(t *testing.T) {
	a := NewApp(nil, nil)
	a.activeView = ui.ViewChatPicker
	a.chatPicker = pickerSessionFixture("s1", &pb.Session{Id: "s1"})

	model, _ := a.Update(chatpicker.RefreshMsg{Session: &pb.Session{Id: "s1", ArchivedAt: timestamppb.Now()}})
	got := model.(App)

	if got.activeView != ui.ViewHome {
		t.Fatalf("activeView = %v after refresh-detected archive, want ViewHome", got.activeView)
	}
}

// TestEscWhileArchivingUnionsOverrides guards that returning from another
// in-flight archive does not replace the first optimistic archive state.
func TestEscWhileArchivingUnionsOverrides(t *testing.T) {
	a := NewApp(nil, nil)
	a.home.MarkArchiving("s1")
	a.activeView = ui.ViewChatPicker
	// Archive in flight (no pre-set cancel) so the real ESC key path through
	// chatpicker.Update sets cancel=true, exercising the actual keyboard route
	// rather than a synthetic flag.
	a.chatPicker = pickerActionFixture("s2", "a")

	model, _ := a.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	got := model.(App)

	if got.activeView != ui.ViewHome {
		t.Fatalf("activeView = %v after esc-while-archiving, want ViewHome", got.activeView)
	}
	for _, id := range []string{"s1", "s2"} {
		if !got.home.IsArchiving(id) || !got.home.ArchiveInFlight(id) {
			t.Fatalf("expected %s to remain archiving and in flight after ESC carry", id)
		}
	}
}

// TestArchiveFailureClearsOverride guards that a failed chatpicker.ArchiveResultMsg
// delivered to the App clears only its matching home override, even when
// the user has already navigated away from the chatpicker (ViewHome).
func TestArchiveFailureClearsOverride(t *testing.T) {
	a := NewApp(nil, nil)
	a.home.MarkArchiving("s1")
	a.home.MarkArchiving("s2")

	model, _ := a.Update(chatpicker.ArchiveResultMsg{SessionID: "s1", Err: errors.New("boom")})
	got := model.(App)

	if got.home.IsArchiving("s1") || got.home.ArchiveInFlight("s1") {
		t.Fatal("expected failed s1 archive override cleared")
	}
	if !got.home.IsArchiving("s2") || !got.home.ArchiveInFlight("s2") {
		t.Fatal("expected concurrent s2 archive state retained")
	}
}

// TestArchiveFailureRebuildsHomeRows ensures a failed archive immediately
// replaces the cached optimistic table status instead of waiting for a spinner
// tick or the next session poll.
func TestArchiveFailureRebuildsHomeRows(t *testing.T) {
	a := NewApp(nil, nil)
	a.home.Sessions = []*pb.Session{{
		Id:            "s1",
		DisplayLabel:  "passing",
		DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_SUCCESS,
	}}
	a.home.MarkArchiving("s1")
	a.home.BuildTableRows()
	if !strings.Contains(a.home.Table.Rows()[0][4], "archiving") {
		t.Fatal("precondition: expected cached row to render archiving")
	}

	model, _ := a.Update(chatpicker.ArchiveResultMsg{SessionID: "s1", Err: errors.New("boom")})
	got := model.(App)

	if strings.Contains(got.home.Table.Rows()[0][4], "archiving") {
		t.Fatal("failed archive left cached row rendering archiving")
	}
	if !strings.Contains(got.home.Table.Rows()[0][4], "passing") {
		t.Fatalf("failed archive row status = %q, want real status", got.home.Table.Rows()[0][4])
	}
}

// TestArchiveSuccessKeepsOverrideAtAppLevel guards the design intent that a
// successful chatpicker.ArchiveResultMsg does NOT clear the override at the app level: the
// override persists until the session actually leaves the session list (cleared
// in the sessionListMsg path), so the row keeps showing "archiving" rather than
// flickering back to the real status before it disappears.
func TestArchiveSuccessKeepsOverrideAtAppLevel(t *testing.T) {
	a := NewApp(nil, nil)
	a.home.MarkArchiving("s1")
	a.home.MarkArchiving("s2")

	model, _ := a.Update(chatpicker.ArchiveResultMsg{SessionID: "s1", Err: nil})
	got := model.(App)

	if !got.home.IsArchiving("s1") {
		t.Fatal("expected successful s1 override retained at app level")
	}
	// The archive resolved, so it is no longer in flight even though the
	// override lingers for rendering until the row leaves the list.
	if got.home.ArchiveInFlight("s1") {
		t.Fatal("expected s1 in-flight state cleared on archive success")
	}
	if !got.home.IsArchiving("s2") || !got.home.ArchiveInFlight("s2") {
		t.Fatal("expected concurrent s2 archive state retained")
	}
}

// TestReEnterArchivingSessionSeedsArchiving guards that re-entering the
// view-session screen while an archive is still in flight seeds the new
// chatpicker.Model with archiving=true so the banner shows the override.
func TestReEnterArchivingSessionSeedsArchiving(t *testing.T) {
	a := NewApp(nil, nil)
	a.home.MarkArchiving("s1")

	model, _ := a.Update(ui.SwitchViewMsg{View: ui.ViewChatPicker, SessionID: "s1"})
	got := model.(App)

	if got.activeView != ui.ViewChatPicker {
		t.Fatalf("activeView = %v, want ViewChatPicker", got.activeView)
	}
	if !got.chatPicker.Archiving() {
		t.Fatalf("chatPicker.archiving = false, want true when re-entering archiving session")
	}
}

// TestReEnterAfterArchiveSuccessDoesNotSeedArchiving guards the stale-override
// case: after a successful archive the override lingers on the still-present
// row for rendering (archiveInFlight=false), but pressing Enter on that row
// must NOT seed archiving=true, which would leave the picker stuck on
// "Archiving session…" swallowing every key but Esc with no chatpicker.ArchiveResultMsg
// ever arriving for the freshly created model.
func TestReEnterAfterArchiveSuccessDoesNotSeedArchiving(t *testing.T) {
	a := NewApp(nil, nil)
	a.home.MarkArchiving("s1")
	a.home.ResolveArchive("s1", nil, false)

	model, _ := a.Update(ui.SwitchViewMsg{View: ui.ViewChatPicker, SessionID: "s1"})
	got := model.(App)

	if got.chatPicker.Archiving() {
		t.Fatalf("chatPicker.archiving = true after archive success, want false (stale override must not re-enter as in-flight)")
	}
}

// TestReEnterNonArchivingSessionNoSeed guards that other sessions are NOT
// seeded with archiving=true when a different session is still in flight.
func TestReEnterNonArchivingSessionNoSeed(t *testing.T) {
	a := NewApp(nil, nil)
	a.home.MarkArchiving("s1")

	model, _ := a.Update(ui.SwitchViewMsg{View: ui.ViewChatPicker, SessionID: "s2"})
	got := model.(App)

	if got.chatPicker.Archiving() {
		t.Fatalf("chatPicker.archiving = true for non-archiving session s2, want false")
	}
}

// TestNewHomeModelPreservesInFlightArchiveOverride guards that rebuilding Home
// (navigating away to a different session or subview and back) preserves an
// archive override whose RPC is still outstanding, so the row keeps rendering
// "archiving" and re-entry stays in-flight instead of reverting to the real
// status.
func TestNewHomeModelCopiesArchiveStateWithoutAliasing(t *testing.T) {
	a := NewApp(nil, nil)
	a.home.MarkArchiving("s1")
	a.home.MarkArchiving("s2")
	a.home.MarkArchiving("s3")
	a.home.ResolveArchive("s3", nil, false)

	rebuilt := a.newHomeModel()

	if !rebuilt.IsArchiving("s1") || !rebuilt.IsArchiving("s2") || !rebuilt.IsArchiving("s3") || !rebuilt.ArchiveInFlight("s1") || !rebuilt.ArchiveInFlight("s2") || rebuilt.ArchiveInFlight("s3") {
		t.Fatal("rebuilt Home did not preserve per-session archive state")
	}
	rebuilt.ResolveArchive("s1", errors.New("boom"), false)
	if !a.home.IsArchiving("s1") || !a.home.ArchiveInFlight("s1") {
		t.Fatal("rebuilt Home aliases archive state from prior Home")
	}
}

// TestNewHomeModelPreservesReorderState guards the other half of the optimistic
// model newHomeModel already copies. home.Sessions is preserved, so a rebuilt
// board still renders the order the reorder chord swapped into it — but without
// moveOverrideOrder the override that DEFENDS that order is gone, so the next
// poll snaps the row back and it jumps again when the write lands: exactly the
// flicker the override exists to prevent. A dropped moveInFlight is worse still,
// because the outstanding reply then arrives against a model that believes
// nothing is in flight, and a dropped queue silently loses a chord the user
// already saw take effect.
func TestNewHomeModelPreservesReorderState(t *testing.T) {
	a := NewApp(nil, nil)
	a.home.Sessions = []*pb.Session{{Id: "s2"}, {Id: "s1"}, {Id: "s3"}}
	a.home.MoveOverrideOrder = []string{"s2", "s1", "s3"}
	a.home.MoveInFlight = 1
	a.home.MovePending = []queuedMove{{SessionID: "s3", Direction: pb.MoveDirection_MOVE_DIRECTION_UP}}

	rebuilt := a.newHomeModel()

	if got := sessionIDOrder(rebuilt.Sessions); len(got) != 3 || got[0] != "s2" {
		t.Fatalf("rebuilt session order = %v, want the optimistic s2 first; the rest of this test "+
			"is about the override that defends exactly this order", got)
	}
	if len(rebuilt.MoveOverrideOrder) != 3 || rebuilt.MoveOverrideOrder[0] != "s2" {
		t.Fatalf("rebuilt moveOverrideOrder = %v, want the prior model's; without it the next poll "+
			"undoes the move the rebuilt board is already showing", rebuilt.MoveOverrideOrder)
	}
	if rebuilt.MoveInFlight != 1 {
		t.Fatalf("rebuilt moveInFlight = %d, want 1; the outstanding reply would otherwise arrive "+
			"against a model that believes nothing is in flight", rebuilt.MoveInFlight)
	}
	if len(rebuilt.MovePending) != 1 || rebuilt.MovePending[0].SessionID != "s3" {
		t.Fatalf("rebuilt movePending = %v, want the queued chord; dropping it loses a move the "+
			"user already saw take effect on the board", rebuilt.MovePending)
	}

	// Copied, not shared: the replacement model owns its own future.
	rebuilt.MoveOverrideOrder[0] = "s9"
	rebuilt.MovePending[0].SessionID = "s9"
	if a.home.MoveOverrideOrder[0] != "s2" || a.home.MovePending[0].SessionID != "s3" {
		t.Fatal("rebuilt Home aliases reorder state from the prior Home")
	}
}

// TestNewHomeModelPreservesQuestionState ensures returning to Home does not
// treat an already-notified question as a new notification edge.
func TestNewHomeModelPreservesQuestionState(t *testing.T) {
	a := NewApp(nil, nil)
	a.home.Sessions = []*pb.Session{{
		Id:           "s1",
		DisplayLabel: displaystatus.QuestionLabel,
	}}

	rebuilt := a.newHomeModel()
	_, cmd := rebuilt.Update(sessionListMsg{Sessions: []*pb.Session{{
		Id:           "s1",
		DisplayLabel: displaystatus.QuestionLabel,
	}}})
	if cmd != nil {
		t.Fatal("recreated Home returned a notification command for an unchanged question")
	}
}

// TestNewHomeModelPreservesRetainedReloginState guards that returning to Home
// does not hide the retained-login warning or show the guest cloud promo again.
func TestNewHomeModelPreservesRetainedReloginState(t *testing.T) {
	a := NewApp(nil, nil)
	a.home.NeedsRelogin = true
	a.home.ReloginReason = auth.ReloginReasonRefreshOutcomeUnknown

	rebuilt := a.newHomeModel()

	if line := rebuilt.AuthReloginLine(); !strings.Contains(line, "Sign in required") {
		t.Fatalf("rebuilt authReloginLine() = %q, want Sign in required", line)
	}
	if rebuilt.ReloginReason != auth.ReloginReasonRefreshOutcomeUnknown {
		t.Fatalf("rebuilt reloginReason = %q, want %q", rebuilt.ReloginReason, auth.ReloginReasonRefreshOutcomeUnknown)
	}
	if rebuilt.GuestCloudOfferVisible() {
		t.Fatal("rebuilt Home shows the guest cloud promo while re-login is required")
	}

	unflaggedApp := NewApp(nil, nil)
	unflagged := unflaggedApp.newHomeModel()
	if line := unflagged.AuthReloginLine(); line != "" {
		t.Fatalf("unflagged authReloginLine() = %q, want empty", line)
	}
}

func TestUpdateLoginClearsRetainedReloginAfterSuccess(t *testing.T) {
	a := NewApp(nil, nil)
	a.activeView = ui.ViewLogin
	a.login = login.NewModel(nil, nil, a.ctx)
	a.home.NeedsRelogin = true
	a.home.ReloginReason = auth.ReloginReasonRefreshOutcomeUnknown
	a.home.AuthStatusGeneration = 7
	a.login, _ = updateModel(a.login, login.CompleteMsg{Verification: auth.LoginVerification{Outcome: auth.LoginVerified}})
	a.login, _ = updateModel(a.login, login.AutoReturnMsg{})

	model, _ := a.updateLogin(nil)
	got := model.(App)

	if got.home.NeedsRelogin || got.home.ReloginReason != "" {
		t.Fatalf("successful login retained relogin state: needsRelogin=%v reason=%q", got.home.NeedsRelogin, got.home.ReloginReason)
	}
	if line := got.home.AuthReloginLine(); line != "" {
		t.Fatalf("successful login Home warning = %q, want empty", line)
	}
	if got.home.AuthStatusGeneration != 8 {
		t.Fatalf("successful login generation = %d, want 8", got.home.AuthStatusGeneration)
	}
}

func TestUpdateLoginClearsVerifiedReloginAfterSubscriptionCancel(t *testing.T) {
	a := NewApp(nil, nil)
	a.activeView = ui.ViewLogin
	a.login = login.NewModel(nil, nil, a.ctx)
	a.home.NeedsRelogin = true
	a.home.ReloginReason = auth.ReloginReasonRefreshOutcomeUnknown
	a.home.AuthStatusGeneration = 3
	a.login, _ = updateModel(a.login, login.CompleteMsg{Verification: auth.LoginVerification{Outcome: auth.LoginVerified}})
	a.login, _ = updateModel(a.login, tea.KeyPressMsg{Code: tea.KeyEsc})
	a.login, _ = updateModel(a.login, login.AutoReturnMsg{})

	model, _ := a.updateLogin(nil)
	got := model.(App)

	if got.home.NeedsRelogin || got.home.ReloginReason != "" {
		t.Fatalf("verified subscription cancel retained relogin state: needsRelogin=%v reason=%q", got.home.NeedsRelogin, got.home.ReloginReason)
	}
	if got.home.AuthStatusGeneration != 4 {
		t.Fatalf("verified subscription cancel generation = %d, want 4", got.home.AuthStatusGeneration)
	}
}

func TestUpdateLoginPreservesRetainedReloginAfterDismissedError(t *testing.T) {
	a := NewApp(nil, nil)
	a.activeView = ui.ViewLogin
	a.login = login.NewModel(nil, nil, a.ctx)
	a.home.NeedsRelogin = true
	a.home.ReloginReason = auth.ReloginReasonRefreshOutcomeUnknown
	a.login, _ = updateModel(a.login, login.ErrorMsg{Err: errors.New("login failed")})
	a.login, _ = updateModel(a.login, tea.KeyPressMsg{Code: tea.KeyEnter})

	model, _ := a.updateLogin(nil)
	got := model.(App)

	if !got.home.NeedsRelogin || got.home.ReloginReason != auth.ReloginReasonRefreshOutcomeUnknown {
		t.Fatalf("dismissed login error relogin state: needsRelogin=%v reason=%q", got.home.NeedsRelogin, got.home.ReloginReason)
	}
	if line := got.home.AuthReloginLine(); !strings.Contains(line, "Sign in required") {
		t.Fatalf("dismissed login error Home warning = %q, want Sign in required", line)
	}
}

func TestUpdateLoginInvalidatesPreLoginAuthStatus(t *testing.T) {
	a := NewApp(nil, nil)
	a.activeView = ui.ViewLogin
	a.login = login.NewModel(nil, nil, a.ctx)
	a.home.NeedsRelogin = true
	a.home.ReloginReason = auth.ReloginReasonRefreshOutcomeUnknown
	a.login, _ = updateModel(a.login, login.CompleteMsg{Verification: auth.LoginVerification{Outcome: auth.LoginVerified}})
	a.login, _ = updateModel(a.login, login.AutoReturnMsg{})

	model, _ := a.updateLogin(nil)
	got := model.(App)
	staleGeneration := got.home.AuthStatusGeneration - 1

	updated, _ := got.home.Update(authStatusMsg{
		Generation:    staleGeneration,
		LoggedIn:      false,
		Email:         "dev@example.com",
		NeedsRelogin:  true,
		ReloginReason: auth.ReloginReasonRefreshOutcomeUnknown,
	})
	home := updated.(HomeModel)

	if home.NeedsRelogin || home.ReloginReason != "" {
		t.Fatalf("stale auth status restored relogin state: needsRelogin=%v reason=%q", home.NeedsRelogin, home.ReloginReason)
	}
	if line := home.AuthReloginLine(); line != "" {
		t.Fatalf("stale auth status restored Home warning = %q, want empty", line)
	}
}

// TestNewHomeModelPreservesResolvedArchiveOverride guards that a resolved
// archive's render override continues until the next poll removes its row.
func TestNewHomeModelPreservesResolvedArchiveOverride(t *testing.T) {
	a := NewApp(nil, nil)
	a.home.MarkArchiving("s1")
	a.home.ResolveArchive("s1", nil, false)

	rebuilt := a.newHomeModel()

	if !rebuilt.IsArchiving("s1") {
		t.Fatal("resolved override should survive a Home rebuild until the row leaves")
	}
	if rebuilt.ArchiveInFlight("s1") {
		t.Fatal("resolved archive should not be in flight after Home rebuild")
	}
}

// TestReturnToHomeFromOtherSessionKeepsArchiveOverride guards the reviewer's
// scenario end-to-end: session s1's archive is in flight (override carried on
// Home), the user opens and exits a different, non-archiving session s2, and
// s1's in-flight override must survive the Home rebuild.
func TestReturnToHomeFromOtherSessionKeepsArchiveOverride(t *testing.T) {
	a := NewApp(nil, nil)
	// s1's archive is in flight; its override rides on Home.
	a.home.MarkArchiving("s1")
	// The user is viewing a different, non-archiving session s2. Real ESC drives
	// the cancel path so the App rebuilds Home through newHomeModel.
	a.activeView = ui.ViewChatPicker
	a.chatPicker = pickerSessionFixture("s2", &pb.Session{Id: "s2"})

	model, _ := a.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	got := model.(App)

	if got.activeView != ui.ViewHome {
		t.Fatalf("activeView = %v, want ViewHome", got.activeView)
	}
	if !got.home.IsArchiving("s1") {
		t.Fatal("s1 override missing after returning from s2")
	}
	if !got.home.ArchiveInFlight("s1") {
		t.Fatal("s1 archive no longer in flight after returning from s2")
	}
}

// TestBannerWithArchivingShowsArchivingLabel guards that renderBanner
// renders the archiving status string when bannerOpts.archiving is true.
func TestBannerWithArchivingShowsArchivingLabel(t *testing.T) {
	sess := &pb.Session{
		Id:            "s1",
		Title:         "My session",
		DisplayLabel:  "✓ passing",
		DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_SUCCESS,
	}
	sp := ui.NewStatusSpinner()
	got := renderBanner(ui.ViewChatPicker, bannerOpts{session: sess, spinner: sp, archiving: true})
	if !strings.Contains(got, "archiving") {
		t.Fatalf("banner with archiving=true should contain 'archiving', got %q", got)
	}
}

// TestAppSettingsA_OpensAccounts covers the Settings → [a] entry point: from
// ViewSettings an 'a' keypress must emit switchViewMsg{view: ViewAgentAccounts,
// returnView: ViewSettings}. Guards the account-management epic entry point.
func TestAppSettingsA_OpensAccounts(t *testing.T) {
	a := NewApp(nil, nil)
	a.activeView = ui.ViewSettings
	a.settings = settings.NewModel(a.ctx)

	_, cmd := a.Update(keyPress('a'))
	msg := runCmd(cmd)
	svm, ok := msg.(ui.SwitchViewMsg)
	if !ok {
		t.Fatalf("settings 'a' produced %T, want switchViewMsg", msg)
	}
	if svm.View != ui.ViewAgentAccounts || svm.ReturnView != ui.ViewSettings {
		t.Fatalf("settings 'a' routed to %v (return %v), want ViewAgentAccounts/ViewSettings", svm.View, svm.ReturnView)
	}
}

// TestAppAccountsCancelReturnsToSettings covers the reverse leg: an Accounts
// view that reports Cancelled() routes the app back to its returnView
// (ViewSettings), mirroring the cron-list return path.
func TestAppAccountsCancelReturnsToSettings(t *testing.T) {
	a := NewApp(nil, nil)
	a.activeView = ui.ViewAgentAccounts
	al := accounts.NewAgentAccountsListModel(nil, a.ctx)
	al, _ = updateModel(al, tea.KeyPressMsg{Code: tea.KeyEscape})
	al.SetReturnView(ui.ViewSettings)
	a.accountsList = al

	model, cmd := a.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	got := model.(App)
	// switchToReturn(ViewSettings) emits a switchViewMsg the app must apply.
	if msg := runCmd(cmd); msg != nil {
		model, _ = got.Update(msg)
		got = model.(App)
	}
	if got.activeView != ui.ViewSettings {
		t.Fatalf("accounts cancel routed to %v, want ViewSettings", got.activeView)
	}
}

// TestAppAccountEditCancelReturnsToAccounts covers the BOS-266 return leg: the
// edit form that reports Cancelled() with returnView=ViewAgentAccounts must route
// the app back to the accounts list (which re-fetches), NOT fall through to
// Home. Regression guard: switchToReturn previously special-cased only
// ViewSettings, silently dropping ViewAgentAccounts to switchToHome().
func TestAppAccountEditCancelReturnsToAccounts(t *testing.T) {
	a := NewApp(nil, nil)
	a.activeView = ui.ViewAgentAccountEdit
	ad := accounts.NewAgentAccountEditModel(nil, a.ctx, &pb.AgentAccount{Id: "acct-1", Label: "l", Status: "active"})
	ad, _ = updateModel(ad, tea.KeyPressMsg{Code: tea.KeyEscape})
	ad.SetReturnView(ui.ViewAgentAccounts)
	a.accountEdit = ad

	model, cmd := a.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	got := model.(App)
	// switchToReturn(ViewAgentAccounts) emits a switchViewMsg the app must apply.
	if msg := runCmd(cmd); msg != nil {
		model, _ = got.Update(msg)
		got = model.(App)
	}
	if got.activeView != ui.ViewAgentAccounts {
		t.Fatalf("account-edit cancel routed to %v, want ViewAgentAccounts", got.activeView)
	}
	// The rebuilt list must route its own esc back to Settings (its only entry).
	if got.accountsList.ReturnView() != ui.ViewSettings {
		t.Fatalf("rebuilt accounts list returnView = %v, want ViewSettings", got.accountsList.ReturnView())
	}
}

// TestAppAccountsA_OpensRegister covers the BOS-267 launch: from ViewAgentAccounts an
// 'a' keypress must emit switchViewMsg{view: ViewAgentAccountRegister, returnView:
// ViewAgentAccounts}, replacing the prior "coming soon" stub.
func TestAppAccountsA_OpensRegister(t *testing.T) {
	a := NewApp(nil, nil)
	a.activeView = ui.ViewAgentAccounts
	a.accountsList = accounts.NewAgentAccountsListModel(nil, a.ctx)

	_, cmd := a.Update(keyPress('a'))
	msg := runCmd(cmd)
	svm, ok := msg.(ui.SwitchViewMsg)
	if !ok {
		t.Fatalf("accounts 'a' produced %T, want switchViewMsg", msg)
	}
	if svm.View != ui.ViewAgentAccountRegister || svm.ReturnView != ui.ViewAgentAccounts {
		t.Fatalf("accounts 'a' routed to %v (return %v), want ViewAgentAccountRegister/ViewAgentAccounts", svm.View, svm.ReturnView)
	}
}

// TestAppAccountRegisterDoneReturnsToRefreshedAccounts covers the completion
// leg: a register flow reporting Done() must route the app back to a rebuilt
// accounts list (so the new account appears) whose own esc returns to Settings.
func TestAppAccountRegisterDoneReturnsToRefreshedAccounts(t *testing.T) {
	a := NewApp(nil, nil)
	a.activeView = ui.ViewAgentAccountRegister
	ar := accounts.NewAgentAccountRegisterModel(nil, a.ctx)
	ar, _ = updateModel(ar, accounts.FlowDoneMsg{})
	ar.SetReturnView(ui.ViewAgentAccounts)
	a.accountRegister = ar

	model, _ := a.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	got := model.(App)
	if got.activeView != ui.ViewAgentAccounts {
		t.Fatalf("register done routed to %v, want ViewAgentAccounts", got.activeView)
	}
	if got.accountsList.ReturnView() != ui.ViewSettings {
		t.Fatalf("rebuilt accounts list returnView = %v, want ViewSettings", got.accountsList.ReturnView())
	}
}

// TestAppAccountReauthDoneCarriesTheVerdictToTheList pins the BOS-1142 leg of
// that same completion: an ADD is self-evidencing, because a row that was not
// there appears. A REAUTHENTICATION is not — it replaces a secret on a row that
// was already on screen — and this pop is immediate, so the flow's closing
// verdict (the only place the daemon's post-save verification is reported to
// the operator) would otherwise be discarded with the flow model.
//
// The verdict is read from the prompter rather than the rendered progress tail
// on purpose: at flowDoneMsg time the reader Cmd may still be holding the
// closing line as an in-flight message, so the tail is short by exactly the line
// that matters.
func TestAppAccountReauthDoneCarriesTheVerdictToTheList(t *testing.T) {
	const verdict = "Reauthenticated account acct-codex-2; the new credential verified. The account is eligible again."

	a := NewApp(nil, nil)
	a.activeView = ui.ViewAgentAccountRegister
	ar := accounts.NewAgentAccountRegisterModel(nil, a.ctx)
	ar, _ = updateModel(ar, accounts.FlowDoneMsg{})
	ar.SetReturnView(ui.ViewAgentAccounts)
	ar.SetReauthAgentAccountID("acct-codex-2")
	prompter := accounts.NewTUIPrompter(a.ctx)
	ar.SetPrompter(prompter)
	prompter.Say("%s", verdict)
	a.accountRegister = ar

	model, _ := a.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	got := model.(App)
	if got.activeView != ui.ViewAgentAccounts {
		t.Fatalf("reauth done routed to %v, want ViewAgentAccounts", got.activeView)
	}
	if got.accountsList.Status() != verdict {
		t.Fatalf("accounts list status = %q, want the flow verdict %q", got.accountsList.Status(), verdict)
	}
	if got.accountsList.StatusError() {
		t.Fatal("a verified reauthentication must not be reported as an error")
	}
}

// TestAppAccountReauthVerdictSurvivesAFailedListReload pins the other half of
// the re-stamp: a reload that FAILED must consume nothing.
//
// The re-stamp exists because the toast is a 5-second transient whose clock
// starts at the pop, before ListAgentAccounts is even dialled. When that load errors,
// AgentAccountsListModel.View returns its full-screen error and never reaches the
// status line — so stamping the verdict there writes it to a surface that
// renders none of it, and clearing the marker at the same time discards the only
// report of the daemon's post-save verification for good. The operator is left
// looking at a dial error with no idea whether the credential they just replaced
// works. The verdict is held instead, and lands on the first load that can show
// it.
func TestAppAccountReauthVerdictSurvivesAFailedListReload(t *testing.T) {
	const verdict = "Reauthenticated account acct-codex-2; the new credential verified. The account is eligible again."

	a := NewApp(nil, nil)
	a.activeView = ui.ViewAgentAccounts
	a.accountsList = accounts.NewAgentAccountsListModel(nil, a.ctx)
	ar := accounts.NewAgentAccountRegisterModel(nil, a.ctx)
	ar.SetReauthAgentAccountID("acct-codex-2")
	prompter := accounts.NewTUIPrompter(a.ctx)
	ar.SetPrompter(prompter)
	prompter.Say("%s", verdict)
	a.accountRegister = ar

	model, _ := a.Update(accounts.AgentAccountsLoadedMsg{Err: errors.New("dial bossd: connection refused")})
	got := model.(App)
	if got.accountRegister.ReauthAgentAccountID() != "acct-codex-2" {
		t.Fatalf("failed reload consumed the reauth marker (now %q); the verdict can never be shown", got.accountRegister.ReauthAgentAccountID())
	}

	// The retry is what the operator actually reaches: [r], or leaving and
	// re-entering the list. The verdict must be waiting for it.
	model, _ = got.Update(accounts.AgentAccountsLoadedMsg{Accounts: []*pb.AgentAccount{{Id: "acct-codex-2", Provider: "codex"}}})
	got = model.(App)
	if got.accountsList.Status() != verdict {
		t.Fatalf("status after the successful reload = %q, want the held verdict %q", got.accountsList.Status(), verdict)
	}
	if got.accountsList.StatusError() {
		t.Fatal("a verified reauthentication must not be reported as an error")
	}
	// And exactly once: a marker left set would re-toast on every later load.
	if got.accountRegister.ReauthAgentAccountID() != "" {
		t.Fatalf("successful reload left the marker set (%q); the verdict will re-toast", got.accountRegister.ReauthAgentAccountID())
	}
}

// TestAppAccountAddDoneCarriesNoStatus is the other half: an add says nothing on
// the list, because the new row IS the report. Without this the reauth carry
// would quietly become a generic "flow said something" toast on every add.
func TestAppAccountAddDoneCarriesNoStatus(t *testing.T) {
	a := NewApp(nil, nil)
	a.activeView = ui.ViewAgentAccountRegister
	ar := accounts.NewAgentAccountRegisterModel(nil, a.ctx)
	ar, _ = updateModel(ar, accounts.FlowDoneMsg{})
	ar.SetReturnView(ui.ViewAgentAccounts)
	prompter := accounts.NewTUIPrompter(a.ctx)
	ar.SetPrompter(prompter)
	prompter.Say("Added account acct-new.")
	a.accountRegister = ar

	model, _ := a.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	got := model.(App)
	if got.accountsList.Status() != "" {
		t.Fatalf("add left status = %q, want empty", got.accountsList.Status())
	}
}

// TestAppAccountRegisterCancelReturnsToAccounts covers the cancel leg: a register
// flow reporting Cancelled() with returnView=ViewAgentAccounts routes back to the
// accounts list rather than falling through to Home.
func TestAppAccountRegisterCancelReturnsToAccounts(t *testing.T) {
	a := NewApp(nil, nil)
	a.activeView = ui.ViewAgentAccountRegister
	ar := accounts.NewAgentAccountRegisterModel(nil, a.ctx)
	ar, _ = updateModel(ar, tea.KeyPressMsg{Code: tea.KeyEscape})
	ar.SetReturnView(ui.ViewAgentAccounts)
	a.accountRegister = ar

	model, cmd := a.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	got := model.(App)
	if msg := runCmd(cmd); msg != nil {
		model, _ = got.Update(msg)
		got = model.(App)
	}
	if got.activeView != ui.ViewAgentAccounts {
		t.Fatalf("register cancel routed to %v, want ViewAgentAccounts", got.activeView)
	}
}

// TestAppSettingsG_OpensGeneralSettings covers the BOS-511 entry point end to
// end: from ViewSettings a 'g' keypress must emit switchViewMsg{view:
// ViewGeneralSettings} and the app must then route to the General list.
func TestAppSettingsG_OpensGeneralSettings(t *testing.T) {
	withTempConfigHome(t)
	a := NewApp(nil, nil)
	a.activeView = ui.ViewSettings
	a.settings = settings.NewModel(a.ctx)

	model, cmd := a.Update(keyPress('g'))
	got := model.(App)
	msg := runCmd(cmd)
	svm, ok := msg.(ui.SwitchViewMsg)
	if !ok {
		t.Fatalf("settings 'g' produced %T, want switchViewMsg", msg)
	}
	if svm.View != ui.ViewGeneralSettings {
		t.Fatalf("settings 'g' routed to %v, want ViewGeneralSettings", svm.View)
	}

	model, _ = got.Update(msg)
	got = model.(App)
	if got.activeView != ui.ViewGeneralSettings {
		t.Fatalf("after applying the switch, activeView = %v, want ViewGeneralSettings", got.activeView)
	}
	if !strings.Contains(got.generalSettings.View().Content, "Worktree base directory") {
		t.Fatal("general settings model was not constructed (no rows)")
	}
}

// TestAppSwitchToGeneralSettingsRoutes covers the BOS-511 entry leg: a
// switchViewMsg{view: ViewGeneralSettings} must make the general settings list
// the active view (and construct it, so its rows are populated).
func TestAppSwitchToGeneralSettingsRoutes(t *testing.T) {
	withTempConfigHome(t)
	a := NewApp(nil, nil)
	a.activeView = ui.ViewSettings

	model, _ := a.Update(ui.SwitchViewMsg{View: ui.ViewGeneralSettings})
	got := model.(App)
	if got.activeView != ui.ViewGeneralSettings {
		t.Fatalf("switchViewMsg routed to %v, want ViewGeneralSettings", got.activeView)
	}
	if !strings.Contains(got.generalSettings.View().Content, "Worktree base directory") {
		t.Fatal("general settings model was not constructed (no rows)")
	}
}

// TestAppGeneralSettingsEscReturnsToSettings covers the BOS-511 return leg: the
// General list's parent is statically the Settings hub, so esc routes back to
// ViewSettings rather than falling through to Home.
func TestAppGeneralSettingsEscReturnsToSettings(t *testing.T) {
	withTempConfigHome(t)
	a := NewApp(nil, nil)
	a.activeView = ui.ViewGeneralSettings
	a.generalSettings = settings.NewGeneralSettingsModel(nil, a.ctx, nil)

	model, cmd := a.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	got := model.(App)
	msg := runCmd(cmd)
	svm, ok := msg.(ui.SwitchViewMsg)
	if !ok {
		t.Fatalf("general settings esc produced %T, want switchViewMsg", msg)
	}
	if svm.View != ui.ViewSettings {
		t.Fatalf("general settings esc routed to %v, want ViewSettings", svm.View)
	}
	model, _ = got.Update(msg)
	got = model.(App)
	if got.activeView != ui.ViewSettings {
		t.Fatalf("after applying the switch, activeView = %v, want ViewSettings", got.activeView)
	}
}

func TestAppSettingsRestoresSelectedSectionAfterReturn(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  tea.KeyPressMsg
		view ui.View
	}{
		{name: "enter Trash", key: tea.KeyPressMsg{Code: tea.KeyEnter}, view: ui.ViewTrash},
		{name: "General hotkey", key: keyPress('g'), view: ui.ViewGeneralSettings},
		{name: "Repositories hotkey", key: keyPress('r'), view: ui.ViewRepoList},
		{name: "Trash hotkey", key: keyPress('t'), view: ui.ViewTrash},
		{name: "Cron jobs hotkey", key: keyPress('c'), view: ui.ViewCron},
		{name: "Accounts hotkey", key: keyPress('a'), view: ui.ViewAgentAccounts},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := NewApp(nil, nil)
			a.activeView = ui.ViewSettings
			a.settings = settings.NewModel(a.ctx)
			if tc.key.Code == tea.KeyEnter {
				for range 4 {
					updated, _ := a.settings.Update(tea.KeyPressMsg{Code: tea.KeyDown})
					a.settings = updated.(settings.Model)
				}
			}

			model, cmd := a.Update(tc.key)
			got := model.(App)
			msg := runCmd(cmd)
			switchMsg, ok := msg.(ui.SwitchViewMsg)
			if !ok || switchMsg.View != tc.view {
				t.Fatalf("settings key %q produced %#v, want %v switch", tc.key, msg, tc.view)
			}
			model, _ = got.Update(msg)
			got = model.(App)

			model, cmd = got.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
			got = model.(App)
			model, _ = got.Update(runCmd(cmd))
			got = model.(App)

			wantKey := tc.key.String()
			if tc.key.Code == tea.KeyEnter {
				wantKey = "t"
			}
			if got.activeView != ui.ViewSettings || got.settings.SelectedKey() != wantKey {
				t.Fatalf("returned to %v with %q selected, want Settings/%q", got.activeView, got.settings.SelectedKey(), wantKey)
			}
		})
	}
}

// TestAppHomeRebuildKeepsShownParks drives the App through a Home rebuild
// between a parked PR's repair alert and its dismissal. A dismiss reaches Home
// only as a later poll, so each scenario ends with the park the board showed
// before the alert hid it.
func TestAppHomeRebuildKeepsShownParks(t *testing.T) {
	session := func(att *pb.SessionAttention) []*pb.Session {
		n := int32(42)
		return []*pb.Session{{Id: "one", PrNumber: &n, Attention: att}}
	}
	parks := map[string][]*pb.Session{
		"fix park":      session(&pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_FIX, Sentence: "Review left must-fix findings open", ParkCodes: []string{"ledger-open"}}),
		"sign-off park": session(&pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_REVIEW, Sentence: "Watch the evidence video, then approve", ParkCodes: []string{"human-review:ui"}}),
	}
	repairFailed := session(&pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_FIX, Sentence: "Automatic repair failed", AlertKey: "repair:failed"})

	type step func(t *testing.T, a App, park []*pb.Session) App
	poll := func(alert bool, wantNotify bool) step {
		return func(t *testing.T, a App, park []*pb.Session) App {
			t.Helper()
			sessions := park
			if alert {
				sessions = repairFailed
			}
			notified := 0
			original := home.RunNotify
			defer func() { home.RunNotify = original }()
			home.RunNotify = func(notifyRequest) error { notified++; return nil }
			model, cmd := a.Update(sessionListMsg{HomeGeneration: a.home.Generation, Sessions: sessions})
			drainCmd(cmd)
			if got := notified > 0; got != wantNotify {
				t.Fatalf("poll of %s (alert=%v): notified=%v, want %v", sessions[0].GetAttention().GetKind(), alert, got, wantNotify)
			}
			return model.(App)
		}
	}
	visit := func(view ui.View) step {
		return func(t *testing.T, a App, _ []*pb.Session) App {
			t.Helper()
			generation := a.home.Generation
			model, _ := a.Update(ui.SwitchViewMsg{View: view, SessionID: "one"})
			model, _ = model.(App).Update(ui.SwitchViewMsg{View: ui.ViewHome})
			got := model.(App)
			if got.activeView != ui.ViewHome || got.home.Generation == generation {
				t.Fatalf("visiting %v did not rebuild Home (view %v, generation %d)", view, got.activeView, got.home.Generation)
			}
			return got
		}
	}

	for _, sc := range []struct {
		name  string
		steps []step
	}{
		{"dismissed in the chat picker", []step{poll(false, false), poll(true, true), visit(ui.ViewChatPicker), poll(false, false)}},
		{"dismissed on Home after a sub-view", []step{poll(false, false), poll(true, true), visit(ui.ViewRepoList), poll(true, false), poll(false, false)}},
	} {
		for name, park := range parks {
			t.Run(sc.name+"/"+name, func(t *testing.T) {
				a := NewApp(nil, nil)
				for _, s := range sc.steps {
					a = s(t, a, park)
				}
			})
		}
	}
}

type routingRestoreClient struct {
	trash.Client
	err error
}

func (c *routingRestoreClient) ResurrectSession(context.Context, string) (rpc.ResurrectSessionStream, error) {
	return routingRestoreStream{}, c.err
}

type routingRestoreStream struct{}

func (routingRestoreStream) Receive() bool { return true }
func (routingRestoreStream) Msg() *pb.ResurrectSessionResponse {
	return &pb.ResurrectSessionResponse{Event: &pb.ResurrectSessionResponse_SessionResurrected{SessionResurrected: &pb.SessionResurrected{Session: &pb.Session{Id: "restored-session"}}}}
}
func (routingRestoreStream) Err() error   { return nil }
func (routingRestoreStream) Close() error { return nil }
