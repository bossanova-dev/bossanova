package tuitest_test

import (
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/tuitest"
)

// waitForLoggedInHome blocks until the home screen has finished fetching auth
// status and the action bar shows the "[l]ogout" entry. Every login test
// depends on that transition before pressing `l` — otherwise the key races
// the async authStatusMsg and we either get the login wizard or a silent no-op.
func waitForLoggedInHome(t *testing.T, h *tuitest.Harness) {
	t.Helper()
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "[l]ogout"); err != nil {
		t.Fatalf("expected [l]ogout in action bar; screen:\n%s", h.Driver.Screen())
	}
}

// TestTUI_LoginView_DisplaysEmail seeds a logged-in user via the e2e auth
// store override, then asserts the action bar flips to "[l]ogout" and that
// pressing `l` surfaces the email in the logout-confirm modal. This exercises
// the full path Status() → authStatusMsg → HomeModel.loggedInEmail → rendered.
func TestTUI_LoginView_DisplaysEmail(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
		tuitest.WithLoggedInUser("test-user@example.com"),
	)

	waitForLoggedInHome(t, h)

	if h.Driver.ScreenContains("[l]ogin") {
		t.Fatalf("expected [l]ogin to be replaced by [l]ogout; screen:\n%s", h.Driver.Screen())
	}

	// Press `l` to open the logout-confirm modal, which embeds the current
	// email in its prompt.
	if err := h.Driver.SendKey('l'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Log out test-user@example.com?"); err != nil {
		t.Fatalf("expected email in logout confirm prompt; screen:\n%s", h.Driver.Screen())
	}

	// Dismiss the modal so the test's t.Cleanup path doesn't race with a
	// lingering confirm dialog.
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if !strings.Contains(h.Driver.Screen(), "Log out test-user@example.com?") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("logout confirm modal did not dismiss on esc; screen:\n%s", h.Driver.Screen())
}

// TestTUI_LoginView_Logout confirms that pressing `l` → `y` while logged in
// clears local auth state (action bar flips back to [l]ogin) and fires a
// NotifyAuthChange("logout") RPC so the daemon can drop any cached bearer
// token.
func TestTUI_LoginView_Logout(t *testing.T) {
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
	if err := h.Driver.SendKey('y'); err != nil {
		t.Fatal(err)
	}

	// Action bar flips back to [l]ogin once HomeModel clears loggedIn.
	if err := h.Driver.WaitForText(waitTimeout, "[l]ogin"); err != nil {
		t.Fatalf("expected [l]ogin after logout; screen:\n%s", h.Driver.Screen())
	}

	// NotifyAuthChange("logout") is dispatched asynchronously via a tea.Cmd,
	// so poll until the mock daemon records it.
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if calls := h.Daemon.NotifyAuthChangeCalls(); len(calls) > 0 {
			if got := calls[len(calls)-1]; got != "logout" {
				t.Fatalf("NotifyAuthChange action = %q, want %q", got, "logout")
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("NotifyAuthChange was never called; screen:\n%s", h.Driver.Screen())
}

// TestTUI_LoginView_LogoutCancel asserts that pressing `l` then `n` dismisses
// the confirm modal without calling NotifyAuthChange and without clearing the
// logged-in state — the action bar keeps showing [l]ogout.
func TestTUI_LoginView_LogoutCancel(t *testing.T) {
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
	if err := h.Driver.SendKey('n'); err != nil {
		t.Fatal(err)
	}

	// Modal goes away, action bar still shows [l]ogout (we stayed logged in).
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return !strings.Contains(screen, "Log out test-user@example.com?") &&
			strings.Contains(screen, "[l]ogout")
	}); err != nil {
		t.Fatalf("expected modal dismissed and [l]ogout still shown; screen:\n%s", h.Driver.Screen())
	}

	// Give any stray tea.Cmd time to land, then confirm no RPC fired.
	time.Sleep(300 * time.Millisecond)
	if calls := h.Daemon.NotifyAuthChangeCalls(); len(calls) != 0 {
		t.Fatalf("NotifyAuthChange should not have been called on cancel; got %v", calls)
	}
}
