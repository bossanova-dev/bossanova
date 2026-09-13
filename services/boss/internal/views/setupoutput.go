package views

import (
	"regexp"
	"strings"
	"unicode"
)

// setupProgressPercentPattern is the percentage gate: a line only ever replaces
// the line before it if BOTH carry a percentage token.
//
// Compiled once at package level rather than per call. foldSetupLine runs once
// per inbound setup frame, and a download that redraws its bar ten times per
// second drives it hard enough that a per-call regexp compile would be the most
// expensive thing on the path.
var setupProgressPercentPattern = regexp.MustCompile(`\d{1,3}%`)

// setupPaneRetainedLines is how many setup-output elements the creating pane
// renders. It is the whole reason foldSetupLine exists (BOS-1237): before redraws
// were collapsed, one progress bar's ten frames filled this window exactly and
// evicted every other line. Named here, beside the fold, so the cap and the
// helper that compensates for it cannot drift apart — widening it does not make
// the fold unnecessary, it only raises how many redraws it takes to evict.
const setupPaneRetainedLines = 10

// setupSkeletonDigitRun stands in for one maximal run of ASCII digits inside a
// skeleton. '0' is deliberate and cannot collide with anything from the input:
// every digit in the line is consumed by the digit branch of setupLineSkeleton,
// so no literal digit is ever emitted, which leaves '0' free while keeping a
// skeleton readable in a failing test's output.
const setupSkeletonDigitRun = '0'

// foldSetupLine folds one inbound setup-script line into the New Session
// pane's output buffer, replacing the previous element when the line is a redraw
// of it and appending otherwise (BOS-1237). The replace path writes through
// buf's last index IN PLACE, so a caller still holding an earlier slice header
// over the same array observes the superseding text; do not snapshot buf and
// expect the snapshot to be stable.
//
// The setup child has no TTY — lib/bossalib/setupscript sets cmd.Stdout to an
// io.MultiWriter, so os/exec hands it a pipe — and both '\n' splitters
// downstream turn each redraw of a progress bar into its own wire frame. With an
// unconditional append, one 94.7 MiB download's ten redraws became ten rows and
// evicted every other line from a pane that keeps only the last
// setupPaneRetainedLines. This helper is where that is collapsed back to one
// advancing row.
//
// The replace-vs-append DECISION is pure — a function of (previous buffered
// line, incoming line) only — so the rule's boundary behaviour is testable
// without a tea.Model, a stream, or a daemon. The write is not: `buf[last] =
// line` stores below len, so every header long enough to include that index —
// every snapshot of buf itself — observes it. That
// is deliberately NOT the same as the aliasing an append already has: append
// writes at or past len, where a shorter header cannot observe it, and reallocates
// once capacity runs out. Safe at today's only call site — handleSetupScriptLine
// assigns straight back into the model it was handed and no earlier NewSessionModel
// copy is retained past the update — but it is a real constraint on the next one.
//
// Both consumers of the folded buffer see the fold, not just the capped pane:
// renderCreating shows the last setupPaneRetainedLines, while renderErr dumps the
// WHOLE buffer under "Setup script output:" on the failure screen. The error
// screen has no eviction problem of its own, so it gains nothing from folding and
// inherits the percentage-gate false positives documented on setupLineSupersedes
// — where the stakes are highest, because that transcript is what a failed setup
// is diagnosed from. Deliberate for now: the pane and the failure screen share one
// buffer, and splitting raw from displayed lines is the deferred producer-signal
// work (a replaces_previous field on SetupScriptOutput), not a rename away.
func foldSetupLine(buf []string, line string) []string {
	line = sanitizeSetupLine(line)
	if len(buf) == 0 {
		return append(buf, line)
	}
	last := len(buf) - 1
	if setupLineSupersedes(buf[last], line) {
		buf[last] = line
		return buf
	}
	return append(buf, line)
}

// sanitizeSetupLine keeps only the text after the last bare carriage return,
// which is what a terminal would have left visible after an in-place redraw.
//
// Defensive rather than load-bearing: the '\n'-only splitters make a CR-joined
// frame unlikely. It earns its place because without it such a frame would
// render as a run-together row AND would be compared as one blob, so the
// sanitized value is what gets buffered and what the next line is measured
// against.
func sanitizeSetupLine(line string) string {
	// Trailing CRs are either CRLF remnants (both splitters cut on '\n' only,
	// leaving the CR behind) or cursor returns with nothing drawn after them.
	// Either way they carry no replacement text, so treating one as a redraw
	// boundary would blank a real line down to "".
	line = strings.TrimRight(line, "\r")
	for i := len(line) - 1; i >= 0; i-- {
		if line[i] != '\r' {
			continue
		}
		// A CR immediately before an LF terminates a line rather than redrawing
		// one; keep scanning left for a bare CR. Both conditions here are
		// unreachable for frames the '\n' splitters produce, which is why neither
		// has a test: an embedded '\n' cannot occur, and the TrimRight above
		// already guarantees i+1 is in range. They stay because they keep those
		// facts local rather than invariants a later edit could silently break.
		if i+1 >= len(line) || line[i+1] == '\n' {
			continue
		}
		return line[i+1:]
	}
	return line
}

// setupLineSupersedes reports whether line is a redraw of prev — that is,
// whether both carry a percentage token and their skeletons are byte-equal.
//
// prev is always an already-sanitized buffered value, so both sides are measured
// in the same shape.
//
// KNOWN FALSE POSITIVE, wider than the acceptance tests scope. Digit runs are
// erased wholesale, so two lines that differ ONLY inside a digit run compare
// equal and the earlier one is lost. Measured, not theorised — folding
// "Fetching 2 of 10 packages: 20%" then "Fetching 3 of 10 packages: 45%" leaves
// ONE element, because both reduce to "Fetching0of0packages:0%". A per-item
// counter in that shape is a plausible thing for a real setup script to print.
//
// It is not tightenable from here without losing the fix: wholesale digit erasure
// is exactly what makes 10% and 100% compare equal, and what lets a bar whose
// transferred-byte count changes still fold. Erasing only the digits inside the
// matched percent token would keep 10%/100% but stop folding any bar that reports
// a moving byte count — the motivating case. The percentage gate keeps the
// exception narrow rather than closing it; closing it needs a supersedence signal
// from the producer (the deferred replaces_previous field), not a better guess
// about text. Reviewed and left open deliberately under BOS-1237.
func setupLineSupersedes(prev, line string) bool {
	if !setupProgressPercentPattern.MatchString(prev) || !setupProgressPercentPattern.MatchString(line) {
		return false
	}
	return setupLineSkeleton(prev) == setupLineSkeleton(line)
}

// setupLineSkeleton reduces a line to the shape it shares with its own redraws:
// every maximal run of an identical rune collapses to one instance, every
// maximal run of ASCII digits becomes one placeholder, and all whitespace is
// stripped.
//
// Rune-run collapsing is what makes a growing fill bar and its shrinking padding
// compare equal, and it must iterate runes rather than bytes: ■ is three
// distinct bytes (E2 96 A0), so a byte-wise pass finds no adjacent equal bytes
// inside a ■ run and would never collapse it. Digit erasure is what makes 10%
// and 100%, and a changing byte count, compare equal.
//
// Whitespace is stripped last, so it still separates two digit runs that were
// separated in the original line — `1 2` reduces to two placeholders, not one.
func setupLineSkeleton(line string) string {
	var b strings.Builder
	b.Grow(len(line))
	// prev tracks the previous rune of the ORIGINAL line, because run collapsing
	// is defined over the input; lastWasDigit tracks the emitted output, because
	// digit-run replacement is defined over the collapsed sequence.
	prev := rune(-1)
	lastWasDigit := false
	for _, r := range line {
		if r == prev {
			continue
		}
		prev = r
		if unicode.IsSpace(r) {
			// Not emitted, but it still ends whatever digit run preceded it.
			lastWasDigit = false
			continue
		}
		if r >= '0' && r <= '9' {
			if lastWasDigit {
				continue
			}
			b.WriteRune(setupSkeletonDigitRun)
			lastWasDigit = true
			continue
		}
		lastWasDigit = false
		b.WriteRune(r)
	}
	return b.String()
}
