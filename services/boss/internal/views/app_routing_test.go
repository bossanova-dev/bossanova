package views

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// appWithRepoList builds an App whose (pre-replacement) repoList holds the given
// repo IDs with the table cursor parked on cursorIdx. The repoAdd-completion and
// repoAdd-cancel handlers read this list to compute which repo to re-highlight.
func appWithRepoList(t *testing.T, ids []string, cursorIdx int) App {
	t.Helper()
	a := NewApp(nil, nil)
	rl := NewRepoListModel(nil, a.ctx)
	repos := make([]*pb.Repo, len(ids))
	for i, id := range ids {
		repos[i] = &pb.Repo{Id: id}
	}
	rl.repos = repos
	rl.buildTable()
	rl.table.SetCursor(cursorIdx)
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
		wantView View
	}{
		{name: "no error zero repos goes home", err: nil, repoLen: 0, wantView: ViewHome},
		{name: "no error one repo goes home", err: nil, repoLen: 1, wantView: ViewHome},
		{name: "no error two repos shows list", err: nil, repoLen: 2, wantView: ViewRepoList},
		{name: "error zero repos shows list", err: context.Canceled, repoLen: 0, wantView: ViewRepoList},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := NewApp(nil, nil)
			a.activeView = ViewRepoAdd
			a.repoAddCompleting = true

			repos := make([]*pb.Repo, tt.repoLen)
			for i := range repos {
				repos[i] = &pb.Repo{Id: "r"}
			}

			model, _ := a.Update(repoAddCompletedMsg{repos: repos, err: tt.err})
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
			a.activeView = ViewRepoAdd

			// Two repos in the message + empty highlightID: falls through to the
			// cursor-based highlight path (non-Home, non-settings branch).
			msg := repoAddCompletedMsg{
				repos:       []*pb.Repo{{Id: "x"}, {Id: "y"}},
				err:         nil,
				highlightID: tt.explicit,
			}
			model, _ := a.Update(msg)
			got := model.(App)

			if got.activeView != ViewRepoList {
				t.Fatalf("activeView = %v, want %v", got.activeView, ViewRepoList)
			}
			if got.repoList.highlightRepoID != tt.wantHighlight {
				t.Fatalf("highlightRepoID = %q, want %q", got.repoList.highlightRepoID, tt.wantHighlight)
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
			a.activeView = ViewRepoAdd
			ra := NewRepoAddModel(nil, a.ctx)
			ra.cancel = true
			a.repoAdd = ra

			model, _ := a.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
			got := model.(App)

			if got.activeView != ViewRepoList {
				t.Fatalf("activeView = %v, want %v", got.activeView, ViewRepoList)
			}
			if got.repoList.highlightRepoID != tt.wantHighlight {
				t.Fatalf("highlightRepoID = %q, want %q", got.repoList.highlightRepoID, tt.wantHighlight)
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
			cp := NewChatPickerModel(nil, a.ctx, "sid", "")
			chats := make([]*pb.ClaudeChat, len(tt.agentIDs))
			for i, id := range tt.agentIDs {
				chats[i] = &pb.ClaudeChat{AgentSessionId: id}
			}
			cp.chats = chats
			cp.buildTableRows()
			cp.table.SetCursor(tt.cursor)
			a.chatPicker = cp

			a.activeView = ViewSessionSettings
			ss := NewSessionSettingsModel(nil, a.ctx, "sid")
			ss.cancel = true
			a.sessionSettings = ss

			model, _ := a.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
			got := model.(App)

			if got.activeView != ViewChatPicker {
				t.Fatalf("activeView = %v, want %v", got.activeView, ViewChatPicker)
			}
			if got.chatPicker.highlightID != tt.wantHighlight {
				t.Fatalf("chatPicker.highlightID = %q, want %q", got.chatPicker.highlightID, tt.wantHighlight)
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
		a.activeView = ViewTrash
		tr := NewTrashModel(nil, a.ctx)
		tr.restoredID = "restored-session"
		a.trash = tr

		model, _ := a.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
		got := model.(App)

		if got.activeView != ViewChatPicker {
			t.Fatalf("activeView = %v, want %v", got.activeView, ViewChatPicker)
		}
	})

	t.Run("no restored session stays in trash", func(t *testing.T) {
		a := NewApp(nil, nil)
		a.activeView = ViewTrash
		a.trash = NewTrashModel(nil, a.ctx)

		model, _ := a.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
		got := model.(App)

		if got.activeView != ViewTrash {
			t.Fatalf("activeView = %v, want %v (no restore, no cancel)", got.activeView, ViewTrash)
		}
	})
}

// TestAppCtrlBOpensBugReport covers the ctrl+b guard at app.go:251
// (`a.activeView == ViewBugReport`). From any non-bug-report view ctrl+b opens
// the bug report modal; while already in it, ctrl+b is a no-op that stays put.
//
// Kills the negation (`==` → `!=`): from Home, a `!=` mutant would take the
// break and never open the modal, leaving the app on Home.
func TestAppCtrlBOpensBugReport(t *testing.T) {
	ctrlB := tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl}
	if ctrlB.String() != "ctrl+b" {
		t.Fatalf("ctrlB.String() = %q, want %q", ctrlB.String(), "ctrl+b")
	}

	a := NewApp(nil, nil)
	a.activeView = ViewHome
	a.width = 80

	model, _ := a.Update(ctrlB)
	got := model.(App)
	if got.activeView != ViewBugReport {
		t.Fatalf("ctrl+b from Home: activeView = %v, want %v", got.activeView, ViewBugReport)
	}

	// Already in the bug report view: ctrl+b must not re-open / leave the view.
	model, _ = got.Update(ctrlB)
	got = model.(App)
	if got.activeView != ViewBugReport {
		t.Fatalf("ctrl+b within bug report: activeView = %v, want %v", got.activeView, ViewBugReport)
	}
}

// TestAppCurrentSession covers app.go:589 (`a.activeView == ViewChatPicker`).
// Only the chat picker exposes a current session; every other view reports nil.
// Kills the negation (`==` → `!=` would return nil for the chat picker).
func TestAppCurrentSession(t *testing.T) {
	a := NewApp(nil, nil)
	want := &pb.Session{Id: "session-1"}
	a.chatPicker.session = want

	a.activeView = ViewChatPicker
	if got := a.currentSession(); got != want {
		t.Fatalf("currentSession() in chat picker = %v, want %v", got, want)
	}

	a.activeView = ViewHome
	if got := a.currentSession(); got != nil {
		t.Fatalf("currentSession() in home = %v, want nil", got)
	}
}

// TestAppChatPickerStaysOnMerge guards the critical regression: after a
// successful merge the app must NOT bounce back to the session list. The user
// stays on ViewChatPicker so they can archive in place.
func TestAppChatPickerStaysOnMerge(t *testing.T) {
	a := NewApp(nil, nil)
	a.activeView = ViewChatPicker
	a.chatPicker = ChatPickerModel{merged: true, sessionID: "s1"}

	model, _ := a.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	got := model.(App)

	if got.activeView != ViewChatPicker {
		t.Fatalf("activeView = %v after merge, want ViewChatPicker (must stay on detail view)", got.activeView)
	}
}

// TestAppChatPickerCancelAfterMergeReturnsHome guards that cancel still
// returns to the session list even when the session was already merged,
// and that mergedOptimisticID is set on the returned home model.
func TestAppChatPickerCancelAfterMergeReturnsHome(t *testing.T) {
	a := NewApp(nil, nil)
	a.activeView = ViewChatPicker
	a.chatPicker = ChatPickerModel{cancel: true, merged: true, sessionID: "s1"}

	model, _ := a.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	got := model.(App)

	if got.activeView != ViewHome {
		t.Fatalf("activeView = %v after cancel+merge, want ViewHome", got.activeView)
	}
	if got.home.mergedOptimisticID != "s1" {
		t.Fatalf("mergedOptimisticID = %q, want %q (optimistic merge must carry through)", got.home.mergedOptimisticID, "s1")
	}
}

// TestAppChatPickerArchiveAfterMergeReturnsHome guards that archive still
// returns to the session list even when the session was already merged,
// and that mergedOptimisticID is set on the returned home model.
func TestAppChatPickerArchiveAfterMergeReturnsHome(t *testing.T) {
	a := NewApp(nil, nil)
	a.activeView = ViewChatPicker
	a.chatPicker = ChatPickerModel{archived: true, merged: true, sessionID: "s1"}

	model, _ := a.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	got := model.(App)

	if got.activeView != ViewHome {
		t.Fatalf("activeView = %v after archive+merge, want ViewHome", got.activeView)
	}
	if got.home.mergedOptimisticID != "s1" {
		t.Fatalf("mergedOptimisticID = %q, want %q (optimistic merge must carry through)", got.home.mergedOptimisticID, "s1")
	}
}
