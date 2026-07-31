package tmux

// The modal half of the delivery readiness gate (BOS-600): the grammar seam
// (ModalDetector), the composer-row resolution the gate anchors on, and the
// refusal sentinel. It lives beside tmux.go rather than inside it because the
// gate is a self-contained concern with its own test file, and tmux.go is
// already the package's largest.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// modalProbeTimeout bounds ONE ModalDetector call. The detector is an
// out-of-process plugin RPC and it runs inside the readiness poll loop, ahead of
// the loop's own deadline test, so an unbounded probe against a wedged plugin
// would hold the send open past opts.deadline until the ambient context expires.
// Bounding each probe keeps the send's deadline authoritative; an overrun is
// treated as any other detector failure and fails open (BOS-600).
const modalProbeTimeout = 2 * time.Second

// ModalDetector reports whether a captured pane is showing a *modal* — an
// approval menu, a question picker, an update interstitial — rather than a live
// composer waiting for a message. It is the "is this a menu?" half of the
// readiness gate, and it is injected rather than implemented here on purpose:
// every agent's modal grammar belongs to that agent (BOS-600).
//
// The daemon wires this to the chat's agent plugin over
// AgentRunnerService.HasQuestionPrompt, so the codex glyph grammar stays in the
// codex plugin and Claude's stays in bossalib/statusdetect; this package never
// imports either. A nil detector disables the check (fail open), which keeps
// every existing caller and fake behaving exactly as before.
//
// An error is treated as "unknown, not a modal": a detector that cannot answer
// must not become a new way for delivery to fail.
type ModalDetector func(ctx context.Context, pane []byte) (bool, error)

// ErrBlockedByModal is the sentinel a delivery carries when the readiness gate
// saw a modal — an approval menu, a question picker, an update interstitial —
// instead of a live composer. Callers match it with errors.Is; OutcomeOf
// reports OutcomeBlockedByModal for the same error, which is what distinguishes
// a refusal from an ordinary "not submitted" (BOS-600).
var ErrBlockedByModal = errors.New("pane is showing a modal, not a composer")

// blockedByModalErr renders the refusal. The message states what did NOT happen
// (nothing typed, no Enter) because that is the operator's first question, and
// embeds the pane so the refusal can be judged without re-capturing a screen
// that has since moved on.
func blockedByModalErr(sessionName, pane string) error {
	return classifySubmit(OutcomeBlockedByModal, fmt.Errorf(
		"refusing to deliver into pane %q: %w; nothing was typed and no Enter was sent; "+
			"answer the prompt in the pane (boss attach) and retry; pane (truncated): %s",
		sessionName, ErrBlockedByModal, truncatePaneForError(pane)))
}

// composerRowIndex returns the index of the bottom-most pane row whose *own
// leading glyph* is marker, or -1 when no such row exists.
//
// This is the "a live input box is drawn" resolution that replaced a pane-wide
// strings.Contains (BOS-600). The old check accepted the marker glyph occurring
// anywhere in the capture — inside a box border, a tip line, or a menu entry —
// so a pane with no composer at all satisfied the gate and delivery typed into
// whatever was on screen. Anchoring the match to the start of a row is the same
// resolution bottomMostPromptMarkerIdx already uses for the submit verifier,
// specialised to the caller's configured marker rather than every known glyph.
//
// "Leading" tolerates a box border, because some agents draw the composer
// inside one and the marker is then the first glyph of the box's *contents*:
// trimComposerRowPrefix strips at most one border rune plus its padding. That
// widening can only ever admit a row this gate would otherwise reject, never
// reject one it accepted; the pre-change strings.Contains admitted those rows
// too, so it is not a regression in strictness — and a row admitted in error is
// exactly what the ModalDetector below is here to catch. The cost of getting it
// wrong is asymmetric: a false accept is caught by the detector, a false reject
// means this pane can never be delivered to at all.
//
// bottomMostPromptMarkerIdx, the submit verifier's row resolution, does NOT
// carry that tolerance, so on a boxed composer the two disagree: delivery
// proceeds and the verifier then reports the outcome unconfirmed rather than
// submitted. That is the safe side of its own contract (an unresolved composer
// withholds the retry Enter), and closing the gap properly means teaching the
// verifier to strip the row's TRAILING border too before it compares content
// against the payload — a change to submission semantics with no captured pane
// to justify it. Every real capture in testdata draws its composer unboxed;
// boxed rows appear only in hand-written test strings. Left divergent
// deliberately, and stated here so the next reader does not mistake it for an
// oversight.
//
// Row-anchoring is necessary but NOT sufficient: codex prepends "› " to row 1
// of an approval menu, so a menu row is still shaped like a composer row. The
// ModalDetector closes that gap; neither half is redundant.
//
// The scan covers the whole capture, which the caller takes with scrollback, so
// a prompt row that has scrolled off-screen still satisfies it. That is
// deliberate: this package cannot know the pane's visible height, and the
// submit verifier's bottomMostPromptMarkerIdx resolves rows the same way, so
// tightening one without the other would only split the two answers apart.
// Taking the bottom-most match keeps the newest drawn prompt authoritative.
func composerRowIndex(pane, marker string) int {
	if marker == "" {
		return -1
	}
	lines := strings.Split(pane, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.HasPrefix(trimComposerRowPrefix(lines[i]), marker) {
			return i
		}
	}
	return -1
}

// composerRowBorders are the box-drawing runes an agent may draw immediately
// left of its composer marker. Only vertical borders qualify: a corner or a
// horizontal run is a box EDGE, never a row that contains the input line.
const composerRowBorders = "│┃║|"

// trimComposerRowPrefix returns the row's content with surrounding whitespace
// and at most ONE leading vertical box border removed. One, not many: a nested
// or repeated border is not a shape any agent draws, and stripping greedily
// would start eating real content on a row that legitimately begins with a pipe.
func trimComposerRowPrefix(line string) string {
	trimmed := strings.TrimSpace(line)
	for _, border := range composerRowBorders {
		if prefix := string(border); strings.HasPrefix(trimmed, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, prefix))
		}
	}
	return trimmed
}

// paneShowsModal consults the supplied ModalDetector. A nil detector or a
// detector error means "not a modal" — the check fails open so an agent without
// a modal grammar, or a plugin that is momentarily unreachable, keeps delivering
// exactly as it did before this gate existed.
//
// Each probe is bounded by modalProbeTimeout: failing open is only safe if it
// also fails FAST, and this call sits ahead of the poll loop's deadline test.
// A probe that starts just under the readiness deadline therefore overruns it,
// so the true worst case for the wait is opts.deadline + modalProbeTimeout —
// one probe, never a compounding series. Clamping the probe to the remaining
// readiness time instead would be strictly worse: the clamp bites hardest when
// the plugin is slowest, and a probe cut short fails OPEN, which converts "we
// asked whether this is a menu and got an answer" into "we ran out of time, so
// type into it". Punctuality is not worth buying with the guarantee this gate
// exists to provide.
// The error itself is reported where a logger exists (the detector the daemon
// injects logs the first failure of each send); swallowing it here keeps the
// tmux client free of a logging dependency it has never had.
func paneShowsModal(ctx context.Context, detector ModalDetector, pane string) bool {
	if detector == nil {
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, modalProbeTimeout)
	defer cancel()
	modal, err := detector(probeCtx, []byte(pane))
	return err == nil && modal
}
