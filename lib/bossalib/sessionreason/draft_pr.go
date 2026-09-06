// Package sessionreason owns structured helpers for session blocked reasons.
package sessionreason

import "strings"

const draftPRCreationFailurePrefix = "draft PR creation failed: "

// DraftPRCreationFailure formats the persisted blocked reason for a failed
// draft PR creation attempt.
func DraftPRCreationFailure(err error) string {
	if err == nil {
		return ""
	}
	return draftPRCreationFailurePrefix + err.Error()
}

// IsDraftPRCreationFailure reports whether a blocked reason was produced by
// DraftPRCreationFailure.
func IsDraftPRCreationFailure(reason *string) bool {
	return reason != nil && strings.HasPrefix(*reason, draftPRCreationFailurePrefix)
}

// transientMarker distinguishes a draft-PR failure that was GitHub flapping from
// one that is the operator's problem (BOS-877).
//
// # Why it NESTS inside draftPRCreationFailurePrefix rather than replacing it
//
// A sibling prefix (say "draft PR creation retrying: ") would read more cleanly
// in isolation and would be wrong. IsDraftPRCreationFailure is a HasPrefix test
// on draftPRCreationFailurePrefix, and it gates every live consumer of this
// state: displaystatus's "? PR failed" label, the TUI's draft-PR warning hint
// and its de-duplication against the attention hint, and the
// apiversion.DraftPRFailureLabelChange down-convert transform. A parallel prefix
// would make all of them silently stop matching — on precisely the failures an
// operator most needs surfaced — with no test failing, because each of those
// consumers is written against the prefix and not against an enumeration of
// reason kinds. Nesting keeps one prefix, one predicate for "is this a draft-PR
// failure at all", and adds a strictly narrower second predicate on top.
//
// It is a full sentence fragment carrying an em dash on purpose. The predicate
// anchors it at a fixed position (see IsDraftPRCreationTransientFailure), so a
// git error that merely mentions the marker cannot misfire — but an error whose
// text *begins* with it still would, and a short marker ("retrying", say) is far
// likelier to start one. Do not shorten it.
const transientMarker = "GitHub was temporarily unreachable — retrying: "

// DraftPRCreationTransientFailure formats the persisted blocked reason for a
// draft PR creation that failed against a flapping remote rather than a real
// misconfiguration. The result still satisfies IsDraftPRCreationFailure — see
// transientMarker for why that nesting is load-bearing.
//
// The nil contract matches DraftPRCreationFailure's: no error, no reason.
func DraftPRCreationTransientFailure(err error) string {
	if err == nil {
		return ""
	}
	return draftPRCreationFailurePrefix + transientMarker + err.Error()
}

// IsDraftPRCreationTransientFailure reports whether a blocked reason carries the
// transient marker DraftPRCreationTransientFailure writes. It tests for the
// marker rather than for provenance, but anchors it where the constructor puts
// it — immediately after the failure prefix — rather than accepting it anywhere
// in the string. DraftPRCreationTransientFailure is the only producer, and it
// always writes prefix+marker+err, so the anchored form matches every reason the
// system can construct while refusing a TERMINAL reason whose raw git error
// merely quotes the marker mid-sentence. It is strictly narrower than
// IsDraftPRCreationFailure: every transient reason is also a failure reason, but
// not the reverse.
func IsDraftPRCreationTransientFailure(reason *string) bool {
	return reason != nil && strings.HasPrefix(*reason, draftPRCreationFailurePrefix+transientMarker)
}

// authMarker distinguishes a draft-PR failure caused by the daemon's gh CLI
// being unable to authenticate at all from every other terminal failure.
//
// # Why it nests, like transientMarker
//
// Same reason, and it is the same load-bearing contract: IsDraftPRCreationFailure
// is a HasPrefix test on draftPRCreationFailurePrefix, and it gates every live
// consumer — displaystatus's "? PR failed" label, the TUI's draft-PR hint and
// its de-duplication against the attention hint, and the apiversion
// DraftPRFailureLabelChange down-convert transform. None of them enumerate reason
// kinds, so a sibling prefix would make all of them silently stop matching.
//
// # A new invariant this file did not previously need
//
// authMarker and transientMarker are BOTH anchored immediately after the failure
// prefix, and each constructor writes prefix+marker+err. A reason can therefore
// carry at most one of them: they are mutually exclusive by construction, and
// IsDraftPRCreationAuthFailure and IsDraftPRCreationTransientFailure can never
// both be true. Callers may rely on that; a third marker must preserve it.
//
// Like transientMarker it is a full sentence fragment on purpose. The predicate
// anchors it at a fixed position, so a gh error that merely mentions the text
// cannot misfire — but a short marker would be far likelier to begin one. Do not
// shorten it.
const authMarker = "bossd's gh CLI could not authenticate to GitHub — run 'boss repair doctor': "

// DraftPRCreationAuthFailure formats the persisted blocked reason for a draft PR
// creation that failed because the daemon's gh CLI had no usable credentials.
// The result still satisfies IsDraftPRCreationFailure — see authMarker for why
// that nesting is load-bearing.
//
// The nil contract matches DraftPRCreationFailure's: no error, no reason.
func DraftPRCreationAuthFailure(err error) string {
	if err == nil {
		return ""
	}
	return draftPRCreationFailurePrefix + authMarker + err.Error()
}

// IsDraftPRCreationAuthFailure reports whether a blocked reason carries the auth
// marker DraftPRCreationAuthFailure writes, anchored where the constructor puts
// it rather than accepted anywhere in the string — so a reason whose raw gh error
// merely quotes the marker mid-sentence is refused. It is strictly narrower than
// IsDraftPRCreationFailure and mutually exclusive with
// IsDraftPRCreationTransientFailure.
func IsDraftPRCreationAuthFailure(reason *string) bool {
	return reason != nil && strings.HasPrefix(*reason, draftPRCreationFailurePrefix+authMarker)
}

// draftPRCreationInFlightReason marks a session whose draft PR is being created
// right now by the background step StartSession spawns (BOS-540). It is a
// deliberately distinct string from DraftPRCreationFailure so the two states are
// never confused: "one is coming" must not read as "this failed". Only
// IsDraftPRCreationFailure gates the "? PR failed" display label, so an in-flight
// marker never renders as a failure.
const draftPRCreationInFlightReason = "draft PR creation in progress"

// DraftPRCreationInFlight is the reason persisted while the background draft PR
// create is running, so a user who does not yet see a PR knows one is coming.
// It is cleared when the PR lands, or replaced by DraftPRCreationFailure when
// the create fails.
func DraftPRCreationInFlight() string {
	return draftPRCreationInFlightReason
}

// IsDraftPRCreationInFlight reports whether a blocked reason is the in-flight
// marker written by DraftPRCreationInFlight.
func IsDraftPRCreationInFlight(reason *string) bool {
	return reason != nil && *reason == draftPRCreationInFlightReason
}
