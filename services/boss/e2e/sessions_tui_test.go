package e2e_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/adapter/realstackrpc"
	"github.com/recurser/boss/internal/tuitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestJourney_Sessions_AC2_HomeView_ShowsSessions(t *testing.T) {
	for _, count := range []int{1, len(testSessions())} {
		t.Run(fmt.Sprintf("%d sessions", count), func(t *testing.T) {
			sessions := testSessions()[:count]
			h := tuitest.New(t,
				tuitest.WithRepos(testRepos()...),
				tuitest.WithSessions(sessions...),
			)
			for _, session := range sessions {
				waitText(t, h, session.Title)
			}
			waitText(t, h, "[n]ew")
		})
	}
}

func newHomeWithSessions(t *testing.T) *tuitest.Harness {
	t.Helper()

	return tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
	)
}

func TestJourney_Sessions_AC36_AC38_HomeView_SlashFilter(t *testing.T) {
	h := newHomeWithSessions(t)
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendKey('/'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendString("dark"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, fmt.Sprintf("(1 of %d)", len(testSessions()))); err != nil {
		t.Fatal(err)
	}
	if screen := h.Driver.Screen(); strings.Contains(screen, "Fix login bug") || !strings.Contains(screen, "Add dark mode") {
		t.Fatalf("filter did not narrow board:\n%s", screen)
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	waitText(t, h, "[/] edit search")
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Fix login bug"); err != nil {
		t.Fatal(err)
	}
}

// screenLineOf returns the 0-based line index the first occurrence of text is
// rendered on, or -1. Rendered ORDER is the artifact the ticket is about, and
// comparing line indices is the only way to read it off a terminal screen —
// asserting both titles are merely present would pass however they are stacked.
func screenLineOf(screen, text string) int {
	for i, line := range strings.Split(screen, "\n") {
		if strings.Contains(line, text) {
			return i
		}
	}
	return -1
}

func waitText(t *testing.T, h *tuitest.Harness, text string) {
	t.Helper()
	if err := h.Driver.WaitForText(waitTimeout, text); err != nil {
		t.Fatalf("waiting for %q: %v\n%s", text, err, h.Driver.Screen())
	}
}

func sendKeys(t *testing.T, h *tuitest.Harness, keys ...byte) {
	t.Helper()
	for _, k := range keys {
		if err := h.Driver.SendKey(k); err != nil {
			t.Fatal(err)
		}
	}
}

// selectTUINamedRow targets the displayed identity rather than relying on a
// database insertion order or a screen's status-based initial cursor.
func selectTUINamedRow(t *testing.T, h *tuitest.Harness, name string) {
	t.Helper()
	if err := h.Driver.WaitForText(waitTimeout, name); err != nil {
		t.Fatal(err)
	}
	for range 20 {
		screen := h.Driver.Screen()
		selected, target := screenLineOf(screen, "❯"), screenLineOf(screen, name)
		if selected == target && target >= 0 {
			return
		}
		if selected < 0 || target < 0 {
			t.Fatalf("cannot select %q:\n%s", name, screen)
		}
		key := "down"
		if target < selected {
			key = "up"
		}
		if err := h.Driver.SendNamedKey(key); err != nil {
			t.Fatal(err)
		}
		if err := h.Driver.WaitFor(waitTimeout, func(next string) bool { return screenRow(next, "❯") != screenRow(screen, "❯") }); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatalf("cannot select %q:\n%s", name, h.Driver.Screen())
}

func startHomeRename(t *testing.T, h *tuitest.Harness) {
	t.Helper()

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}
	selectTUINamedRow(t, h, "Add dark mode")
	if err := h.Driver.SendKey('r'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "[enter] rename"); err != nil {
		t.Fatalf("[r] did not open the rename editor: %v; screen:\n%s", err, h.Driver.Screen())
	}
}

func assertRenderedAbove(t *testing.T, screen, upper, lower string) {
	t.Helper()
	up, low := screenLineOf(screen, upper), screenLineOf(screen, lower)
	if up < 0 || low < 0 {
		t.Fatalf("expected both %q (line %d) and %q (line %d) on screen:\n%s", upper, up, lower, low, screen)
	}
	if up >= low {
		t.Fatalf("expected %q (line %d) to render above %q (line %d):\n%s", upper, up, lower, low, screen)
	}
}

func chevronIsOn(screen, text string) bool {
	for _, line := range strings.Split(screen, "\n") {
		if strings.Contains(line, text) {
			return strings.Contains(line, "❯")
		}
	}
	return false
}

const pollSettle = 2 * time.Second

func TestJourney_Sessions_AC13_ManualOrder(t *testing.T) {
	t.Run("HomeView_AltUpReordersTheSessionList", func(t *testing.T) {
		h := newHomeWithSessions(t)

		if err := h.Driver.WaitForText(waitTimeout, "Fix login bug"); err != nil {
			t.Fatal(err)
		}
		// Baseline: the daemon's order. Without this the assertion after the chord
		// could pass against a list that was already in that order.
		assertRenderedAbove(t, h.Driver.Screen(), "Add dark mode", "Fix login bug")

		selectTUINamedRow(t, h, "Fix login bug")
		if err := h.Driver.SendNamedKey("alt+up"); err != nil {
			t.Fatal(err)
		}

		if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
			up, low := screenLineOf(screen, "Fix login bug"), screenLineOf(screen, "Add dark mode")
			return up >= 0 && low >= 0 && up < low
		}); err != nil {
			t.Fatalf("alt+up did not reorder the rendered list: %v; screen:\n%s", err, h.Driver.Screen())
		}

		// The chevron follows the session that moved, not the row it vacated.
		screen := h.Driver.Screen()
		if !chevronIsOn(screen, "Fix login bug") {
			t.Fatalf("the cursor did not follow the moved session; screen:\n%s", screen)
		}
		// The reorder is not a one-frame flicker: the daemon now serves the new
		// order, and the list still shows it several poll intervals later.
		time.Sleep(3 * pollSettle)
		assertRenderedAbove(t, h.Driver.Screen(), "Fix login bug", "Add dark mode")
	})
	t.Run("HomeView_AltDownReordersTheSessionList", func(t *testing.T) {
		h := newHomeWithSessions(t)
		// Down only moves ranked rows. Pin the second row and then the first,
		// preserving the fixture order while giving both genuine daemon ranks.
		ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
		defer cancel()
		for _, key := range []string{"sess-bbb-222", "sess-aaa-111"} {
			if _, err := h.Stack.Client().MoveSession(ctx, realstackrpc.Request(&pb.MoveSessionRequest{Id: h.SessionIDs[key], Direction: pb.MoveDirection_MOVE_DIRECTION_UP})); err != nil {
				t.Fatal(err)
			}
		}

		if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
			up, low := screenLineOf(screen, "Add dark mode"), screenLineOf(screen, "Fix login bug")
			return up >= 0 && low >= 0 && up < low
		}); err != nil {
			t.Fatalf("ranked baseline did not reach the rendered list: %v; screen:\n%s", err, h.Driver.Screen())
		}

		selectTUINamedRow(t, h, "Add dark mode")
		if err := h.Driver.SendNamedKey("alt+down"); err != nil {
			t.Fatal(err)
		}

		if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
			up, low := screenLineOf(screen, "Fix login bug"), screenLineOf(screen, "Add dark mode")
			return up >= 0 && low >= 0 && up < low
		}); err != nil {
			t.Fatalf("alt+down did not reorder the rendered list: %v; screen:\n%s", err, h.Driver.Screen())
		}
		if !chevronIsOn(h.Driver.Screen(), "Add dark mode") {
			t.Fatalf("the cursor did not follow the moved session; screen:\n%s", h.Driver.Screen())
		}
	})
	t.Run("HomeView_BareArrowsStillOnlyNavigate", func(t *testing.T) {
		h := newHomeWithSessions(t)

		if err := h.Driver.WaitForText(waitTimeout, "Fix login bug"); err != nil {
			t.Fatal(err)
		}
		initial := newWizardSessions(t, h)
		initialIDs := make([]string, len(initial))
		for i, session := range initial {
			initialIDs[i] = session.GetId()
		}
		for _, key := range []string{"down", "down", "up"} {
			if err := h.Driver.SendNamedKey(key); err != nil {
				t.Fatal(err)
			}
			time.Sleep(200 * time.Millisecond)
		}

		stored := newWizardSessions(t, h)
		storedIDs := make([]string, len(stored))
		for i, session := range stored {
			storedIDs[i] = session.GetId()
		}
		if !slices.Equal(initialIDs, storedIDs) {
			t.Fatalf("navigation changed stored order: before %v, after %v", initialIDs, storedIDs)
		}
	})
	t.Run("HomeView_ListConvergesOnAMultiPositionDaemonMove", func(t *testing.T) {
		h := tuitest.New(t,
			tuitest.WithRepos(testRepos()...),
			tuitest.WithSessions(testSessions()...),
		)

		const third = "Add rate limiting to public API"
		if err := h.Driver.WaitForText(waitTimeout, third); err != nil {
			t.Fatal(err)
		}
		// Baseline: the daemon's own order, so the assertion after the chord cannot
		// pass against a list that was already arranged that way.
		assertRenderedAbove(t, h.Driver.Screen(), "Add dark mode", third)

		for i := 0; i < 6 && !chevronIsOn(h.Driver.Screen(), third); i++ {
			if err := h.Driver.SendNamedKey("down"); err != nil {
				t.Fatal(err)
			}
			time.Sleep(200 * time.Millisecond)
		}
		if !chevronIsOn(h.Driver.Screen(), third) {
			t.Fatalf("the cursor never reached %q; screen:\n%s", third, h.Driver.Screen())
		}

		if err := h.Driver.SendNamedKey("alt+up"); err != nil {
			t.Fatal(err)
		}

		// The daemon lifted it to the TOP, two rows above the adjacent swap the TUI
		// rendered for the first frame.
		if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
			moved, first := screenLineOf(screen, third), screenLineOf(screen, "Add dark mode")
			return moved >= 0 && first >= 0 && moved < first
		}); err != nil {
			t.Fatalf("the list never converged on the daemon's order: %v; screen:\n%s", err, h.Driver.Screen())
		}

		// And it stays there: the override must not re-impose the optimistic guess
		// on every subsequent poll.
		time.Sleep(3 * pollSettle)
		screen := h.Driver.Screen()
		assertRenderedAbove(t, screen, third, "Add dark mode")
		assertRenderedAbove(t, screen, third, "Fix login bug")
	})
}

func TestJourney_Sessions_AC26_HiddenRename(t *testing.T) {
	t.Run("HomeView_HiddenRenameCommitsTheNewTitle", func(t *testing.T) {
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

		if err := h.Driver.WaitFor(waitTimeout, func(string) bool {
			for _, session := range newWizardSessions(t, h) {
				if session.GetId() == h.SessionIDs["sess-aaa-111"] && session.GetTitle() == "Add dark mode renamed" {
					return true
				}
			}
			return false
		}); err != nil {
			t.Fatal(err)
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
		for _, s := range newWizardSessions(t, h) {
			if s.GetId() == h.SessionIDs["sess-aaa-111"] && s.Title != "Add dark mode renamed" {
				t.Fatalf("daemon session title = %q, want %q", s.Title, "Add dark mode renamed")
			}
		}
	})
	t.Run("HomeView_HiddenRenameSwallowsQuit", func(t *testing.T) {
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
	})
	t.Run("HomeView_HiddenRenameEscapeCancels", func(t *testing.T) {
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
		for _, session := range newWizardSessions(t, h) {
			if session.GetId() == h.SessionIDs["sess-aaa-111"] && session.GetTitle() != "Add dark mode" {
				t.Fatalf("cancel persisted title %q", session.GetTitle())
			}
		}
		screen := h.Driver.Screen()
		if !strings.Contains(screen, "Add dark mode") {
			t.Fatalf("original title missing after cancel; screen:\n%s", screen)
		}
		if strings.Contains(screen, "Add dark mode nope") {
			t.Fatalf("abandoned edit survived the cancel; screen:\n%s", screen)
		}
	})
}
