// Package chatdelivery holds the wording a chat delivery's outcome is reported
// in, for every surface that renders one.
//
// It exists because that wording is not decoration: notice_text is the ONE field
// that survives every converter between bossd and a caller, so the resend
// guidance appended to it is also the only tell a proxied response carries that
// a submit was UNCONFIRMED (the proxy response has no delivery_state mirror).
// Surfaces therefore MATCH on the sentence, and a retyped copy is a behavioural
// coupling that no compiler or test can see: rewording the daemon's copy alone
// silently stops the CLI recognising an unconfirmed delivery, which drops the
// headline and the pane name the operator needs — with every test still green.
// Defining it once puts that coupling under the compiler.
package chatdelivery

import "strings"

// ResendGuidance is the one action an UNCONFIRMED delivery calls for. bossd
// appends it to notice_text; surfaces that render a delivery outcome print it
// and, for a response that lost delivery_state in transit, key on it.
const ResendGuidance = "the message may already have been submitted; check the pane before resending"

// QueuedGuidance is the one action a QUEUED delivery calls for, and it is the
// OPPOSITE of ResendGuidance's: the agent is mid-turn and has already taken the
// message into its own queue, so it is delivered and a resend adds a second
// copy that will run twice. It lives beside ResendGuidance, and is deliberately
// a distinct sentence, because the two are told apart by a substring match on a
// response whose delivery_state a converter dropped — sharing a prefix would
// make the queued case read as unconfirmed on exactly the surfaces that have no
// other tell.
const QueuedGuidance = "the agent already holds the message and will run it when the current turn ends; do not resend"

// SplitGuidance separates a notice into its detail and whichever trailing
// delivery guidance it carries, so a renderer can place the two independently —
// the guidance reads as an instruction on its own line rather than trailing a
// few hundred characters behind the detail it applies to. The returned guidance
// is the constant itself, so a caller can switch on it to recover the delivery
// state a converter dropped.
//
// A notice that carries no guidance is returned whole as the detail, with an
// empty guidance: that is a mechanically-handled message (a "/boss switch"
// account switch, say), not a delivery whose outcome needs qualifying, and the
// empty guidance is how a caller tells the two apart.
func SplitGuidance(notice string) (detail, guidance string) {
	for _, candidate := range []string{QueuedGuidance, ResendGuidance} {
		if detail, found := splitOn(notice, candidate); found {
			return detail, candidate
		}
	}
	return strings.TrimSpace(notice), ""
}

// SplitResendGuidance is SplitGuidance narrowed to the unconfirmed-delivery
// sentence: a queued notice is returned whole, with an empty guidance, so a
// caller that only knows how to render "resend at your own risk" never applies
// that wording to a message the agent has already accepted.
func SplitResendGuidance(notice string) (detail, guidance string) {
	if detail, found := splitOn(notice, ResendGuidance); found {
		return detail, ResendGuidance
	}
	return strings.TrimSpace(notice), ""
}

// splitOn returns the notice's leading detail when it carries guidance, with
// the separator punctuation bossd joined the two with trimmed back off.
func splitOn(notice, guidance string) (detail string, found bool) {
	before, _, found := strings.Cut(strings.TrimSpace(notice), guidance)
	if !found {
		return "", false
	}
	return strings.TrimRight(strings.TrimSpace(before), ";:, "), true
}
