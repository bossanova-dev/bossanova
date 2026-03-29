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
	if err := h.Driver.WaitForText(waitTimeout, "[n]ew session"); err != nil {
		t.Fatal(err)
	}
}

func TestTUI_HomeView_EmptyState(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "No active sessions"); err != nil {
		t.Fatal(err)
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

func TestTUI_HomeView_ArchiveSession(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('a'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Archive"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendKey('y'); err != nil {
		t.Fatal(err)
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

func TestTUI_HomeView_ArchiveCancel(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('a'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Archive"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendKey('n'); err != nil {
		t.Fatal(err)
	}

	time.Sleep(500 * time.Millisecond)
	if !h.Driver.ScreenContains("Add dark mode") {
		t.Fatalf("session disappeared after cancel; screen:\n%s", h.Driver.Screen())
	}
}

func TestTUI_HomeView_ArchiveSecondSession(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	// Move down to second session, then archive.
	if err := h.Driver.SendKey('j'); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	if err := h.Driver.SendKey('a'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Archive"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendKey('y'); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitForNoText(waitTimeout, "Fix login bug"); err != nil {
		t.Fatal(err)
	}

	// First session should still be visible.
	if !h.Driver.ScreenContains("Add dark mode") {
		t.Fatalf("first session disappeared; screen:\n%s", h.Driver.Screen())
	}
}

func TestTUI_HomeView_ArrowKeys(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	// Send down arrow (ESC [ B) and up arrow (ESC [ A).
	if err := h.Driver.SendString("\x1b[B"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	if err := h.Driver.SendString("\x1b[A"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	screen := h.Driver.Screen()
	if !strings.Contains(screen, "Add dark mode") {
		t.Fatalf("expected 'Add dark mode' on screen:\n%s", screen)
	}
	if !strings.Contains(screen, "Fix login bug") {
		t.Fatalf("expected 'Fix login bug' on screen:\n%s", screen)
	}
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
	// With sessions present, the action bar should show archive and new session options.
	if !strings.Contains(screen, "[n]ew session") {
		t.Fatalf("expected '[n]ew session' in action bar; screen:\n%s", screen)
	}
	if !strings.Contains(screen, "[a]rchive") {
		t.Fatalf("expected '[a]rchive' in action bar; screen:\n%s", screen)
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
	if !strings.Contains(screen, "[n]ew session") {
		t.Fatalf("expected action bar with single session; screen:\n%s", screen)
	}
}

func TestTUI_HomeView_JKNavigation(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('j'); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	if err := h.Driver.SendKey('k'); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	screen := h.Driver.Screen()
	if !strings.Contains(screen, "Add dark mode") {
		t.Fatalf("expected 'Add dark mode' on screen:\n%s", screen)
	}
	if !strings.Contains(screen, "Fix login bug") {
		t.Fatalf("expected 'Fix login bug' on screen:\n%s", screen)
	}
}
