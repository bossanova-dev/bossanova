package tuitest_test

import (
	"strings"
	"testing"

	"github.com/recurser/boss/internal/tuitest"
)

func TestTUI_NewSessionView_RepoSelect(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testMultiRepos()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "no active sessions"); err != nil {
		t.Fatal(err)
	}

	// Press 'n' for new session.
	if err := h.Driver.SendKey('n'); err != nil {
		t.Fatal(err)
	}

	// With 2 repos, it should show "Select a repository".
	if err := h.Driver.WaitForText(waitTimeout, "Select a repository"); err != nil {
		t.Fatalf("expected repo select; screen:\n%s", h.Driver.Screen())
	}

	screen := h.Driver.Screen()
	if !strings.Contains(screen, "my-app") {
		t.Fatalf("expected 'my-app' in repo select; screen:\n%s", screen)
	}
	if !strings.Contains(screen, "my-api") {
		t.Fatalf("expected 'my-api' in repo select; screen:\n%s", screen)
	}
}

func TestTUI_NewSessionView_TypeSelect(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testMultiRepos()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "no active sessions"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('n'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Select a repository"); err != nil {
		t.Fatal(err)
	}

	// Select first repo.
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	// Should show session type options.
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Create a new PR") ||
			strings.Contains(screen, "Quick chat")
	}); err != nil {
		t.Fatalf("expected type select; screen:\n%s", h.Driver.Screen())
	}
}

func TestTUI_NewSessionView_SingleRepoSkipsSelect(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...), // Only 1 repo.
	)

	if err := h.Driver.WaitForText(waitTimeout, "no active sessions"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('n'); err != nil {
		t.Fatal(err)
	}

	// With only 1 repo, should skip repo select and go directly to type select.
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Create a new PR") ||
			strings.Contains(screen, "Quick chat") ||
			strings.Contains(screen, "Starting a new session")
	}); err != nil {
		t.Fatalf("expected type select (skipped repo select); screen:\n%s", h.Driver.Screen())
	}
}

func TestTUI_NewSessionView_FormPhase_EscGoesBackToTypeSelect(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...), // Single repo — skips repo select.
	)

	if err := h.Driver.WaitForText(waitTimeout, "no active sessions"); err != nil {
		t.Fatal(err)
	}

	// Press 'n' for new session.
	if err := h.Driver.SendKey('n'); err != nil {
		t.Fatal(err)
	}

	// Single repo skips repo select — should see type select.
	if err := h.Driver.WaitForText(waitTimeout, "Create a new PR"); err != nil {
		t.Fatalf("expected type select; screen:\n%s", h.Driver.Screen())
	}

	// Select "Create a new PR" (first option, already highlighted).
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	// Should be on the form phase with "Session title".
	if err := h.Driver.WaitForText(waitTimeout, "Session title"); err != nil {
		t.Fatalf("expected form phase; screen:\n%s", h.Driver.Screen())
	}

	// Press esc — should go back to type select, not home.
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}

	// Should see type select options again.
	if err := h.Driver.WaitForText(waitTimeout, "Create a new PR"); err != nil {
		t.Fatalf("expected type select after esc from form; screen:\n%s", h.Driver.Screen())
	}

	// Should NOT be on home screen.
	screen := h.Driver.Screen()
	if strings.Contains(screen, "no active sessions") {
		t.Fatalf("should not have returned to home; screen:\n%s", screen)
	}
}

func TestTUI_NewSessionView_Cancel(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testMultiRepos()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "no active sessions"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('n'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Select a repository"); err != nil {
		t.Fatal(err)
	}

	// Press esc to cancel.
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitForText(waitTimeout, "no active sessions"); err != nil {
		t.Fatalf("expected home view after cancel; screen:\n%s", h.Driver.Screen())
	}
}
