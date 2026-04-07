package tuitest_test

import (
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/tuitest"
)

func TestTUI_ViewNavigation(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Bossanova"); err != nil {
		t.Fatal(err)
	}

	// Settings: s → esc.
	if err := h.Driver.SendKey('s'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Settings"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	// Trash: t → esc (title now in banner, not inline).
	if err := h.Driver.SendKey('t'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Archived Sessions"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	// Repos: r → esc (q no longer works on sub-screens).
	if err := h.Driver.SendKey('r'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "PATH"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}
}

func TestTUI_NavigationRoundTrip_AllViews(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Bossanova"); err != nil {
		t.Fatal(err)
	}

	// Settings: s → esc.
	if err := h.Driver.SendKey('s'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Settings"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	// Trash: t → esc (title now in banner).
	if err := h.Driver.SendKey('t'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Archived Sessions"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	// Repos: r → esc (q no longer works on sub-screens).
	if err := h.Driver.SendKey('r'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "PATH"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	// Autopilot: p → esc.
	if err := h.Driver.SendKey('p'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "No workflows") ||
			strings.Contains(screen, "Autopilot") ||
			strings.Contains(screen, "Error")
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}
}

// chevronOnLine returns true if any screen line contains both the chevron ❯
// and the given text, indicating the cursor is on that row.
func chevronOnLine(screen, text string) bool {
	for _, line := range strings.Split(screen, "\n") {
		if strings.Contains(line, "❯") && strings.Contains(line, text) {
			return true
		}
	}
	return false
}

func TestTUI_HomeCursorPreserved_AfterSettings(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Fix login bug"); err != nil {
		t.Fatal(err)
	}

	// Move cursor to second session.
	if err := h.Driver.SendKey('j'); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	// Navigate to settings and back.
	if err := h.Driver.SendKey('s'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Settings"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Fix login bug"); err != nil {
		t.Fatal(err)
	}

	// The chevron should be on the "Fix login bug" row, not "Add dark mode".
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return chevronOnLine(screen, "Fix login bug")
	}); err != nil {
		t.Fatalf("cursor not preserved on 'Fix login bug' after settings; screen:\n%s", h.Driver.Screen())
	}
}

func TestTUI_HomeCursorPreserved_AfterTrash(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Fix login bug"); err != nil {
		t.Fatal(err)
	}

	// Move cursor to second session.
	if err := h.Driver.SendKey('j'); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	// Navigate to trash and back.
	if err := h.Driver.SendKey('t'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Archived Sessions"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Fix login bug"); err != nil {
		t.Fatal(err)
	}

	// The chevron should be on the "Fix login bug" row.
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return chevronOnLine(screen, "Fix login bug")
	}); err != nil {
		t.Fatalf("cursor not preserved on 'Fix login bug' after trash; screen:\n%s", h.Driver.Screen())
	}
}

func TestTUI_HomeCursorPreserved_AfterRepoList(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Fix login bug"); err != nil {
		t.Fatal(err)
	}

	// Move cursor to second session.
	if err := h.Driver.SendKey('j'); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	// Navigate to repo list and back.
	if err := h.Driver.SendKey('r'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "PATH"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Fix login bug"); err != nil {
		t.Fatal(err)
	}

	// The chevron should be on the "Fix login bug" row.
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return chevronOnLine(screen, "Fix login bug")
	}); err != nil {
		t.Fatalf("cursor not preserved on 'Fix login bug' after repo list; screen:\n%s", h.Driver.Screen())
	}
}

func TestTUI_QuitWithQ(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('q'); err != nil {
		t.Fatal(err)
	}

	select {
	case <-h.Driver.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("boss process did not exit after 'q'")
	}
}

func TestTUI_CtrlC_QuitsFromHome(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendCtrlC(); err != nil {
		t.Fatal(err)
	}

	select {
	case <-h.Driver.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("boss process did not exit after Ctrl+C")
	}
}

func TestTUI_CtrlC_QuitsFromSettings(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "No active sessions"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('s'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Settings"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendCtrlC(); err != nil {
		t.Fatal(err)
	}

	select {
	case <-h.Driver.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("boss process did not exit after Ctrl+C from settings")
	}
}

func TestTUI_CtrlC_QuitsFromTrash(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('t'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Archived Sessions"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendCtrlC(); err != nil {
		t.Fatal(err)
	}

	select {
	case <-h.Driver.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("boss process did not exit after Ctrl+C from trash")
	}
}
