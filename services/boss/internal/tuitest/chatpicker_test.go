package tuitest_test

import (
	"fmt"
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
	assertBlankLineAbove(t, h.Driver.Screen(), "Merged PR #597")
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
				for _, text := range []string{"Timeline", "Needed from you", "[enter] open chat"} {
					if err := h.Driver.WaitForText(waitTimeout, text); err != nil {
						t.Fatalf("missing %q: %v\n%s", text, err, h.Driver.Screen())
					}
				}
				if kind == pb.AttentionKind_ATTENTION_KIND_REVIEW {
					if err := h.Driver.WaitForText(waitTimeout, "Needed from you: Does this UI change look right in the"); err != nil {
						t.Fatal(err)
					}
					assertAnswerHangs(t, h.Driver.Screen(), "Does this UI change look right in the", "video?")
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

// assertAnswerHangs: the answer wraps at the chat table's width, so its last
// word drops to the row below and starts under the answer's first word.
func assertAnswerHangs(t *testing.T, screen, first, wrapped string) {
	t.Helper()
	lines := strings.Split(screen, "\n")
	for i, line := range lines[:len(lines)-1] {
		col := strings.Index(line, "Needed from you: "+first)
		if col < 0 {
			continue
		}
		want := strings.Repeat(" ", col+len("Needed from you: ")) + wrapped
		if got := strings.TrimRight(lines[i+1], " "); got != want {
			t.Fatalf("row below the label is %q, want %q:\n%s", got, want, screen)
		}
		return
	}
	t.Fatalf("no %q row with a row below it:\n%s", "Needed from you: "+first, screen)
}

// A session that needs you keeps its status in the details header, and an
// archive in flight shows the archiving spinner there, as for any session.
func TestTUI_ChatPicker_AttentionHeaderKeepsStatusAndArchiving(t *testing.T) {
	sessions := testSessions()[:1]
	sessions[0].DisplayLabel = "fix"
	sessions[0].DisplayIntent = pb.DisplayIntent_DISPLAY_INTENT_DANGER
	sessions[0].Attention = &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_FIX, Sentence: "Failed after 3 attempts"}
	h := tuitest.New(t, tuitest.WithRepos(testRepos()...), tuitest.WithSessions(sessions...), tuitest.WithChats(testChats()...), tuitest.WithTerminalSize(100, 24), tuitest.WithArchiveDelay(3*time.Second))
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"Add dark mode (fix)", "add-dark-mode · work-claude", "Needed from you", "[a]rchive"} {
		if err := h.Driver.WaitForText(waitTimeout, text); err != nil {
			t.Fatalf("missing %q: %v\n%s", text, err, h.Driver.Screen())
		}
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
		return strings.Contains(screen, "Archiving session…") && strings.Contains(screen, "archiving)")
	}); err != nil {
		t.Fatalf("header lost the archiving spinner: %v\n%s", err, h.Driver.Screen())
	}
	t.Logf("PTY archiving capture:\n%s", h.Driver.Screen())
}

// TestTUI_ChatPicker_NarrowAnswerRenders: on a terminal narrow and short
// enough to leave the answer one or two rows, an answer with a word wider than
// the label leaves still renders the picker instead of freezing on "Loading
// chats", and paging reaches its last word.
func TestTUI_ChatPicker_NarrowAnswerRenders(t *testing.T) {
	for _, size := range [][2]int{{40, 20}, {30, 24}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			sessions := testSessions()[:1]
			sessions[0].Attention = &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_QUESTION, Sentence: "Should we keep the implementation? It touches services/boss/internal/views and supercalifragilisticexpialidociousness-everywhere-on-every-platform, so say yes or no. FINAL?"}
			h := tuitest.New(t, tuitest.WithRepos(testRepos()...), tuitest.WithSessions(sessions...), tuitest.WithChats(testChats()...), tuitest.WithTerminalSize(size[0], size[1]))
			if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
				t.Fatal(err)
			}
			if err := h.Driver.SendEnter(); err != nil {
				t.Fatal(err)
			}
			if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
				return strings.Contains(screen, "Needed from you") && !strings.Contains(screen, "Loading chats")
			}); err != nil {
				t.Fatalf("picker never rendered the answer: %v\n%s", err, h.Driver.Screen())
			}
			t.Logf("PTY first page at %dx%d:\n%s", size[0], size[1], h.Driver.Screen())
			for range 30 {
				if err := h.Driver.SendNamedKey("pgdn"); err != nil {
					t.Fatal(err)
				}
			}
			if err := h.Driver.WaitForText(waitTimeout, "FINAL?"); err != nil {
				t.Fatalf("paging never reached the last word: %v\n%s", err, h.Driver.Screen())
			}
			t.Logf("PTY last page at %dx%d:\n%s", size[0], size[1], h.Driver.Screen())
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
	for _, text := range []string{"Needed from you: Should we keep", "pgup/pgdown (1–", "Initial implementation", "[enter] open chat"} {
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
