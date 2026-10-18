package tuitest_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/fixtures"

	"github.com/recurser/boss/internal/tuitest"
	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestTUI_HomeView_ShowsSessions(t *testing.T) {
	h := tuitest.New(t,
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
	h := tuitest.New(t,
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
	h := tuitest.New(t,
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
	h := tuitest.New(t,
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
	h := tuitest.New(t,
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
	h := tuitest.New(t,
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
// trip through a real terminal: the unit tests in internal/views stub the client,
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

	return tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
	)
}

// TestTUI_HomeView_AttentionBoard drives the real executable through a PTY.
func TestTUI_HomeView_AttentionBoard(t *testing.T) {
	for _, width := range []int{80, 120} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			h := tuitest.New(t, tuitest.WithRepos(testRepos()...), tuitest.WithSessions(fixtures.AttentionSessions()...), tuitest.WithTerminalSize(width, 60))
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
	h := tuitest.New(t, tuitest.WithRepos(testRepos()...), tuitest.WithSessions(sessions...))
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
	h := tuitest.New(t, tuitest.WithRepos(testRepos()...), tuitest.WithSessions(sessions...), tuitest.WithChats(testChats()...))
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
			h := tuitest.New(t, tuitest.WithRepos(testRepos()...), tuitest.WithSessions(sessions...), tuitest.WithChats(testChats()...))
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
	h := tuitest.New(t, tuitest.WithRepos(testRepos()...), tuitest.WithSessions(sessions...), tuitest.WithChats(testChats()...))
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
