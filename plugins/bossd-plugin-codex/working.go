package main

import (
	"bytes"
	"regexp"

	"github.com/recurser/bossalib/statusdetect"
)

// codexEscToInterrupt matches the stable half of codex's active-turn spinner
// footer ("• Working (34m 54s • esc to interrupt)"). Only the phrase is
// matched — never the gerund or the elapsed counter, both of which animate and
// both of which the sibling codexWorking regex shows the cost of pinning.
//
// A match needs no freshness check. Codex redraws the spinner away the instant
// the turn ends, so it cannot linger the way a completed-turn summary does.
// Claude's grammar happens to use the identical phrase
// (statusdetect.escToInterruptRe); the duplication here is deliberate, because
// this is the per-agent seam and codex must not inherit claude's rules by
// accident.
var codexEscToInterrupt = regexp.MustCompile(`esc to interrupt`)

// codexBackgroundWork matches codex's background-work footer — the row it draws
// above the composer while a child started with `/ps`-visible backgrounding is
// still alive:
//
//	1 background terminal running · /ps to view · /stop to close
//
// This is the signal a completed codex turn keeps ALL of its liveness in. The
// pane stops changing the moment the turn ends, so the poller's content-diff
// path flips the chat to IDLE after IdleThreshold even though a child process
// is still running — which is what this detector exists to prevent.
//
// The match is anchored to the START of a line, and that anchor is the whole
// defence against the failure mode this class already burned on (BOS-889: three
// panes stuck WORKING for hours on a footer nothing would ever evict). The
// footer owns its entire row, whereas the agent's own end-of-turn prose
// mentioning background work reads "- Left 1 background terminal running for
// the smoke test" — the count never opens the line. Without the anchor that
// sentence pins an idle chat WORKING until fresh output scrolls it out of the
// window, and nothing here would evict it, because unlike claude's lingering
// summary this footer has no terminator to look for.
//
// The noun is matched generically rather than enumerated as `terminals?`, for
// the reason statusdetect.backgroundWorkRunningRe documents: codex renders the
// count for whatever kind of background work it grows next, and an enumerated
// list re-runs this silent failure on the first new noun. "background" on the
// left and "running" on the right keep it tight enough that the generic noun
// costs nothing.
var codexBackgroundWork = regexp.MustCompile(`(?m)^[ \t]*[0-9]+ background [a-z]+ running\b`)

// codexWorkingTailLines bounds the detector to the current screen. It matches
// codexModalTailLines on purpose, so the question and working detectors read
// the same "what the pane shows now" region and cannot disagree about which
// frame they are looking at.
const codexWorkingTailLines = 30

// hasCodexWorkingIndicator reports whether the pane shows an affirmative "this
// chat is busy" marker — the live spinner, or a background child still running
// after the turn that spawned it returned.
//
// It is the positive signal that rescues a static-but-busy pane. The daemon's
// content-diff path can only see change, so it reads "turn finished, child
// still running" as idle; only an affirmative read of the chrome can tell that
// apart from a genuinely finished chat. The control fixture
// (testdata/panes/idle_no_background_work.txt) is an ended turn with no
// background work and must stay false, which is what keeps this from
// degenerating into "any ended codex turn is working".
//
// Trailing blank rows are trimmed BEFORE the tail is taken. `tmux capture-pane`
// pads its output to the pane height, and codex draws its composer inline
// rather than pinned to the pane bottom, so a tall terminal running a short
// session leaves dozens of blank rows below the chrome. Counting the window
// from the raw end would spend it entirely on padding and miss a footer that is
// plainly on screen — the same trim, for the same reason,
// statusdetect.NormalizeSpinner applies before computing spinnerPresent.
func hasCodexWorkingIndicator(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	// Normalize NBSP and friends to ASCII space up front: codex pads its
	// footers with them, and both matchers below assume an ASCII space.
	spaced := codexUnicodeSpace.ReplaceAll(statusdetect.StripANSI(data), []byte(" "))
	tail := statusdetect.LastNLines(bytes.TrimRight(spaced, " \t\r\n"), codexWorkingTailLines)
	return codexEscToInterrupt.Match(tail) || codexBackgroundWork.Match(tail)
}
