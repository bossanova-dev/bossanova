// BOS-1231: PTY coverage of the hidden alt+up / alt+down reorder chords. The
// in-process Update tests in services/boss/api/tui prove the model
// reorders; only a real boss process driven through a real terminal can prove
// the chord survives the input parser, reaches the home view, and changes what
// is actually on screen.

package e2e_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/fixtures"
	"github.com/recurser/boss/internal/tuitest"
	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
)

func TestTUI_HomeView_ShowsSessions(t *testing.T) {
	h := tuitest.NewMock(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Fix login bug"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "[n]ew"); err != nil {
		t.Fatal(err)
	}
}

func TestTUI_HomeView_LogoutConfirm(t *testing.T) {
	h := tuitest.NewMock(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
		tuitest.WithLoggedInUser("test-user@example.com"),
	)

	waitForLoggedInHome(t, h)

	if err := h.Driver.SendKey('l'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Log out test-user@example.com?"); err != nil {
		t.Fatalf("expected logout confirm; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitForText(waitTimeout, "[l]ogin"); err != nil {
		t.Fatalf("expected [l]ogin after logout; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.WaitFor(waitTimeout, func(_ string) bool {
		calls := h.Daemon.NotifyAuthChangeCalls()
		return len(calls) > 0 && calls[len(calls)-1] == "logout"
	}); err != nil {
		t.Fatalf("NotifyAuthChange logout was never called; screen:\n%s", h.Driver.Screen())
	}
}

func TestTUI_HomeView_LogoutFailureKeepsSignedInPresentation(t *testing.T) {
	h := tuitest.NewMock(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
		tuitest.WithLoggedInUser("test-user@example.com"),
		tuitest.WithE2ELogoutError(),
	)

	waitForLoggedInHome(t, h)
	if err := h.Driver.SendKey('l'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Log out test-user@example.com?"); err != nil {
		t.Fatalf("expected logout confirm; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Logout: e2e memory store: logout failed") &&
			strings.Contains(screen, "[l]ogout") &&
			strings.Contains(screen, "test-user@example.com") &&
			!strings.Contains(screen, "Log out test-user@example.com?")
	}); err != nil {
		t.Fatalf("failed logout did not restore signed-in Home presentation; screen:\n%s", h.Driver.Screen())
	}
	if calls := h.Daemon.NotifyAuthChangeCalls(); len(calls) != 0 {
		t.Fatalf("NotifyAuthChange should not be called after failed logout; got %v", calls)
	}
}

func TestTUI_HomeView_LogoutCancel(t *testing.T) {
	h := tuitest.NewMock(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
		tuitest.WithLoggedInUser("test-user@example.com"),
	)

	waitForLoggedInHome(t, h)

	if err := h.Driver.SendKey('l'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Log out test-user@example.com?"); err != nil {
		t.Fatalf("expected logout confirm; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return !strings.Contains(screen, "Log out test-user@example.com?") &&
			strings.Contains(screen, "[l]ogout")
	}); err != nil {
		t.Fatalf("expected logout confirm to cancel back to home; screen:\n%s", h.Driver.Screen())
	}
	if calls := h.Daemon.NotifyAuthChangeCalls(); len(calls) != 0 {
		t.Fatalf("NotifyAuthChange should not have been called on cancel; got %v", calls)
	}
}

func TestTUI_HomeView_MovedKeysAreInert(t *testing.T) {
	h := tuitest.NewMock(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	// 'r' left this list when BOS-837 gave it the hidden rename shortcut; the
	// keys below are still the ones that moved to the session detail view.
	for _, key := range []byte{'t', 'c', 'a'} {
		if err := h.Driver.SendKey(key); err != nil {
			t.Fatal(err)
		}
	}

	if err := h.Driver.WaitForText(waitTimeout, "[n]ew"); err != nil {
		t.Fatal(err)
	}
	screen := h.Driver.Screen()
	if !strings.Contains(screen, "Add dark mode") {
		t.Fatalf("expected to stay on home; screen:\n%s", screen)
	}
	for _, confirmation := range []string{"Archive this session", "[y/enter] confirm"} {
		if strings.Contains(screen, confirmation) {
			t.Fatalf("expected no archive confirmation %q; screen:\n%s", confirmation, screen)
		}
	}
}

func TestTUI_HomeView_SingleSession(t *testing.T) {
	sessions := testSessions()[:1] // Only first session.
	h := tuitest.NewMock(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(sessions...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	screen := h.Driver.Screen()
	if !strings.Contains(screen, "[n]ew") {
		t.Fatalf("expected action bar with single session; screen:\n%s", screen)
	}
}

// startHomeRename opens the hidden [r] editor on the first session of a freshly
// started home list and returns once its footer is on screen.
func startHomeRename(t *testing.T, h *tuitest.Harness) {
	t.Helper()

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendKey('r'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "[enter] rename"); err != nil {
		t.Fatalf("[r] did not open the rename editor: %v; screen:\n%s", err, h.Driver.Screen())
	}
}

// TestTUI_HomeView_HiddenRenameCommitsTheNewTitle drives the whole BOS-837 round
// trip through a real terminal: the unit tests in api/tui stub the client,
// so this is the only place that proves the keystrokes reach a daemon and that
// what comes back is what lands on the list.
func TestTUI_HomeView_HiddenRenameCommitsTheNewTitle(t *testing.T) {
	h := newHomeWithSessions(t)
	startHomeRename(t, h)

	if err := h.Driver.SendString(" renamed"); err != nil {
		t.Fatal(err)
	}
	// Wait for the edit to show up in the input before committing, so a failure
	// here reads as "typing was dropped" rather than "the wrong title was saved".
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode renamed"); err != nil {
		t.Fatalf("typed text never reached the rename input: %v; screen:\n%s", err, h.Driver.Screen())
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(waitTimeout)
	calls := h.Daemon.UpdateSessionCalls()
	for time.Now().Before(deadline) && len(calls) == 0 {
		time.Sleep(50 * time.Millisecond)
		calls = h.Daemon.UpdateSessionCalls()
	}
	if len(calls) == 0 {
		t.Fatalf("UpdateSession was never called; screen:\n%s", h.Driver.Screen())
	}
	if len(calls) != 1 {
		t.Fatalf("UpdateSession called %d times, want exactly 1: %v", len(calls), calls)
	}
	req := calls[0]
	if req.Id != "sess-aaa-111" {
		t.Fatalf("UpdateSession wrote session %q, want sess-aaa-111", req.Id)
	}
	if req.Title == nil {
		t.Fatal("UpdateSession sent no title")
	}
	if *req.Title != "Add dark mode renamed" {
		t.Fatalf("UpdateSession title = %q, want %q", *req.Title, "Add dark mode renamed")
	}

	// The editor closes and the renamed row is what the operator is left looking
	// at — checking both together rules out a screen that still shows the title
	// only because the input is still open.
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Add dark mode renamed") &&
			strings.Contains(screen, "[n]ew session") &&
			!strings.Contains(screen, "[enter] rename")
	}); err != nil {
		t.Fatalf("renamed title never settled on the list: %v; screen:\n%s", err, h.Driver.Screen())
	}
	for _, s := range h.Daemon.Sessions() {
		if s.Id == "sess-aaa-111" && s.Title != "Add dark mode renamed" {
			t.Fatalf("daemon session title = %q, want %q", s.Title, "Add dark mode renamed")
		}
	}
}

// TestTUI_HomeView_HiddenRenameSwallowsQuit is the regression guard for the trap
// BOS-837 introduced: home's [q] quits the whole binary, so a rename editor that
// let 'q' fall through would kill boss mid-edit. This cannot pass vacuously — a
// real quit ends the process and Done() closes.
func TestTUI_HomeView_HiddenRenameSwallowsQuit(t *testing.T) {
	h := newHomeWithSessions(t)
	startHomeRename(t, h)

	if err := h.Driver.SendString("q"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.Driver.Done():
		t.Fatal("boss quit while the rename editor was open; [q] must be typed, not obeyed")
	case <-time.After(750 * time.Millisecond):
	}
	// 'q' is not merely swallowed, it is text: it belongs in the title.
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "[enter] rename") &&
			strings.Contains(screen, "Add dark modeq")
	}); err != nil {
		t.Fatalf("q did not reach the rename input: %v; screen:\n%s", err, h.Driver.Screen())
	}
}

// TestTUI_HomeView_HiddenRenameEscapeCancels covers the abandon path: esc must
// leave the title alone and write nothing at all.
func TestTUI_HomeView_HiddenRenameEscapeCancels(t *testing.T) {
	h := newHomeWithSessions(t)
	startHomeRename(t, h)

	if err := h.Driver.SendString(" nope"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode nope"); err != nil {
		t.Fatalf("typed text never reached the rename input: %v; screen:\n%s", err, h.Driver.Screen())
	}
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "[n]ew session") && !strings.Contains(screen, "[enter] rename")
	}); err != nil {
		t.Fatalf("esc did not close the rename editor: %v; screen:\n%s", err, h.Driver.Screen())
	}
	// Give a wrongly dispatched save time to land before declaring none happened.
	time.Sleep(250 * time.Millisecond)
	if calls := h.Daemon.UpdateSessionCalls(); len(calls) != 0 {
		t.Fatalf("esc wrote %d session update(s), want none: %v", len(calls), calls)
	}
	screen := h.Driver.Screen()
	if !strings.Contains(screen, "Add dark mode") {
		t.Fatalf("original title missing after cancel; screen:\n%s", screen)
	}
	if strings.Contains(screen, "Add dark mode nope") {
		t.Fatalf("abandoned edit survived the cancel; screen:\n%s", screen)
	}
}

func newHomeWithSessions(t *testing.T) *tuitest.Harness {
	t.Helper()

	return tuitest.NewMock(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
	)
}

// TestTUI_HomeView_AttentionBoard drives the real executable through a PTY.
func TestTUI_HomeView_AttentionBoard(t *testing.T) {
	for _, width := range []int{80, 120} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			h := tuitest.NewMock(t, tuitest.WithRepos(testRepos()...), tuitest.WithSessions(fixtures.AttentionSessions()...), tuitest.WithTerminalSize(width, 60))
			for _, text := range []string{"─ NEEDS YOU ─", "─ IN PROGRESS ─", "? question", "checking 4/9", "[BOS-1509]"} {
				if err := h.Driver.WaitForText(waitTimeout, text); err != nil {
					t.Fatal(err)
				}
			}
			before := h.Driver.Screen()
			if strings.Contains(before, "[r]esume") || strings.Contains(before, "[x] dismiss") {
				t.Fatalf("fix footer advertises the unadvertised resume or dismiss:\n%s", before)
			}
			if strings.Contains(before, "◌") {
				t.Fatalf("unexpected progress glyph:\n%s", before)
			}
			// One down lands on the oldest review.
			if err := h.Driver.SendKey('j'); err != nil {
				t.Fatal(err)
			}
			if err := h.Driver.WaitForText(waitTimeout, "[m]erge"); err != nil {
				t.Fatal(err)
			}
			screen := h.Driver.Screen()
			if !strings.Contains(screen, "[v]ideo") {
				t.Fatalf("missing [v]ideo:\n%s", screen)
			}
			if strings.Contains(screen, "[y]") || strings.Contains(screen, "[b]") {
				t.Fatalf("review footer still offers approve or send back:\n%s", screen)
			}
			if strings.Contains(screen, "[g]") {
				t.Fatalf("home footer advertises the hidden [g]:\n%s", screen)
			}
			if strings.Contains(screen, "[r]esume") || strings.Contains(screen, "[x] dismiss") {
				t.Fatalf("review footer retained fix actions:\n%s", screen)
			}
			for _, line := range strings.Split(screen, "\n") {
				if len([]rune(line)) > width {
					t.Fatalf("line overflows %d: %q", width, line)
				}
			}
			t.Logf("PTY Needs you board at %d columns, selected review:\n%s", width, screen)
		})
	}
}

// assertBlankLineAbove checks that the first screen line containing want has
// exactly one blank line above it, with a drawn line above that.
func assertBlankLineAbove(t *testing.T, screen, want string) {
	t.Helper()
	lines := strings.Split(screen, "\n")
	for i, line := range lines {
		if !strings.Contains(line, want) {
			continue
		}
		if i < 2 || strings.TrimSpace(lines[i-1]) != "" || strings.TrimSpace(lines[i-2]) == "" {
			t.Fatalf("%q does not sit one blank line below the line above it:\n%s", want, screen)
		}
		return
	}
	t.Fatalf("%q not on screen:\n%s", want, screen)
}

// TestTUI_Home_AttentionReviewMerge drives [m] on a sign-off park's review
// row through the real executable: a plain confirm, then one approving merge
// pinned to the head the row showed.
func TestTUI_Home_AttentionReviewMerge(t *testing.T) {
	const head = "0123456789abcdef0123456789abcdef01234567"
	sessions := testSessions()
	sessions[0].PrDisplayHeadSha = head
	sessions[0].Attention = attention.Compute(attention.Input{ParkCodes: []string{"human-review:ui"}})
	h := tuitest.NewMock(t, tuitest.WithRepos(testRepos()...), tuitest.WithSessions(sessions...))
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendKey('m'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Merge PR #597?"); err != nil {
		t.Fatalf("confirmation missing: %v\n%s", err, h.Driver.Screen())
	}
	if calls := h.Daemon.OperatorMergeCalls(); len(calls) != 0 {
		t.Fatalf("request before confirmation: %v", calls)
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Merged PR #597"); err != nil {
		t.Fatalf("result missing: %v\n%s", err, h.Driver.Screen())
	}
	assertBlankLineAbove(t, h.Driver.Screen(), "Merged PR #597")
	calls := h.Daemon.OperatorMergeCalls()
	if len(calls) != 1 || calls[0].GetId() != "sess-aaa-111" || calls[0].GetExpectedHeadSha() != head || !calls[0].GetShouldApprove() || len(calls[0].GetWaives()) != 1 || calls[0].GetWaives()[0] != "human-review:ui" {
		t.Fatalf("operator merge calls=%v", calls)
	}
	if calls := h.Daemon.ApproveSessionPRCalls(); len(calls) != 0 {
		t.Fatalf("the TUI ran the CLI approve verb: %v", calls)
	}
}

// Enter on a needs-you row opens the session's chat list with the chat the
// attention names already selected, so a second enter opens that chat.
func TestTUI_Home_AttentionEnterSelectsTheTargetChat(t *testing.T) {
	sessions := testSessions()
	sessions[0].Attention = &pb.SessionAttention{
		Kind:         pb.AttentionKind_ATTENTION_KIND_FIX,
		Sentence:     "Failed after 3 attempts",
		Actions:      []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_RESUME, pb.AttentionAction_ATTENTION_ACTION_OPEN_CHAT},
		TargetChatId: "claude-333",
	}
	h := tuitest.NewMock(t, tuitest.WithRepos(testRepos()...), tuitest.WithSessions(sessions...), tuitest.WithChats(testChats()...))
	if err := h.Driver.WaitForText(waitTimeout, "[enter] select"); err != nil {
		t.Fatalf("plain enter hint missing: %v\n%s", err, h.Driver.Screen())
	}
	if h.Driver.ScreenContains("open the failed chat") {
		t.Fatalf("footer still names the failed chat:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	selected := func(screen string) bool {
		for _, line := range strings.Split(screen, "\n") {
			if strings.Contains(line, "❯") && strings.Contains(line, "Address review comments") {
				return true
			}
		}
		return false
	}
	if err := h.Driver.WaitFor(waitTimeout, selected); err != nil {
		t.Fatalf("details did not select the failed chat: %v\n%s", err, h.Driver.Screen())
	}
}

// Selecting a FIX row whose chat was deleted, or that names none, opens the
// chat list and starts nothing. Before #3238 enter attached the target, and
// bossd turned that attach into a brand-new chat (PR #3220).
func TestTUI_Home_AttentionEnterOnADeletedChatStartsNothing(t *testing.T) {
	for name, target := range map[string]string{"deleted chat": "ce2c3b43-9ed1-495e-ab1f-058c51d5b76a", "no chat": ""} {
		t.Run(name, func(t *testing.T) {
			sessions := testSessions()
			sessions[0].Attention = &pb.SessionAttention{
				Kind:         pb.AttentionKind_ATTENTION_KIND_FIX,
				Sentence:     "Run blocked: automatic repair left the PR conflicted",
				Actions:      []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_RESUME, pb.AttentionAction_ATTENTION_ACTION_DISMISS, pb.AttentionAction_ATTENTION_ACTION_OPEN_CHAT},
				TargetChatId: target,
			}
			h := tuitest.NewMock(t, tuitest.WithRepos(testRepos()...), tuitest.WithSessions(sessions...), tuitest.WithChats(testChats()...))
			if err := h.Driver.WaitForText(waitTimeout, "[enter] select"); err != nil {
				t.Fatalf("home did not load: %v\n%s", err, h.Driver.Screen())
			}
			if err := h.Driver.SendEnter(); err != nil {
				t.Fatal(err)
			}
			if err := h.Driver.WaitForText(waitTimeout, "Initial implementation"); err != nil {
				t.Fatalf("enter did not open the chat list: %v\n%s", err, h.Driver.Screen())
			}
			time.Sleep(time.Second)
			if h.Driver.ScreenContains("Launching") {
				t.Fatalf("enter launched a chat:\n%s", h.Driver.Screen())
			}
			if calls := h.Daemon.RecordChatCalls(); len(calls) != 0 {
				t.Fatalf("enter recorded %d chat(s): %v", len(calls), calls)
			}
			if req := h.Daemon.LastCreateSession(); req != nil {
				t.Fatalf("enter created a session: %v", req)
			}
		})
	}
}

// [r] on a blocked run asks before resuming, on Home and on the details view:
// both screens also bind a hidden [r]ename, so the confirmation is what tells
// the operator which one they pressed. Esc resumes nothing; y and enter resume.
func TestTUI_AttentionResumeAsksFirst(t *testing.T) {
	sessions := testSessions()
	sessions[0].Attention = &pb.SessionAttention{
		Kind:         pb.AttentionKind_ATTENTION_KIND_FIX,
		Sentence:     "Run blocked: MAD-1751 implemented",
		Actions:      []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_RESUME, pb.AttentionAction_ATTENTION_ACTION_DISMISS, pb.AttentionAction_ATTENTION_ACTION_OPEN_CHAT},
		TargetChatId: "claude-333",
	}
	h := tuitest.NewMock(t, tuitest.WithRepos(testRepos()...), tuitest.WithSessions(sessions...), tuitest.WithChats(testChats()...))
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}
	resumeAfter := func(where string, confirm func() error, want int) {
		t.Helper()
		if err := h.Driver.SendKey('r'); err != nil {
			t.Fatal(err)
		}
		if err := h.Driver.WaitForText(waitTimeout, "Resume this run?"); err != nil {
			t.Fatalf("%s: [r] did not ask to resume: %v\n%s", where, err, h.Driver.Screen())
		}
		if err := h.Driver.SendEscape(); err != nil {
			t.Fatal(err)
		}
		if err := h.Driver.WaitForNoText(waitTimeout, "Resume this run?"); err != nil {
			t.Fatalf("%s: esc left the confirmation up: %v\n%s", where, err, h.Driver.Screen())
		}
		if calls := h.Daemon.ResumeSessionRunCalls(); len(calls) != want-1 {
			t.Fatalf("%s: esc resumed: %v", where, calls)
		}
		if err := h.Driver.SendKey('r'); err != nil {
			t.Fatal(err)
		}
		if err := h.Driver.WaitForText(waitTimeout, "Resume this run?"); err != nil {
			t.Fatalf("%s: second [r] did not ask: %v\n%s", where, err, h.Driver.Screen())
		}
		if err := confirm(); err != nil {
			t.Fatal(err)
		}
		if err := h.Driver.WaitForText(waitTimeout, "Run resumed"); err != nil {
			t.Fatalf("%s: no resume result: %v\n%s", where, err, h.Driver.Screen())
		}
		calls := h.Daemon.ResumeSessionRunCalls()
		if len(calls) != want || calls[want-1].GetSessionId() != "sess-aaa-111" {
			t.Fatalf("%s: resume calls=%v", where, calls)
		}
	}
	resumeAfter("home", func() error { return h.Driver.SendKey('y') }, 1)
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Address review comments"); err != nil {
		t.Fatalf("details did not open: %v\n%s", err, h.Driver.Screen())
	}
	resumeAfter("details", h.Driver.SendEnter, 2)
}

func TestTUI_HomeView_SlashFilter(t *testing.T) {
	h := newHomeWithSessions(t)
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendKey('/'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendString("dark"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, fmt.Sprintf("(1 of %d)", len(testSessions()))); err != nil {
		t.Fatal(err)
	}
	if screen := h.Driver.Screen(); strings.Contains(screen, "Fix login bug") || !strings.Contains(screen, "Add dark mode") {
		t.Fatalf("filter did not narrow board:\n%s", screen)
	}
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Fix login bug"); err != nil {
		t.Fatal(err)
	}
}

// screenLineOf returns the 0-based line index the first occurrence of text is
// rendered on, or -1. Rendered ORDER is the artifact the ticket is about, and
// comparing line indices is the only way to read it off a terminal screen —
// asserting both titles are merely present would pass however they are stacked.
func screenLineOf(screen, text string) int {
	for i, line := range strings.Split(screen, "\n") {
		if strings.Contains(line, text) {
			return i
		}
	}
	return -1
}

func assertRenderedAbove(t *testing.T, screen, upper, lower string) {
	t.Helper()
	up, low := screenLineOf(screen, upper), screenLineOf(screen, lower)
	if up < 0 || low < 0 {
		t.Fatalf("expected both %q (line %d) and %q (line %d) on screen:\n%s", upper, up, lower, low, screen)
	}
	if up >= low {
		t.Fatalf("expected %q (line %d) to render above %q (line %d):\n%s", upper, up, lower, low, screen)
	}
}

// TestTUI_HomeView_AltUpReordersTheSessionList is AC10: the real binary, the
// real chord, and a rendered order that actually changed.
func TestTUI_HomeView_AltUpReordersTheSessionList(t *testing.T) {
	h := newHomeWithSessions(t)

	if err := h.Driver.WaitForText(waitTimeout, "Fix login bug"); err != nil {
		t.Fatal(err)
	}
	// Baseline: the daemon's order. Without this the assertion after the chord
	// could pass against a list that was already in that order.
	assertRenderedAbove(t, h.Driver.Screen(), "Add dark mode", "Fix login bug")

	// Put the cursor on the second session, then move it up.
	if err := h.Driver.SendNamedKey("down"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return chevronIsOn(screen, "Fix login bug")
	}); err != nil {
		t.Fatalf("the cursor never reached 'Fix login bug': %v; screen:\n%s", err, h.Driver.Screen())
	}
	if err := h.Driver.SendNamedKey("alt+up"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		up, low := screenLineOf(screen, "Fix login bug"), screenLineOf(screen, "Add dark mode")
		return up >= 0 && low >= 0 && up < low
	}); err != nil {
		t.Fatalf("alt+up did not reorder the rendered list: %v; screen:\n%s", err, h.Driver.Screen())
	}

	// The chevron follows the session that moved, not the row it vacated.
	screen := h.Driver.Screen()
	if !chevronIsOn(screen, "Fix login bug") {
		t.Fatalf("the cursor did not follow the moved session; screen:\n%s", screen)
	}
	calls := h.Daemon.MoveSessionCalls()
	if len(calls) != 1 {
		t.Fatalf("the chord issued %d MoveSession calls, want exactly 1", len(calls))
	}
	if got := calls[0].GetId(); got != "sess-bbb-222" {
		t.Fatalf("MoveSession moved %q, want the selected session sess-bbb-222", got)
	}

	// The reorder is not a one-frame flicker: the daemon now serves the new
	// order, and the list still shows it several poll intervals later.
	time.Sleep(3 * pollSettle)
	assertRenderedAbove(t, h.Driver.Screen(), "Fix login bug", "Add dark mode")
}

// TestTUI_HomeView_AltDownReordersTheSessionList is AC10's other direction.
func TestTUI_HomeView_AltDownReordersTheSessionList(t *testing.T) {
	h := newHomeWithSessions(t)

	if err := h.Driver.WaitForText(waitTimeout, "Fix login bug"); err != nil {
		t.Fatal(err)
	}
	assertRenderedAbove(t, h.Driver.Screen(), "Add dark mode", "Fix login bug")

	if err := h.Driver.SendNamedKey("alt+down"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		up, low := screenLineOf(screen, "Fix login bug"), screenLineOf(screen, "Add dark mode")
		return up >= 0 && low >= 0 && up < low
	}); err != nil {
		t.Fatalf("alt+down did not reorder the rendered list: %v; screen:\n%s", err, h.Driver.Screen())
	}
	if !chevronIsOn(h.Driver.Screen(), "Add dark mode") {
		t.Fatalf("the cursor did not follow the moved session; screen:\n%s", h.Driver.Screen())
	}
}

// TestTUI_HomeView_BareArrowsStillOnlyNavigate is AC3 at the binary level: the
// unmodified arrow the table owns must move the cursor and nothing else. It is
// also the control that keeps the two tests above honest — if plain "down" were
// reordering, their "the order changed" assertions would prove nothing about
// the chord.
func TestTUI_HomeView_BareArrowsStillOnlyNavigate(t *testing.T) {
	h := newHomeWithSessions(t)

	if err := h.Driver.WaitForText(waitTimeout, "Fix login bug"); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"down", "down", "up"} {
		if err := h.Driver.SendNamedKey(key); err != nil {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
	}

	assertRenderedAbove(t, h.Driver.Screen(), "Add dark mode", "Fix login bug")
	if calls := h.Daemon.MoveSessionCalls(); len(calls) != 0 {
		t.Fatalf("bare navigation issued %d MoveSession calls, want none: %v", len(calls), calls)
	}
}

// pollSettle is Home's session-poll interval. Waiting a few of them is how a
// test distinguishes a reorder that stuck from one the next poll undid.
const pollSettle = 2 * time.Second

// chevronIsOn reports whether the cursor chevron is rendered on the line
// carrying text.
func chevronIsOn(screen, text string) bool {
	for _, line := range strings.Split(screen, "\n") {
		if strings.Contains(line, text) {
			return strings.Contains(line, "❯")
		}
	}
	return false
}

// TestRankedBlockMoveBehaviourRisesPastUnrankedRows pins the mock rule the PTY
// test below depends on. Without this the PTY case could pass against a
// behaviour that quietly degraded to an adjacent swap, which is exactly the
// blind spot it exists to close.
func TestRankedBlockMoveBehaviourRisesPastUnrankedRows(t *testing.T) {
	sessions := []*pb.Session{{Id: "a"}, {Id: "b"}, {Id: "c"}, {Id: "d"}}
	ids := func(got []*pb.Session) string {
		out := make([]string, len(got))
		for i, s := range got {
			out[i] = s.GetId()
		}
		return strings.Join(out, ",")
	}

	move := tuitest.RankedBlockMoveBehaviour()

	// Nothing is ranked, so the third row does NOT swap with the second: it
	// joins the end of the (empty) ranked block and lands on top.
	got, moved := move(sessions, 2, true)
	if !moved {
		t.Fatal("moving the third row up reported a no-op")
	}
	if ids(got) != "c,a,b,d" {
		t.Fatalf("order = %s, want c,a,b,d; a rise of two is the whole point — an adjacent swap "+
			"would give a,c,b,d and the TUI's optimistic guess would be right by construction", ids(got))
	}

	// The row directly below the block rises by exactly one, as the daemon
	// documents ("From the first or second unranked row that is exactly one
	// position").
	got, moved = move(got, 1, true)
	if !moved || ids(got) != "a,c,b,d" {
		t.Fatalf("order = %s (moved=%v), want a,c,b,d", ids(got), moved)
	}

	// An unranked row has no rank to clear, so moving it down is the daemon's
	// successful no-op rather than an error.
	if _, moved := move(got, 3, false); moved {
		t.Fatal("moving an unranked row down reported a move; the ordering model cannot express it")
	}
}

// TestTUI_HomeView_ListConvergesOnAMultiPositionDaemonMove is the PTY half of
// the same guarantee: the real binary, the real chord, and a daemon that settles
// somewhere the TUI did not guess. The TUI paints an adjacent swap optimistically
// and the daemon answers is_moved=true from two rows higher — the board must end
// up showing the DAEMON's order, and keep showing it across later polls. An
// override released only on exact agreement pins the wrong order here forever.
func TestTUI_HomeView_ListConvergesOnAMultiPositionDaemonMove(t *testing.T) {
	h := tuitest.NewMock(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
		tuitest.WithMoveSessionBehaviour(tuitest.RankedBlockMoveBehaviour()),
	)

	const third = "Add rate limiting to public API"
	if err := h.Driver.WaitForText(waitTimeout, third); err != nil {
		t.Fatal(err)
	}
	// Baseline: the daemon's own order, so the assertion after the chord cannot
	// pass against a list that was already arranged that way.
	assertRenderedAbove(t, h.Driver.Screen(), "Add dark mode", third)

	for i := 0; i < 6 && !chevronIsOn(h.Driver.Screen(), third); i++ {
		if err := h.Driver.SendNamedKey("down"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !chevronIsOn(h.Driver.Screen(), third) {
		t.Fatalf("the cursor never reached %q; screen:\n%s", third, h.Driver.Screen())
	}

	if err := h.Driver.SendNamedKey("alt+up"); err != nil {
		t.Fatal(err)
	}

	// The daemon lifted it to the TOP, two rows above the adjacent swap the TUI
	// rendered for the first frame.
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		moved, first := screenLineOf(screen, third), screenLineOf(screen, "Add dark mode")
		return moved >= 0 && first >= 0 && moved < first
	}); err != nil {
		t.Fatalf("the list never converged on the daemon's order: %v; screen:\n%s", err, h.Driver.Screen())
	}

	// And it stays there: the override must not re-impose the optimistic guess
	// on every subsequent poll.
	time.Sleep(3 * pollSettle)
	screen := h.Driver.Screen()
	assertRenderedAbove(t, screen, third, "Add dark mode")
	assertRenderedAbove(t, screen, third, "Fix login bug")
}

// TestTUI_HomeView_TicketNoticeShowsUntilDismissed covers the TUI half of a
// ticket bossd could not move after a merge: the note it leaves shows on Home
// until [c] clears it, and the dismissal is stored on the note itself.
func TestTUI_HomeView_TicketNoticeShowsUntilDismissed(t *testing.T) {
	repoID := testRepos()[0].GetId()
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
		tuitest.WithTerminalSize(160, 40),
		tuitest.WithNotes(
			&pb.Note{Id: "n-unseen", RepoId: repoID, Body: `PR #6660 merged, but its ticket MAD-1764 could not be moved (state "Done" not found)`, Tags: []string{models.NoteTagTicketNotMoved, "tracker"}},
			&pb.Note{Id: "n-seen", RepoId: repoID, Body: `PR #5555 merged, but its ticket MAD-1 could not be moved (state "Done" not found)`, Tags: []string{models.NoteTagSeen, models.NoteTagTicketNotMoved, "tracker"}},
			&pb.Note{Id: "n-agent", RepoId: repoID, Body: "PR #4444 agent note about the tracker", Tags: []string{"tracker"}},
		),
	)

	if err := h.Driver.WaitForText(waitTimeout, "PR #6660"); err != nil {
		t.Fatalf("the unseen ticket notice never showed; screen:\n%s", h.Driver.Screen())
	}
	screen := h.Driver.Screen()
	if !strings.Contains(screen, "[c]lear") {
		t.Fatalf("the notice offers no dismissal; screen:\n%s", screen)
	}
	for _, hidden := range []string{"PR #5555", "PR #4444"} {
		if strings.Contains(screen, hidden) {
			t.Fatalf("%s is shown, want only unseen ticket notices; screen:\n%s", hidden, screen)
		}
	}

	if err := h.Driver.SendKey('c'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForNoText(waitTimeout, "PR #6660"); err != nil {
		t.Fatalf("the notice stayed after [c]; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.WaitFor(waitTimeout, func(string) bool {
		for _, note := range realNotes(t, h) {
			if strings.Contains(note.GetBody(), "PR #6660") {
				return slices.Contains(note.GetTags(), models.NoteTagSeen)
			}
		}
		return false
	}); err != nil {
		t.Fatalf("the dismissed note was never tagged seen: %v", err)
	}
	for _, note := range realNotes(t, h) {
		if strings.Contains(note.GetBody(), "PR #4444") && slices.Contains(note.GetTags(), models.NoteTagSeen) {
			t.Fatal("unrelated agent note was marked seen")
		}
	}

}

// One operator path across the wizard, the launch screen, the chat list and the
// home board, checking at each step what the daemon was asked to do.
func TestJourney_Sessions_AC1_AC2_AC14_AC25_CreateOpenListDismiss(t *testing.T) {
	const (
		sessID   = "sess-journey-1"
		title    = "journey chat"
		alertKey = "alert-journey-1"
	)
	h := tuitest.NewMock(t, tuitest.WithRepos(testRepos()...))
	h.Daemon.SetCreateSessionScript([]*pb.CreateSessionResponse{
		{Event: &pb.CreateSessionResponse_SessionCreated{SessionCreated: &pb.SessionCreated{Session: &pb.Session{
			Id:              sessID,
			RepoId:          "repo-1",
			RepoDisplayName: "my-app",
			BranchName:      "boss/journey-chat",
			State:           pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
		}}}},
	}, 0)
	h.Daemon.SetCreateSessionPersists(true)
	// Hold the chat launch on the launch screen so esc lands before tmux would
	// take the terminal.
	h.Daemon.SetRPCStallFor("RecordChat", time.Minute)

	if !t.Run("AC1 n, Quick Chat, a name and enter create the session and open it", func(t *testing.T) {
		waitText(t, h, "no active sessions")
		sendKeys(t, h, 'n')
		waitText(t, h, "Quick Chat")
		sendKeys(t, h, 'j', 'j')
		sendEnter(t, h)
		waitText(t, h, "Session name")
		if err := h.Driver.SendString(title); err != nil {
			t.Fatal(err)
		}
		sendEnter(t, h)
		waitText(t, h, "Launching")
		waitText(t, h, title)

		req := h.Daemon.LastCreateSession()
		if req == nil || req.GetRepoId() != "repo-1" || req.GetTitle() != title || !req.GetIsQuickChat() {
			t.Fatalf("CreateSession request = %v", req)
		}

		if err := h.Driver.SendEscape(); err != nil {
			t.Fatal(err)
		}
		waitText(t, h, "[esc] back")
		h.Daemon.SetRPCStallFor("RecordChat", 0)
		calls := waitCalls(t, h, h.Daemon.RecordChatCalls, 1)
		if c := calls[0]; c.GetSessionId() != sessID || c.GetResume() || c.GetRole() != pb.ChatRole_CHAT_ROLE_OPERATOR {
			t.Fatalf("RecordChat request = %v, want a fresh operator chat in %s", c, sessID)
		}
	}) {
		return
	}

	if !t.Run("AC2 home half: the new session is listed on the home board", func(t *testing.T) {
		if err := h.Driver.SendEscape(); err != nil {
			t.Fatal(err)
		}
		waitText(t, h, "[n]ew session")
		waitText(t, h, title)
		if h.Driver.ScreenContains("no active sessions") {
			t.Fatalf("home still shows the empty state:\n%s", h.Driver.Screen())
		}
	}) {
		return
	}

	if !t.Run("AC14 in part: a fix alert from the daemon turns the row into a fix row without its sentence", func(t *testing.T) {
		h.Daemon.SetSessionAttention(sessID, &pb.SessionAttention{
			Kind:     pb.AttentionKind_ATTENTION_KIND_FIX,
			Sentence: "Run blocked: the tests failed twice",
			AlertKey: alertKey,
			Actions:  []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_DISMISS},
		})
		if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool { return rowReadsFix(screen, title) }); err != nil {
			t.Fatalf("row never read fix: %v\n%s", err, h.Driver.Screen())
		}
		screen := h.Driver.Screen()
		if strings.Contains(screen, "Run blocked") {
			t.Fatalf("home list shows the attention sentence:\n%s", screen)
		}
		if strings.Contains(screen, "[x] dismiss") {
			t.Fatalf("home advertises the unadvertised dismiss:\n%s", screen)
		}
	}) {
		return
	}

	if !t.Run("AC25 x confirms once, then dismisses the captured alert key", func(t *testing.T) {
		sendKeys(t, h, 'x')
		waitText(t, h, "Dismiss this alert?")
		if calls := h.Daemon.DismissSessionAlertCalls(); len(calls) != 0 {
			t.Fatalf("DismissSessionAlert before confirmation: %v", calls)
		}
		sendEnter(t, h)
		waitText(t, h, "Alert dismissed")
		calls := h.Daemon.DismissSessionAlertCalls()
		if len(calls) != 1 || calls[0].GetSessionId() != sessID || calls[0].GetAlertKey() != alertKey {
			t.Fatalf("DismissSessionAlert calls = %v, want one for %s/%s", calls, sessID, alertKey)
		}

		h.Daemon.SetSessionAttention(sessID, nil)
		if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
			return screenRow(screen, title) != "" && !rowReadsFix(screen, title)
		}); err != nil {
			t.Fatalf("row kept the fix status after the daemon cleared the alert: %v\n%s", err, h.Driver.Screen())
		}
		if calls := h.Daemon.DismissSessionAlertCalls(); len(calls) != 1 {
			t.Fatalf("DismissSessionAlert calls = %d after settling, want 1", len(calls))
		}
	}) {
		return
	}
}

// rowReadsFix reports whether the home row naming title ends in the fix status.
func rowReadsFix(screen, title string) bool {
	return strings.HasSuffix(strings.TrimSpace(screenRow(screen, title)), " fix")
}

func waitText(t *testing.T, h *tuitest.Harness, text string) {
	t.Helper()
	if err := h.Driver.WaitForText(waitTimeout, text); err != nil {
		t.Fatalf("waiting for %q: %v\n%s", text, err, h.Driver.Screen())
	}
}

func sendKeys(t *testing.T, h *tuitest.Harness, keys ...byte) {
	t.Helper()
	for _, k := range keys {
		if err := h.Driver.SendKey(k); err != nil {
			t.Fatal(err)
		}
	}
}

func sendEnter(t *testing.T, h *tuitest.Harness) {
	t.Helper()
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
}

// waitCalls polls a mock daemon recorder until it holds want calls.
func waitCalls[T any](t *testing.T, h *tuitest.Harness, calls func() []T, want int) []T {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for {
		got := calls()
		if len(got) >= want || time.Now().After(deadline) {
			if len(got) != want {
				t.Fatalf("daemon recorded %d calls, want %d\n%s", len(got), want, h.Driver.Screen())
			}
			return got
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// navigateToSessionSettings walks Home → ChatPicker → SessionSettings for the
// first seeded session (sess-aaa-111 / "Add dark mode").
func navigateToSessionSettings(t *testing.T, h *tuitest.Harness) {
	t.Helper()
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	// ChatPicker appears regardless of chats being present. The action-bar
	// hint "[n]ew chat" is a stable marker.
	if err := h.Driver.WaitForText(waitTimeout, "[n]ew chat"); err != nil {
		t.Fatalf("expected ChatPicker; screen:\n%s", h.Driver.Screen())
	}
	// 's' routes to ViewSessionSettings.
	if err := h.Driver.SendKey('s'); err != nil {
		t.Fatal(err)
	}
	// Loaded state shows "Name: <title>" and an "[enter/space] edit" action bar.
	if err := h.Driver.WaitForText(waitTimeout, "[enter/space] edit"); err != nil {
		t.Fatalf("expected SessionSettings view; screen:\n%s", h.Driver.Screen())
	}
}

// TestTUI_SessionSettingsView_UpdateTitle exercises the full edit-and-save
// path: enter edit mode, append to the existing title, hit enter to save, and
// assert the new title is persisted by the real daemon.
func TestTUI_SessionSettingsView_UpdateTitle(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
	)

	navigateToSessionSettings(t, h)

	// Press enter to activate the Name row for editing. The text input is
	// pre-populated with the current title ("Add dark mode") and the cursor
	// sits at the end, so SendString appends.
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "[enter] save"); err != nil {
		t.Fatalf("expected edit mode; screen:\n%s", h.Driver.Screen())
	}

	// Append " v2" and commit.
	if err := h.Driver.SendString(" v2"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitFor(waitTimeout, func(string) bool {
		for _, session := range realSessions(t, h) {
			if session.GetTitle() == "Add dark mode v2" {
				return true
			}
		}
		return false
	}); err != nil {
		t.Fatalf("session title was not persisted: %v", err)
	}

}

// TestTUI_SessionSettingsView_Cancel verifies that escaping out of the Name
// edit preserves the original session title in the real daemon.
func TestTUI_SessionSettingsView_Cancel(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
	)

	navigateToSessionSettings(t, h)

	// Activate edit mode, type a change, then cancel with esc.
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "[enter] save"); err != nil {
		t.Fatalf("expected edit mode; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendString(" DISCARDED"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}

	// Back out of edit mode — action bar flips to "[enter/space] edit".
	if err := h.Driver.WaitForText(waitTimeout, "[enter/space] edit"); err != nil {
		t.Fatalf("expected return to row view after esc; screen:\n%s", h.Driver.Screen())
	}

	sessions := realSessions(t, h)
	found := false
	for _, session := range sessions {
		if session.GetTitle() == "Add dark mode" {
			found = true
		}
		if strings.Contains(session.GetTitle(), "DISCARDED") {
			t.Fatalf("cancelled title was persisted: %q", session.GetTitle())
		}
	}
	if !found {
		t.Fatal("original session title disappeared after cancel")
	}

}

func TestTUI_ReenterSessionSetup(t *testing.T) {
	for _, escape := range []bool{false, true} {
		name := "settle"
		if escape {
			name = "escape"
		}
		t.Run(name, func(t *testing.T) {
			sessions := testSessions()[:1]
			sessions[0].DisplaySettingUp = true
			h := tuitest.NewMock(t, tuitest.WithRepos(testRepos()...), tuitest.WithSessions(sessions...), tuitest.WithChats(testChats()...))
			settle := h.Daemon.SetSetupWatchScript(sessions[0].Id, []*pb.WatchSessionSetupResponse{
				{Event: &pb.WatchSessionSetupResponse_SetupOutput{SetupOutput: &pb.SetupScriptOutput{Text: "replayed setup line"}}},
				{Event: &pb.WatchSessionSetupResponse_SetupOutput{SetupOutput: &pb.SetupScriptOutput{Text: "live setup line"}}},
				{Event: &pb.WatchSessionSetupResponse_SetupSettled{SetupSettled: &pb.SessionSetupSettled{}}},
			}, 0)
			t.Cleanup(settle)
			if err := h.Driver.WaitForText(waitTimeout, sessions[0].Title); err != nil {
				t.Fatal(err)
			}
			if err := h.Driver.SendEnter(); err != nil {
				t.Fatal(err)
			}
			if err := h.Driver.WaitForText(waitTimeout, "replayed setup line"); err != nil {
				t.Fatal(err)
			}
			for _, text := range []string{"initializing", "Running setup script…", "Session " + sessions[0].Id, sessions[0].Title} {
				if !h.Driver.ScreenContains(text) {
					t.Fatalf("missing %q: %s", text, h.Driver.Screen())
				}
			}
			if escape {
				if err := h.Driver.SendEscape(); err != nil {
					t.Fatal(err)
				}
				if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
					return strings.Contains(screen, sessions[0].Title) && !strings.Contains(screen, "Running setup script…")
				}); err != nil {
					t.Fatal(err)
				}
				// Re-enter immediately: the prior stream's cancelled read must not change
				// or consume frames from the new watcher.
				if err := h.Driver.SendEnter(); err != nil {
					t.Fatal(err)
				}
				if err := h.Driver.WaitForText(waitTimeout, "replayed setup line"); err != nil {
					t.Fatal(err)
				}
			}
			if err := h.Driver.WaitForText(waitTimeout, "live setup line"); err != nil {
				t.Fatal(err)
			}
			settle()
			if err := h.Driver.WaitForText(waitTimeout, "Initial implementation"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
