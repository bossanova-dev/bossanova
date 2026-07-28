package views

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
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

func TestAppCtrlBBlockedWhileHomeBusy(t *testing.T) {
	ctrlB := tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl}
	tests := []struct {
		name       string
		upgrading  bool
		restarting bool
	}{
		{name: "upgrading", upgrading: true},
		{name: "restarting", restarting: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := NewApp(nil, nil)
			a.activeView = ViewHome
			a.home.upgrading = tt.upgrading
			a.home.restarting = tt.restarting

			model, cmd := a.Update(ctrlB)
			got := model.(App)
			if cmd != nil {
				t.Fatalf("ctrl+b while busy returned command %T, want nil", cmd)
			}
			if got.activeView != ViewHome {
				t.Fatalf("ctrl+b while busy: activeView = %v, want %v", got.activeView, ViewHome)
			}
		})
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

// TestAppChatPickerRefreshDetectedArchiveReturnsHome guards the poll-detected
// archive path: no TUI-initiated archive RPC, just a refreshed session that
// already carries ArchivedAt (e.g. archive-after-merge fired server-side, or
// an external/manual archive). The chatPickerRefreshMsg must flow through
// ChatPickerModel.Update (setting m.archived), and the app loop must then
// route back to ViewHome exactly as it does for a TUI-initiated archive.
func TestAppChatPickerRefreshDetectedArchiveReturnsHome(t *testing.T) {
	a := NewApp(nil, nil)
	a.activeView = ViewChatPicker
	a.chatPicker = ChatPickerModel{sessionID: "s1", session: &pb.Session{Id: "s1"}}

	model, _ := a.Update(chatPickerRefreshMsg{session: &pb.Session{Id: "s1", ArchivedAt: timestamppb.Now()}})
	got := model.(App)

	if got.activeView != ViewHome {
		t.Fatalf("activeView = %v after refresh-detected archive, want ViewHome", got.activeView)
	}
}

// TestEscWhileArchivingUnionsOverrides guards that returning from another
// in-flight archive does not replace the first optimistic archive state.
func TestEscWhileArchivingUnionsOverrides(t *testing.T) {
	a := NewApp(nil, nil)
	a.home.markArchiving("s1")
	a.activeView = ViewChatPicker
	// Archive in flight (no pre-set cancel) so the real ESC key path through
	// chatpicker.Update sets cancel=true, exercising the actual keyboard route
	// rather than a synthetic flag.
	a.chatPicker = ChatPickerModel{archiving: true, sessionID: "s2"}

	model, _ := a.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	got := model.(App)

	if got.activeView != ViewHome {
		t.Fatalf("activeView = %v after esc-while-archiving, want ViewHome", got.activeView)
	}
	for _, id := range []string{"s1", "s2"} {
		if !got.home.isArchiving(id) || !got.home.archiveInFlight(id) {
			t.Fatalf("expected %s to remain archiving and in flight after ESC carry", id)
		}
	}
}

// TestArchiveFailureClearsOverride guards that a failed archiveResultMsg
// delivered to the App clears only its matching home override, even when
// the user has already navigated away from the chatpicker (ViewHome).
func TestArchiveFailureClearsOverride(t *testing.T) {
	a := NewApp(nil, nil)
	a.home.markArchiving("s1")
	a.home.markArchiving("s2")

	model, _ := a.Update(archiveResultMsg{sessionID: "s1", err: errors.New("boom")})
	got := model.(App)

	if got.home.isArchiving("s1") || got.home.archiveInFlight("s1") {
		t.Fatal("expected failed s1 archive override cleared")
	}
	if !got.home.isArchiving("s2") || !got.home.archiveInFlight("s2") {
		t.Fatal("expected concurrent s2 archive state retained")
	}
}

// TestArchiveFailureRebuildsHomeRows ensures a failed archive immediately
// replaces the cached optimistic table status instead of waiting for a spinner
// tick or the next session poll.
func TestArchiveFailureRebuildsHomeRows(t *testing.T) {
	a := NewApp(nil, nil)
	a.home.sessions = []*pb.Session{{
		Id:            "s1",
		DisplayLabel:  "passing",
		DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_SUCCESS,
	}}
	a.home.markArchiving("s1")
	a.home.buildTableRows()
	if !strings.Contains(a.home.table.Rows()[0][5], "archiving") {
		t.Fatal("precondition: expected cached row to render archiving")
	}

	model, _ := a.Update(archiveResultMsg{sessionID: "s1", err: errors.New("boom")})
	got := model.(App)

	if strings.Contains(got.home.table.Rows()[0][5], "archiving") {
		t.Fatal("failed archive left cached row rendering archiving")
	}
	if !strings.Contains(got.home.table.Rows()[0][5], "passing") {
		t.Fatalf("failed archive row status = %q, want real status", got.home.table.Rows()[0][5])
	}
}

// TestArchiveSuccessKeepsOverrideAtAppLevel guards the design intent that a
// successful archiveResultMsg does NOT clear the override at the app level: the
// override persists until the session actually leaves the session list (cleared
// in the sessionListMsg path), so the row keeps showing "archiving" rather than
// flickering back to the real status before it disappears.
func TestArchiveSuccessKeepsOverrideAtAppLevel(t *testing.T) {
	a := NewApp(nil, nil)
	a.home.markArchiving("s1")
	a.home.markArchiving("s2")

	model, _ := a.Update(archiveResultMsg{sessionID: "s1", err: nil})
	got := model.(App)

	if !got.home.isArchiving("s1") {
		t.Fatal("expected successful s1 override retained at app level")
	}
	// The archive resolved, so it is no longer in flight even though the
	// override lingers for rendering until the row leaves the list.
	if got.home.archiveInFlight("s1") {
		t.Fatal("expected s1 in-flight state cleared on archive success")
	}
	if !got.home.isArchiving("s2") || !got.home.archiveInFlight("s2") {
		t.Fatal("expected concurrent s2 archive state retained")
	}
}

// TestReEnterArchivingSessionSeedsArchiving guards that re-entering the
// view-session screen while an archive is still in flight seeds the new
// ChatPickerModel with archiving=true so the banner shows the override.
func TestReEnterArchivingSessionSeedsArchiving(t *testing.T) {
	a := NewApp(nil, nil)
	a.home.markArchiving("s1")

	model, _ := a.Update(switchViewMsg{view: ViewChatPicker, sessionID: "s1"})
	got := model.(App)

	if got.activeView != ViewChatPicker {
		t.Fatalf("activeView = %v, want ViewChatPicker", got.activeView)
	}
	if !got.chatPicker.archiving {
		t.Fatalf("chatPicker.archiving = false, want true when re-entering archiving session")
	}
}

// TestReEnterAfterArchiveSuccessDoesNotSeedArchiving guards the stale-override
// case: after a successful archive the override lingers on the still-present
// row for rendering (archiveInFlight=false), but pressing Enter on that row
// must NOT seed archiving=true, which would leave the picker stuck on
// "Archiving session..." swallowing every key but Esc with no archiveResultMsg
// ever arriving for the freshly created model.
func TestReEnterAfterArchiveSuccessDoesNotSeedArchiving(t *testing.T) {
	a := NewApp(nil, nil)
	a.home.markArchiving("s1")
	a.home.resolveArchive("s1", nil)

	model, _ := a.Update(switchViewMsg{view: ViewChatPicker, sessionID: "s1"})
	got := model.(App)

	if got.chatPicker.archiving {
		t.Fatalf("chatPicker.archiving = true after archive success, want false (stale override must not re-enter as in-flight)")
	}
}

// TestReEnterNonArchivingSessionNoSeed guards that other sessions are NOT
// seeded with archiving=true when a different session is still in flight.
func TestReEnterNonArchivingSessionNoSeed(t *testing.T) {
	a := NewApp(nil, nil)
	a.home.markArchiving("s1")

	model, _ := a.Update(switchViewMsg{view: ViewChatPicker, sessionID: "s2"})
	got := model.(App)

	if got.chatPicker.archiving {
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
	a.home.markArchiving("s1")
	a.home.markArchiving("s2")
	a.home.markArchiving("s3")
	a.home.resolveArchive("s3", nil)

	rebuilt := a.newHomeModel()

	if !rebuilt.isArchiving("s1") || !rebuilt.isArchiving("s2") || !rebuilt.isArchiving("s3") || !rebuilt.archiveInFlight("s1") || !rebuilt.archiveInFlight("s2") || rebuilt.archiveInFlight("s3") {
		t.Fatal("rebuilt Home did not preserve per-session archive state")
	}
	rebuilt.resolveArchive("s1", errors.New("boom"))
	if !a.home.isArchiving("s1") || !a.home.archiveInFlight("s1") {
		t.Fatal("rebuilt Home aliases archive state from prior Home")
	}
}

// TestNewHomeModelPreservesQuestionState ensures returning to Home does not
// treat an already-notified question as a new notification edge.
func TestNewHomeModelPreservesQuestionState(t *testing.T) {
	a := NewApp(nil, nil)
	a.home.sessions = []*pb.Session{{
		Id:           "s1",
		DisplayLabel: displaystatus.QuestionLabel,
	}}

	rebuilt := a.newHomeModel()
	_, cmd := rebuilt.Update(sessionListMsg{sessions: []*pb.Session{{
		Id:           "s1",
		DisplayLabel: displaystatus.QuestionLabel,
	}}})
	if cmd != nil {
		t.Fatal("recreated Home returned a notification command for an unchanged question")
	}
}

// TestNewHomeModelPreservesResolvedArchiveOverride guards that a resolved
// archive's render override continues until the next poll removes its row.
func TestNewHomeModelPreservesResolvedArchiveOverride(t *testing.T) {
	a := NewApp(nil, nil)
	a.home.markArchiving("s1")
	a.home.resolveArchive("s1", nil)

	rebuilt := a.newHomeModel()

	if !rebuilt.isArchiving("s1") {
		t.Fatal("resolved override should survive a Home rebuild until the row leaves")
	}
	if rebuilt.archiveInFlight("s1") {
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
	a.home.markArchiving("s1")
	// The user is viewing a different, non-archiving session s2. Real ESC drives
	// the cancel path so the App rebuilds Home through newHomeModel.
	a.activeView = ViewChatPicker
	a.chatPicker = ChatPickerModel{sessionID: "s2"}

	model, _ := a.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	got := model.(App)

	if got.activeView != ViewHome {
		t.Fatalf("activeView = %v, want ViewHome", got.activeView)
	}
	if !got.home.isArchiving("s1") {
		t.Fatal("s1 override missing after returning from s2")
	}
	if !got.home.archiveInFlight("s1") {
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
	sp := newStatusSpinner()
	got := renderBanner(ViewChatPicker, bannerOpts{session: sess, spinner: sp, archiving: true})
	if !strings.Contains(got, "archiving") {
		t.Fatalf("banner with archiving=true should contain 'archiving', got %q", got)
	}
}

// TestAppSettingsA_OpensAccounts covers the Settings → [a] entry point: from
// ViewSettings an 'a' keypress must emit switchViewMsg{view: ViewAccounts,
// returnView: ViewSettings}. Guards the account-management epic entry point.
func TestAppSettingsA_OpensAccounts(t *testing.T) {
	a := NewApp(nil, nil)
	a.activeView = ViewSettings
	a.settings = NewSettingsModel(nil, a.ctx)

	_, cmd := a.Update(keyPress('a'))
	msg := runCmd(cmd)
	svm, ok := msg.(switchViewMsg)
	if !ok {
		t.Fatalf("settings 'a' produced %T, want switchViewMsg", msg)
	}
	if svm.view != ViewAccounts || svm.returnView != ViewSettings {
		t.Fatalf("settings 'a' routed to %v (return %v), want ViewAccounts/ViewSettings", svm.view, svm.returnView)
	}
}

// TestAppAccountsCancelReturnsToSettings covers the reverse leg: an Accounts
// view that reports Cancelled() routes the app back to its returnView
// (ViewSettings), mirroring the cron-list return path.
func TestAppAccountsCancelReturnsToSettings(t *testing.T) {
	a := NewApp(nil, nil)
	a.activeView = ViewAccounts
	al := NewAccountsListModel(nil, a.ctx)
	al.cancel = true
	al.returnView = ViewSettings
	a.accountsList = al

	model, cmd := a.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	got := model.(App)
	// switchToReturn(ViewSettings) emits a switchViewMsg the app must apply.
	if msg := runCmd(cmd); msg != nil {
		model, _ = got.Update(msg)
		got = model.(App)
	}
	if got.activeView != ViewSettings {
		t.Fatalf("accounts cancel routed to %v, want ViewSettings", got.activeView)
	}
}

// TestAppAccountEditCancelReturnsToAccounts covers the BOS-266 return leg: the
// edit form that reports Cancelled() with returnView=ViewAccounts must route
// the app back to the accounts list (which re-fetches), NOT fall through to
// Home. Regression guard: switchToReturn previously special-cased only
// ViewSettings, silently dropping ViewAccounts to switchToHome().
func TestAppAccountEditCancelReturnsToAccounts(t *testing.T) {
	a := NewApp(nil, nil)
	a.activeView = ViewAccountEdit
	ad := NewAccountEditModel(nil, a.ctx, &pb.Account{Id: "acct-1", Label: "l", Status: "active"})
	ad.cancel = true
	ad.returnView = ViewAccounts
	a.accountEdit = ad

	model, cmd := a.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	got := model.(App)
	// switchToReturn(ViewAccounts) emits a switchViewMsg the app must apply.
	if msg := runCmd(cmd); msg != nil {
		model, _ = got.Update(msg)
		got = model.(App)
	}
	if got.activeView != ViewAccounts {
		t.Fatalf("account-edit cancel routed to %v, want ViewAccounts", got.activeView)
	}
	// The rebuilt list must route its own esc back to Settings (its only entry).
	if got.accountsList.returnView != ViewSettings {
		t.Fatalf("rebuilt accounts list returnView = %v, want ViewSettings", got.accountsList.returnView)
	}
}

// TestAppAccountsA_OpensRegister covers the BOS-267 launch: from ViewAccounts an
// 'a' keypress must emit switchViewMsg{view: ViewAccountRegister, returnView:
// ViewAccounts}, replacing the prior "coming soon" stub.
func TestAppAccountsA_OpensRegister(t *testing.T) {
	a := NewApp(nil, nil)
	a.activeView = ViewAccounts
	a.accountsList = NewAccountsListModel(nil, a.ctx)

	_, cmd := a.Update(keyPress('a'))
	msg := runCmd(cmd)
	svm, ok := msg.(switchViewMsg)
	if !ok {
		t.Fatalf("accounts 'a' produced %T, want switchViewMsg", msg)
	}
	if svm.view != ViewAccountRegister || svm.returnView != ViewAccounts {
		t.Fatalf("accounts 'a' routed to %v (return %v), want ViewAccountRegister/ViewAccounts", svm.view, svm.returnView)
	}
}

// TestAppAccountRegisterDoneReturnsToRefreshedAccounts covers the completion
// leg: a register flow reporting Done() must route the app back to a rebuilt
// accounts list (so the new account appears) whose own esc returns to Settings.
func TestAppAccountRegisterDoneReturnsToRefreshedAccounts(t *testing.T) {
	a := NewApp(nil, nil)
	a.activeView = ViewAccountRegister
	ar := NewAccountRegisterModel(nil, a.ctx)
	ar.done = true
	ar.returnView = ViewAccounts
	a.accountRegister = ar

	model, _ := a.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	got := model.(App)
	if got.activeView != ViewAccounts {
		t.Fatalf("register done routed to %v, want ViewAccounts", got.activeView)
	}
	if got.accountsList.returnView != ViewSettings {
		t.Fatalf("rebuilt accounts list returnView = %v, want ViewSettings", got.accountsList.returnView)
	}
}

// TestAppAccountRegisterCancelReturnsToAccounts covers the cancel leg: a register
// flow reporting Cancelled() with returnView=ViewAccounts routes back to the
// accounts list rather than falling through to Home.
func TestAppAccountRegisterCancelReturnsToAccounts(t *testing.T) {
	a := NewApp(nil, nil)
	a.activeView = ViewAccountRegister
	ar := NewAccountRegisterModel(nil, a.ctx)
	ar.cancelled = true
	ar.returnView = ViewAccounts
	a.accountRegister = ar

	model, cmd := a.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	got := model.(App)
	if msg := runCmd(cmd); msg != nil {
		model, _ = got.Update(msg)
		got = model.(App)
	}
	if got.activeView != ViewAccounts {
		t.Fatalf("register cancel routed to %v, want ViewAccounts", got.activeView)
	}
}

// TestAppSettingsG_OpensGeneralSettings covers the BOS-511 entry point end to
// end: from ViewSettings a 'g' keypress must emit switchViewMsg{view:
// ViewGeneralSettings} and the app must then route to the General list.
func TestAppSettingsG_OpensGeneralSettings(t *testing.T) {
	withTempConfigHome(t)
	a := NewApp(nil, nil)
	a.activeView = ViewSettings
	a.settings = NewSettingsModel(nil, a.ctx)

	model, cmd := a.Update(keyPress('g'))
	got := model.(App)
	msg := runCmd(cmd)
	svm, ok := msg.(switchViewMsg)
	if !ok {
		t.Fatalf("settings 'g' produced %T, want switchViewMsg", msg)
	}
	if svm.view != ViewGeneralSettings {
		t.Fatalf("settings 'g' routed to %v, want ViewGeneralSettings", svm.view)
	}

	model, _ = got.Update(msg)
	got = model.(App)
	if got.activeView != ViewGeneralSettings {
		t.Fatalf("after applying the switch, activeView = %v, want ViewGeneralSettings", got.activeView)
	}
	if len(got.generalSettings.rows) == 0 {
		t.Fatal("general settings model was not constructed (no rows)")
	}
}

// TestAppSwitchToGeneralSettingsRoutes covers the BOS-511 entry leg: a
// switchViewMsg{view: ViewGeneralSettings} must make the general settings list
// the active view (and construct it, so its rows are populated).
func TestAppSwitchToGeneralSettingsRoutes(t *testing.T) {
	withTempConfigHome(t)
	a := NewApp(nil, nil)
	a.activeView = ViewSettings

	model, _ := a.Update(switchViewMsg{view: ViewGeneralSettings})
	got := model.(App)
	if got.activeView != ViewGeneralSettings {
		t.Fatalf("switchViewMsg routed to %v, want ViewGeneralSettings", got.activeView)
	}
	if len(got.generalSettings.rows) == 0 {
		t.Fatal("general settings model was not constructed (no rows)")
	}
}

// TestAppGeneralSettingsEscReturnsToSettings covers the BOS-511 return leg: the
// General list's parent is statically the Settings hub, so esc routes back to
// ViewSettings rather than falling through to Home.
func TestAppGeneralSettingsEscReturnsToSettings(t *testing.T) {
	withTempConfigHome(t)
	a := NewApp(nil, nil)
	a.activeView = ViewGeneralSettings
	a.generalSettings = NewGeneralSettingsModel(nil, a.ctx)

	model, cmd := a.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	got := model.(App)
	msg := runCmd(cmd)
	svm, ok := msg.(switchViewMsg)
	if !ok {
		t.Fatalf("general settings esc produced %T, want switchViewMsg", msg)
	}
	if svm.view != ViewSettings {
		t.Fatalf("general settings esc routed to %v, want ViewSettings", svm.view)
	}
	model, _ = got.Update(msg)
	got = model.(App)
	if got.activeView != ViewSettings {
		t.Fatalf("after applying the switch, activeView = %v, want ViewSettings", got.activeView)
	}
}

func TestAppSettingsRestoresSelectedSectionAfterReturn(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  tea.KeyPressMsg
		view View
	}{
		{name: "enter Trash", key: tea.KeyPressMsg{Code: tea.KeyEnter}, view: ViewTrash},
		{name: "General hotkey", key: keyPress('g'), view: ViewGeneralSettings},
		{name: "Repositories hotkey", key: keyPress('r'), view: ViewRepoList},
		{name: "Trash hotkey", key: keyPress('t'), view: ViewTrash},
		{name: "Cron jobs hotkey", key: keyPress('c'), view: ViewCron},
		{name: "Accounts hotkey", key: keyPress('a'), view: ViewAccounts},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := NewApp(nil, nil)
			a.activeView = ViewSettings
			a.settings = NewSettingsModel(nil, a.ctx)
			if tc.key.Code == tea.KeyEnter {
				a.settings.cursor = 4
			}

			model, cmd := a.Update(tc.key)
			got := model.(App)
			msg := runCmd(cmd)
			switchMsg, ok := msg.(switchViewMsg)
			if !ok || switchMsg.view != tc.view {
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
			if got.activeView != ViewSettings || got.settings.selectedKey() != wantKey {
				t.Fatalf("returned to %v with %q selected, want Settings/%q", got.activeView, got.settings.selectedKey(), wantKey)
			}
		})
	}
}
