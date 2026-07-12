package tuidriver

import "testing"

// mirroredSeqs pins the raw byte sequence the parser-contract test
// (keybytes_parser_test.go, package tuidriver_test) asserts against the live
// namedKeys entry of the same name. Because the contract test mirrors these
// sequences (namedKeys is unexported), this in-package guard is what keeps the
// mirror from drifting: if any namedKeys value changes, this test goes red and
// forces the mirror to be updated in lockstep.
var mirroredSeqs = map[string]string{
	"up":        "\x1b[A",
	"down":      "\x1b[B",
	"right":     "\x1b[C",
	"left":      "\x1b[D",
	"shift+tab": "\x1b[Z",
	"pgup":      "\x1b[5~",
	"pgdn":      "\x1b[6~",
	"home":      "\x1b[H",
	"end":       "\x1b[F",
	"delete":    "\x1b[3~",
	"f1":        "\x1bOP",
	"f2":        "\x1bOQ",
	"f3":        "\x1bOR",
	"f4":        "\x1bOS",
	"f5":        "\x1b[15~",
	"f6":        "\x1b[17~",
	"f7":        "\x1b[18~",
	"f8":        "\x1b[19~",
	"f9":        "\x1b[20~",
	"f10":       "\x1b[21~",
	"f11":       "\x1b[23~",
	"f12":       "\x1b[24~",
}

func TestMirroredSeqsMatchNamedKeys(t *testing.T) {
	for name, want := range mirroredSeqs {
		got, ok := namedKeys[name]
		if !ok {
			t.Errorf("namedKeys is missing mirrored key %q", name)
			continue
		}
		if got != want {
			t.Errorf("namedKeys[%q] = %q, but parser-contract mirror expects %q; update the mirror in keybytes_parser_test.go", name, got, want)
		}
	}
}

// TestNamedKeysMultiByteAllMirrored is the completeness half of the drift guard.
// TestMirroredSeqsMatchNamedKeys proves each mirrored row still matches the live
// map; this proves the reverse: every DISTINCT multi-byte sequence VALUE in
// namedKeys is covered by the parser-contract mirror (matched by value, since
// aliases like "uparrow" reuse an already-mirrored sequence). Without this, a
// newly-added multi-byte key with a fresh sequence (e.g. "insert" -> "\x1b[2~")
// would slip in with no decode assertion in keybytes_parser_test.go and no
// failing test. Bare control bytes (len 1: enter/tab/esc/backspace) are excluded
// here — they are intentionally not in the mirror and are covered by TestKeyBytes.
func TestNamedKeysMultiByteAllMirrored(t *testing.T) {
	mirroredValues := make(map[string]bool, len(mirroredSeqs))
	for _, seq := range mirroredSeqs {
		mirroredValues[seq] = true
	}
	for name, seq := range namedKeys {
		if len(seq) <= 1 {
			continue // bare control byte; covered by TestKeyBytes, not the decoder contract
		}
		if !mirroredValues[seq] {
			t.Errorf("namedKeys[%q] = %q is a multi-byte sequence with no parser-contract coverage; add a row to mirroredSeqs (keybytes_internal_test.go) and parserContractRows (keybytes_parser_test.go)", name, seq)
		}
	}
}
