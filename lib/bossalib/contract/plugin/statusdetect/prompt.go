package statusdetect

import (
	"bytes"
	"regexp"
	"strings"
)

// userPromptLineRe matches lines rendered in the "❯ " input/prompt column.
// This covers two shapes that both use the "❯ " glyph (same as the
// AskUserQuestion selector):
//   - the user's previously-submitted prompt in conversation history, and
//   - Claude Code's suggested/auto-filled follow-up prompt sitting in the input
//     box at the bottom of the pane (e.g. "❯ Want me to file the ticket now?").
//
// Neither is a question Claude is actively waiting on -- the suggestion is
// Claude proposing a prompt, not asking one -- so any "?" on these lines must
// not trigger question detection. Only Claude's own response text (prefixed
// with ⏺, never ❯) should fire the state.
var userPromptLineRe = regexp.MustCompile(`(?m)^[ ]*❯ [^\n]*`)

// stripUserPromptLines removes the "❯ " input/prompt lines so a "?" the user
// typed (e.g. "what does this do?") or a "?" in Claude's suggested follow-up
// prompt doesn't trigger question detection.
//
// A long prompt wraps, and a multi-line one keeps its later lines, onto rows
// indented two columns past the glyph with no "❯ " of their own. Those rows
// are removed too, up to the first blank or shallower row: Claude Code always
// leaves a blank line before the collapsed tool summaries that share the
// indent ("  Ran 5 shell commands").
func stripUserPromptLines(data []byte) []byte {
	var out bytes.Buffer
	continuationIndent := ""
	for _, line := range strings.SplitAfter(string(data), "\n") {
		lineText := strings.TrimSuffix(line, "\n")
		if userPromptLineRe.MatchString(lineText) {
			out.WriteString(line[len(lineText):])
			continuationIndent = strings.Repeat(" ", strings.Index(lineText, "❯")+2)
			continue
		}
		if continuationIndent != "" && isPromptContinuation(lineText, continuationIndent) {
			continue
		}
		continuationIndent = ""
		out.WriteString(line)
	}
	return out.Bytes()
}

// isPromptContinuation reports whether lineText continues a "❯ " prompt whose
// text starts at the given indent. A "⎿" row is the tool output that follows a
// prompt, not part of it.
func isPromptContinuation(lineText, indent string) bool {
	rest, ok := strings.CutPrefix(lineText, indent)
	return ok && strings.TrimSpace(rest) != "" && !strings.HasPrefix(strings.TrimLeft(rest, " "), "⎿")
}

// numberedSelectorOptionRe matches a selection cursor sitting on a numbered
// option line, e.g. "❯ 1. Skip, leave open". Unlike user prompt history or a
// suggested follow-up (free prose after "❯ "), this is the highlighted option
// of an active AskUserQuestion card, so the structural card check (Pattern 2)
// must keep it rather than discard it as prompt chrome.
var numberedSelectorOptionRe = regexp.MustCompile(`^[ ]*❯ [0-9]+\.[ ]`)

// stripUserPromptLinesKeepNumberedOptions behaves like stripUserPromptLines but
// preserves "❯ N. ..." selector-on-option lines. Without this, a card whose
// FIRST option carries the selection cursor loses option 1 before Pattern 2
// inspects it, so the numbered run appears to start at 2 and Pattern 2's
// strictly-increasing-from-1 check rejects a real card.
func stripUserPromptLinesKeepNumberedOptions(data []byte) []byte {
	var out bytes.Buffer
	for _, line := range strings.SplitAfter(string(data), "\n") {
		lineText := strings.TrimSuffix(line, "\n")
		if userPromptLineRe.MatchString(lineText) && !numberedSelectorOptionRe.MatchString(lineText) {
			continue
		}
		out.WriteString(line)
	}
	return out.Bytes()
}

// selectorCursorRe matches a leading selection cursor "❯ " (after optional
// indentation). Pattern 2 normalizes it to blanks so the highlighted option
// reads as an ordinary numbered option line and the run is counted from 1.
var selectorCursorRe = regexp.MustCompile(`(?m)^([ ]*)❯ `)

// normalizeSelectorCursor replaces a leading "❯ " selection cursor with two
// spaces so numberedOptionRe matches the highlighted option line.
func normalizeSelectorCursor(data []byte) []byte {
	return selectorCursorRe.ReplaceAll(data, []byte("${1}  "))
}
