package pty

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

// wheelSeq is the expected SGR 1006 wheel report, spelled out independently of
// the production helper so a mutation to wheelEvent's format cannot silently
// agree with the test that checks it.
func wheelSeq(button, cx, cy int) string {
	return "\x1b[<" + itoa(button) + ";" + itoa(cx) + ";" + itoa(cy) + "M"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// TestExpandScrollChords_UpEmitsWheelUpTicks verifies Shift+PageUp becomes
// exactly ticksPerScrollChord wheel-UP reports at the requested point.
func TestExpandScrollChords_UpEmitsWheelUpTicks(t *testing.T) {
	got := expandScrollChords([]byte("\x1b[5;2~"), 107, 29)

	want := strings.Repeat(wheelSeq(btnWheelUp, 107, 29), ticksPerScrollChord)
	if string(got) != want {
		t.Fatalf("Shift+PageUp expansion:\n got %q\nwant %q", got, want)
	}
	if n := bytes.Count(got, []byte("M")); n != ticksPerScrollChord {
		t.Errorf("expected %d wheel reports, got %d", ticksPerScrollChord, n)
	}
	// Literal 64, not btnWheelUp: the assertion above builds its expectation from
	// the same constant it is checking, so on its own it would still pass if the
	// two button codes were swapped. This pins the chord-to-button MAPPING to the
	// xterm protocol number rather than to whatever the constant currently holds.
	if !bytes.Contains(got, []byte("\x1b[<64;")) {
		t.Errorf("Shift+PageUp must emit xterm wheel-up (button 64); got %q", got)
	}
}

// TestScrollChordBytesMatchTerminfo pins the intercepted sequences to the
// terminal's own definition of these keys, verified against `infocmp -x
// xterm-ghostty`. If these drift the feature silently stops intercepting
// anything, which no behavioural test above would notice — they all feed the
// chord bytes in by hand.
func TestScrollChordBytesMatchTerminfo(t *testing.T) {
	wantUp := []string{"\x1b[5;2~", "\x1b[1;2A"}   // kPRV, kUP
	wantDown := []string{"\x1b[6;2~", "\x1b[1;2B"} // kNXT, kDN

	var gotUp, gotDown []string
	for _, c := range scrollChordsUp {
		gotUp = append(gotUp, string(c))
	}
	for _, c := range scrollChordsDown {
		gotDown = append(gotDown, string(c))
	}
	if !slices.Equal(gotUp, wantUp) {
		t.Errorf("scrollChordsUp = %q, want terminfo kPRV+kUP %q", gotUp, wantUp)
	}
	if !slices.Equal(gotDown, wantDown) {
		t.Errorf("scrollChordsDown = %q, want terminfo kNXT+kDN %q", gotDown, wantDown)
	}
}

// TestExpandScrollChords_ShiftArrows covers the ergonomic spelling — the one
// actually reachable on a laptop, where PageUp only exists as Fn+Arrow — and
// asserts it produces the identical expansion to the page-key spelling, so the
// two can never drift into scrolling by different amounts.
func TestExpandScrollChords_ShiftArrows(t *testing.T) {
	up := expandScrollChords([]byte("\x1b[1;2A"), 7, 9)
	if want := strings.Repeat(wheelSeq(btnWheelUp, 7, 9), ticksPerScrollChord); string(up) != want {
		t.Errorf("Shift+Up:\n got %q\nwant %q", up, want)
	}
	down := expandScrollChords([]byte("\x1b[1;2B"), 7, 9)
	if want := strings.Repeat(wheelSeq(btnWheelDown, 7, 9), ticksPerScrollChord); string(down) != want {
		t.Errorf("Shift+Down:\n got %q\nwant %q", down, want)
	}
	if string(up) != string(expandScrollChords([]byte("\x1b[5;2~"), 7, 9)) {
		t.Error("Shift+Up and Shift+PageUp must expand identically")
	}
	if string(down) != string(expandScrollChords([]byte("\x1b[6;2~"), 7, 9)) {
		t.Error("Shift+Down and Shift+PageDown must expand identically")
	}
}

// TestExpandScrollChords_BareArrowsUntouched is the negative that keeps the new
// binding honest. Bare Up/Down are the agents' history and navigation keys;
// intercepting them would break input in a way that is very hard to attribute
// back to a scroll feature.
func TestExpandScrollChords_BareArrowsUntouched(t *testing.T) {
	for _, in := range []string{"\x1b[A", "\x1b[B", "\x1bOA", "\x1bOB"} {
		if got := expandScrollChords([]byte(in), 5, 5); string(got) != in {
			t.Errorf("bare arrow %q was rewritten to %q; it belongs to the agent", in, got)
		}
	}
	// Other modifiers on the arrows are not ours either — only Shift (mod 2).
	for _, in := range []string{"\x1b[1;5A", "\x1b[1;3B", "\x1b[1;6A"} {
		if got := expandScrollChords([]byte(in), 5, 5); string(got) != in {
			t.Errorf("modified arrow %q was rewritten to %q; only Shift is intercepted", in, got)
		}
	}
}

// TestExpandScrollChords_DownEmitsWheelDownTicks is the mirror, and pins the
// button code apart from wheel-up so a swapped constant is caught.
func TestExpandScrollChords_DownEmitsWheelDownTicks(t *testing.T) {
	got := expandScrollChords([]byte("\x1b[6;2~"), 10, 20)

	want := strings.Repeat(wheelSeq(btnWheelDown, 10, 20), ticksPerScrollChord)
	if string(got) != want {
		t.Fatalf("Shift+PageDown expansion:\n got %q\nwant %q", got, want)
	}
	if bytes.Contains(got, []byte("<64;")) {
		t.Error("Shift+PageDown emitted a wheel-UP button code")
	}
}

// TestExpandScrollChords_BarePageKeysUntouched is the important negative: plain
// PageUp/PageDown belong to the agent's own paging, and intercepting them would
// break it. Only the Shift variants are ours.
func TestExpandScrollChords_BarePageKeysUntouched(t *testing.T) {
	for _, in := range []string{"\x1b[5~", "\x1b[6~"} {
		got := expandScrollChords([]byte(in), 5, 5)
		if string(got) != in {
			t.Errorf("bare page key %q was rewritten to %q; it belongs to the agent", in, got)
		}
	}
}

// TestExpandScrollChords_PassesOtherInputThrough covers the common case — every
// ordinary keystroke — and the surrounding-bytes case, so an expansion cannot
// eat neighbouring input.
func TestExpandScrollChords_PassesOtherInputThrough(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain text", "hello world", "hello world"},
		{"empty", "", ""},
		{"arrow key", "\x1b[A", "\x1b[A"},
		{"bracketed paste", "\x1b[200~body\x1b[201~", "\x1b[200~body\x1b[201~"},
		{"kitty ctrl-x", "\x1b[120;5u", "\x1b[120;5u"},
		{
			"chord between text",
			"ab\x1b[5;2~cd",
			"ab" + strings.Repeat(wheelSeq(btnWheelUp, 4, 3), ticksPerScrollChord) + "cd",
		},
		{
			"two chords in one chunk",
			"\x1b[5;2~\x1b[6;2~",
			strings.Repeat(wheelSeq(btnWheelUp, 4, 3), ticksPerScrollChord) +
				strings.Repeat(wheelSeq(btnWheelDown, 4, 3), ticksPerScrollChord),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := expandScrollChords([]byte(tc.in), 4, 3)
			if string(got) != tc.want {
				t.Errorf("expandScrollChords(%q):\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestExpandScrollChords_SplitChordFailsOpen pins the documented tradeoff: with
// no carry-over state, a chord split across two reads is not recognised and each
// fragment passes through untouched. Fail-open is deliberate — holding a partial
// escape back is how an input path starts swallowing keystrokes.
func TestExpandScrollChords_SplitChordFailsOpen(t *testing.T) {
	head, tail := "\x1b[5", ";2~"

	if got := expandScrollChords([]byte(head), 4, 3); string(got) != head {
		t.Errorf("chord head was rewritten: got %q, want %q", got, head)
	}
	if got := expandScrollChords([]byte(tail), 4, 3); string(got) != tail {
		t.Errorf("chord tail was rewritten: got %q, want %q", got, tail)
	}
}

// TestCentrePoint clamps degenerate sizes into the pane. An out-of-bounds
// coordinate is not cosmetic: tmux routes a mouse report to a pane BY its
// coordinates, so a zero would be dropped or land on the wrong pane.
func TestCentrePoint(t *testing.T) {
	tests := []struct {
		name           string
		rows, cols     int
		wantCx, wantCy int
	}{
		{"typical terminal", 59, 215, 107, 29},
		{"unknown size", 0, 0, 1, 1},
		{"one cell", 1, 1, 1, 1},
		{"negative", -5, -5, 1, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cx, cy := centrePoint(tc.rows, tc.cols)
			if cx != tc.wantCx || cy != tc.wantCy {
				t.Errorf("centrePoint(%d, %d) = (%d, %d), want (%d, %d)",
					tc.rows, tc.cols, cx, cy, tc.wantCx, tc.wantCy)
			}
			if cx < 1 || cy < 1 {
				t.Errorf("centrePoint returned an out-of-pane coordinate (%d, %d)", cx, cy)
			}
		})
	}
}

// TestWheelEventMatchesWebEncoding pins the byte format to the one
// services/web/src/lib/touchScroll.ts already emits (`\x1b[<${button};${cx};${cy}M`).
// The two paths feed the same agents through the same tmux, so they must not
// drift into two spellings of the same event.
func TestWheelEventMatchesWebEncoding(t *testing.T) {
	if got, want := string(wheelEvent(64, 12, 34)), "\x1b[<64;12;34M"; got != want {
		t.Errorf("wheelEvent encoding = %q, want the touchScroll.ts form %q", got, want)
	}
	if got, want := string(wheelEvent(65, 1, 1)), "\x1b[<65;1;1M"; got != want {
		t.Errorf("wheelEvent encoding = %q, want %q", got, want)
	}
}
