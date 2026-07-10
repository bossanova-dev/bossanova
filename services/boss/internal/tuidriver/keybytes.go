package tuidriver

import (
	"fmt"
	"strings"
)

// namedKeys maps case-folded proof key names (and aliases) to the raw xterm
// byte sequences bubbletea v2's input parser (ultraviolet) decodes with
// TERM=xterm-256color in normal (non-application) mode. The sequences are
// grounded line-by-line in ultraviolet's key_table.go; do not invent bytes.
//
// namedKeys is the single source of truth for testdata/key-vocab.json: the
// golden test (TestKeyVocabGolden) derives that file's `named` list from these
// keys, and the proof send_keys doc-drift guard reads it back to assert the
// tool description enumerates the full vocabulary.
var namedKeys = map[string]string{
	// enter / esc (pre-existing precedent, kept byte-for-byte).
	"enter":  "\r",
	"return": "\r",
	"esc":    "\x1b",
	"escape": "\x1b",
	// Arrow keys (CSI, normal mode).
	"up":         "\x1b[A",
	"uparrow":    "\x1b[A",
	"down":       "\x1b[B",
	"downarrow":  "\x1b[B",
	"right":      "\x1b[C",
	"rightarrow": "\x1b[C",
	"left":       "\x1b[D",
	"leftarrow":  "\x1b[D",
	// Tab / shift+tab (back-tab).
	"tab":       "\t",
	"shift+tab": "\x1b[Z",
	"shifttab":  "\x1b[Z",
	"backtab":   "\x1b[Z",
	// Paging.
	"pgup":     "\x1b[5~",
	"pageup":   "\x1b[5~",
	"pgdn":     "\x1b[6~",
	"pagedown": "\x1b[6~",
	// Home / end (xterm normal mode).
	"home": "\x1b[H",
	"end":  "\x1b[F",
	// Backspace (DEL 0x7f — what real terminals send; 0x08 is ctrl+h) / delete.
	"backspace": "\x7f",
	"bs":        "\x7f",
	"delete":    "\x1b[3~",
	"del":       "\x1b[3~",
	// Function keys: F1-F4 SS3, F5-F12 CSI.
	"f1":  "\x1bOP",
	"f2":  "\x1bOQ",
	"f3":  "\x1bOR",
	"f4":  "\x1bOS",
	"f5":  "\x1b[15~",
	"f6":  "\x1b[17~",
	"f7":  "\x1b[18~",
	"f8":  "\x1b[19~",
	"f9":  "\x1b[20~",
	"f10": "\x1b[21~",
	"f11": "\x1b[23~",
	"f12": "\x1b[24~",
}

// KeyBytes maps a proof key name to the raw bytes to write to the PTY.
// "ctrl+<a-z>" -> control byte; a single character -> that byte (case
// preserved). Named keys (case-insensitive) cover enter/return, esc/escape,
// arrows (up/down/right/left + *arrow aliases), tab, shift+tab (shifttab/
// backtab), pgup/pgdn (pageup/pagedown), home/end, backspace (bs)/delete (del),
// and f1-f12. Any other name is an error.
func KeyBytes(name string) ([]byte, error) {
	if len(name) == 1 {
		return []byte{name[0]}, nil
	}
	key := strings.TrimSpace(name)
	lower := strings.ToLower(key)
	if len(lower) == 6 && lower[:5] == "ctrl+" && lower[5] >= 'a' && lower[5] <= 'z' {
		// Ctrl+<letter> is the control byte: 'a'->0x01, 'b'->0x02, ...
		return []byte{lower[5] - 'a' + 1}, nil
	}
	if seq, ok := namedKeys[lower]; ok {
		return []byte(seq), nil
	}
	return nil, fmt.Errorf("unsupported proof key %q", name)
}
