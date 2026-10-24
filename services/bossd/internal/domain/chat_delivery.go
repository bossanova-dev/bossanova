package domain

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ModalDetector checks whether a pane is showing a modal rather than a composer.
type ModalDetector func(context.Context, []byte) (bool, error)
type SendHooks struct {
	// BeforeSubmit runs after the payload is staged and immediately before the
	// first Enter, for external observation baselines.
	BeforeSubmit func()
	// Submitted reports whether the caller holds independent proof that the
	// agent took the payload (Claude's UserPromptSubmit receipt). When it
	// answers true the verifier reports the send submitted without reading the
	// pane further and never runs its clear-and-redeliver retry. Nil leaves the
	// pane verifier as the only signal.
	Submitted func() bool
	// ReceiptRequired says Submitted reads the agent's own record of every
	// turn it takes (Codex's rollout), so its absence is evidence too: a
	// composer that reads empty without a receipt is unconfirmed rather than
	// submitted, and one still holding the payload gets the clear-and-redeliver
	// retry. Claude's hook can fail to fire, so its receipt leaves this false.
	ReceiptRequired bool
}
type SubmitOutcome int

const (
	// OutcomeUnclassified is the zero value and means "this error did not come
	// from the submit verifier": a ready-marker timeout, a delivery command that
	// failed to run, an empty session name. Such a failure keeps the pre-existing
	// error handling — only the three verifier outcomes below are reclassified.
	OutcomeUnclassified SubmitOutcome = iota
	// OutcomeSubmitted: a positive signal confirmed the payload left the composer.
	OutcomeSubmitted
	// OutcomeNotSubmitted: the payload was typed and the wait budget elapsed with
	// it still held by the live composer (errSubmissionPending), or a tmux call in
	// the clear-and-redeliver retry failed. A genuine failure, but a safely
	// retryable one, because the pane state is known. A send refused before
	// typing is OutcomeComposerOccupied, not this.
	OutcomeNotSubmitted
	// OutcomeUnconfirmed: verification itself failed (capture-pane error or a
	// cancelled context), so the pane state is unknown and the payload may or may
	// not have been submitted.
	OutcomeUnconfirmed
	// OutcomeQueued: the payload left the composer into the agent's visible
	// message queue, so it was accepted mid-turn and will be consumed when the
	// running turn ends. A SUCCESS — a more precise OutcomeSubmitted, not a
	// softened OutcomeNotSubmitted — and the one outcome a caller must never
	// retry: the message is not lost, and a resend would queue a second copy
	// (BOS-599).
	OutcomeQueued
	// OutcomeBlockedByModal: the readiness gate saw a modal (an approval menu, a
	// question picker, an update interstitial) instead of a live composer, so
	// delivery was refused before anything was typed and before any Enter. It is
	// deliberately distinct from OutcomeNotSubmitted: nothing was delivered, so a
	// caller must surface the modal for a human rather than re-send into it
	// (BOS-600).
	OutcomeBlockedByModal
	// OutcomeComposerOccupied: the live composer already held a draft this
	// send did not type, so the send was refused before any key or Enter
	// (refuseOccupiedComposer). Like OutcomeBlockedByModal, nothing was
	// delivered: a prompt receipt seen afterwards belongs to someone else's
	// message, and the draft must not be cleared, since it may be an operator's.
	OutcomeComposerOccupied
)

// String renders the outcome for an error message or a log line.
func (o SubmitOutcome) String() string {
	switch o {
	case OutcomeSubmitted:
		return "submitted"
	case OutcomeNotSubmitted:
		return "not submitted"
	case OutcomeUnconfirmed:
		return "unconfirmed"
	case OutcomeQueued:
		return "queued"
	case OutcomeBlockedByModal:
		return "blocked by modal"
	case OutcomeComposerOccupied:
		return "composer occupied"
	default:
		return "unclassified"
	}
}

// OutcomeOf reads delivery evidence carried across the send error boundary.
func OutcomeOf(err error) SubmitOutcome {
	if err == nil {
		return OutcomeSubmitted
	}
	var carrier interface{ SubmitOutcome() SubmitOutcome }
	if errors.As(err, &carrier) {
		return carrier.SubmitOutcome()
	}
	return OutcomeUnclassified
}
func ChatSessionName(repoID, agentSessionID string) string {
	repoShort := repoID
	if len(repoShort) > 8 {
		repoShort = repoShort[:8]
	}
	chatShort := agentSessionID
	if len(chatShort) > 8 {
		chatShort = chatShort[:8]
	}
	return fmt.Sprintf("boss-%s-%s", repoShort, chatShort)
}
func WillSubmit(submit bool, text string) bool {
	return submit && strings.TrimSpace(text) != ""
}

// ConfigHomeKeys returns the config-home selectors in precedence order.
func ConfigHomeKeys() []string { return []string{"CODEX_HOME", "HOME"} }
