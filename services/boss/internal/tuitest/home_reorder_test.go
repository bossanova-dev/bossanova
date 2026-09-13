// BOS-1231: PTY coverage of the hidden alt+up / alt+down reorder chords. The
// in-process Update tests in services/boss/internal/views prove the model
// reorders; only a real boss process driven through a real terminal can prove
// the chord survives the input parser, reaches the home view, and changes what
// is actually on screen.

package tuitest_test

import (
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/tuitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

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

// TestTUI_HomeView_AltUpReordersTheSessionList is AC10: the real binary, the
// real chord, and a rendered order that actually changed.
func TestTUI_HomeView_AltUpReordersTheSessionList(t *testing.T) {
	h := newHomeWithSessions(t)

	if err := h.Driver.WaitForText(waitTimeout, "Fix login bug"); err != nil {
		t.Fatal(err)
	}
	// Baseline: the daemon's order. Without this the assertion after the chord
	// could pass against a list that was already in that order.
	assertRenderedAbove(t, h.Driver.Screen(), "Add dark mode", "Fix login bug")

	// Put the cursor on the second session, then move it up.
	if err := h.Driver.SendNamedKey("down"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return chevronIsOn(screen, "Fix login bug")
	}); err != nil {
		t.Fatalf("the cursor never reached 'Fix login bug': %v; screen:\n%s", err, h.Driver.Screen())
	}
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
	calls := h.Daemon.MoveSessionCalls()
	if len(calls) != 1 {
		t.Fatalf("the chord issued %d MoveSession calls, want exactly 1", len(calls))
	}
	if got := calls[0].GetId(); got != "sess-bbb-222" {
		t.Fatalf("MoveSession moved %q, want the selected session sess-bbb-222", got)
	}

	// The reorder is not a one-frame flicker: the daemon now serves the new
	// order, and the list still shows it several poll intervals later.
	time.Sleep(3 * pollSettle)
	assertRenderedAbove(t, h.Driver.Screen(), "Fix login bug", "Add dark mode")
}

// TestTUI_HomeView_AltDownReordersTheSessionList is AC10's other direction.
func TestTUI_HomeView_AltDownReordersTheSessionList(t *testing.T) {
	h := newHomeWithSessions(t)

	if err := h.Driver.WaitForText(waitTimeout, "Fix login bug"); err != nil {
		t.Fatal(err)
	}
	assertRenderedAbove(t, h.Driver.Screen(), "Add dark mode", "Fix login bug")

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
}

// TestTUI_HomeView_BareArrowsStillOnlyNavigate is AC3 at the binary level: the
// unmodified arrow the table owns must move the cursor and nothing else. It is
// also the control that keeps the two tests above honest — if plain "down" were
// reordering, their "the order changed" assertions would prove nothing about
// the chord.
func TestTUI_HomeView_BareArrowsStillOnlyNavigate(t *testing.T) {
	h := newHomeWithSessions(t)

	if err := h.Driver.WaitForText(waitTimeout, "Fix login bug"); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"down", "down", "up"} {
		if err := h.Driver.SendNamedKey(key); err != nil {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
	}

	assertRenderedAbove(t, h.Driver.Screen(), "Add dark mode", "Fix login bug")
	if calls := h.Daemon.MoveSessionCalls(); len(calls) != 0 {
		t.Fatalf("bare navigation issued %d MoveSession calls, want none: %v", len(calls), calls)
	}
}

// TestTUI_HomeView_ReorderChordsAreUnadvertised is AC7, shaped like
// TestTUI_HomeView_HiddenRenameIsUnadvertised: the chords exist, and the action
// bar never grows a hint for them.
func TestTUI_HomeView_ReorderChordsAreUnadvertised(t *testing.T) {
	h := newHomeWithSessions(t)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}
	assertReorderChordsUnadvertised(t, h.Driver.Screen())

	// And still unadvertised after the chord has been used — a hint that only
	// appeared once the feature was exercised would be just as much clutter.
	if err := h.Driver.SendNamedKey("alt+down"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		up, low := screenLineOf(screen, "Fix login bug"), screenLineOf(screen, "Add dark mode")
		return up >= 0 && low >= 0 && up < low
	}); err != nil {
		t.Fatalf("the chord never took effect, so this test would not be checking the post-reorder bar: %v; screen:\n%s",
			err, h.Driver.Screen())
	}
	assertReorderChordsUnadvertised(t, h.Driver.Screen())
}

func assertReorderChordsUnadvertised(t *testing.T, screen string) {
	t.Helper()
	// Positive control: the keys that ARE advertised are on screen, so a blank
	// or half-drawn frame cannot pass by containing nothing at all.
	for _, advertised := range []string{"[n]ew session", "[enter] select", "[s]ettings", "[q]uit"} {
		if !strings.Contains(screen, advertised) {
			t.Fatalf("expected the action bar to advertise %q; screen:\n%s", advertised, screen)
		}
	}
	for _, hidden := range []string{"alt+", "[alt]", "alt+up", "alt+down"} {
		if strings.Contains(screen, hidden) {
			t.Fatalf("the reorder chords must stay hidden, but %q is on screen:\n%s", hidden, screen)
		}
	}
}

// pollSettle is Home's session-poll interval. Waiting a few of them is how a
// test distinguishes a reorder that stuck from one the next poll undid.
const pollSettle = 2 * time.Second

// chevronIsOn reports whether the cursor chevron is rendered on the line
// carrying text.
func chevronIsOn(screen, text string) bool {
	for _, line := range strings.Split(screen, "\n") {
		if strings.Contains(line, text) {
			return strings.Contains(line, "❯")
		}
	}
	return false
}

// TestRankedBlockMoveBehaviourRisesPastUnrankedRows pins the mock rule the PTY
// test below depends on. Without this the PTY case could pass against a
// behaviour that quietly degraded to an adjacent swap, which is exactly the
// blind spot it exists to close.
func TestRankedBlockMoveBehaviourRisesPastUnrankedRows(t *testing.T) {
	sessions := []*pb.Session{{Id: "a"}, {Id: "b"}, {Id: "c"}, {Id: "d"}}
	ids := func(got []*pb.Session) string {
		out := make([]string, len(got))
		for i, s := range got {
			out[i] = s.GetId()
		}
		return strings.Join(out, ",")
	}

	move := tuitest.RankedBlockMoveBehaviour()

	// Nothing is ranked, so the third row does NOT swap with the second: it
	// joins the end of the (empty) ranked block and lands on top.
	got, moved := move(sessions, 2, true)
	if !moved {
		t.Fatal("moving the third row up reported a no-op")
	}
	if ids(got) != "c,a,b,d" {
		t.Fatalf("order = %s, want c,a,b,d; a rise of two is the whole point — an adjacent swap "+
			"would give a,c,b,d and the TUI's optimistic guess would be right by construction", ids(got))
	}

	// The row directly below the block rises by exactly one, as the daemon
	// documents ("From the first or second unranked row that is exactly one
	// position").
	got, moved = move(got, 1, true)
	if !moved || ids(got) != "a,c,b,d" {
		t.Fatalf("order = %s (moved=%v), want a,c,b,d", ids(got), moved)
	}

	// An unranked row has no rank to clear, so moving it down is the daemon's
	// successful no-op rather than an error.
	if _, moved := move(got, 3, false); moved {
		t.Fatal("moving an unranked row down reported a move; the ordering model cannot express it")
	}
}

// TestTUI_HomeView_ListConvergesOnAMultiPositionDaemonMove is the PTY half of
// the same guarantee: the real binary, the real chord, and a daemon that settles
// somewhere the TUI did not guess. The TUI paints an adjacent swap optimistically
// and the daemon answers is_moved=true from two rows higher — the board must end
// up showing the DAEMON's order, and keep showing it across later polls. An
// override released only on exact agreement pins the wrong order here forever.
func TestTUI_HomeView_ListConvergesOnAMultiPositionDaemonMove(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
		tuitest.WithMoveSessionBehaviour(tuitest.RankedBlockMoveBehaviour()),
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
}
