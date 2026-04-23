// Package statusdetect provides shared detection logic for Claude Code
// session statuses (working, idle, question). It is used by both the
// client-side PTY monitor and the daemon-side tmux status poller.
package statusdetect

import (
	"bytes"
	"regexp"
	"unicode/utf8"
)

// cursorFwdRe matches CSI cursor-forward sequences: ESC[nC (move right n columns).
// Bubbletea uses these instead of spaces between words.
var cursorFwdRe = regexp.MustCompile(`\x1b\[([0-9]+)C`)

// cursorPosRe matches CSI cursor-position sequences: ESC[row;colH or ESC[H.
// These indicate line transitions in the TUI rendering.
var cursorPosRe = regexp.MustCompile(`\x1b\[[0-9;]*H`)

// ansiRe matches remaining ANSI escape sequences: CSI (ESC[...X), OSC (ESC]...ST),
// and two-byte sequences (ESC followed by a single character like ESC(B).
var ansiRe = regexp.MustCompile(`\x1b(?:\[[0-9;?]*[A-Za-z]|\][^\x07\x1b]*(?:\x07|\x1b\\)|\(.|.)`)

// StripANSI converts raw PTY bytes to readable text by:
// 1. Replacing cursor-forward sequences (ESC[nC) with n spaces
// 2. Replacing cursor-position sequences (ESC[...H) with newlines
// 3. Normalizing \r\n and bare \r to \n
// 4. Stripping all remaining ANSI escape sequences
func StripANSI(data []byte) []byte {
	// Step 1: cursor-forward -> spaces.
	out := cursorFwdRe.ReplaceAllFunc(data, func(m []byte) []byte {
		// Parse the number from ESC[nC.
		sub := cursorFwdRe.FindSubmatch(m)
		if len(sub) < 2 {
			return []byte(" ")
		}
		n := 0
		for _, c := range sub[1] {
			n = n*10 + int(c-'0')
		}
		if n <= 0 {
			n = 1
		}
		if n > 120 {
			n = 120 // cap to terminal width
		}
		return bytes.Repeat([]byte(" "), n)
	})

	// Step 2: cursor-position (ESC[...H) -> newline.
	out = cursorPosRe.ReplaceAll(out, []byte("\n"))

	// Step 3: normalize line endings: \r\n -> \n, bare \r -> \n.
	out = bytes.ReplaceAll(out, []byte("\r\n"), []byte("\n"))
	out = bytes.ReplaceAll(out, []byte("\r"), []byte("\n"))

	// Step 4: strip all remaining ANSI sequences.
	out = ansiRe.ReplaceAll(out, nil)

	return out
}

// selectorRe matches the bubbletea selection cursor at the start of a line
// pointing at an actual option (non-whitespace after "❯ "). The same ❯ glyph
// is used by Claude Code's own empty input prompt ("❯ " on a line by itself),
// so we require following content to avoid false positives when the prompt is
// just waiting for input.
// ❯ is U+276F (HEAVY RIGHT-POINTING ANGLE QUOTATION MARK).
var selectorRe = regexp.MustCompile(`(?m)^[^\S\n]*❯ \S`)

// optionRe matches indented option lines (2+ leading spaces followed by text).
var optionRe = regexp.MustCompile(`(?m)^[ ]{2,}\S`)

// trailingQuestionRe matches a line ending with "?" (optional trailing whitespace).
var trailingQuestionRe = regexp.MustCompile(`\?[\s]*(?:\n|$)`)

// toolOutputBlockRe matches a Claude Code tool-result block: a line whose first
// non-space character is ⎿ (U+23BF), plus any following lines indented 4+
// spaces (continuation of the same tool result). Claude Code renders system
// text here — including the "Interrupted · What should Claude do instead?"
// artifact when a tool call is cancelled — which must not be mistaken for a
// conversational question from Claude.
var toolOutputBlockRe = regexp.MustCompile(`(?m)^[ ]*⎿[^\n]*(?:\n[ ]{4,}[^\n]*)*`)

// stripToolOutput removes tool-result blocks from text so incidental "?" in
// tool output (notably the interrupt artifact) doesn't trigger question
// detection. Non-tool-output text is left untouched.
func stripToolOutput(data []byte) []byte {
	return toolOutputBlockRe.ReplaceAll(data, nil)
}

// tipLineRe matches Claude Code's contextual "Tip:" status lines rendered
// beneath the working/thinking spinner. These are UI chrome, not Claude's
// words, so any trailing "?" on them must not trigger question detection.
// Shape seen in the wild:
//
//	"  ⎿  Tip: Did you know you can drag and drop image files …?"
//	"  Tip: Run /help for a list of commands"
//
// Match leading whitespace, an optional ⎿ (U+23BF) connector with its
// following space(s), then the literal "Tip:" prefix and the rest of the
// line. Anchored to line start to avoid mid-sentence false positives.
var tipLineRe = regexp.MustCompile(`(?m)^[ ]*(?:⎿[ ]+)?Tip:[^\n]*`)

// stripTipLines removes Claude Code "Tip:" status lines so the incidental
// "?" that ends many of them doesn't trigger question detection.
func stripTipLines(data []byte) []byte {
	return tipLineRe.ReplaceAll(data, nil)
}

// userPromptLineRe matches lines that are the user's previously-submitted
// prompt rendered in conversation history. Claude Code prefixes these with
// "❯ " (same glyph as the AskUserQuestion selector). Any "?" in the user's
// own words must not trigger question detection -- only Claude's questions
// should fire the state.
var userPromptLineRe = regexp.MustCompile(`(?m)^[ ]*❯ [^\n]*`)

// stripUserPromptLines removes the user's prompt history lines so a "?" the
// user typed (e.g. "what does this do?") doesn't trigger question detection.
func stripUserPromptLines(data []byte) []byte {
	return userPromptLineRe.ReplaceAll(data, nil)
}

// optionStopMarkers are the leading runes that signal the end of an
// AskUserQuestion option block. If a non-blank line after the selector starts
// (after trimming spaces) with one of these, it's not an option -- it's
// Claude conversation, tool output, a spinner, or another prompt entry.
var optionStopMarkers = map[rune]bool{
	'⎿': true, // tool output continuation (U+23BF)
	'⏺': true, // Claude response marker (U+23FA)
	'·': true, // working spinner (U+00B7)
	'✻': true, // thinking spinner (U+273B)
	'❯': true, // another prompt entry (U+276F)
}

// countConsecutiveOptionLines counts how many consecutive indented option
// lines follow a selector. Walks forward line-by-line, skipping blank lines
// (real prompts may have blank-separated option blocks). Returns:
//   - count: number of valid consecutive option lines
//   - brokenByMarker: true if the run was terminated by a Claude marker
//     (⎿, ⏺, ·, ✻, ❯), which signals the candidate selector is not a real
//     prompt -- it's user prompt history followed by Claude conversation.
//
// A non-indented line (like a "────" divider) or EOF terminates the run
// without setting brokenByMarker -- those are normal AskUserQuestion
// terminators.
func countConsecutiveOptionLines(data []byte) (count int, brokenByMarker bool) {
	for len(data) > 0 {
		nl := bytes.IndexByte(data, '\n')
		var line []byte
		if nl < 0 {
			line = data
			data = nil
		} else {
			line = data[:nl]
			data = data[nl+1:]
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		trimmed := bytes.TrimLeft(line, " ")
		if r, _ := utf8.DecodeRune(trimmed); optionStopMarkers[r] {
			return count, true
		}
		if !optionRe.Match(line) {
			return count, false
		}
		count++
	}
	return count, false
}

// HasQuestionPrompt checks whether the last portion of PTY output looks like
// a Claude Code question prompt. It detects three patterns:
//  1. AskUserQuestion/permission prompt: selector cursor + consecutive options
//  2. Conversational question: Claude response ending with ?
//  3. Fallback: trailing "?" in recent output when response marker is outside the tail
//
// All three patterns require a "?" somewhere in the cleaned tail -- a real
// question always has one (in the question text above the selector, or in
// the response itself).
func HasQuestionPrompt(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	clean := StripANSI(data)
	if len(clean) == 0 {
		return false
	}

	// Only check the last ~30 lines (enough for the question UI at screen bottom).
	tail := LastNLines(clean, 30)

	// Pattern 1: AskUserQuestion / permission prompt -- selector + consecutive
	// indented option lines. Requires a "?" somewhere in the cleaned tail (the
	// question text above the selector). Without that gate, the user's own
	// previously-submitted prompt (rendered as "❯ <text>" in conversation
	// history) gets mistaken for the selector and surrounding indented lines
	// like "  Read 4 files..." or "  ⎿  Tip: ..." get mistaken for options.
	cleanedTail := stripUserPromptLines(stripTipLines(stripToolOutput(tail)))
	if bytes.ContainsRune(cleanedTail, '?') {
		selectorMatches := selectorRe.FindAllIndex(tail, -1)
		// Iterate newest-first: AskUserQuestion is always at the bottom of the pane.
		for i := len(selectorMatches) - 1; i >= 0; i-- {
			loc := selectorMatches[i]
			lineEnd := bytes.IndexByte(tail[loc[1]:], '\n')
			if lineEnd < 0 {
				continue
			}
			count, broken := countConsecutiveOptionLines(tail[loc[1]+lineEnd+1:])
			if !broken && count >= 1 {
				return true
			}
		}
	}

	// Pattern 2: Claude response ending with a question mark.
	// Find the last response marker and check if the text from there to the end
	// contains a trailing "?".
	if idx := bytes.LastIndex(clean, []byte("\u23FA")); idx >= 0 {
		afterMarker := stripTipLines(stripToolOutput(clean[idx:]))
		if trailingQuestionRe.Match(afterMarker) {
			return true
		}
		// Response marker found but no trailing "?" -- definitely not a question.
		return false
	}

	// Pattern 3: Fallback when response marker is outside the detection tail.
	// Claude Code's TUI renders dividers, status bars, and cursor positioning
	// after the response text. With wide terminals or re-renders, this
	// post-response content can push the marker out of the tail buffer.
	// Check if any line in the last 30 lines ends with "?" (excluding tool
	// output, tip lines, and the user's prompt history -- none of those are
	// Claude's words).
	if trailingQuestionRe.Match(stripUserPromptLines(stripTipLines(stripToolOutput(tail)))) {
		return true
	}

	return false
}

// LastNLines returns the last n lines of data as a single byte slice.
func LastNLines(data []byte, n int) []byte {
	// Walk backwards to find the start of the last n lines.
	count := 0
	i := len(data) - 1
	// Skip trailing newline.
	if i >= 0 && data[i] == '\n' {
		i--
	}
	for ; i >= 0; i-- {
		if data[i] == '\n' {
			count++
			if count == n {
				return data[i+1:]
			}
		}
	}
	return data
}
