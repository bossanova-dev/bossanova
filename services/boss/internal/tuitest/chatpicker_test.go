package tuitest_test

import (
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/tuitest"
	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func testMergeableSessions() []*pb.Session {
	sessions := testSessions()
	sessions[0].DisplayStatus = pb.DisplayStatus_DISPLAY_STATUS_PASSING
	return sessions
}

func TestTUI_ChatPickerView_ShowsChats(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
		tuitest.WithChats(testChats()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	// Press enter to open the first session's chat picker.
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	// Should see "Loading chats" or the chat titles.
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Initial implementation") ||
			strings.Contains(screen, "Loading chats")
	}); err != nil {
		t.Fatalf("expected chat picker content; screen:\n%s", h.Driver.Screen())
	}

	// If we see loading, wait for actual chats.
	if h.Driver.ScreenContains("Loading chats") {
		if err := h.Driver.WaitForText(waitTimeout, "Initial implementation"); err != nil {
			t.Fatalf("expected chat title after loading; screen:\n%s", h.Driver.Screen())
		}
	}

	screen := h.Driver.Screen()
	if !strings.Contains(screen, "Follow-up review") {
		t.Fatalf("expected second chat title on screen:\n%s", screen)
	}
}

func TestTUI_ChatPickerView_DeleteConfirm(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
		tuitest.WithChats(testChats()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitForText(waitTimeout, "Initial implementation"); err != nil {
		t.Fatal(err)
	}

	// Press 'd' to delete the first chat.
	if err := h.Driver.SendKey('d'); err != nil {
		t.Fatal(err)
	}

	// Wait for the actual confirmation dialog (not the action bar "[d]elete").
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "[y/enter] confirm") &&
			strings.Contains(screen, "Delete")
	}); err != nil {
		t.Fatalf("expected delete confirmation dialog; screen:\n%s", h.Driver.Screen())
	}
	time.Sleep(200 * time.Millisecond)

	// Confirm with 'y'.
	if err := h.Driver.SendKey('y'); err != nil {
		t.Fatal(err)
	}

	// First wait for the confirmation dialog to close.
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return !strings.Contains(screen, "[y/enter] confirm")
	}); err != nil {
		t.Fatalf("confirmation dialog did not close; screen:\n%s", h.Driver.Screen())
	}

	// Wait for the deleted chat to disappear from the UI.
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return !strings.Contains(screen, "Initial implementation")
	}); err != nil {
		t.Fatalf("expected 'Initial implementation' to be removed; screen:\n%s", h.Driver.Screen())
	}

	// Verify the remaining chat is still present.
	screen := h.Driver.Screen()
	if !strings.Contains(screen, "Follow-up review") {
		t.Fatalf("expected 'Follow-up review' to remain after deletion; screen:\n%s", screen)
	}
}

func TestTUI_ChatPickerView_DeleteCancel(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
		tuitest.WithChats(testChats()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitForText(waitTimeout, "Initial implementation"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('d'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "[y/enter] confirm"); err != nil {
		t.Fatal(err)
	}

	// Cancel with 'n'.
	if err := h.Driver.SendKey('n'); err != nil {
		t.Fatal(err)
	}

	time.Sleep(500 * time.Millisecond)
	if !h.Driver.ScreenContains("Initial implementation") {
		t.Fatalf("chat disappeared after cancel; screen:\n%s", h.Driver.Screen())
	}
}

func TestTUI_ChatPicker_MergeConfirm_CancelKeepsPicker(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testMergeableSessions()...),
		tuitest.WithChats(testChats()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitForText(waitTimeout, "[m]erge"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('m'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "[y/enter] confirm"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('n'); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitForText(waitTimeout, "[m]erge"); err != nil {
		t.Fatalf("expected merge action after cancel; screen:\n%s", h.Driver.Screen())
	}

	time.Sleep(500 * time.Millisecond)
	if h.Driver.ScreenContains("Couldn't merge") {
		t.Fatalf("cancel triggered merge; screen:\n%s", h.Driver.Screen())
	}
}

// TestTUI_ChatPicker_MergeParkedPR merges a sign-off park from the details
// view: one approving merge pinned to the head the view showed.
func TestTUI_ChatPicker_MergeParkedPR(t *testing.T) {
	const head = "0123456789abcdef0123456789abcdef01234567"
	sessions := testSessions()
	sessions[0].DisplayStatus = pb.DisplayStatus_DISPLAY_STATUS_NEEDS_HUMAN
	sessions[0].AttentionStatus = &pb.AttentionStatus{Reason: pb.AttentionReason_ATTENTION_REASON_AWAITING_HUMAN_INPUT, Summary: "needs human: human-review:ui"}
	sessions[0].PrDisplayHeadSha = head
	sessions[0].Attention = attention.Compute(attention.Input{ParkCodes: []string{"human-review:ui"}})
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(sessions...),
		tuitest.WithChats(testChats()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	// Home offers [m]erge on this row too, so wait for a details-only key
	// before reading the details footer.
	if err := h.Driver.WaitForText(waitTimeout, "[n]ew chat"); err != nil {
		t.Fatalf("details view did not open; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.WaitForText(waitTimeout, "[m]erge"); err != nil {
		t.Fatalf("parked PR does not offer [m]erge; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendKey('m'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Merge PR #597?"); err != nil {
		t.Fatalf("parked merge confirm not shown; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendKey('y'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Merged PR #597"); err != nil {
		t.Fatalf("merge outcome not shown; screen:\n%s", h.Driver.Screen())
	}
	calls := h.Daemon.OperatorMergeCalls()
	if len(calls) != 1 || calls[0].GetId() != "sess-aaa-111" || calls[0].GetExpectedHeadSha() != head || !calls[0].GetShouldApprove() {
		t.Fatalf("OperatorMergeCalls = %v, want one approving merge of sess-aaa-111 at %s", calls, head)
	}
}

func TestTUI_ChatPicker_DeleteConfirm_CancelKeepsPicker(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
		tuitest.WithChats(testChats()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitForText(waitTimeout, "[d]elete"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('d'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "[y/enter] confirm"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('n'); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitForText(waitTimeout, "[d]elete"); err != nil {
		t.Fatalf("expected delete action after cancel; screen:\n%s", h.Driver.Screen())
	}
}

func TestTUI_ChatPicker_Archive_ConfirmReturnsHome(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
		tuitest.WithChats(testChats()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Initial implementation") &&
			strings.Contains(screen, "[a]rchive")
	}); err != nil {
		t.Fatalf("expected chat picker archive action; screen:\n%s", h.Driver.Screen())
	}

	if err := h.Driver.SendKey('a'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Archive this session?"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('y'); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitForText(waitTimeout, "[n]ew"); err != nil {
		t.Fatalf("expected home view after archive; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.WaitForNoText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, s := range h.Daemon.Sessions() {
		if s.Id == "sess-aaa-111" {
			found = true
			if s.ArchivedAt == nil {
				t.Fatal("expected session to be archived in daemon")
			}
		}
	}
	if !found {
		t.Fatal("session sess-aaa-111 not found in daemon state")
	}
}

func TestTUI_ChatPicker_Archive_CancelKeepsPicker(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
		tuitest.WithChats(testChats()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Initial implementation") &&
			strings.Contains(screen, "[a]rchive")
	}); err != nil {
		t.Fatalf("expected chat picker archive action; screen:\n%s", h.Driver.Screen())
	}

	if err := h.Driver.SendKey('a'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Archive this session?"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitForText(waitTimeout, "[a]rchive"); err != nil {
		t.Fatalf("expected archive action after cancel; screen:\n%s", h.Driver.Screen())
	}
}

func TestTUI_ChatPicker_Archive_InFlightBlocksNavigation(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
		tuitest.WithChats(testChats()...),
		tuitest.WithArchiveDelay(750*time.Millisecond),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Initial implementation") &&
			strings.Contains(screen, "[a]rchive")
	}); err != nil {
		t.Fatalf("expected chat picker archive action; screen:\n%s", h.Driver.Screen())
	}

	if err := h.Driver.SendKey('a'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Archive this session?"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendKey('y'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Archiving session…") &&
			!strings.Contains(screen, "[a]rchive") &&
			!strings.Contains(screen, "[m]erge")
	}); err != nil {
		t.Fatalf("expected archive in-flight state without actions; screen:\n%s", h.Driver.Screen())
	}

	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	time.Sleep(200 * time.Millisecond)
	if !h.Driver.ScreenContains("Archiving session…") {
		t.Fatalf("archive in-flight enter navigated away; screen:\n%s", h.Driver.Screen())
	}

	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "[n]ew") &&
			!strings.Contains(screen, "Add dark mode") &&
			!strings.Contains(screen, "Initial implementation")
	}); err != nil {
		t.Fatalf("expected home view after delayed archive; screen:\n%s", h.Driver.Screen())
	}
}

func TestTUI_ChatPicker_Archive_LoadingKeysDoNotConfirm(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
		tuitest.WithChats(testChats()...),
		tuitest.WithChatListDelay(750*time.Millisecond),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitForText(waitTimeout, "Loading chats for Add dark mode"); err != nil {
		t.Fatalf("expected chat picker loading screen; screen:\n%s", h.Driver.Screen())
	}

	if err := h.Driver.SendKey('a'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendKey('y'); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Initial implementation") &&
			strings.Contains(screen, "[a]rchive")
	}); err != nil {
		t.Fatalf("expected chat picker to load without archiving; screen:\n%s", h.Driver.Screen())
	}

	for _, s := range h.Daemon.Sessions() {
		if s.Id == "sess-aaa-111" && s.ArchivedAt != nil {
			t.Fatal("loading-state archive keys archived the session")
		}
	}
}

func TestTUI_ChatPicker_Archive_ErrorStateAllowsArchive(t *testing.T) {
	// A failed chat listing still loads the session, so archiving must remain
	// reachable — the chat picker is now the only TUI archive path. Without the
	// error-state archive flow a session with a corrupt chat list could never be
	// archived from the TUI.
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
		tuitest.WithChatListError("chat list failed"),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Error:") &&
			strings.Contains(screen, "[a]rchive")
	}); err != nil {
		t.Fatalf("expected error screen offering archive; screen:\n%s", h.Driver.Screen())
	}

	if err := h.Driver.SendKey('a'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Archive this session?"); err != nil {
		t.Fatalf("expected archive confirmation in error state; screen:\n%s", h.Driver.Screen())
	}

	if err := h.Driver.SendKey('y'); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "[n]ew") &&
			!strings.Contains(screen, "Add dark mode")
	}); err != nil {
		t.Fatalf("expected home view after error-state archive; screen:\n%s", h.Driver.Screen())
	}

	var found bool
	for _, s := range h.Daemon.Sessions() {
		if s.Id == "sess-aaa-111" {
			found = true
			if s.ArchivedAt == nil {
				t.Fatal("expected session to be archived from error state")
			}
		}
	}
	if !found {
		t.Fatal("session sess-aaa-111 not found in daemon state")
	}
}

func TestTUI_ChatPicker_Archive_ErrorStateSurfacesFailure(t *testing.T) {
	// When archiving from the error state fails, the error screen must show the
	// failure status rather than silently dropping the user back to the bare
	// error view.
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
		tuitest.WithChatListError("chat list failed"),
		tuitest.WithArchiveError("archive boom"),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitForText(waitTimeout, "[a]rchive"); err != nil {
		t.Fatalf("expected error screen offering archive; screen:\n%s", h.Driver.Screen())
	}

	if err := h.Driver.SendKey('a'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Archive this session?"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('y'); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Couldn't archive session") &&
			strings.Contains(screen, "Error:") &&
			strings.Contains(screen, "[a]rchive")
	}); err != nil {
		t.Fatalf("expected archive failure surfaced on error screen; screen:\n%s", h.Driver.Screen())
	}

	for _, s := range h.Daemon.Sessions() {
		if s.Id == "sess-aaa-111" && s.ArchivedAt != nil {
			t.Fatal("failed archive should not mark the session archived")
		}
	}
}

func TestTUI_ChatPicker_Archive_ErrorStateCancelKeepsError(t *testing.T) {
	// Cancelling the error-state archive confirmation returns to the error
	// screen without archiving.
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
		tuitest.WithChatListError("chat list failed"),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitForText(waitTimeout, "[a]rchive"); err != nil {
		t.Fatalf("expected error screen offering archive; screen:\n%s", h.Driver.Screen())
	}

	if err := h.Driver.SendKey('a'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Archive this session?"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('n'); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Error:") &&
			strings.Contains(screen, "[a]rchive") &&
			!strings.Contains(screen, "Archive this session?")
	}); err != nil {
		t.Fatalf("expected error screen after cancel; screen:\n%s", h.Driver.Screen())
	}

	for _, s := range h.Daemon.Sessions() {
		if s.Id == "sess-aaa-111" && s.ArchivedAt != nil {
			t.Fatal("cancelled error-state archive archived the session")
		}
	}
}

func TestTUI_ChatPicker_ArchivingStatusPersistsAcrossNavigation(t *testing.T) {
	// Regression test for BOS-125: ESC-while-archiving used to drop the
	// archivingOptimisticID override, so the home row reverted to the session's
	// PR status and re-entering the session opened the chat picker without the
	// in-flight archiving state. WithArchiveDelay keeps the RPC in-flight long
	// enough for the full ESC→home→re-enter round-trip to complete.
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
		tuitest.WithChats(testChats()...),
		tuitest.WithArchiveDelay(3*time.Second),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	// Enter the session chat picker.
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	// Wait for the chat picker with archive action available.
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Initial implementation") &&
			strings.Contains(screen, "[a]rchive")
	}); err != nil {
		t.Fatalf("expected chat picker with archive action; screen:\n%s", h.Driver.Screen())
	}

	// Initiate archive and confirm.
	if err := h.Driver.SendKey('a'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Archive this session?"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendKey('y'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	// Wait for the in-flight archiving state.
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Archiving session…")
	}); err != nil {
		t.Fatalf("expected in-flight archive state; screen:\n%s", h.Driver.Screen())
	}

	// Press ESC to navigate back to the home list while archive is still in-flight.
	// Before the BOS-125 fix, ESC dropped archivingOptimisticID, causing the home
	// row to revert to the session's real PR status.
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}

	// The home list must show "archiving" for the session row. renderArchivingStatus
	// emits "archiving" (with a spinner prefix) while archivingOptimisticID is set.
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "[n]ew") &&
			strings.Contains(screen, "Add dark mode") &&
			strings.Contains(screen, "archiving")
	}); err != nil {
		t.Fatalf("expected home view with archiving status after ESC; screen:\n%s", h.Driver.Screen())
	}

	// Re-enter the session. The archivingOptimisticID must propagate so the
	// chat picker re-opens in the in-flight archiving state.
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	// The chat picker should show "Archiving session…" after re-entry.
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Archiving session…")
	}); err != nil {
		t.Fatalf("expected archiving state in chat picker after re-entry; screen:\n%s", h.Driver.Screen())
	}
}

func TestTUI_ChatPickerView_Back(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
		tuitest.WithChats(testChats()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Initial implementation") ||
			strings.Contains(screen, "Loading chats")
	}); err != nil {
		t.Fatal(err)
	}

	// Press esc to go back.
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatalf("expected home view after esc; screen:\n%s", h.Driver.Screen())
	}
}

// The details surface keeps the park, its history and the actionable question
// together when entered through the real application and PTY.
func TestTUI_ChatPicker_AttentionTimeline(t *testing.T) {
	for _, kind := range []pb.AttentionKind{pb.AttentionKind_ATTENTION_KIND_REVIEW, pb.AttentionKind_ATTENTION_KIND_FIX, pb.AttentionKind_ATTENTION_KIND_UNSPECIFIED} {
		t.Run(kind.String(), func(t *testing.T) {
			sessions := testSessions()[:1]
			sessions[0].PrDisplayHeadSha = "2bbaede9000"
			if kind != pb.AttentionKind_ATTENTION_KIND_UNSPECIFIED {
				sessions[0].Attention = &pb.SessionAttention{Kind: kind, Sentence: "Failed after 3 attempts", ParkCodes: []string{"human-review:ui"}, Actions: []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_APPROVE, pb.AttentionAction_ATTENTION_ACTION_VIDEO}, EvidenceUrl: "https://example.com/video"}
			}
			h := tuitest.New(t, tuitest.WithRepos(testRepos()...), tuitest.WithSessions(sessions...), tuitest.WithChats(testChats()...), tuitest.WithTerminalSize(100, 24))
			h.Daemon.SetSessionTimeline(sessions[0].GetId(), &pb.TimelineStep{Title: "Build started", State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE})
			if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
				t.Fatal(err)
			}
			if err := h.Driver.SendEnter(); err != nil {
				t.Fatal(err)
			}
			if err := h.Driver.WaitForText(waitTimeout, "Initial implementation"); err != nil {
				t.Fatal(err)
			}
			if kind != pb.AttentionKind_ATTENTION_KIND_UNSPECIFIED {
				for _, text := range []string{"Timeline", "Needed from you", "head 2bbaede", "[enter] open chat"} {
					if err := h.Driver.WaitForText(waitTimeout, text); err != nil {
						t.Fatalf("missing %q: %v\n%s", text, err, h.Driver.Screen())
					}
				}
				if kind == pb.AttentionKind_ATTENTION_KIND_REVIEW {
					if err := h.Driver.WaitForText(waitTimeout, "Does this UI change look right in the video?"); err != nil {
						t.Fatal(err)
					}
				} else if err := h.Driver.WaitForText(waitTimeout, "Waiting for a fix"); err != nil {
					t.Fatal(err)
				}
			} else if h.Driver.ScreenContains("Needed from you") {
				t.Fatal("legacy view has attention block")
			}
			t.Logf("PTY details capture:\n%s", h.Driver.Screen())
		})
	}
}

func TestTUI_ChatPicker_LongQuestionScroll(t *testing.T) {
	sessions := testSessions()[:1]
	sessions[0].Attention = &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_QUESTION, Sentence: strings.Repeat("Should we keep the behavior for every supported platform? ", 40) + "FINAL QUESTION?"}
	h := tuitest.New(t, tuitest.WithRepos(testRepos()...), tuitest.WithSessions(sessions...), tuitest.WithChats(testChats()...), tuitest.WithTerminalSize(100, 24))
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"Needed from you · pgup/pgdown", "Initial implementation", "[enter] open chat"} {
		if err := h.Driver.WaitForText(waitTimeout, text); err != nil {
			t.Fatal(err)
		}
	}
	// More pages than this question needs; the final page clamps to its end.
	for range 30 {
		if err := h.Driver.SendNamedKey("pgdn"); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.Driver.WaitForText(waitTimeout, "FINAL QUESTION?"); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"Initial implementation", "[enter] open chat", "[n]ew chat"} {
		if !h.Driver.ScreenContains(text) {
			t.Fatalf("lost %q after scrolling: %s", text, h.Driver.Screen())
		}
	}
	t.Logf("PTY long question capture:\n%s", h.Driver.Screen())
}

func TestTUI_ChatPicker_DeferredArchive_KeepsActions(t *testing.T) {
	sessions := testSessions()
	sessions[0].ArchivePending = true
	h := tuitest.New(t, tuitest.WithRepos(testRepos()...), tuitest.WithSessions(sessions...), tuitest.WithChats(testChats()...))
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Archiving session once every chat is idle") && strings.Contains(screen, "[n]ew chat") && strings.Contains(screen, "[a]rchive") && strings.Contains(screen, "[esc] back")
	}); err != nil {
		t.Fatalf("expected pending archive status and actions; screen:\n%s", h.Driver.Screen())
	}
	t.Logf("deferred archive screen:\n%s", h.Driver.Screen())
}
