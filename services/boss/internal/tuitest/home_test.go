package tuitest_test

import (
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/tuitest"
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

func TestTUI_HomeView_EmptyState(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "no active sessions"); err != nil {
		t.Fatal(err)
	}
}

func TestTUI_HomeView_NoRepoActionBarHidesNewSession(t *testing.T) {
	h := tuitest.New(t)

	// With no repos, the app lands on the Home empty state (no auto-route). The
	// primary action is adding the first repository, not creating a session.
	if err := h.Driver.WaitForText(waitTimeout, "Welcome to Bossanova"); err != nil {
		t.Fatal(err)
	}
	screen := h.Driver.Screen()
	if strings.Contains(screen, "[n]ew session") {
		t.Fatalf("home: should not offer [n]ew session when no repos exist; screen:\n%s", screen)
	}
	for _, kept := range []string{"[enter] add repository", "[q]uit"} {
		if !strings.Contains(screen, kept) {
			t.Fatalf("home: expected action bar to contain %q; screen:\n%s", kept, screen)
		}
	}
}

func TestTUI_HomeView_DataDisplay(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	screen := h.Driver.Screen()
	if !strings.Contains(screen, "my-app") {
		t.Fatalf("expected repo name 'my-app' on screen:\n%s", screen)
	}
	if !strings.Contains(screen, "Bossanova") {
		t.Fatalf("expected 'Bossanova' banner on screen:\n%s", screen)
	}
}

func TestTUI_HomeView_ArrowKeys(t *testing.T) {
	h := newHomeWithSessions(t)

	// Send down arrow (ESC [ B) and up arrow (ESC [ A).
	assertHomeNavigationKeepsSessions(t, h,
		func() error { return h.Driver.SendString("\x1b[B") },
		func() error { return h.Driver.SendString("\x1b[A") },
	)
}

func TestTUI_HomeView_ActionBar(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	screen := h.Driver.Screen()
	// With sessions present, the action bar should show session creation and selection.
	if !strings.Contains(screen, "[n]ew") {
		t.Fatalf("expected '[n]ew' in action bar; screen:\n%s", screen)
	}
	if !strings.Contains(screen, "[enter] select") {
		t.Fatalf("expected '[enter] select' in action bar; screen:\n%s", screen)
	}
}

func TestTUI_HomeView_ActionBarHasNoMovedItems(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "[n]ew"); err != nil {
		t.Fatal(err)
	}

	screen := h.Driver.Screen()
	for _, moved := range []string{"[r]epos", "[c]ron", "[t]rash", "[a]rchive"} {
		if strings.Contains(screen, moved) {
			t.Fatalf("expected action bar not to contain %q; screen:\n%s", moved, screen)
		}
	}
	for _, kept := range []string{"[n]ew", "[s]ettings", "[q]uit"} {
		if !strings.Contains(screen, kept) {
			t.Fatalf("expected action bar to contain %q; screen:\n%s", kept, screen)
		}
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
	// keys below are still the ones that moved to the session detail view. The
	// rename round trip, its cancel path and its hidden-ness are covered by
	// TestTUI_HomeView_HiddenRename* further down this file.
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

func TestTUI_HomeView_JKNavigation(t *testing.T) {
	h := newHomeWithSessions(t)

	assertHomeNavigationKeepsSessions(t, h,
		func() error { return h.Driver.SendKey('j') },
		func() error { return h.Driver.SendKey('k') },
	)
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

// TestTUI_HomeView_HiddenRenameIsUnadvertised pins the "hidden" half of the
// ticket: the shortcut exists but the action bar never grows a [r]ename hint.
func TestTUI_HomeView_HiddenRenameIsUnadvertised(t *testing.T) {
	h := newHomeWithSessions(t)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}
	screen := h.Driver.Screen()
	// Positive control: the keys that ARE advertised are on screen, so a blank or
	// half-drawn frame cannot pass this test by containing nothing at all.
	for _, advertised := range []string{"[n]ew session", "[s]ettings"} {
		if !strings.Contains(screen, advertised) {
			t.Fatalf("expected the action bar to advertise %q; screen:\n%s", advertised, screen)
		}
	}
	if strings.Contains(screen, "[r]ename") {
		t.Fatalf("the rename shortcut must stay hidden; screen:\n%s", screen)
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

func assertHomeNavigationKeepsSessions(t *testing.T, h *tuitest.Harness, navigate ...func() error) {
	t.Helper()

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}
	for _, step := range navigate {
		if err := step(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
	}

	screen := h.Driver.Screen()
	if !strings.Contains(screen, "Add dark mode") {
		t.Fatalf("expected 'Add dark mode' on screen:\n%s", screen)
	}
	if !strings.Contains(screen, "Fix login bug") {
		t.Fatalf("expected 'Fix login bug' on screen:\n%s", screen)
	}
}
