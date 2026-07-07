package main

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/spinner"
	"charm.land/lipgloss/v2"
)

func TestNormalizeAnimationReplacesFrameEqualWidth(t *testing.T) {
	// A whole frame followed by a label, as sp.View()+"working" renders.
	in := string(spinner.Dot.Frames[0]) + "working"
	got := normalizeAnimation(in)
	if strings.ContainsAny(got, "⣾⣽⣻⢿⡿⣟⣯⣷") {
		t.Fatalf("normalizeAnimation left a spinner glyph: %q", got)
	}
	// Display width preserved so downstream columns don't shift.
	if lipgloss.Width(in) != lipgloss.Width(got) {
		t.Fatalf("display width changed: in=%d out=%d", lipgloss.Width(in), lipgloss.Width(got))
	}
	if !strings.Contains(got, "working") {
		t.Fatalf("label lost: %q", got)
	}
}

func TestNormalizeAnimationCollapsesEveryFrameToSameToken(t *testing.T) {
	// Two different frames + the same label must normalize equal, so an
	// animating spinner reads as stable across settle poll ticks.
	a := normalizeAnimation(string(spinner.Dot.Frames[0]) + "checking")
	b := normalizeAnimation(string(spinner.Dot.Frames[1]) + "checking")
	if a != b {
		t.Fatalf("distinct frames normalized differently: %q vs %q", a, b)
	}
	// Every frame must be covered — its braille rune may not survive.
	for _, f := range spinner.Dot.Frames {
		got := normalizeAnimation(f + "x")
		for _, r := range f {
			if r == ' ' {
				continue
			}
			if strings.ContainsRune(got, r) {
				t.Fatalf("frame rune %q survived normalization: %q", string(r), got)
			}
		}
	}
}

func TestNormalizeAnimationNoFrameIsIdentity(t *testing.T) {
	in := "REPO   STATUS   working session at row 3   "
	if got := normalizeAnimation(in); got != in {
		t.Fatalf("normalizeAnimation changed animation-free screen:\n in=%q\nout=%q", in, got)
	}
}

// A braille rune that appears as ordinary UI data — NOT as a rendered spinner
// frame with its trailing space — must be left untouched, so settle() still
// detects it changing from one such rune to another. Regression for the P2
// review finding that global bare-rune replacement could mask real content
// changes (e.g. a user-provided session title in the home table).
func TestNormalizeAnimationLeavesBareBrailleUntouched(t *testing.T) {
	// The braille rune is immediately followed by a letter, not the spinner's
	// trailing space, so it is not a spinner frame.
	titleA := "my ⣾project session"
	titleB := "my ⣽project session"
	if got := normalizeAnimation(titleA); got != titleA {
		t.Fatalf("bare braille rune masked in title: %q", got)
	}
	if normalizeAnimation(titleA) == normalizeAnimation(titleB) {
		t.Fatalf("distinct titles collapsed to equal after normalization")
	}
}
