package tmux

import (
	"strings"
	"unicode/utf8"
)

// promptMarkers are the leading glyphs an agent's input box renders.
var promptMarkers = []string{"❯", "›", "┃", ">"}

// composerMarkerIdx returns the index of the live input box's prompt-marker
// row, or -1 if none is present. Everything below it is footer, statusline or
// menu chrome, so submission checks scan from this row.
//
// A Claude prompt row opening the box between the bottom two rules is the input
// box. Claude draws its slash-command menu below that box and marks the
// selected entry with the same "❯" (testdata/panes/claude_typed_slash_menu_real.txt),
// so the bottom-most marker row is the input box only when no such box is drawn.
//
// Antigravity draws the same ruled box with ">" and the same menu below it
// (testdata/panes/antigravity_slash_menu_real.txt). Its earlier user turns
// also open with ">" under a 60-column turn rule, and while an approval menu
// hides the box the bottom two rules are that turn rule and the menu's header
// rule (testdata/panes/antigravity_approval_modal_real.txt). So a ">" row counts
// as the box only between two rules of the same width.
func composerMarkerIdx(lines []string) int {
	if top, bottom, ok := inputBoxRules(lines); ok && top+1 < bottom {
		row := strings.TrimSpace(lines[top+1])
		if strings.HasPrefix(row, sendPlanReadyMarker) ||
			(strings.HasPrefix(row, antigravityReadyMarker) && sameWidthRules(lines[top], lines[bottom])) {
			return top + 1
		}
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if hasPromptMarker(strings.TrimSpace(lines[i])) {
			return i
		}
	}
	return -1
}

// sameWidthRules reports whether two horizontal-rule rows span the same number
// of columns, as the two edges of one box do.
func sameWidthRules(a, b string) bool {
	return utf8.RuneCountInString(strings.TrimSpace(a)) == utf8.RuneCountInString(strings.TrimSpace(b))
}

// hasPromptMarker reports whether a trimmed pane row begins with one of the
// input-box prompt indicators.
func hasPromptMarker(text string) bool {
	_, ok := composerContent(text)
	return ok
}

// composerContent returns the text a prompt-marker row carries after its glyph,
// and whether the row was a prompt-marker row at all.
func composerContent(trimmed string) (string, bool) {
	for _, marker := range promptMarkers {
		if rest, ok := strings.CutPrefix(trimmed, marker); ok {
			return strings.TrimSpace(rest), true
		}
	}
	return "", false
}
