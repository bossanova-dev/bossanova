package e2e_test

import (
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/tuitest"
)

func TestJourney_ChatPicker_AC1_ChatPickerView_ShowsChats(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
		tuitest.WithChats(testChats()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	// Press enter to open the first session's chat picker.
	selectTUINamedRow(t, h, "Add dark mode")
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	// Should see "Loading chats" or the chat titles.
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Initial implementation") ||
			strings.Contains(screen, "Loading chats")
	}); err != nil {
		t.Fatalf("expected chat picker content; screen:\n%s", h.Driver.Screen())
	}

	// If we see loading, wait for actual chats.
	if h.Driver.ScreenContains("Loading chats") {
		if err := h.Driver.WaitForText(waitTimeout, "Initial implementation"); err != nil {
			t.Fatalf("expected chat title after loading; screen:\n%s", h.Driver.Screen())
		}
	}

	waitText(t, h, "STATUS")
	screen := h.Driver.Screen()
	if !strings.Contains(screen, "Follow-up review") {
		t.Fatalf("expected second chat title on screen:\n%s", screen)
	}
}

func TestJourney_Sessions_AC3_ChatPicker_Archive_ConfirmReturnsHome(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
		tuitest.WithChats(testChats()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	selectTUINamedRow(t, h, "Add dark mode")
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Initial implementation") &&
			strings.Contains(screen, "[a]rchive")
	}); err != nil {
		t.Fatalf("expected chat picker archive action; screen:\n%s", h.Driver.Screen())
	}

	if err := h.Driver.SendKey('a'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Archive this session?"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('y'); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitForText(waitTimeout, "[n]ew"); err != nil {
		t.Fatalf("expected home view after archive; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.WaitForNoText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, s := range storedSessionsIncludingArchived(t, h) {
		if s.Id == h.SessionIDs["sess-aaa-111"] {
			found = true
			if s.ArchivedAt == nil {
				t.Fatal("expected session to be archived in daemon")
			}
		}
	}
	if !found {
		t.Fatal("session sess-aaa-111 not found in daemon state")
	}
	openTrashFromSettings(t, h, "Add dark mode")
}

// screenRow returns the first screen line containing marker, or "".
func screenRow(screen, marker string) string {
	for _, line := range strings.Split(screen, "\n") {
		if strings.Contains(line, marker) {
			return line
		}
	}
	return ""
}

// selectRow moves the cursor down until the selected line names marker.

func TestJourney_ChatPicker_AC8_LegacyActions(t *testing.T) {
	t.Run("ChatPickerView_DeleteConfirm", func(t *testing.T) {
		h := tuitest.New(t,
			tuitest.WithRepos(testRepos()...),
			tuitest.WithSessions(testSessions()...),
			tuitest.WithChats(testChats()...),
		)

		if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
			t.Fatal(err)
		}

		selectTUINamedRow(t, h, "Add dark mode")
		if err := h.Driver.SendEnter(); err != nil {
			t.Fatal(err)
		}

		if err := h.Driver.WaitForText(waitTimeout, "Initial implementation"); err != nil {
			t.Fatal(err)
		}

		// Press 'd' to delete the first chat.
		selectTUINamedRow(t, h, "Initial implementation")
		if err := h.Driver.SendKey('d'); err != nil {
			t.Fatal(err)
		}

		// Wait for the actual confirmation dialog (not the action bar "[d]elete").
		if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
			return strings.Contains(screen, "[y/enter] confirm") &&
				strings.Contains(screen, "Delete")
		}); err != nil {
			t.Fatalf("expected delete confirmation dialog; screen:\n%s", h.Driver.Screen())
		}
		time.Sleep(200 * time.Millisecond)

		// Confirm with 'y'.
		if err := h.Driver.SendKey('y'); err != nil {
			t.Fatal(err)
		}

		// First wait for the confirmation dialog to close.
		if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
			return !strings.Contains(screen, "[y/enter] confirm")
		}); err != nil {
			t.Fatalf("confirmation dialog did not close; screen:\n%s", h.Driver.Screen())
		}

		// Wait for the deleted chat to disappear from the UI.
		if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
			return !strings.Contains(screen, "Initial implementation")
		}); err != nil {
			t.Fatalf("expected 'Initial implementation' to be removed; screen:\n%s", h.Driver.Screen())
		}

		// Verify the remaining chat is still present.
		screen := h.Driver.Screen()
		if !strings.Contains(screen, "Follow-up review") {
			t.Fatalf("expected 'Follow-up review' to remain after deletion; screen:\n%s", screen)
		}
	})
	t.Run("ChatPickerView_DeleteCancel", func(t *testing.T) {
		h := tuitest.New(t,
			tuitest.WithRepos(testRepos()...),
			tuitest.WithSessions(testSessions()...),
			tuitest.WithChats(testChats()...),
		)

		if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
			t.Fatal(err)
		}

		selectTUINamedRow(t, h, "Add dark mode")
		if err := h.Driver.SendEnter(); err != nil {
			t.Fatal(err)
		}

		if err := h.Driver.WaitForText(waitTimeout, "Initial implementation"); err != nil {
			t.Fatal(err)
		}

		selectTUINamedRow(t, h, "Initial implementation")
		if err := h.Driver.SendKey('d'); err != nil {
			t.Fatal(err)
		}
		if err := h.Driver.WaitForText(waitTimeout, "[y/enter] confirm"); err != nil {
			t.Fatal(err)
		}

		// Cancel with 'n'.
		if err := h.Driver.SendKey('n'); err != nil {
			t.Fatal(err)
		}

		time.Sleep(500 * time.Millisecond)
		if !h.Driver.ScreenContains("Initial implementation") {
			t.Fatalf("chat disappeared after cancel; screen:\n%s", h.Driver.Screen())
		}
	})
	t.Run("ChatPicker_DeleteConfirm_CancelKeepsPicker", func(t *testing.T) {
		h := tuitest.New(t,
			tuitest.WithRepos(testRepos()...),
			tuitest.WithSessions(testSessions()...),
			tuitest.WithChats(testChats()...),
		)

		if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
			t.Fatal(err)
		}

		selectTUINamedRow(t, h, "Add dark mode")
		if err := h.Driver.SendEnter(); err != nil {
			t.Fatal(err)
		}

		if err := h.Driver.WaitForText(waitTimeout, "[d]elete"); err != nil {
			t.Fatal(err)
		}

		selectTUINamedRow(t, h, "Initial implementation")
		if err := h.Driver.SendKey('d'); err != nil {
			t.Fatal(err)
		}
		if err := h.Driver.WaitForText(waitTimeout, "[y/enter] confirm"); err != nil {
			t.Fatal(err)
		}

		if err := h.Driver.SendKey('n'); err != nil {
			t.Fatal(err)
		}

		if err := h.Driver.WaitForText(waitTimeout, "[d]elete"); err != nil {
			t.Fatalf("expected delete action after cancel; screen:\n%s", h.Driver.Screen())
		}
	})
	t.Run("ChatPicker_Archive_CancelKeepsPicker", func(t *testing.T) {
		h := tuitest.New(t,
			tuitest.WithRepos(testRepos()...),
			tuitest.WithSessions(testSessions()...),
			tuitest.WithChats(testChats()...),
		)

		if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
			t.Fatal(err)
		}

		selectTUINamedRow(t, h, "Add dark mode")
		if err := h.Driver.SendEnter(); err != nil {
			t.Fatal(err)
		}

		if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
			return strings.Contains(screen, "Initial implementation") &&
				strings.Contains(screen, "[a]rchive")
		}); err != nil {
			t.Fatalf("expected chat picker archive action; screen:\n%s", h.Driver.Screen())
		}

		if err := h.Driver.SendKey('a'); err != nil {
			t.Fatal(err)
		}
		if err := h.Driver.WaitForText(waitTimeout, "Archive this session?"); err != nil {
			t.Fatal(err)
		}

		if err := h.Driver.SendEscape(); err != nil {
			t.Fatal(err)
		}

		if err := h.Driver.WaitForText(waitTimeout, "[a]rchive"); err != nil {
			t.Fatalf("expected archive action after cancel; screen:\n%s", h.Driver.Screen())
		}
	})
	t.Run("ChatPickerView_Back", func(t *testing.T) {
		h := tuitest.New(t,
			tuitest.WithRepos(testRepos()...),
			tuitest.WithSessions(testSessions()...),
			tuitest.WithChats(testChats()...),
		)

		if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
			t.Fatal(err)
		}

		selectTUINamedRow(t, h, "Add dark mode")
		if err := h.Driver.SendEnter(); err != nil {
			t.Fatal(err)
		}

		if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
			return strings.Contains(screen, "Initial implementation") ||
				strings.Contains(screen, "Loading chats")
		}); err != nil {
			t.Fatal(err)
		}

		// Press esc to go back.
		if err := h.Driver.SendEscape(); err != nil {
			t.Fatal(err)
		}

		if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
			t.Fatalf("expected home view after esc; screen:\n%s", h.Driver.Screen())
		}
	})
}
