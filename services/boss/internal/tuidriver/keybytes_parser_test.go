package tuidriver_test

import (
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
)

// parserContractRow mirrors an intended (proof-key name -> raw byte sequence ->
// decoded key) mapping from namedKeys in keybytes.go. Because namedKeys is
// unexported, the sequences are duplicated here and pinned back to the live
// map by the in-package drift-guard in keybytes_internal_test.go, so this
// mirror cannot silently drift.
//
// This is the parser-contract guard referenced by the namedKeys/KeyBytes doc
// comments: it decodes each normal-mode sequence through a zero-value
// ultraviolet EventDecoder (the same parser boss's Bubble Tea v2 input path
// uses) and proves it maps to the intended key. If a future ultraviolet upgrade
// dropped the normal-mode CSI rows or the SS3 F-key rows, this test goes red
// instead of the proof-tui key vocabulary silently breaking.
type parserContractRow struct {
	name string // namedKeys key this mirrors
	seq  string // raw bytes written to the PTY
	code rune   // expected ultraviolet Key.Code
	mod  uv.KeyMod
}

// parserContractRows covers every MULTI-BYTE escape sequence in namedKeys. The
// bare control bytes (enter="\r", tab="\t", esc="\x1b", backspace="\x7f") are
// intentionally excluded: a lone "\x1b" is an ambiguous escape prefix to the
// decoder (it needs following bytes to disambiguate), so asserting a decode for
// it would be brittle. Their raw values are already covered by TestKeyBytes.
var parserContractRows = []parserContractRow{
	{"up", "\x1b[A", uv.KeyUp, 0},
	{"down", "\x1b[B", uv.KeyDown, 0},
	{"right", "\x1b[C", uv.KeyRight, 0},
	{"left", "\x1b[D", uv.KeyLeft, 0},
	{"shift+tab", "\x1b[Z", uv.KeyTab, uv.ModShift},
	{"pgup", "\x1b[5~", uv.KeyPgUp, 0},
	{"pgdn", "\x1b[6~", uv.KeyPgDown, 0},
	{"home", "\x1b[H", uv.KeyHome, 0},
	{"end", "\x1b[F", uv.KeyEnd, 0},
	{"delete", "\x1b[3~", uv.KeyDelete, 0},
	{"f1", "\x1bOP", uv.KeyF1, 0},
	{"f2", "\x1bOQ", uv.KeyF2, 0},
	{"f3", "\x1bOR", uv.KeyF3, 0},
	{"f4", "\x1bOS", uv.KeyF4, 0},
	{"f5", "\x1b[15~", uv.KeyF5, 0},
	{"f6", "\x1b[17~", uv.KeyF6, 0},
	{"f7", "\x1b[18~", uv.KeyF7, 0},
	{"f8", "\x1b[19~", uv.KeyF8, 0},
	{"f9", "\x1b[20~", uv.KeyF9, 0},
	{"f10", "\x1b[21~", uv.KeyF10, 0},
	{"f11", "\x1b[23~", uv.KeyF11, 0},
	{"f12", "\x1b[24~", uv.KeyF12, 0},
}

// TestKeyBytesDecodeContract proves that every multi-byte namedKeys sequence,
// written normal-mode straight into the PTY, is decoded by ultraviolet to the
// intended key regardless of application (DECCKM) cursor-key mode — the parser
// maps both the normal-mode CSI and application-mode/SS3 forms to the same key
// unconditionally, so the driver never has to negotiate DECCKM.
func TestKeyBytesDecodeContract(t *testing.T) {
	for _, tt := range parserContractRows {
		t.Run(tt.name, func(t *testing.T) {
			var dec uv.EventDecoder
			n, ev := dec.Decode([]byte(tt.seq))
			if n != len(tt.seq) {
				t.Fatalf("Decode(%q) consumed %d bytes, want %d (whole sequence)", tt.seq, n, len(tt.seq))
			}
			kp, ok := ev.(uv.KeyPressEvent)
			if !ok {
				t.Fatalf("Decode(%q) returned %T, want ultraviolet.KeyPressEvent", tt.seq, ev)
			}
			key := kp.Key()
			if key.Code != tt.code {
				t.Errorf("Decode(%q).Key().Code = %d, want %d", tt.seq, key.Code, tt.code)
			}
			if key.Mod != tt.mod {
				t.Errorf("Decode(%q).Key().Mod = %d, want %d", tt.seq, key.Mod, tt.mod)
			}
		})
	}
}
