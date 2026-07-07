package main

import (
	"strings"

	"charm.land/bubbles/v2/spinner"
	"charm.land/lipgloss/v2"
)

// animPlaceholder replaces the animating spinner cell during the settle
// stability comparison. It is display-width 1 (U+00B7 MIDDLE DOT); padded to a
// frame's full display width it preserves every downstream column position.
const animPlaceholder = '·'

// spinnerFramePlaceholder returns the width-preserving replacement for one
// spinner frame: the animation placeholder followed by enough spaces to match
// the frame's display width. spinner.Dot renders each frame as a braille rune
// plus a trailing space (display width 2), so every frame collapses to the same
// "· " token — an animating spinner therefore compares equal across settle poll
// ticks no matter which frame is currently showing. Returns "" for a
// zero-width frame so the caller can skip it.
func spinnerFramePlaceholder(frame string) string {
	w := lipgloss.Width(frame)
	if w <= 0 {
		return ""
	}
	return string(animPlaceholder) + strings.Repeat(" ", w-1)
}

// spinnerFrameReplacer rewrites each *whole* spinner frame — the animating
// braille rune together with its trailing space, exactly as newStatusSpinner()
// emits it via sp.View() and lipgloss wraps it as one contiguous cell — to a
// fixed, width-matched placeholder. Matching the full frame (rune + trailing
// space) rather than the bare braille rune means a braille rune that appears as
// ordinary UI data (e.g. inside a user-provided session title rendered in the
// home table) is left untouched, so settle() still sees such content change
// instead of masking it. Derived once at package load from spinner.Dot.Frames
// (a compile-time constant) since settle() calls normalizeAnimation on every
// poll tick; deriving from the frames keeps this correct if the bubbles Dot
// definition changes.
var spinnerFrameReplacer = func() *strings.Replacer {
	frames := spinner.Dot.Frames
	pairs := make([]string, 0, len(frames)*2)
	seen := map[string]bool{}
	for _, f := range frames {
		if f == "" || seen[f] {
			continue
		}
		placeholder := spinnerFramePlaceholder(f)
		if placeholder == "" {
			continue
		}
		seen[f] = true
		pairs = append(pairs, f, placeholder)
	}
	if len(pairs) == 0 {
		return nil
	}
	return strings.NewReplacer(pairs...)
}()

// normalizeAnimation returns screen with every whole spinner frame replaced by a
// width-matched placeholder. It is byte-identical to screen when no frame is
// present. Only used for the settle stability COMPARISON — the raw screen is
// what settle() returns.
func normalizeAnimation(screen string) string {
	if spinnerFrameReplacer == nil {
		return screen
	}
	return spinnerFrameReplacer.Replace(screen)
}
