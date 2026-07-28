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

	for _, key := range []byte{'r', 't', 'c', 'a'} {
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
