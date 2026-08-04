package termreset

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	creackpty "github.com/creack/pty/v2"
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

func TestWriteMouseResetIfTerminalWritesToTerminal(t *testing.T) {
	master, slave, err := creackpty.Open()
	if err != nil {
		t.Fatalf("open pty: %v", err)
	}
	defer master.Close() //nolint:errcheck // test cleanup
	defer slave.Close()  //nolint:errcheck // test cleanup

	if !WriteMouseResetIfTerminal(slave) {
		t.Fatal("WriteMouseResetIfTerminal() = false for a real terminal, want true")
	}

	got := make([]byte, len(MouseReset))
	if err := readFullWithin(master, got, ptyReadTimeout); err != nil {
		t.Fatalf("read reset from pty: %v", err)
	}
	if string(got) != MouseReset {
		t.Errorf("reset written to terminal = %q, want %q", got, MouseReset)
	}
}

// TestReadFullWithinBoundsAStalledRead proves the BOS-698 bound actually
// fires. An io.Pipe with no writer never delivers, standing in for the wedged
// pty master that burned the full 300s Bazel timeout on PR #1816.
func TestReadFullWithinBoundsAStalledRead(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close() //nolint:errcheck // test cleanup

	buf := make([]byte, 4)
	start := time.Now()
	err := readFullWithin(pr, buf, 20*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("readFullWithin returned nil for a reader that never delivers; want a timeout error")
	}
	if !strings.Contains(err.Error(), "stalled") {
		t.Errorf("error %q does not name the stall", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("readFullWithin took %s; want it bounded near 20ms", elapsed)
	}
}

// TestReadFullWithinReturnsDeliveredBytes keeps the bound from breaking the
// happy path: a reader that does deliver must fill the buffer and report nil.
func TestReadFullWithinReturnsDeliveredBytes(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte("abcd"))
		_ = pw.Close()
	}()

	buf := make([]byte, 4)
	if err := readFullWithin(pr, buf, 5*time.Second); err != nil {
		t.Fatalf("readFullWithin: %v", err)
	}
	if string(buf) != "abcd" {
		t.Errorf("readFullWithin filled buf with %q, want %q", buf, "abcd")
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

// ptyReadTimeout bounds the pty master read below. The release matrix can delay
// an unsandboxed PTY transfer past five seconds under whole-graph load, so leave
// room for that scheduling variance while still cutting a true wedge well below
// Bazel's 300s test timeout.
const ptyReadTimeout = 20 * time.Second

// readFullWithin is io.ReadFull with an upper bound (BOS-698). A transient
// runner-level pty anomaly used to wedge the read forever, turning a 0.3s test
// into a 300s Bazel TIMEOUT whose failure text said nothing about the cause.
//
// It deliberately does NOT use (*os.File).SetReadDeadline: that returns
// ErrNoDeadline for any file the runtime poller did not adopt, which would
// leave the read silently unbounded — a fail-open, and the exact failure this
// bound exists to prevent. A goroutine plus select is portable and cannot
// fail open.
//
// The goroutine leaks only on the stall path, where the test is failing and the
// process is about to exit anyway; the channel is buffered so it can never
// block on a receiver that has already timed out.
func readFullWithin(r io.Reader, buf []byte, d time.Duration) error {
	type result struct {
		n   int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		n, err := io.ReadFull(r, buf)
		ch <- result{n: n, err: err}
	}()

	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case res := <-ch:
		return res.err
	case <-timer.C:
		return fmt.Errorf("pty read stalled: %d bytes not delivered within %s", len(buf), d)
	}
}
