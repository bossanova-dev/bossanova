package tuitest_test

import (
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/tuitest"
)

func TestTUI_RepoListView(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('r'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "PATH"); err != nil {
		t.Fatal(err)
	}

	screen := h.Driver.Screen()
	if !strings.Contains(screen, "my-app") {
		t.Fatalf("expected repo name 'my-app' on screen:\n%s", screen)
	}

	if err := h.Driver.SendKey('q'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}
}

func TestTUI_RepoListView_EmptyState(t *testing.T) {
	h := tuitest.New(t)

	if err := h.Driver.WaitForText(waitTimeout, "No active sessions"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('r'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "No repositories registered"); err != nil {
		t.Fatal(err)
	}
}

func TestTUI_RepoListView_RemoveConfirm(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "No active sessions"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('r'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "my-app"); err != nil {
		t.Fatal(err)
	}

	// Press 'd' to remove.
	if err := h.Driver.SendKey('d'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Remove"); err != nil {
		t.Fatal(err)
	}

	// Confirm with 'y'.
	if err := h.Driver.SendKey('y'); err != nil {
		t.Fatal(err)
	}

	// Repo should be removed.
	err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "No repositories registered") ||
			!strings.Contains(screen, "my-app")
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify daemon state.
	if len(h.Daemon.Repos()) != 0 {
		t.Fatalf("expected 0 repos after removal, got %d", len(h.Daemon.Repos()))
	}
}

func TestTUI_RepoListView_RemoveCancel(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "No active sessions"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('r'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "my-app"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('d'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Remove"); err != nil {
		t.Fatal(err)
	}

	// Cancel with 'n'.
	if err := h.Driver.SendKey('n'); err != nil {
		t.Fatal(err)
	}

	time.Sleep(500 * time.Millisecond)
	if !h.Driver.ScreenContains("my-app") {
		t.Fatalf("repo disappeared after cancel; screen:\n%s", h.Driver.Screen())
	}
}

func TestTUI_RepoListView_MultipleRepos(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testMultiRepos()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "No active sessions"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('r'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "PATH"); err != nil {
		t.Fatal(err)
	}

	screen := h.Driver.Screen()
	if !strings.Contains(screen, "my-app") {
		t.Fatalf("expected 'my-app' on screen:\n%s", screen)
	}
	if !strings.Contains(screen, "my-api") {
		t.Fatalf("expected 'my-api' on screen:\n%s", screen)
	}
}

func TestTUI_RepoListView_NavigateToSettings(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "No active sessions"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('r'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "my-app"); err != nil {
		t.Fatal(err)
	}

	// Press enter to open repo settings.
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	// Should see repo settings content.
	if err := h.Driver.WaitForText(waitTimeout, "Name"); err != nil {
		t.Fatalf("expected repo settings view; screen:\n%s", h.Driver.Screen())
	}
}
