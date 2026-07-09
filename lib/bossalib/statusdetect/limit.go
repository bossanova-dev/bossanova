package statusdetect

import (
	"bytes"
	"regexp"
	"time"
)

// limitTailLines bounds usage-limit detection to the current screen, matching
// the 30-line window HasQuestionPrompt/HasWorkingIndicator use but with extra
// headroom: a usage-cap banner can render as several rows in the status region
// below the input box, so a slightly larger tail keeps the whole banner in view
// while staying O(tail).
const limitTailLines = 40

// promptMarkers are the leading glyphs an agent's input box renders (Claude
// "❯", Codex "›", and a generic ">"). This set is a DELIBERATE, small
// duplication of services/bossd/internal/tmux/tmux_submit_verify.go's
// promptMarkers / hasPromptMarker: plugins consume statusdetect but cannot
// import that host-only tmux package, and the "tmux delivery cross-package
// tests" lesson warns that refactoring the submit-verifier to export a shared
// helper destabilizes internal/session integration tests. So both sides keep an
// independently-tested copy of the bottom-most-prompt-marker anchoring; if you
// change one, change the other. Do NOT modify tmux_submit_verify.go from here.
var promptMarkers = [][]byte{[]byte("❯"), []byte("›"), []byte(">")}

// hasPromptMarker reports whether a trimmed pane row begins with one of the
// input-box prompt glyphs. Mirror of tmux_submit_verify.go hasPromptMarker
// (see promptMarkers note above).
func hasPromptMarker(trimmed []byte) bool {
	for _, marker := range promptMarkers {
		if bytes.HasPrefix(trimmed, marker) {
			return true
		}
	}
	return false
}

// StatusRegionBelowPrompt returns the CLI-owned status region: the bytes
// strictly BELOW the bottom-most row carrying an input-box prompt marker. That
// bottom-most marker is the live input box; everything under it is footer /
// statusline / banner chrome the CLI owns and the user does not type. Restricting
// usage-limit matching to this region is the D13 anchoring guard: a limit
// message quoted in the transcript body (above the input box) must never trigger
// detection.
//
// clean is expected to be ANSI-stripped (StripANSI, which also normalizes NBSP
// to ASCII space). When no prompt marker is present at all, there is no status
// region to match against, so it returns nil — the fail-safe "nothing to match
// => not limited" path.
func StatusRegionBelowPrompt(clean []byte) []byte {
	lines := bytes.Split(clean, []byte("\n"))
	markerIdx := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if hasPromptMarker(bytes.TrimSpace(lines[i])) {
			markerIdx = i
			break
		}
	}
	if markerIdx == -1 || markerIdx == len(lines)-1 {
		// No marker, or the marker is the last line (empty region below it).
		return nil
	}
	region := bytes.Join(lines[markerIdx+1:], []byte("\n"))
	if len(bytes.TrimSpace(region)) == 0 {
		return nil
	}
	return region
}

// bannerLineLeadAllowance bounds how far into a line the cap-notice pattern may
// begin for the "last-turn output above the input box" region (see
// DetectUsageLimit region 3). The real CLI renders the notice as its own line,
// so the match starts at (or very near) the line start — allowing only a short
// leading subject like "You've " / "Claude ". A limit phrase buried deeper in a
// line is prose that quotes or discusses a limit ("The model said: …", "We
// should discuss the … usage limit policy"), which must NOT trip detection. This
// is the D13 guard, refined for inline rendering: anchoring by position instead
// of by "below the input box".
const bannerLineLeadAllowance = 12

// escToCancelRe matches the Claude decision-modal footer affordance ("Enter to
// confirm · Esc to cancel"). It is deliberately narrower than the working
// spinner's "esc to interrupt" (see question.go escToInterruptRe) so an active
// turn is never mistaken for the blocking usage-limit modal.
var escToCancelRe = regexp.MustCompile(`(?i)esc to cancel`)

// isDecisionModal reuses question.go's numberedOptionRe to count the menu's
// option rows.

// DetectUsageLimit reports whether pane content shows the agent's usage-cap
// banner in one of the three regions the real CLI renders it, and (when the
// banner carries a parseable reset time) the resolved reset timestamp:
//
//  1. The CLI-owned status region strictly below the bottom-most prompt marker
//     (a true footer/statusline). Matched by substring — no prose lives here.
//  2. The interactive "What do you want to do?" decision modal (isDecisionModal):
//     when the blocking usage-limit menu is up, its selection cursor ❯
//     masquerades as the input box and pushes the banner above it, so the whole
//     visible screen is scanned. The modal is itself CLI-owned chrome.
//  3. The last turn's output directly above the live input box — how a dismissed
//     cap surfaces (the notice is the most recent inline output). Prose can live
//     here, so a pattern counts only when it BEGINS its line
//     (bannerLineLeadAllowance): the D13 anchoring guard, by position.
//
// patterns is the caller-supplied, per-agent banner grammar (D8): the shared
// machinery here does ANSI stripping, tail slicing, and region anchoring; each
// plugin contributes only its own provider patterns. parseReset is injected (a
// thin adapter over BOS-164's agenterr.ParseResetTime) so statusdetect stays
// free of a hard dependency on the taxonomy package.
//
// Fail-safe by construction: no prompt marker and no decision modal yields
// limited=false; a limit phrase buried in prose above the input box never trips
// it. limited=true fires ONLY on an explicit pattern match inside one of the
// three regions; ambiguity never yields a limit. When no reset time parses,
// hasReset is false and resetAt is the zero time.
func DetectUsageLimit(
	pane []byte,
	patterns []*regexp.Regexp,
	parseReset func(raw string, now time.Time) (time.Time, bool),
) (limited bool, resetAt time.Time, hasReset bool) {
	if len(pane) == 0 || len(patterns) == 0 {
		return false, time.Time{}, false
	}

	// StripANSI also normalizes NBSP (U+00A0) / narrow NBSP (U+202F) to ASCII
	// space, so patterns can assume ordinary spacing.
	clean := StripANSI(pane)
	clean = LastNLines(clean, limitTailLines)

	// Region 1: footer below the live input box (substring match, CLI-owned).
	if region := StatusRegionBelowPrompt(clean); len(region) > 0 {
		if lim, at, has := matchLimit(region, patterns, parseReset, false); lim {
			return lim, at, has
		}
	}
	// Region 2: the blocking decision modal. When it is up, the whole screen is
	// CLI-owned chrome, so scan it all (substring match).
	if isDecisionModal(clean) {
		if lim, at, has := matchLimit(clean, patterns, parseReset, false); lim {
			return lim, at, has
		}
	}
	// Region 3: the last turn's output directly above the live input box
	// (position-anchored match to keep quoted/discussed limits out).
	if tail := LastTurnAbovePrompt(clean); len(tail) > 0 {
		if lim, at, has := matchLimit(tail, patterns, parseReset, true); lim {
			return lim, at, has
		}
	}
	return false, time.Time{}, false
}

// matchLimit runs patterns over region and, on the first match, parses the reset
// time from the region text. When anchored is true a pattern counts only if it
// begins a line within bannerLineLeadAllowance of the (whitespace-trimmed) line
// start — the position-based D13 guard for prose-bearing regions.
func matchLimit(
	region []byte,
	patterns []*regexp.Regexp,
	parseReset func(raw string, now time.Time) (time.Time, bool),
	anchored bool,
) (limited bool, resetAt time.Time, hasReset bool) {
	matched := false
	if anchored {
		for line := range bytes.SplitSeq(region, []byte("\n")) {
			trimmed := bytes.TrimLeft(line, " \t")
			for _, p := range patterns {
				if loc := p.FindIndex(trimmed); loc != nil && loc[0] <= bannerLineLeadAllowance {
					matched = true
					break
				}
			}
			if matched {
				break
			}
		}
	} else {
		for _, p := range patterns {
			if p.Match(region) {
				matched = true
				break
			}
		}
	}
	if !matched {
		return false, time.Time{}, false
	}
	if parseReset != nil {
		if at, ok := parseReset(string(region), time.Now()); ok {
			resetAt = at
			hasReset = true
		}
	}
	return true, resetAt, hasReset
}

// isDecisionModal reports whether the cleaned pane shows the CLI's blocking
// usage-limit decision menu ("What do you want to do? / 1. … / 2. … / Esc to
// cancel"). It requires the esc-to-cancel affordance (not the working spinner's
// esc-to-interrupt) plus at least two numbered option rows, so an ordinary
// working turn or a lone numbered list never counts.
func isDecisionModal(clean []byte) bool {
	if !escToCancelRe.Match(clean) {
		return false
	}
	options := 0
	for line := range bytes.SplitSeq(clean, []byte("\n")) {
		if numberedOptionRe.Match(line) {
			options++
			if options >= 2 {
				return true
			}
		}
	}
	return false
}

// LastTurnAbovePrompt returns the contiguous block of lines directly above the
// bottom-most prompt marker (the live input box) — BUT ONLY when the turn
// boundary that closes that block above is a user-command echo (a "> …" / "❯ …"
// line carrying content), i.e. the block is the direct output of the user's last
// command. That is where the CLI renders a cap notice inline (a slash command's
// system output has no ⏺ assistant-turn glyph). When the boundary is instead an
// assistant turn (⏺) or there is no boundary at all, the block is model prose —
// which can quote or discuss a limit verbatim (the D13 hazard, whose text is
// indistinguishable from a real banner) — so nil is returned and the caller
// never matches there. A stale banner behind a later turn boundary is likewise
// excluded. Returns nil when there is no prompt marker or the block is blank.
func LastTurnAbovePrompt(clean []byte) []byte {
	lines := bytes.Split(clean, []byte("\n"))
	markerIdx := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if hasPromptMarker(bytes.TrimSpace(lines[i])) {
			markerIdx = i
			break
		}
	}
	if markerIdx <= 0 {
		return nil
	}
	start := markerIdx
	boundaryIsCommandEcho := false
	for j := markerIdx - 1; j >= 0; j-- {
		trimmed := bytes.TrimSpace(lines[j])
		if hasPromptMarker(trimmed) {
			boundaryIsCommandEcho = markerHasContent(trimmed)
			break
		}
		if bytes.HasPrefix(trimmed, []byte("⏺")) {
			break
		}
		start = j
	}
	if !boundaryIsCommandEcho || start == markerIdx {
		return nil
	}
	region := bytes.Join(lines[start:markerIdx], []byte("\n"))
	if len(bytes.TrimSpace(region)) == 0 {
		return nil
	}
	return region
}

// markerHasContent reports whether a trimmed prompt-marker line carries content
// after its glyph — i.e. it is a command echo ("> /boss switch", "❯ 1. …") rather
// than a bare, empty input box ("❯"). Used to tell the user's last-typed command
// (a live-notice boundary) apart from an idle prompt.
func markerHasContent(trimmed []byte) bool {
	for _, marker := range promptMarkers {
		if bytes.HasPrefix(trimmed, marker) {
			return len(bytes.TrimSpace(trimmed[len(marker):])) > 0
		}
	}
	return false
}
