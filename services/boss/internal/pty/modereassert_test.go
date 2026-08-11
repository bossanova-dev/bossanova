package pty

import (
	"strings"
	"testing"

	"github.com/recurser/boss/internal/termreset"
)

// The unscrollable-pane bug in one sentence: a detach turns mouse reporting OFF
// on the real terminal, the tmux client that would turn it back on is kept alive
// rather than respawned, and so nothing ever re-sends the enable. These tests pin
// the two halves of the repair — the teardown marking, and the next attach
// undoing it — at the level where the decision is actually made.

// TestModesClobberedRoundTrip covers the flag's contract: set by a teardown,
// consumed exactly once by the next attach.
func TestModesClobberedRoundTrip(t *testing.T) {
	var p Process

	if p.TakeModesClobbered() {
		t.Fatal("a fresh process must not claim clobbered modes; it runs its own terminal init")
	}

	p.MarkModesClobbered()
	if !p.TakeModesClobbered() {
		t.Fatal("a marked process must report clobbered modes to the next attach")
	}
	if p.TakeModesClobbered() {
		t.Error("the flag must clear on read: re-asserting on every later attach would " +
			"write modes over a child that has since set its own")
	}
}

// TestModesClobberedIsIdempotentPerClobber guards the count, not just the
// boolean: two teardowns before one attach still owe exactly one re-assert.
func TestModesClobberedIsIdempotentPerClobber(t *testing.T) {
	var p Process
	p.MarkModesClobbered()
	p.MarkModesClobbered()

	if !p.TakeModesClobbered() {
		t.Fatal("expected the pending re-assert")
	}
	if p.TakeModesClobbered() {
		t.Error("two marks must not owe two re-asserts")
	}
}

// TestMouseEnableIsNarrowerThanReset is the substance of the fix's one real
// judgement call. MouseReset is a blanket clear — clearing a mode that was never
// set is free — but SETTING one is not. Re-enabling everything it clears would
// hand the terminal ?1003 any-event motion reporting that the child never asked
// for, putting every idle pointer movement across the PTY. The enable must
// therefore restore only what tmux `mouse on` actually negotiates.
func TestMouseEnableIsNarrowerThanReset(t *testing.T) {
	// Bound to a local first: gocritic's argOrder heuristic reads a package
	// constant in the haystack position as a reversed argument.
	enable := termreset.MouseEnable

	for _, want := range []string{"\x1b[?1000h", "\x1b[?1002h", "\x1b[?1006h"} {
		if !strings.Contains(enable, want) {
			t.Errorf("MouseEnable must restore %q; got %q", want, enable)
		}
	}
	for _, banned := range []string{
		"\x1b[?1003h", // any-event motion — the expensive one
		"\x1b[?1005h", // legacy UTF-8 encoding
		"\x1b[?1015h", // urxvt encoding
		"\x1b[?9h",    // X10
	} {
		if strings.Contains(enable, banned) {
			t.Errorf("MouseEnable must NOT set %q — it is cleared defensively, not owed back", banned)
		}
	}
	if strings.Contains(enable, "l") {
		t.Errorf("MouseEnable must contain no DECRST (…l) sequences; got %q", enable)
	}
}

// TestMouseEnableUndoesTheResetItPairsWith checks the two constants stay a
// matching pair for the modes that matter. If MouseReset ever stops clearing one
// of these, the enable is re-asserting something nothing disabled.
func TestMouseEnableUndoesTheResetItPairsWith(t *testing.T) {
	reset, enable := termreset.MouseReset, termreset.MouseEnable
	for _, mode := range []string{"1000", "1002", "1006"} {
		if !strings.Contains(reset, "\x1b[?"+mode+"l") {
			t.Errorf("MouseReset no longer clears ?%s, so MouseEnable setting it is unpaired", mode)
		}
		if !strings.Contains(enable, "\x1b[?"+mode+"h") {
			t.Errorf("MouseEnable no longer sets ?%s, leaving the reset unmatched", mode)
		}
	}
}

// TestWriteMouseEnableEmitsTheSequence guards the writer against silently
// emitting nothing — the failure mode that would restore the original bug while
// every other test here still passed.
func TestWriteMouseEnableEmitsTheSequence(t *testing.T) {
	var sb strings.Builder
	termreset.WriteMouseEnable(&sb)
	if sb.String() != termreset.MouseEnable {
		t.Errorf("WriteMouseEnable wrote %q, want %q", sb.String(), termreset.MouseEnable)
	}
	if sb.Len() == 0 {
		t.Error("WriteMouseEnable wrote nothing; scrolling would stay dead after a re-attach")
	}
}
