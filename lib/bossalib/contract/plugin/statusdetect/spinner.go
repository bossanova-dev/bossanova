package statusdetect

import (
	"bytes"
	"regexp"
	"strings"
	"unicode/utf8"
)

// --- Spinner-aware capture normalization (BOS-805) ---
//
// The tmux status poller decides "did this chat produce output?" by diffing
// consecutive pane captures. A running agent redraws its spinner footer several
// times a second — the elapsed counter ticks, the token counter grows, and
// Claude rotates the gerund ("Thinking…" → "Percolating…") — so a raw byte diff
// reports fresh output forever, even when the agent has emitted nothing to the
// pane for forty minutes. NormalizeSpinner removes exactly that volatility so a
// consumer can tell real work from a spinner.
//
// It lives here, as ONE shared pure function over pane bytes, rather than behind
// the per-agent HasWorkingIndicator RPC, because the poller calls that RPC only
// in the would-be-idle branch (deliberately, to avoid one RPC per chat per 3s
// tick), so it is not available on the ticks that matter. This comment used to
// give a second reason — that the codex runner answered the RPC with a
// hardcoded false — which is no longer true: codex owns a real detector now
// (plugins/bossd-plugin-codex/working.go). The two are not redundant. That RPC
// answers "is this chat busy", including background work the agent is no longer
// attending; spinnerPresent answers the narrower "is the agent computing right
// now", which a background child does not make true. The daemon already
// imports this package directly for the same kind of pane-shape read
// (IsLoginRequired, IsTransientAPIError), so both agents' grammars are carried
// here together.

// spinnerGlyphs is the closed set of leading runes an agent status footer line
// may open with. Claude animates its frame through several of these and codex
// uses the bullet; a line that does not open with one is ordinary output, never
// a status footer. It is deliberately an allowlist: widening it to "any
// non-letter rune" would let indented tool output whose first character happens
// to be punctuation qualify as a counter line and have its digits erased.
var spinnerGlyphs = map[rune]bool{
	'·': true, // U+00B7 middle dot — claude's working frame and separator
	'•': true, // U+2022 bullet — codex's working frame and separator
	'✻': true, // U+273B
	'✽': true, // U+273D
	'✢': true, // U+2722
	'✳': true, // U+2733
	'✶': true, // U+2736
	'∗': true, // U+2217
	'*': true, // ASCII fallback frame
}

// claudeLiveSpinnerRe matches the complete current Claude spinner row. The
// ellipsis and leading parenthesized duration distinguish dual-use frame glyphs
// from ordinary bullets; custom multi-word spinner verbs are allowed.
var claudeLiveSpinnerRe = regexp.MustCompile(`(?m)^[ \t]*[·✻✽✢✳✶∗*] [^…\n]+… \((?:[0-9]+h )?(?:[0-9]+m )?[0-9]+s(?: · [^\n]*)?\)[ \t]*$`)

// claudeSpinnerIsLive shares the current-row freshness rule between both
// detectors: only a completed-turn summary below the last spinner retires it.
func claudeSpinnerIsLive(tail []byte) bool {
	return lastFooterIsLive(tail, claudeLiveSpinnerRe, completedTurnSummaryBelow)
}

func completedTurnSummaryBelow(rest []byte) bool {
	for line := range bytes.SplitSeq(rest, []byte("\n")) {
		if isCompletedTurnSummary(line) {
			return true
		}
	}
	return false
}

// spinnerElapsedRe matches the elapsed-time counter of a status footer in the
// ONE position that grammar puts it: directly after "for" — "Cooked for 48s",
// "Thought for 1m", "Ran for 1.5s". The unit must terminate the word, so
// "1 shell" and "1.2k" do not match — a background-shell count is real state,
// not a redraw artefact.
//
// The "for " anchor is load-bearing and is what keeps this from swallowing
// genuine output. A bare duration matcher would rewrite any glyph-led line
// carrying a number-plus-unit anywhere on it, and spinnerGlyphs deliberately
// includes DUAL-USE runes (· and • are each an agent's working frame AND its
// ordinary output bullet; * is a markdown bullet). So "• Ran make test in 12s"
// — real codex output — would normalize to "• Ran make test in #", making a
// chat that is visibly emitting output read as producing none. That is the
// silent false negative this file's doc comment calls the worse failure.
var spinnerElapsedRe = regexp.MustCompile(`\bfor [0-9]+(?:\.[0-9]+)?[smh]\b`)

// spinnerTokenCountRe matches the token-usage counter Claude renders inside the
// spinner footer ("↑ 1.2k tokens"). It grows on every redraw for the same
// reason the elapsed counter does.
var spinnerTokenCountRe = regexp.MustCompile(`\b[0-9]+(?:\.[0-9]+)?[kKmM]? tokens\b`)

// claudeRunningToolRe matches the header of a foreground tool Claude is still
// running: "⏺ Counting files · 7s". Claude ticks the trailing elapsed timer and
// blinks the ⏺ glyph, blanking it on alternate frames, so the two-space form is
// the same row (captured from Claude Code 2.1.312 in
// testdata/claude_spinner_redraw_a_plain.txt). Only the glyph and the timer are
// volatile; the description is real state and is preserved.
var claudeRunningToolRe = regexp.MustCompile(`^(?:⏺ |  )(\S[^\n]*) · (?:[0-9]+h )?(?:[0-9]+m )?[0-9]+s[ \t]*$`)

// spinnerCounterPlaceholder replaces a volatile counter token. Any fixed string
// works — the normalized buffer is only ever compared against another
// normalized buffer, never parsed.
const spinnerCounterPlaceholder = "#"

// Spinner line classes, in descending order of how volatile the line is.
const (
	// spinnerNone: an ordinary line, preserved byte-for-byte.
	spinnerNone = iota
	// spinnerLive: a current Claude row, either agent's legacy "esc to
	// interrupt" row, or Claude's background-agent wait. The whole row is
	// volatile; liveness separately checks freshness for lingering shapes.
	spinnerLive
	// spinnerCounter: a glyph-led line in a recognized status-line grammar —
	// the completed-turn summary "✻ Cooked for 48s · 1 shell still running" or
	// the usage counter "↑ 1.2k tokens". Only the counter is volatile — the
	// rest of the line ("1 shell" → "2 shells") is real state — so only the
	// anchored counter tokens are replaced. Glyph-led output that merely
	// mentions a duration is NOT this class; see spinnerElapsedRe.
	spinnerCounter
	// spinnerToolTimer: a running foreground tool header. Its blinking glyph
	// and elapsed timer are volatile; its description is real state.
	spinnerToolTimer
)

// classifySpinnerLine reports which spinner class a single line (no trailing
// newline, Unicode spaces already normalized to ASCII) belongs to.
func classifySpinnerLine(line []byte) int {
	// escToInterrupt covers BOTH agents: claude renders
	// "· Working (3s · esc to interrupt)" with U+00B7 separators and codex
	// renders "• Working (3s • esc to interrupt)" with U+2022. Matching the
	// phrase rather than the punctuation is what makes one rule serve both.
	if claudeLiveSpinnerRe.Match(line) || escToInterruptRe.Match(line) || waitingForBackgroundAgentRe.Match(line) {
		return spinnerLive
	}
	if claudeRunningToolRe.Match(line) {
		return spinnerToolTimer
	}
	trimmed := bytes.TrimLeft(line, " \t")
	if len(trimmed) == 0 {
		return spinnerNone
	}
	r, _ := utf8.DecodeRune(trimmed)
	if !spinnerGlyphs[r] {
		return spinnerNone
	}
	// The leading glyph alone is NOT enough, because the glyph set is dual-use:
	// · and • each serve as an agent's working frame and as its ordinary output
	// bullet. The line must also carry one of the two recognized status-line
	// counter grammars — the "Cooked for 48s" elapsed form or the "↑ 1.2k
	// tokens" usage form. A duration sitting in ordinary prose position
	// ("• Ran make test in 12s") matches neither and stays byte-for-byte.
	if !spinnerElapsedRe.Match(trimmed) && !spinnerTokenCountRe.Match(trimmed) {
		return spinnerNone
	}
	return spinnerCounter
}

// NormalizeSpinner returns a copy of the captured pane content with the volatile
// agent-spinner regions removed, plus whether a LIVE spinner is on the current
// screen.
//
// The normalization pass and the spinnerPresent verdict are computed separately.
// They ask different questions of the same bytes — "is this line volatile?"
// versus "is an agent working right now?" — and only the second needs the
// freshness checks for lingering Claude rows and background-agent footers, so folding
// them back into one pass would reintroduce that bug.
//
// normalized is for DIFFING ONLY. It is a comparison key, not display text: two
// captures whose normalized forms are equal differ only in spinner chrome, and
// the caller should treat that as "no substantive output". It is never parsed,
// rendered, or persisted.
//
// Normalization is anchored to the known spinner shapes and touches nothing
// else, because over-normalizing fails SILENTLY: a swallowed one-character
// change makes a chat that IS producing output read as stalled, which is worse
// than the permanently-fresh reading this replaces. Concretely, only lines
// classified as spinnerLive or spinnerCounter are rewritten; every other line is
// copied byte-for-byte, and the line count and per-line indent are preserved so
// a caller slicing a tail off the normalized buffer sees the same window the raw
// capture would give.
//
// spinnerPresent is scoped to the last workingTailLines rendered lines after
// trimming pane padding. Current Claude rows and background-agent waits use the
// same freshness predicates as HasWorkingIndicator; legacy interrupt rows are
// self-evicting. Completed summaries alone never prove current computation.
func NormalizeSpinner(data []byte) (normalized []byte, spinnerPresent bool) {
	if len(data) == 0 {
		return nil, false
	}

	// Normalize Unicode space separators (NBSP etc.) to ASCII space up front.
	// Claude and codex both pad their footers with NBSP, and every matcher below
	// assumes an ASCII space. Doing it over the whole buffer rather than per
	// match keeps the rewrite offsets aligned (NBSP is multi-byte), and is
	// harmless for the diff because both sides of the comparison get it.
	spaced := unicodeSpaceRe.ReplaceAll(data, []byte(" "))

	var out bytes.Buffer
	out.Grow(len(spaced))
	for _, line := range strings.SplitAfter(string(spaced), "\n") {
		text := strings.TrimSuffix(line, "\n")
		newline := line[len(text):]
		switch classifySpinnerLine([]byte(text)) {
		case spinnerLive:
			// Keep the indent and the newline, drop the content. Same
			// shape-preserving drop stripTipLines uses: the buffer's line count
			// and column structure stay intact, so nothing downstream shifts.
			out.WriteString(text[:len(text)-len(strings.TrimLeft(text, " \t"))])
			out.WriteString(newline)
		case spinnerCounter:
			// Both rewrites carry their anchor through ("for 48s" → "for #",
			// "1.2k tokens" → "# tokens"), so only the counter itself is
			// erased. Any OTHER number on the line — a background-shell count,
			// a duration quoted in prose — is real state and survives.
			rewritten := spinnerTokenCountRe.ReplaceAllString(text, spinnerCounterPlaceholder+" tokens")
			rewritten = spinnerElapsedRe.ReplaceAllString(rewritten, "for "+spinnerCounterPlaceholder)
			out.WriteString(rewritten)
			out.WriteString(newline)
		case spinnerToolTimer:
			out.WriteString(claudeRunningToolRe.ReplaceAllString(text, "  ${1} · "+spinnerCounterPlaceholder))
			out.WriteString(newline)
		default:
			out.WriteString(line)
		}
	}

	tail := LastNLines(bytes.TrimRight(spaced, " \t\r\n"), workingTailLines)
	spinnerPresent = escToInterruptRe.Match(tail) || claudeSpinnerIsLive(tail) || backgroundAgentFooterIsLive(tail)

	return out.Bytes(), spinnerPresent
}
