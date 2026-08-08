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

// unicodeSpaceRe matches Unicode space separators (category Zs): the regular
// ASCII space U+0020, the non-breaking space U+00A0, narrow NBSP U+202F, figure
// space U+2007, and the rest. Claude Code renders the input-box prompt as
// "❯ <text>" with a NON-BREAKING space after the glyph (and uses NBSP for
// other layout padding), but every detection regex below assumes an ASCII
// space -- so without normalizing, a draft prompt ending in "?" slips past
// stripUserPromptLines and false-positives the question state. Replacing all Zs
// runes with U+0020 keeps regular spaces unchanged and leaves tabs (U+0009,
// category Cc) untouched, so the existing "[ \t]" / "[ ]{4,}" classes keep
// working.
var unicodeSpaceRe = regexp.MustCompile(`\p{Zs}`)

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

	// Step 5: normalize Unicode space separators (NBSP etc.) to ASCII space so
	// the "❯ "/"☐ "/option/indent regexes match Claude Code's NBSP-rendered
	// input box and layout padding.
	out = unicodeSpaceRe.ReplaceAll(out, []byte(" "))

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

// tipPrefix is the literal label that opens a Claude Code "Tip:" footer line.
const tipPrefix = "Tip:"

// Decoration glyphs Claude Code renders immediately before the "Tip:" label.
const (
	tipDecorationToolConnector = '⎿' // U+23BF, the spinner-footer connector
	tipDecorationBoxCorner     = '└' // U+2514, box-drawing variant of the same
	tipDecorationReference     = '※' // U+203B, the recap/reference block prefix
	tipDecorationBullet        = '•' // U+2022
	tipDecorationMiddleDot     = '·' // U+00B7
)

// tipDecorations is the closed allowlist of glyphs that may introduce a tip
// footer line. It is deliberately an allowlist rather than an open "any
// non-letter rune" rule, because two glyphs MUST NOT be accepted here:
//
//   - ⏺ (U+23FA) is Claude's own response marker. "⏺ Tip: consider caching the
//     result. Does that make sense to you?" is Claude speaking, not UI chrome,
//     and is guarded by the positive assertion in TestHasQuestionPrompt_ClaudeCodeTips.
//   - ❯ (U+276F) introduces the input/prompt column, which stripUserPromptLines
//     already removes; accepting it here would additionally swallow the lines
//     below a "❯ …Tip:…" prompt.
var tipDecorations = map[rune]bool{
	tipDecorationToolConnector: true,
	tipDecorationBoxCorner:     true,
	tipDecorationReference:     true,
	tipDecorationBullet:        true,
	tipDecorationMiddleDot:     true,
}

// tipStartDecoration reports whether lineText opens a Claude Code tip footer --
// leading spaces, an OPTIONAL single decoration glyph from tipDecorations
// followed by one or more spaces, then the literal "Tip:" -- and returns the
// decoration rune it opened with (0 for the bare, undecorated form).
func tipStartDecoration(lineText string) (rune, bool) {
	rest := strings.TrimLeft(lineText, " ")
	if strings.HasPrefix(rest, tipPrefix) {
		return 0, true
	}
	r, size := utf8.DecodeRuneInString(rest)
	if size == 0 || !tipDecorations[r] {
		return 0, false
	}
	afterGlyph := rest[size:]
	afterSpaces := strings.TrimLeft(afterGlyph, " ")
	if len(afterSpaces) == len(afterGlyph) {
		// The decoration must be separated from the label by whitespace, so
		// prose like "·Tip:" is not treated as chrome.
		return 0, false
	}
	if !strings.HasPrefix(afterSpaces, tipPrefix) {
		return 0, false
	}
	return r, true
}

// toolOutputContinuationIndent is the indent that makes a line a continuation
// of the ⎿ tool-output block above it. It mirrors toolOutputBlockRe's `[ ]{4,}`
// and MUST stay in step with it -- both the width and the fact that the run is
// contiguous, i.e. the first shallower row ends the block.
const toolOutputContinuationIndent = "    "

// isTipRuleRune reports whether r is one of the runes a horizontal-rule line
// may consist of: the box-drawing block (U+2500-U+257F, which covers ─ ━ ═ │ …)
// plus the ASCII dash and underscore Claude Code uses for the same separator.
func isTipRuleRune(r rune) bool {
	return (r >= 0x2500 && r <= 0x257F) || r == '-' || r == '_'
}

// tipBlockExtraStops are leading runes that end a tip block but are not in
// optionStopMarkers:
//
//   - ☐ (U+2610) opens an AskUserQuestion card title row (see cardHeaderRe).
//     Without this stop the sweep eats the header, the question text and the
//     numbered options below it, flipping a LIVE MODAL card to "no question".
//     That is the costly direction: HasModalPrompt gates delivery (BOS-600), so
//     a false negative types a message into a pane whose keystrokes are
//     consumed as selections.
//   - • (U+2022) opens a fresh bullet row, which is never the wrapped tail of
//     the line above.
//
// This set is deliberately NOT derived from tipDecorations. Three decorations
// are stops (⎿ and · via optionStopMarkers, • here); the other two, └ (U+2514)
// and ※ (U+203B), MUST NOT be, because they are absent from optionStopMarkers.
// A row kept here re-enters countConsecutiveOptionLines, whose optionRe gate
// accepts any 2+-space-indented row and whose stop set is optionStopMarkers --
// so a kept "  └  Did you mean …?" row under a selector counts as a live option
// and fires Pattern 1, flipping HasModalPrompt false->TRUE against base. That
// modal FALSE POSITIVE is the regression this exclusion exists to prevent; it is
// pinned by TestHasQuestionPrompt_DecoratedRowUnderSelector.
//
// The root cause is pre-existing and lives elsewhere: └ is documented as the
// box-drawing variant of the ⎿ tool connector yet is missing from
// optionStopMarkers. Adding it there is the real fix and touches three counters,
// so it is tracked separately rather than widened into this change.
var tipBlockExtraStops = map[rune]bool{
	'☐': true,
	'•': true,
}

// maxTipContinuationLines bounds the continuation sweep of ONE tip start line.
// The bound keeps a stray "Tip:"-prefixed prose or bullet line from erasing an
// unbounded run of real content beneath it when none of the stop conditions
// happen to fire.
//
// Safety is width-dependent, not absolute: a tip leaks residue only when its
// body exceeds (maxTipContinuationLines+1) x paneWidth. At the 43 columns
// bossd's agent panes use that is >172 characters, against a 71-character
// longest-observed tip ("Did you know you can drag and drop image files into
// your terminal?"), and it still holds at 20 columns. When it is exceeded the
// residue produces a spurious ping, not a swallowed question -- the safe
// direction -- so prefer this bound over widening the sweep.
const maxTipContinuationLines = 3

// tipBlockStops reports whether lineText terminates a tip block. The stop line
// itself is KEPT -- it is the next piece of real content, not part of the tip.
func tipBlockStops(lineText string) bool {
	trimmed := strings.TrimSpace(lineText)
	if trimmed == "" {
		return true
	}
	// NOT derived from tipDecorations: └ and ※ open a tip but must not close
	// one, or a kept decorated row counts as a live option under a selector.
	// See the tipBlockExtraStops doc comment for the full reasoning.
	if r, _ := utf8.DecodeRuneInString(trimmed); optionStopMarkers[r] || tipBlockExtraStops[r] {
		return true
	}
	// A numbered option row ("  1. Rebase") opens a selection list. A wrapped
	// tip continuation never starts "N. ", and swallowing the option run under
	// a card leaves Pattern 2 with a header and no options -- another silent
	// HasModalPrompt false negative.
	if numberedOptionRe.MatchString(lineText) {
		return true
	}
	// A horizontal rule ("────…") closes the footer region.
	for _, r := range trimmed {
		if !isTipRuleRune(r) {
			return false
		}
	}
	return true
}

// stripTipLines removes Claude Code's contextual "Tip:" footer BLOCKS so the
// incidental "?" that ends many of them doesn't trigger question detection.
// Tips are UI chrome, not Claude's words.
//
// Shapes seen in the wild:
//
//	"  ⎿  Tip: Did you know you can drag and drop image files …?"
//	"  Tip: Run /help for a list of commands"
//	"  ※ Tip: …"
//
// A tip is a BLOCK, not a line: bossd captures panes with
// `tmux capture-pane -p -S -1000` and no `-J`, so a tip wider than the pane
// (live agent panes are 43 columns) arrives as a start line plus one or more
// continuation lines -- and the trailing "?" lands on the continuation. The
// scan therefore drops the start line and the following lines until the block
// is closed by a blank/whitespace-only line, a line whose first non-space rune
// is an optionStopMarker (⎿ ⏺ · ✻ ❯), a tip decoration (⎿ └ ※ • ·), ☐, a
// numbered option row, a horizontal rule, maxTipContinuationLines swept rows,
// or end of input. The closing line itself is kept. Dropped rows are blanked in
// place, keeping their newline AND their indent, so neither the line count nor
// a surrounding tool block's shape changes.
//
// One exception: a ⎿-decorated tip is simultaneously a tool-output block
// header, so its rows first follow toolOutputBlockRe's own rule -- indented 4+
// spaces and contiguous, the first shallower row ending the tool block for
// good. This pass removes the ⎿ header stripToolOutput anchors on, so those
// rows have no second chance to be stripped. Once that run ends, the ordinary
// bounded prose sweep still applies to the rows below it, so a ⎿ tip's total
// reach is the contiguous 4+-indented run PLUS up to maxTipContinuationLines
// further rows.
//
// The sweep is deliberately bounded on BOTH ends -- a stop-rune set that covers
// every glyph that opens new content, and a hard row cap -- because dropping
// too much is a SILENT failure: a swallowed question never pings a human, and a
// swallowed AskUserQuestion card opens the BOS-600 delivery gate on a modal
// pane. Widen the sweep only with a fixture proving the shape is real chrome.
//
// Accepted risk (stated precisely, because it is what the bounds do NOT cover):
// bare prose -- at any indent, opening with none of the stop runes -- rendered
// within maxTipContinuationLines rows below a tip start with no blank line
// between, is swallowed. Two shapes fall in that set:
//
//   - A conversational question whose ⏺ marker is on an earlier line, e.g. a
//     wrapped "…Should I continue?" tail. Claude prefixes its own turn with ⏺
//     and a tip is footer chrome at the very bottom of the pane with only the
//     composer below it, so a tip does not normally precede Claude's prose.
//   - A permission prompt's question row ("Claude wants to run a command.
//     Allow?"), which is bare prose above its ❯ options. The ❯ row itself stops
//     the sweep, but losing the question row costs Pattern 1 its "?" gate.
//
// Both need the tip to render directly ABOVE live prompt content rather than
// below it as footer chrome, so they are accepted rather than guarded -- there
// is no rune or indent that distinguishes such a row from a genuine wrapped tip
// continuation, and guarding by heuristic would reopen the false positive this
// function exists to close. The row cap keeps the exposure to a few rows PER TIP
// START LINE (it re-arms on each one) instead of the rest of the buffer; under a
// ⎿ tip the exposure is instead the contiguous 4+-indented run, which is exactly
// what stripToolOutput removed before this pass existed.
func stripTipLines(data []byte) []byte {
	var out bytes.Buffer
	inTipBlock := false
	inToolBlock := false
	swept := 0
	for _, line := range strings.SplitAfter(string(data), "\n") {
		lineText := strings.TrimSuffix(line, "\n")
		// SHAPE-PRESERVING DROP: a removed row is replaced by its own leading
		// whitespace plus its newline, never by nothing. This pass runs first,
		// so the later regex passes must still see the buffer they expect:
		//
		//   - Keeping the NEWLINE keeps the line count. LastNLines counts lines,
		//     so deleting rows would slide the 30-line window further back
		//     through the 1000-line capture and pull stale chrome (an answered
		//     card's "Chat about this" terminator) into the tail.
		//   - Keeping the INDENT keeps toolOutputBlockRe's `[ ]{4,}` continuation
		//     run contiguous. A bare "Tip:" row inside a FOREIGN ⎿ block (one
		//     headed by a command, not by a tip) would otherwise truncate that
		//     block at the blank, orphaning every row below it into the tail.
		//
		// Both failure modes were measured, and both flip HasModalPrompt to a
		// FALSE POSITIVE -- the direction that costs the message on BOS-600's
		// delivery gate. A whitespace-only row is inert everywhere downstream:
		// every option/marker scan trims and skips it, and optionRe requires a
		// non-space character.
		blank := lineText[:len(lineText)-len(strings.TrimLeft(lineText, " \t"))] + line[len(lineText):]
		if decoration, ok := tipStartDecoration(lineText); ok {
			inTipBlock = true
			inToolBlock = decoration == tipDecorationToolConnector
			swept = 0
			out.WriteString(blank)
			continue
		}
		if inTipBlock {
			// A ⎿-headed tip is also a tool-output block header, so its rows
			// follow toolOutputBlockRe's own rule rather than the bounded prose
			// sweep: 4+-space indented and CONTIGUOUS, with the first shallower
			// row ending the block for good. This pass deletes the ⎿ header that
			// stripToolOutput anchors on, so without this branch every row past
			// the cap would be orphaned into the tail as ordinary text --
			// reopening the false positive for a long tool result, and letting a
			// question card pasted inside one reach the modal patterns. Matching
			// the regex's contiguity matters just as much: letting the branch
			// re-engage after a shallower row would skip the stop runes and the
			// cap entirely and swallow live UI below the tip.
			if inToolBlock {
				if strings.HasPrefix(lineText, toolOutputContinuationIndent) {
					out.WriteString(blank)
					continue
				}
				inToolBlock = false
			}
			if swept < maxTipContinuationLines && !tipBlockStops(lineText) {
				swept++
				out.WriteString(blank)
				continue
			}
			inTipBlock = false
		}
		out.WriteString(line)
	}
	return out.Bytes()
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
// Patterns 0-2 are MODAL (the composer is gone, keystrokes select); 3 and 4 are
// conversational (the composer is live). Callers deciding whether input is
// SAFE TO DELIVER want HasModalPrompt, not this.
//
// Patterns 1-4 require a "?" somewhere in the cleaned tail. Pattern 0 is the
// fast-path for new long-body AskUserQuestion layouts where the question is
// embedded in the ☐ header title and the body (ELI10/Stakes/Pros&cons) has
// pushed both the header and any literal "?" outside the 30-line tail.
func HasQuestionPrompt(data []byte) bool {
	return hasQuestionPrompt(data, false)
}

// HasModalPrompt reports whether the pane is showing a MODAL prompt: a
// selection UI (AskUserQuestion card, permission prompt, option picker) that
// has taken over the composer, so a keystroke is consumed as a choice rather
// than typed as text.
//
// It is the strict subset of HasQuestionPrompt covering patterns 0, 0b, 1 and
// 2, and deliberately EXCLUDES patterns 3 and 4 (a Claude turn ending in "?").
// Those describe a conversational question asked with a live, empty composer --
// the pane is idle and typing into it is exactly the right thing to do.
//
// The distinction exists because the two predicates answer questions with
// opposite failure costs. HasQuestionPrompt decides whether to NOTIFY a human,
// where a false positive costs a spurious ping; HasModalPrompt decides whether
// to REFUSE DELIVERY (BOS-600), where a false positive costs the message --
// and would break the commonest case of all, answering a question the agent
// just asked. Callers gating input MUST use this one.
//
// Both predicates run the same scan in the same order and split at one early
// return, so the modal subset cannot drift away from the superset by
// construction: any change to the shared prep or to patterns 0-2 moves both.
func HasModalPrompt(data []byte) bool {
	return hasQuestionPrompt(data, true)
}

// hasQuestionPrompt implements both predicates. modalOnly stops the scan after
// the modal patterns (0, 0b, 1, 2) instead of falling through to the
// conversational ones (3, 4).
func hasQuestionPrompt(data []byte, modalOnly bool) bool {
	if len(data) == 0 {
		return false
	}
	clean := StripANSI(data)
	if len(clean) == 0 {
		return false
	}

	// Remove "Tip:" footer blocks from the FULL buffer BEFORE stripToolOutput.
	// Order matters: the commonest tip shape is ⎿-connected, so stripToolOutput
	// would otherwise consume the "  ⎿  Tip: …" START line as a one-line tool
	// block (its continuation sweep needs a 4+-space indent, which a wrapped tip
	// continuation usually lacks) and leave the continuation -- carrying the
	// trailing "?" -- orphaned in the tail with nothing left for stripTipLines to
	// anchor to. Running the tip scan first keeps whole tip blocks out of every
	// downstream view.
	//
	// This pass subsumes the per-view stripTipLines calls further down, which
	// currently remove nothing. The per-view transforms are either line-wise
	// deletions or line-aligned suffix slices, which cannot synthesize a tip
	// start line -- with one exception, normalizeSelectorCursor, which rewrites
	// "❯ " to blanks and so can turn "  ❯ Tip: …" into "    Tip: …". That is
	// harmless because in both views that use it (footerTail, p2Tail) it runs
	// AFTER stripTipLines, so no synthesized line is ever re-stripped. The
	// per-view calls are retained as a cheap invariant guard so reordering these
	// strips cannot silently reopen the bug.
	clean = stripTipLines(clean)

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

	// Everything below describes a CONVERSATIONAL question -- Claude's turn
	// ended with a "?" while the composer stayed live and empty. That is a
	// reason to notify a human, and emphatically not a reason to refuse input:
	// the pane is waiting to be typed into. A caller gating delivery stops here.
	//
	// This single early return is the ONLY thing separating the two predicates,
	// which is deliberate: it is what makes the modal set a subset of the notify
	// set by construction rather than by two grammars agreeing to stay in step.
	// Splitting them into separate scans would silently break BOS-600's delivery
	// gate, which refuses a send only for the modal set. TestHasQuestionPrompt
	// asserts the subset relation over every fixture in this package's table.
	if modalOnly {
		return false
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

// escToInterruptRe matches Claude's active thinking/working spinner footer
// (e.g. "✻ Thinking… (esc to interrupt)", "· Working (3s · esc to interrupt)").
// Claude drops "(esc to interrupt)" the instant a turn ends and re-renders the
// spinner line in place, so it never lingers as stale transcript history — a
// match anywhere in the current screen is a live signal.
var escToInterruptRe = regexp.MustCompile(`esc to interrupt`)

// shellsRunningRe matches Claude's "N shell still running" / "N shells still
// running" background-shell footer (e.g. "✻ Cooked for 48s · 1 shell still
// running"). Unlike the spinner, Claude prints this as each completed turn's
// summary line, so a finished turn leaves the footer frozen in the transcript.
// A bare match is therefore NOT proof a shell is running now — see
// HasWorkingIndicator for the freshness check.
var shellsRunningRe = regexp.MustCompile(`[0-9]+ shells? still running`)

// waitingForBackgroundAgentRe matches Claude's footer while the main thread
// is blocked on background subagents (e.g. "✻ Waiting for 1 background agent
// to finish", "Waiting for 2 background agents to finish"). Like the spinner,
// Claude renders this only while actively waiting and redraws it away the
// instant the agents return, so it is self-evicting — a match anywhere in the
// current-screen tail is a live WORKING signal (no freshness check needed).
//
// The phrase is matched independent of the leading spinner glyph (robust across
// animation frames and after StripANSI) but anchored to the end of its line
// ((?m)…$, tolerating trailing tmux padding). Unlike "esc to interrupt", this
// phrasing is natural-language-plausible, so the end-of-line anchor rejects it
// when it appears embedded mid-sentence in prose/tool output (e.g. "…still
// waiting for 1 background agent to finish before I proceed."), which would
// otherwise risk pinning a genuinely-idle chat as WORKING.
var waitingForBackgroundAgentRe = regexp.MustCompile(`(?m)Waiting for [0-9]+ background agents? to finish[ \t]*$`)

// responseMarkerRe matches a Claude response / tool-result marker (⏺, U+23FA) at
// the start of a line. When one renders after a "shells still running" footer,
// the shell has since finished (Claude printed e.g. `⏺ Background command "…"
// completed`) and the footer is stale history, not a live signal.
var responseMarkerRe = regexp.MustCompile(`(?m)^[^\S\n]*\x{23FA}`)

// HasWorkingIndicator reports whether the pane shows an affirmative "the agent
// is busy" marker. Unlike working/idle inference from content-change timing,
// this is a positive signal: it stays true while a background shell runs or the
// spinner is active, so a static-but-busy pane is not misclassified as idle.
//
// The matcher is deliberately narrow: the "Esc to cancel" AskUserQuestion
// footer does not contain "esc to interrupt", so there is no overlap with
// HasQuestionPrompt (QUESTION still takes precedence over WORKING).
func HasWorkingIndicator(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	// Scope the match to the current screen (the last ~30 lines) rather than
	// the whole buffer, mirroring HasQuestionPrompt. Both callers feed history,
	// not just the live screen: the boss in-process PTY monitor scans the raw
	// ring buffer, where every bubbletea re-render frame accumulates as text
	// (StripANSI turns each frame's cursor-position sequences into fresh lines),
	// and the daemon tmux poller captures 1000 lines of scrollback. A spinner /
	// "N shell still running" footer from an earlier frame or a completed turn
	// therefore lingers as text after the pane has returned to an idle prompt.
	// Matching the full history keeps reporting WORKING until that stale footer
	// is evicted; restricting to the current-screen tail reflects only what the
	// pane shows now. A genuine marker always renders at the visible-pane
	// bottom, so it stays inside the window.
	tail := LastNLines(StripANSI(data), workingTailLines)

	// The active spinner is unambiguously live (it never lingers), so a match
	// anywhere in the current screen means WORKING.
	if escToInterruptRe.Match(tail) {
		return true
	}

	// A blocked-on-background-agents footer is a live spinner state, self-evicting
	// like escToInterrupt — bare match in the current-screen tail means WORKING.
	if waitingForBackgroundAgentRe.Match(tail) {
		return true
	}

	// The background-shell footer also renders as a completed-turn summary that
	// lingers in the transcript — even inside the current-screen window when the
	// run goes idle right after (the 30-line scope alone does not evict it). It
	// is live only when it is the bottom-most agent status line: a response
	// marker (⏺) rendered after the last such footer proves the shell finished
	// and Claude redrew, so the footer is stale.
	shellMatches := shellsRunningRe.FindAllIndex(tail, -1)
	if len(shellMatches) == 0 {
		return false
	}
	lastFooterEnd := shellMatches[len(shellMatches)-1][1]
	return !responseMarkerRe.Match(tail[lastFooterEnd:])
}

// workingTailLines bounds HasWorkingIndicator to the current screen. It matches
// the 30-line window HasQuestionPrompt uses so both detectors read the same
// "recent pane" region.
const workingTailLines = 30

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
