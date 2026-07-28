package pty

import (
	"bytes"
	"testing"
)

// TestWriteTerminalModeReset verifies that writeTerminalModeReset emits the
// xterm mouse-tracking DECRST disable sequences. This is the deterministic,
// race-enabled regression guard for BOS-499: when boss reclaims the terminal
// after proxying tmux (which enables mouse reporting on `set -g mouse on`),
// it must actively disable those modes or the outer terminal is left stranded
// in mouse-reporting mode, breaking native drag-select.
func TestWriteTerminalModeReset(t *testing.T) {
	var buf bytes.Buffer
	writeTerminalModeReset(&buf)
	got := buf.String()

	// Core set: exactly what tmux `mouse on` enables (?1000/?1002/?1003/?1006).
	// Defensive extras: legacy UTF-8 (?1005), urxvt (?1015), X10 (?9).
	wants := []string{
		"\x1b[?1000l", // ResetModeMouseNormal (core)
		"\x1b[?1002l", // ResetModeMouseButtonEvent (core)
		"\x1b[?1003l", // ResetModeMouseAnyEvent (core)
		"\x1b[?1006l", // ResetModeMouseExtSgr (core)
		"\x1b[?1005l", // legacy UTF-8 (defensive)
		"\x1b[?1015l", // urxvt (defensive)
		"\x1b[?9l",    // ResetModeMouseX10 (defensive)
	}
	for _, want := range wants {
		if !bytes.Contains(buf.Bytes(), []byte(want)) {
			t.Errorf("writeTerminalModeReset output missing %q; got %q", want, got)
		}
	}
}
