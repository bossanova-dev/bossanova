package pty

import (
	"os"
	"strconv"

	creackpty "github.com/creack/pty/v2"
)

// Scroll chords and their expansion into SGR 1006 wheel events.
//
// Why this exists: a boss chat pane runs the agent on the terminal's ALTERNATE
// screen, so `#{alternate_on}` is 1 for its whole life. tmux's default root
// binding is
//
//	WheelUpPane if -F "#{||:#{alternate_on},#{pane_in_mode},#{mouse_any_flag}}" \
//	    { send-keys -M } { copy-mode -e }
//
// so alternate_on alone makes tmux ALWAYS forward the wheel to the agent and
// NEVER enter copy-mode — and the pane's `#{history_size}` is 0, because
// alternate-screen output never reaches the scrollback buffer, so copy-mode
// would show an empty buffer even if it could be entered. There is therefore no
// tmux-side scrollback to fall back on: 100% of scrolling in a chat pane is the
// agent consuming forwarded SGR mouse events.
//
// That makes scrolling depend on an end-to-end mouse path — the outer terminal
// must have mouse reporting enabled AND must not be trapping the wheel for its
// own scrollback — which is observably fragile: panes semi-regularly become
// unscrollable until the user detaches and reattaches, with the tmux-side state
// of a broken pane byte-identical to a healthy one.
//
// A keystroke needs none of that. Terminals deliver key escapes unconditionally,
// with no reporting mode to strand and nothing upstream that can swallow them,
// so translating a key chord into the very bytes the agent already understands
// gives a scroll path that cannot break the way the mouse path does. This is the
// same trick services/web/src/lib/touchScroll.ts plays for touch drags, which is
// why the encoding below is deliberately identical to that file's.
const (
	// SGR 1006 wheel button codes (xterm protocol), matching touchScroll.ts.
	btnWheelUp   = 64
	btnWheelDown = 65

	// Wheel events emitted per chord press. 5 matches tmux's own wheel
	// granularity — its copy-mode bindings scroll with `send-keys -X -N 5` — so a
	// chord press moves about as far as a trackpad notch does, rather than
	// inventing a third scroll distance.
	ticksPerScrollChord = 5
)

// The intercepted chords, two spellings each for up and down.
//
// Shift+PageUp/PageDown is the conventional "scroll the terminal, not the app"
// binding (xterm, GNOME Terminal, Konsole). It is kept, but it cannot be the
// only one: a laptop keyboard has no PageUp key, so on the Macs this is used
// from it is really Fn+Shift+Arrow — a three-key contortion for the one action
// you reach for when scrolling is already broken.
//
// Shift+Up/Down is therefore the ergonomic spelling, and the one to reach for.
// It is safe to take: neither agent binds Shift+Arrow (bare Up/Down are their
// history and paging keys, and those are deliberately NOT intercepted), and
// nothing else in this repo binds it either.
//
// The Shift variants specifically, never the bare keys: those belong to the
// agent, and stealing them would break its own navigation.
//
// Byte sequences are the terminal's own, from `infocmp -x xterm-ghostty`:
// kPRV/kNXT for the page keys and kUP/kDN for the arrows. Both the legacy and
// the Kitty keyboard encodings of all four are these same bytes — Kitty keeps
// the legacy form for keys that have one — so one pattern each covers both and
// no separate CSI-u spelling is needed.
var (
	scrollChordsUp = [][]byte{
		[]byte("\x1b[5;2~"), // kPRV — Shift+PageUp
		[]byte("\x1b[1;2A"), // kUP  — Shift+Up
	}
	scrollChordsDown = [][]byte{
		[]byte("\x1b[6;2~"), // kNXT — Shift+PageDown
		[]byte("\x1b[1;2B"), // kDN  — Shift+Down
	}
)

// wheelEvent renders one SGR 1006 wheel report at column cx, row cy (1-based).
func wheelEvent(button, cx, cy int) []byte {
	out := make([]byte, 0, 16)
	out = append(out, 0x1b, '[', '<')
	out = strconv.AppendInt(out, int64(button), 10)
	out = append(out, ';')
	out = strconv.AppendInt(out, int64(cx), 10)
	out = append(out, ';')
	out = strconv.AppendInt(out, int64(cy), 10)
	return append(out, 'M')
}

// expandScrollChords replaces every scroll chord in data with ticksPerScrollChord
// SGR wheel events reported at (cx, cy), leaving all other bytes untouched. It
// returns data unchanged (and does not copy it) when no chord is present, which
// is the overwhelmingly common case — every ordinary keystroke.
//
// cx/cy should be inside the pane: tmux routes a mouse report to a pane by its
// coordinates, so an out-of-bounds position would be delivered to the wrong pane
// or dropped. Callers pass the terminal's centre, which is always in-bounds for
// the single full-window pane a chat session runs.
//
// Deliberately stateless, with no carry-over buffer across reads. A chord split
// across two reads is simply not recognised and passes through to the agent as
// the original keypress — inert, since the agent does not bind Shift+PageUp.
// That fail-open is the point: the alternative, holding a partial escape back to
// wait for its tail, is the exact shape of bug that makes an input path swallow
// keystrokes, and this path is on every byte the user types.
func expandScrollChords(data []byte, cx, cy int) []byte {
	if !containsScrollChord(data) {
		return data
	}
	out := make([]byte, 0, len(data))
	for i := 0; i < len(data); {
		button, width := matchScrollChord(data[i:])
		if width == 0 {
			out = append(out, data[i])
			i++
			continue
		}
		for range ticksPerScrollChord {
			out = append(out, wheelEvent(button, cx, cy)...)
		}
		i += width
	}
	return out
}

// containsScrollChord reports whether data holds at least one chord, so the
// common no-chord chunk avoids allocating a rewritten copy.
func containsScrollChord(data []byte) bool {
	for i := range data {
		if _, width := matchScrollChord(data[i:]); width > 0 {
			return true
		}
	}
	return false
}

// matchScrollChord reports the wheel button a chord at the start of buf maps to
// and the chord's length in bytes, or a zero width when buf does not begin with
// one.
func matchScrollChord(buf []byte) (button, width int) {
	for _, chord := range scrollChordsUp {
		if hasPrefix(buf, chord) {
			return btnWheelUp, len(chord)
		}
	}
	for _, chord := range scrollChordsDown {
		if hasPrefix(buf, chord) {
			return btnWheelDown, len(chord)
		}
	}
	return 0, 0
}

// hasPrefix is bytes.HasPrefix, inlined to keep this file dependency-free and
// allocation-free on the per-keystroke path.
func hasPrefix(buf, prefix []byte) bool {
	if len(buf) < len(prefix) {
		return false
	}
	for i := range prefix {
		if buf[i] != prefix[i] {
			return false
		}
	}
	return true
}

// scrollReportPoint returns the position this attach reports synthetic wheel
// events at: the centre of the real terminal, or (1, 1) when its size cannot be
// read (a non-file stdout in tests, a failed ioctl). Both are inside the pane,
// which is all tmux needs to route the event.
func (c *PTYCommand) scrollReportPoint() (cx, cy int) {
	f, ok := c.stdout.(*os.File)
	if !ok {
		return 1, 1
	}
	rows, cols, err := creackpty.Getsize(f)
	if err != nil {
		return 1, 1
	}
	return centrePoint(rows, cols)
}

// centrePoint returns the 1-based centre of a rows x cols terminal. Degenerate
// or unknown sizes clamp to (1, 1), which is always inside the pane.
func centrePoint(rows, cols int) (cx, cy int) {
	cx, cy = cols/2, rows/2
	if cx < 1 {
		cx = 1
	}
	if cy < 1 {
		cy = 1
	}
	return cx, cy
}
