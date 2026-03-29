package tuitest_test

import (
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/tuitest"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestTUI_TrashView(t *testing.T) {
	sessions := testSessions()
	sessions[0].ArchivedAt = timestamppb.Now()

	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(sessions...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Fix login bug"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('t'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Archived Sessions"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}
}

func TestTUI_TrashRestore(t *testing.T) {
	sessions := testSessions()
	sessions[0].ArchivedAt = timestamppb.Now()

	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(sessions...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Fix login bug"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('t'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Archived Sessions"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('r'); err != nil {
		t.Fatal(err)
	}

	err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Trash is empty") ||
			!strings.Contains(screen, "Add dark mode")
	})
	if err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, s := range h.Daemon.Sessions() {
		if s.Id == "sess-aaa-111" {
			found = true
			if s.ArchivedAt != nil {
				t.Fatal("expected session to be restored in daemon")
			}
		}
	}
	if !found {
		t.Fatal("session sess-aaa-111 not found in daemon state")
	}
}

func TestTUI_TrashDelete(t *testing.T) {
	sessions := testSessions()
	sessions[0].ArchivedAt = timestamppb.Now()

	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(sessions...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Fix login bug"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('t'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('d'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Permanently delete"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendKey('y'); err != nil {
		t.Fatal(err)
	}

	err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Trash is empty") ||
			!strings.Contains(screen, "Add dark mode")
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTUI_TrashEmptyState(t *testing.T) {
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
	if err := h.Driver.WaitForText(waitTimeout, "Trash is empty"); err != nil {
		t.Fatal(err)
	}
}

func TestTUI_TrashEmptyAll(t *testing.T) {
	sessions := testSessions()
	sessions[0].ArchivedAt = timestamppb.Now()
	sessions[1].ArchivedAt = timestamppb.Now()

	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(sessions...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "No active sessions"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('t'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Archived Sessions"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('a'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Permanently delete all"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendKey('y'); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitForText(waitTimeout, "Trash is empty"); err != nil {
		t.Fatal(err)
	}

	if len(h.Daemon.Sessions()) != 0 {
		t.Fatalf("expected 0 sessions after empty trash, got %d", len(h.Daemon.Sessions()))
	}
}

func TestTUI_TrashDeleteCancel(t *testing.T) {
	sessions := testSessions()
	sessions[0].ArchivedAt = timestamppb.Now()

	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(sessions...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Fix login bug"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('t'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('d'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Permanently delete"); err != nil {
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

func TestTUI_TrashEmptyAllCancel(t *testing.T) {
	sessions := testSessions()
	sessions[0].ArchivedAt = timestamppb.Now()
	sessions[1].ArchivedAt = timestamppb.Now()

	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(sessions...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "No active sessions"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('t'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Archived Sessions"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	// Press 'a' to empty all, then 'n' to cancel.
	if err := h.Driver.SendKey('a'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Permanently delete all"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendKey('n'); err != nil {
		t.Fatal(err)
	}

	// Sessions should still be visible.
	time.Sleep(500 * time.Millisecond)
	if !h.Driver.ScreenContains("Add dark mode") {
		t.Fatalf("sessions disappeared after cancel; screen:\n%s", h.Driver.Screen())
	}
}

func TestTUI_TrashRestoreAndVerifyHome(t *testing.T) {
	sessions := testSessions()
	sessions[0].ArchivedAt = timestamppb.Now()

	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(sessions...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Fix login bug"); err != nil {
		t.Fatal(err)
	}

	// Navigate to trash.
	if err := h.Driver.SendKey('t'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	// Restore the session.
	if err := h.Driver.SendKey('r'); err != nil {
		t.Fatal(err)
	}
	err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Trash is empty") ||
			!strings.Contains(screen, "Add dark mode")
	})
	if err != nil {
		t.Fatal(err)
	}

	// Go back to home.
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}

	// The restored session should now appear on home.
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatalf("restored session not visible on home; screen:\n%s", h.Driver.Screen())
	}
}
