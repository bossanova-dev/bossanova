// Package statusdetect provides shared detection logic for Claude Code
// session statuses (working, idle, question). It is used by both the
// client-side PTY monitor and the daemon-side tmux status poller.
package statusdetect

import (
	"bytes"
	"regexp"
	"strings"
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
func stripUserPromptLines(data []byte) []byte {
	return userPromptLineRe.ReplaceAll(data, nil)
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

// suggestedPromptHeaderRe matches assistant-rendered headings that introduce
// suggested follow-up prompts. Questions inside those lists are suggestions for
// the user's next input, not questions Claude is waiting on.
var suggestedPromptHeaderRe = regexp.MustCompile(`(?i)^[ \t]*(?:suggested(?: next| follow[- ]?up)? prompts?|suggested follow[- ]?ups?|follow[- ]?up prompts?|next prompts?):[ \t]*$`)

// suggestedPromptLineRe matches a single suggested prompt line under a
// suggested-prompt heading. It accepts numbered, bulleted, and plain prompt
// lines, but only strips lines that end in "?".
var suggestedPromptLineRe = regexp.MustCompile(`^[ \t]*(?:[-*•][ \t]+|[0-9]+[.)][ \t]+)?\S.*\?[ \t]*$`)

func stripSuggestedFollowUpLines(data []byte) []byte {
	var out bytes.Buffer
	inSuggestedPrompts := false
	sawSuggestion := false
	for _, line := range strings.SplitAfter(string(data), "\n") {
		lineText := strings.TrimSuffix(line, "\n")
		trimmed := strings.TrimSpace(lineText)
		if suggestedPromptHeaderRe.MatchString(lineText) {
			inSuggestedPrompts = true
			sawSuggestion = false
			out.WriteString(line)
			continue
		}
		if inSuggestedPrompts {
			if trimmed == "" {
				// A blank line BEFORE any suggestion is tolerated (the UI may
				// pad between the header and the list). A blank line AFTER the
				// suggestion list ends the block: the list is the contiguous run
				// after the header, so anything below the blank -- e.g. a real
				// follow-up question in the same response -- is not a suggested
				// prompt and must be preserved for question detection.
				if sawSuggestion {
					inSuggestedPrompts = false
				}
				out.WriteString(line)
				continue
			}
			r, _ := utf8.DecodeRuneInString(trimmed)
			switch {
			case optionStopMarkers[r]:
				// A Claude response / tool output / spinner / prompt marker
				// begins new content; the suggested-prompt block has ended.
				// Fall through to write the marker line below.
				inSuggestedPrompts = false
			case suggestedPromptLineRe.MatchString(lineText):
				// Question-shaped suggestion: drop it.
				sawSuggestion = true
				continue
			default:
				// Non-question suggestion -- numbered, bulleted, or a plain
				// unbulleted imperative line ("Run tests"). Keep it, but stay
				// in the section so a later question-shaped suggestion in the
				// same contiguous block is still stripped rather than leaking
				// out and firing question detection.
				sawSuggestion = true
				out.WriteString(line)
				continue
			}
		}
		out.WriteString(line)
	}
	return out.Bytes()
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

// cardHeaderRe matches an AskUserQuestion card title row "☐ <title>". ☐ is
// U+2610 (BALLOT BOX). Claude Code's TODO list also uses this glyph, so this
// regex is only one of several signals -- Pattern 4 also requires a numbered
// option block and "?" in the question region between header and options.
var cardHeaderRe = regexp.MustCompile(`(?m)^[ \t]*☐ \S`)

// numberedOptionRe matches a left-column numbered option line (1+ leading
// spaces, then digits, ".", 1+ spaces, then non-whitespace). The capture
// group is the integer used to verify that consecutive options form a
// strictly-increasing run (1., 2., 3., ...).
var numberedOptionRe = regexp.MustCompile(`(?m)^[ ]+([0-9]+)\.[ ]+\S`)

// askUserQuestionTypeSomethingRe matches the "  N. Type something." numbered
// option that always appears as the second-to-last option (above the divider)
// in the AskUserQuestion UI. The leading-indent + numbered-option shape
// prevents prose containing the phrase from matching.
var askUserQuestionTypeSomethingRe = regexp.MustCompile(`(?m)^[ ]+[0-9]+\.[ ]+Type something\.[ ]*$`)

// askUserQuestionChatAboutThisRe matches the "  N. Chat about this" final
// option that always appears below the divider in the AskUserQuestion UI.
var askUserQuestionChatAboutThisRe = regexp.MustCompile(`(?m)^[ ]+[0-9]+\.[ ]+Chat about this[ ]*$`)

// askUserQuestionCardFooterRe matches the AskUserQuestion card terminator --
// the "Chat about this" final option immediately followed (blank lines only in
// between) by the instruction footer:
//
//	  Chat about this
//
//	Enter to select · ↑/↓ to navigate · Esc to cancel
//	Enter to select · ↑/↓ to navigate · n to add notes · Esc to cancel
//
// The "Chat about this" option may be numbered or, in the side-by-side preview
// layout (whose box-drawing panel desynchronizes the option numbering),
// un-numbered. Matching the two lines as one ordered, adjacent sequence -- not
// as two independent regexes anywhere in the tail -- is what makes this a
// reliable live-prompt signal: the footer and terminator each render in every
// card directly above one another, whereas assistant prose documenting the UI
// would have to reproduce the exact two-line sequence (with only blank lines
// between) to false-positive. The footer half matches the three stable phrases
// in order (rather than the exact ·/arrow glyphs) to tolerate the variable
// inter-token spacing introduced by cursor-forward (ESC[nC) rendering and the
// optional "n to add notes" segment, and is anchored to a full line so prose
// mentioning "Enter to select" mid-sentence cannot match.
var askUserQuestionCardFooterRe = regexp.MustCompile(`(?m)^[ \t]*(?:[0-9]+\.[ \t]+)?Chat about this[ \t]*$\n(?:[ \t]*\n)*[ \t]*Enter to select\b[^\n]*\bto navigate\b[^\n]*\bEsc to cancel\b[ \t]*$`)

// hasAskUserQuestionFooter reports whether data contains the AskUserQuestion
// terminator: a "Type something." numbered option above the divider and a
// "Chat about this" numbered option below it. Together these two phrases as
// numbered option lines are unique to the AskUserQuestion UI -- they do not
// appear together in normal Claude output -- and so are a definitive signal
// of an active question prompt regardless of where the question text or "?"
// lives.
func hasAskUserQuestionFooter(data []byte) bool {
	return askUserQuestionTypeSomethingRe.Match(data) && askUserQuestionChatAboutThisRe.Match(data)
}

// countConsecutiveNumberedOptions walks data line-by-line and counts how many
// numbered-option lines (1., 2., 3., ...) appear at the same indent with
// strictly-increasing integers. Blank lines and indented continuation lines
// (lines that are not a new numbered option but are still indented) are
// allowed between options. A line whose first non-space rune is one of
// optionStopMarkers (⎿ ⏺ · ✻ ❯) aborts the run and sets brokenByMarker.
func countConsecutiveNumberedOptions(data []byte) (count int, brokenByMarker bool) {
	prev := 0
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
		trimmed := bytes.TrimLeft(line, " \t")
		if r, _ := utf8.DecodeRune(trimmed); optionStopMarkers[r] {
			return count, true
		}
		m := numberedOptionRe.FindSubmatch(line)
		if m == nil {
			// Non-blank, non-option, non-marker line. Allow it as a
			// continuation only if it starts with whitespace; a line at
			// column 0 ends the run.
			if line[0] != ' ' && line[0] != '\t' {
				return count, false
			}
			continue
		}
		// Parse the captured integer.
		n := 0
		for _, c := range m[1] {
			n = n*10 + int(c-'0')
		}
		if n != prev+1 {
			return count, false
		}
		prev = n
		count++
	}
	return count, false
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
// a Claude Code question prompt. It detects five patterns:
//  0. AskUserQuestion footer: "Type something." + "Chat about this" terminator
//     0b. AskUserQuestion instruction footer: "Enter to select · … · Esc to cancel"
//  1. AskUserQuestion/permission prompt: selector cursor + consecutive options
//  2. Question card by structure: ☐ header + numbered options + "?", no chevron required
//  3. Conversational question: Claude response ending with ?
//  4. Fallback: trailing "?" in recent output when response marker is outside the tail
//
// Patterns 1-4 require a "?" somewhere in the cleaned tail. Pattern 0 is the
// fast-path for new long-body AskUserQuestion layouts where the question is
// embedded in the ☐ header title and the body (ELI10/Stakes/Pros&cons) has
// pushed both the header and any literal "?" outside the 30-line tail.
func HasQuestionPrompt(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	clean := StripANSI(data)
	if len(clean) == 0 {
		return false
	}

	// Remove tool-output blocks from the FULL buffer before slicing the tail.
	// A block's ⎿ header can sit more than 30 lines above the bottom, so
	// stripping only after LastNLines would leave orphaned continuation lines
	// (indented 4+ spaces) in the tail that stripToolOutput can no longer
	// anchor to and remove. A pasted fixture whose tool output ends with the
	// card footer ("Chat about this" + "Enter to select … Esc to cancel") would
	// then false-positive Pattern 0b. Stripping first keeps whole blocks --
	// header and continuations -- out of the tail, and means every pattern
	// below (including Pattern 3, which scans this buffer) sees tool-free text.
	clean = stripToolOutput(clean)

	// Only check the last ~30 lines (enough for the question UI at screen bottom).
	tail := LastNLines(clean, 30)

	// Pattern 0: AskUserQuestion footer fast-path. The "Type something." and
	// "Chat about this" numbered options are the unique terminators of the
	// AskUserQuestion UI. When both appear in the tail this is definitively
	// an active question prompt, regardless of whether a "?" or ☐ header is
	// still in the tail. Runs first so it short-circuits Pattern 3's
	// early-return-false branch when a ⏺ marker (working spinner, earlier
	// response) sits outside the question card.
	if hasAskUserQuestionFooter(tail) {
		return true
	}

	// cleanedTail strips "Tip:" status lines, "❯ " input/prompt lines, and
	// suggested-follow-up lines from the tail -- all non-prompt content that
	// must not be mistaken for an active question. Tool-output blocks are
	// already gone (stripped from the full buffer above). Patterns 1 and 4
	// match against this filtered view rather than the raw tail.
	cleanedTail := stripSuggestedFollowUpLines(stripUserPromptLines(stripTipLines(tail)))

	// Pattern 0b: AskUserQuestion card-footer fast-path. The "Chat about this"
	// terminal option immediately followed by the "Enter to select · … · Esc to
	// cancel" instruction line renders below every question card. Unlike Pattern
	// 0 (which keys on the "Type something." + "Chat about this" numbered
	// options), this survives the side-by-side preview layout, where the preview
	// panel's box-drawing characters split the option run and "Chat about this"
	// is rendered un-numbered. Runs before Pattern 1 so it short-circuits ahead
	// of Pattern 3's early-return-false branch.
	//
	// Matches footerTail, not the raw tail: the footer text is low-specificity,
	// so when Claude reads or prints a file/test fixture that contains it the
	// line arrives as tool-output continuation (a ⎿ block indented 4+ spaces)
	// and would otherwise false-positive. Tool blocks are already stripped from
	// the buffer above, so a genuine card footer -- UI chrome below the card,
	// never inside a tool block -- survives while the fixture's does not.
	//
	// footerTail (unlike cleanedTail) does NOT run stripUserPromptLines and
	// instead normalizes the selection cursor: when the user arrows onto the
	// final action, Claude Code renders the terminator with the same "❯ "
	// selector as any option ("❯ Chat about this" / "❯ N. Chat about this").
	// stripUserPromptLines would delete that row, flipping an active card to
	// non-question; normalizeSelectorCursor instead rewrites "❯ " to blanks so
	// the terminator reads as an ordinary line and the footer regex still
	// matches. Tip lines are still stripped so a "❯ "-free "Tip:" line cannot
	// interpose. The regex's full-line anchoring keeps dropped prompt/suggested
	// lines from mattering -- only an exact "Chat about this" terminator line
	// can match.
	//
	// askUserQuestionCardFooterRe requires the two lines as one ordered,
	// adjacent sequence (blank lines only between), not two independent matches
	// anywhere in the tail. Matching them independently still fired on ordinary
	// assistant prose that happened to mention both a standalone "Chat about
	// this" and the footer line; requiring the exact two-line card structure
	// rejects documented/pasted prompt fixtures while preserving the live
	// preview-layout card, where the terminator always renders directly above
	// the footer.
	footerTail := normalizeSelectorCursor(stripTipLines(tail))
	if askUserQuestionCardFooterRe.Match(footerTail) {
		return true
	}

	// Pattern 1: AskUserQuestion / permission prompt -- selector + consecutive
	// indented option lines. Requires a "?" somewhere in the cleaned tail (the
	// question text above the selector). Without that gate, the user's own
	// previously-submitted prompt (rendered as "❯ <text>" in conversation
	// history) gets mistaken for the selector and surrounding indented lines
	// like "  Read 4 files..." or "  ⎿  Tip: ..." get mistaken for options.
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

	// Pattern 2: AskUserQuestion card detected by structure -- \u2610 header with a
	// "?" in the question region followed by 2+ consecutive numbered options.
	// Catches cards rendered without the bubbletea selector cursor on a
	// left-column option line. Must run before the response-marker pattern
	// below, because that pattern's early-return-false branch fires whenever
	// the LAST \u23FA in the buffer has no trailing "?", which would swallow this
	// card whenever the conversation has a \u23FA marker below it (working spinner,
	// status chrome, or post-card response text).
	//
	// Uses its own filtered view rather than cleanedTail: cleanedTail strips
	// every "\u276F " line, which deletes the FIRST option when the selection cursor
	// sits on it ("\u276F 1. ..."), making the numbered run appear to start at 2.
	// p2Tail instead keeps "\u276F N." selector-on-option lines and normalizes the
	// cursor to blanks so the run is counted from 1.
	p2Tail := normalizeSelectorCursor(stripSuggestedFollowUpLines(stripUserPromptLinesKeepNumberedOptions(stripTipLines(tail))))
	if header := cardHeaderRe.FindIndex(p2Tail); header != nil {
		afterHeader := p2Tail[header[1]:]
		if optMatch := numberedOptionRe.FindIndex(afterHeader); optMatch != nil {
			questionRegion := afterHeader[:optMatch[0]]
			if bytes.ContainsRune(questionRegion, '?') {
				// `brokenByMarker` is intentionally ignored here: a Claude
				// marker (⏺ etc.) AFTER the option run is just status chrome
				// below the card. What matters is that the run itself
				// produced 2+ strictly-increasing options before any marker
				// interrupted it.
				count, _ := countConsecutiveNumberedOptions(afterHeader[optMatch[0]:])
				if count >= 2 {
					return true
				}
			}
		}
	}

	// Pattern 3: Claude response ending with a question mark.
	// Find the last response marker and check if the text from there to the end
	// contains a trailing "?".
	if idx := bytes.LastIndex(clean, []byte("\u23FA")); idx >= 0 {
		// Strip the "\u276F " input/prompt lines too: a suggested follow-up prompt
		// (e.g. "\u276F Want me to file the ticket now?") rendered in the input box
		// after the last response marker is Claude proposing a prompt, not
		// asking one, and must not fire the state.
		afterMarker := stripSuggestedFollowUpLines(stripUserPromptLines(stripTipLines(clean[idx:])))
		if trailingQuestionRe.Match(afterMarker) {
			return true
		}
		// Response marker found but no trailing "?" -- definitely not a question.
		return false
	}

	// Pattern 4: Fallback when response marker is outside the detection tail.
	// Claude Code's TUI renders dividers, status bars, and cursor positioning
	// after the response text. With wide terminals or re-renders, this
	// post-response content can push the marker out of the tail buffer.
	// Check if any line in the last 30 lines ends with "?" (excluding tool
	// output, tip lines, and the user's prompt history -- none of those are
	// Claude's words).
	if trailingQuestionRe.Match(stripSuggestedFollowUpLines(stripUserPromptLines(stripTipLines(tail)))) {
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
