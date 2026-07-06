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

// DetectUsageLimit reports whether pane content shows the agent's usage-cap
// banner in the CLI-owned status region below the bottom-most prompt marker, and
// (when the banner carries a parseable reset time) the resolved reset timestamp.
//
// patterns is the caller-supplied, per-agent banner grammar (D8): the shared
// machinery here does ANSI stripping, tail slicing, and status-region anchoring;
// each plugin contributes only its own provider patterns. parseReset is injected
// (a thin adapter over BOS-164's agenterr.ParseResetTime) so statusdetect stays
// free of a hard dependency on the taxonomy package.
//
// Fail-safe by construction: no prompt marker (=> empty status region) or no
// pattern match yields limited=false. limited=true fires ONLY on an explicit
// pattern match inside the status region; ambiguity never yields a limit. When
// no reset time parses, hasReset is false and resetAt is the zero time.
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

	region := StatusRegionBelowPrompt(clean)
	if len(region) == 0 {
		return false, time.Time{}, false
	}

	for _, p := range patterns {
		if !p.Match(region) {
			continue
		}
		limited = true
		if parseReset != nil {
			if at, ok := parseReset(string(region), time.Now()); ok {
				resetAt = at
				hasReset = true
			}
		}
		return limited, resetAt, hasReset
	}
	return false, time.Time{}, false
}
