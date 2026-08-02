package termreset

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// TestMouseResetContainsEveryDisableSequence pins the exact DECRST set, in
// order. The core four (?1000/?1002/?1003/?1006) are what tmux `mouse on`
// enables; the defensive extras (?1005 legacy UTF-8, ?1015 urxvt, ?9 X10)
// cover exotic terminal encodings. See BOS-499 / BOS-650.
func TestMouseResetContainsEveryDisableSequence(t *testing.T) {
	wants := []string{
		"\x1b[?1000l", // ResetModeMouseNormal (core)
		"\x1b[?1002l", // ResetModeMouseButtonEvent (core)
		"\x1b[?1003l", // ResetModeMouseAnyEvent (core)
		"\x1b[?1006l", // ResetModeMouseExtSgr (core)
		"\x1b[?1005l", // legacy UTF-8 (defensive)
		"\x1b[?1015l", // urxvt (defensive)
		"\x1b[?9l",    // ResetModeMouseX10 (defensive)
	}

	offset := 0
	for _, want := range wants {
		idx := strings.Index(MouseReset[offset:], want)
		if idx < 0 {
			t.Fatalf("MouseReset missing %q (or out of order) at/after offset %d; got %q", want, offset, MouseReset)
		}
		offset += idx + len(want)
	}
	if offset != len(MouseReset) {
		t.Errorf("MouseReset has %d unexpected trailing bytes: %q", len(MouseReset)-offset, MouseReset[offset:])
	}
}

// TestWriteMouseResetWritesExactlyMouseReset guards the writer against
// drifting from the constant callers assert on.
func TestWriteMouseResetWritesExactlyMouseReset(t *testing.T) {
	var buf bytes.Buffer
	WriteMouseReset(&buf)
	if got := buf.String(); got != MouseReset {
		t.Errorf("WriteMouseReset wrote %q, want %q", got, MouseReset)
	}
}

// TestAbnormalExitResetMatchesBubbleTeaTeardown pins the CLI-layer set:
// everything MouseReset disables, plus each input-reporting mode Bubble Tea's
// renderer enables on start and undoes only in its clean close() — which a
// SIGHUP skips. Stranded, each injects characters: ?1004 types a literal
// "I"/"O" on focus change, ?2004 wraps pastes in "200~"…"201~", and
// modifyOtherKeys/Kitty turn keypresses into CSI-u escape codes.
func TestAbnormalExitResetMatchesBubbleTeaTeardown(t *testing.T) {
	const (
		focusOff  = "\x1b[?1004l"
		pasteOff  = "\x1b[?2004l"
		otherKeys = "\x1b[>4m"
	)
	// The raw literal must stay exactly what Bubble Tea's close() emits.
	if want := ansi.KittyKeyboard(0, 1); resetKittyKeyboard != want {
		t.Errorf("resetKittyKeyboard = %q, want ansi.KittyKeyboard(0, 1) = %q", resetKittyKeyboard, want)
	}

	want := MouseReset + focusOff + pasteOff + otherKeys + resetKittyKeyboard
	if AbnormalExitReset != want {
		t.Errorf("AbnormalExitReset = %q, want %q", AbnormalExitReset, want)
	}
	var buf bytes.Buffer
	WriteAbnormalExitReset(&buf)
	if got := buf.String(); got != AbnormalExitReset {
		t.Errorf("WriteAbnormalExitReset wrote %q, want %q", got, AbnormalExitReset)
	}
}

// TestMouseResetStaysMouseOnly is the PTY-boundary tripwire. internal/pty's
// teardown writes MouseReset when the proxy hands the terminal back to boss's
// own TUI, and Bubble Tea owns the non-mouse modes across that window (its
// close() disables them before the proxy runs, its start() re-asserts them
// after) — so MouseReset's job is only what the *foreign pane* enabled.
// Widening it would have the PTY layer fighting Bubble Tea for modes it does
// not own. The pty-side test cannot catch that: it asserts with bytes.Contains,
// which a wider set still passes.
func TestMouseResetStaysMouseOnly(t *testing.T) {
	notMouse := map[string]string{
		"focus reporting":  "\x1b[?1004",
		"bracketed paste":  "\x1b[?2004",
		"modifyOtherKeys":  "\x1b[>4",
		"Kitty keyboard":   "\x1b[=",
		"alternate screen": "\x1b[?1049",
	}
	for name, seq := range notMouse {
		if strings.Contains(MouseReset, seq) {
			t.Errorf("MouseReset must not touch %s (%q); got %q", name, seq, MouseReset)
		}
	}
}

// TestWriteResetIfTerminalSkipsNonTerminal is the guard that keeps
// `boss ls --json | jq` from being corrupted: a pipe is not a terminal, so
// nothing may be written. Both gated writers are covered — the abnormal-exit
// one is what the startup self-heal and the SIGHUP rescue actually call.
func TestWriteResetIfTerminalSkipsNonTerminal(t *testing.T) {
	writers := map[string]func(*os.File) bool{
		"WriteMouseResetIfTerminal":        WriteMouseResetIfTerminal,
		"WriteAbnormalExitResetIfTerminal": WriteAbnormalExitResetIfTerminal,
	}
	for name, write := range writers {
		t.Run(name, func(t *testing.T) {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatalf("os.Pipe: %v", err)
			}
			defer r.Close() //nolint:errcheck // test cleanup
			defer w.Close() //nolint:errcheck // test cleanup

			if write(w) {
				t.Fatalf("%s returned true for a pipe (not a terminal)", name)
			}

			// Nothing may have been written: close the write end and confirm the
			// read end is immediately at EOF.
			if err := w.Close(); err != nil {
				t.Fatalf("close write end: %v", err)
			}
			var got bytes.Buffer
			if _, err := got.ReadFrom(r); err != nil {
				t.Fatalf("read pipe: %v", err)
			}
			if got.Len() != 0 {
				t.Errorf("%s wrote %q to a non-terminal; want nothing", name, got.String())
			}
		})
	}
}

// TestWriteResetIfTerminalHandlesNilFile keeps the helpers safe on the startup
// path, where os.Stdout can be nil in exotic embeddings.
func TestWriteResetIfTerminalHandlesNilFile(t *testing.T) {
	if WriteMouseResetIfTerminal(nil) {
		t.Error("WriteMouseResetIfTerminal(nil) returned true; want false")
	}
	if WriteAbnormalExitResetIfTerminal(nil) {
		t.Error("WriteAbnormalExitResetIfTerminal(nil) returned true; want false")
	}
}
