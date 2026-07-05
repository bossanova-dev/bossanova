package tmux

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// This file holds the submission-verification and pane-scraping heuristics used
// by the Send* delivery paths in tmux.go: after an Enter is sent, poll
// capture-pane and decide — shape-aware, failing toward "still pending" — whether
// the payload actually left the composer, retrying the Enter once before erroring
// loudly. The cluster is churn-prone (its glyph grammars track the Claude/Codex
// TUIs) so it lives beside, but separate from, the stable tmux CLI wrappers.

// errSubmissionPending is the sentinel returned by waitForSubmission when the
// wait budget elapses with the payload still sitting at the prompt (a confirmed
// "not submitted" result). verifyWithEnterRetry retries the Enter only for this
// sentinel — an infrastructure failure (capture-pane error, context
// cancellation) is NOT a confirmed still-pending, so it must not fire a stray
// extra Enter into a pane that may already have submitted.
var errSubmissionPending = errors.New("payload still present at the tmux prompt")

// verifyWithEnterRetry confirms the payload was submitted, retrying the Enter
// once if the TUI swallowed the first one. It verifies within the wait budget;
// on failure it sends a single additional Enter and re-verifies within a fresh
// budget. If that still fails it returns a loud error naming the session and
// making clear the payload was not submitted even after the retry. Used by both
// the sendPlan and sendLine submit paths; payload is the raw (untrimmed) text so
// waitForSubmission can detect its shape.
func (c *Client) verifyWithEnterRetry(ctx context.Context, sessionName, payload string, opts sendPlanOpts) error {
	err := c.waitForSubmission(ctx, sessionName, payload, opts.submitVerifyWait, opts.submitVerifyTick)
	if err == nil {
		return nil
	}
	// Only retry on a CONFIRMED still-pending result. A capture-pane failure or
	// a cancelled context is not evidence the payload is unsubmitted, so firing
	// another Enter could inject a stray keystroke into a pane that already
	// submitted — surface the infrastructure error instead.
	if !errors.Is(err, errSubmissionPending) {
		return err
	}
	// The first Enter may have been swallowed by the TUI (the failure mode this
	// guards against). Send one more and re-verify before giving up.
	if err := c.sendEnter(ctx, sessionName); err != nil {
		return err
	}
	if err := c.waitForSubmission(ctx, sessionName, payload, opts.submitVerifyWait, opts.submitVerifyTick); err != nil {
		return fmt.Errorf("payload not submitted to tmux session %q after an Enter retry: %w", sessionName, err)
	}
	return nil
}

// waitForSubmission polls capture-pane until the payload is confirmed submitted
// or the wait budget elapses, at which point it returns a loud error. The
// confirmation predicate is shape-aware: a single-line payload is submitted when
// its trimmed text has left the bottom-most prompt row (lineStillAtPrompt), and
// a multi-line payload — which never sits as one matchable row — is submitted
// when multiLineSubmitted reads a positive signal off the pane. Both fail toward
// "still pending" so a delivery that loaded but never executed surfaces as an
// error rather than a silent no-op.
func (c *Client) waitForSubmission(ctx context.Context, sessionName, payload string, waitFor, tickEvery time.Duration) error {
	if tickEvery <= 0 {
		tickEvery = 100 * time.Millisecond
	}

	trimmed := strings.TrimSpace(payload)
	multiLine := strings.ContainsAny(trimmed, "\r\n")

	deadline := time.NewTimer(waitFor)
	defer deadline.Stop()

	ticker := time.NewTicker(tickEvery)
	defer ticker.Stop()

	for {
		pane, err := c.CapturePane(ctx, sessionName)
		if err != nil {
			return fmt.Errorf("verify command submission: %w", err)
		}
		submitted := false
		if multiLine {
			submitted = multiLineSubmitted(pane, payload)
		} else {
			submitted = !lineStillAtPrompt(pane, trimmed)
		}
		if submitted {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("command was not submitted; %q is still present at the tmux prompt: %w", trimmed, errSubmissionPending)
		case <-ticker.C:
		}
	}
}

// multiLineSubmitted reports whether the multi-line payload pasted into the
// composer has been submitted. A multi-line plan never sits as one matchable
// prompt row, so instead of checking for its literal text we read two positive
// signals off the bottom-most prompt-marker row (the live input box): agent
// activity rendered below it (the agent accepted the paste and started working)
// OR a cleared composer (the marker row holds only the prompt glyph, the input
// box emptied on submit). Absent either signal — or a prompt marker at all — it
// fails toward "still pending" so a paste that loaded but never executed
// surfaces as an error rather than a silent no-op.
//
// The agent-activity scan is payload-aware: a plan pasted into the composer but
// NOT submitted still shows its own lines below the marker, and a plan line that
// leads with an agent-activity glyph (e.g. a "•" or "·" bullet) would otherwise
// be mistaken for the agent's own activity — a false "submitted" that resurrects
// the silent no-op this guards against. So a below-marker row that matches one
// of the payload's own lines is treated as still-in-composer content, never as
// activity; only a distinct row the agent itself rendered qualifies.
func multiLineSubmitted(pane, payload string) bool {
	lines := strings.Split(pane, "\n")

	markerIdx := bottomMostPromptMarkerIdx(lines)
	if markerIdx == -1 {
		return false
	}

	// The payload's own trimmed, non-empty lines. A below-marker row matching
	// one of these is the pasted plan still sitting in the composer, not agent
	// output, so it must not be read as activity.
	payloadLines := make(map[string]struct{})
	for _, l := range strings.Split(payload, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			payloadLines[t] = struct{}{}
		}
	}

	// Agent activity below the marker: the paste was accepted and the agent is
	// responding, so the payload has left the prompt (mirrors lineStillAtPrompt).
	// Skip rows that are the payload's own lines echoed in the composer, but note
	// that the payload is still visible below the marker so the cleared-composer
	// signal below cannot fire on it.
	payloadStillBelowMarker := false
	for _, l := range lines[markerIdx+1:] {
		trimmed := strings.TrimSpace(l)
		if _, isPayload := payloadLines[trimmed]; isPayload {
			payloadStillBelowMarker = true
			continue
		}
		if isAgentActivity(trimmed) {
			return true
		}
	}

	// Cleared composer: the input box emptied on submit, leaving only the glyph.
	// Only trust an empty marker row when NONE of the payload's own lines are
	// still visible below it: a composer that renders the prompt glyph on its own
	// row ABOVE the pasted continuation lines (the "❯\nline one\nline two" shape,
	// or a payload with a blank leading line) is an unsubmitted composer, not a
	// cleared one — trusting the empty glyph there would report a swallowed paste
	// as submitted and resurrect the silent no-op.
	if payloadStillBelowMarker {
		return false
	}
	return promptRowIsEmpty(strings.TrimSpace(lines[markerIdx]))
}

func lineStillAtPrompt(pane, line string) bool {
	lines := strings.Split(pane, "\n")

	// Locate the bottom-most prompt-marker row: the live input box. Everything
	// below it is footer — a separator rule, the "model | cwd" line, and any
	// custom statusline rows, which are arbitrary user text (e.g. "PR #133",
	// "◉ xhigh · /effort") that no fixed predicate can enumerate — so the scan
	// skips all of it while finding the marker. The empty prompt ("❯" with no
	// text) is a marker too, so a cleared/submitted input reports false below.
	markerIdx := bottomMostPromptMarkerIdx(lines)
	if markerIdx == -1 {
		return false
	}

	// If agent activity appears below that marker, the marker is a submitted
	// prompt echoed into the transcript ("❯ do the thing" above the agent's
	// response or working spinner) with no fresh input box drawn yet, so the
	// payload has already left the prompt. Footer and statusline rows are not
	// agent activity, so a still-pending payload beneath a custom statusline is
	// never mistaken for a submitted one.
	for _, l := range lines[markerIdx+1:] {
		if isAgentActivity(strings.TrimSpace(l)) {
			return false
		}
	}
	return strings.Contains(strings.TrimSpace(lines[markerIdx]), line)
}

// promptMarkers are the leading glyphs an agent's input box renders. Shared by
// hasPromptMarker and promptRowIsEmpty so the list lives in one place.
var promptMarkers = []string{"❯", "›", ">"}

// bottomMostPromptMarkerIdx returns the index of the bottom-most row carrying an
// input-box prompt marker, or -1 if none is present. It is the live input box:
// everything below it is footer / statusline chrome, so submission checks scan
// from this row. Shared by lineStillAtPrompt and multiLineSubmitted.
func bottomMostPromptMarkerIdx(lines []string) int {
	for i := len(lines) - 1; i >= 0; i-- {
		if hasPromptMarker(strings.TrimSpace(lines[i])) {
			return i
		}
	}
	return -1
}

// hasPromptMarker reports whether a trimmed pane row begins with one of the
// input-box prompt indicators.
func hasPromptMarker(text string) bool {
	for _, marker := range promptMarkers {
		if strings.HasPrefix(text, marker) {
			return true
		}
	}
	return false
}

// promptRowIsEmpty reports whether a trimmed prompt-marker row carries only the
// marker glyph with no payload text after it — a cleared/submitted composer.
func promptRowIsEmpty(trimmed string) bool {
	for _, marker := range promptMarkers {
		if rest, ok := strings.CutPrefix(trimmed, marker); ok {
			return strings.TrimSpace(rest) == ""
		}
	}
	return false
}

// agentActivityMarkers are the leading glyphs that mark a pane row as agent
// activity below the input box — an assistant/tool response, tool output, or a
// thinking/working spinner — rather than input-box footer or statusline chrome.
// The verifier runs against both Claude Code and Codex panes, so it covers
// both grammars: the Claude working/output markers statusdetect already trusts
// (lib/bossalib/statusdetect/question.go optionStopMarkers: ⎿ ⏺ · ✻) plus the
// "✽" spinner frame, and Codex's working bullet (plugins/bossd-plugin-codex/
// question.go codexWorking: "• Working (…)"). This lets the predicate recognise
// the activity row each agent renders immediately after accepting a line and
// before any response body appears.
var agentActivityMarkers = []string{
	"⏺", // Claude response / tool-use bullet (U+23FA)
	"⎿", // Claude tool-result branch (U+23BF)
	"·", // Claude working spinner (U+00B7)
	"✻", // Claude thinking spinner (U+273B)
	"✽", // Claude thinking spinner (U+273D)
	"•", // Codex working bullet (U+2022)
}

// isAgentActivity reports whether a trimmed pane row begins with an agent
// activity marker. Matching only the leading glyph keeps custom statusline rows
// safe (e.g. "◉ xhigh · /effort" has a mid-row "·" but does not start with
// one). It is deliberately conservative on unknown rows: misclassifying footer
// chrome as activity would let lineStillAtPrompt report a still-pending payload
// as submitted — the silent cron no-op this guards against — so only these
// distinctive glyphs qualify and arbitrary footer/statusline text never does.
func isAgentActivity(text string) bool {
	for _, marker := range agentActivityMarkers {
		if strings.HasPrefix(text, marker) {
			return true
		}
	}
	return false
}
