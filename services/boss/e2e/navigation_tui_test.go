package e2e_test

import (
	"strings"
	"testing"

	"github.com/recurser/boss/internal/tuitest"
)

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

func TestLayout_HomeCursorPreserved_AfterSettings(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow TUI test in -short; run make test-boss for coverage")
	}
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
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool { return chevronOnLine(screen, "Fix login bug") }); err != nil {
		t.Fatal(err)
	}

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
