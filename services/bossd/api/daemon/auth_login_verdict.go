package server

import pb "github.com/recurser/bossalib/gen/bossanova/v1"

// LoginOutcome enumerates what the daemon actually observed when it reloaded
// credentials after a `boss login`. It is the non-secret half of LoginVerdict.
//
// It lives in this package, with AuthNotifier, for the same reason in reverse:
// the adapter in cmd/main.go produces these values, and keeping them out of the
// generated proto package means cmd does not have to import pb just to answer
// this one question. NotifyAuthChange maps them onto the wire enum.
type LoginOutcome int

const (
	// LoginOutcomeUnspecified is the zero value: nothing was evaluated. It is
	// what a logout, a nil notifier, or a daemon too old to have a verdict
	// reports, and it must render as silence rather than as a false success.
	LoginOutcomeUnspecified LoginOutcome = iota
	// LoginOutcomeOK means the reloaded record was usable and the auth gate
	// was cleared.
	LoginOutcomeOK
	// LoginOutcomeCredentialsFlagged means the reloaded record still carried a
	// persisted re-login marker, so the auth gate was NOT cleared.
	LoginOutcomeCredentialsFlagged
	// LoginOutcomeCredentialsMissing means the reloaded record carried no
	// access token and no marker, so the auth gate was NOT cleared. Distinct
	// from Flagged because the two have opposite causes — a rejected refresh
	// token versus a login whose credentials never reached this process — and
	// therefore opposite remedies.
	LoginOutcomeCredentialsMissing
	// LoginOutcomeRegisterFailed means credentials were clean and the auth
	// gate WAS cleared, but the proactive re-register failed. The reactive
	// path remains the backstop, so this is reported, not fatal.
	LoginOutcomeRegisterFailed
	// LoginOutcomeRemovedByAnotherUser means credentials were clean, but
	// bosso refused the registration: the machine's previous owner removed it,
	// and this login is another account's, which can never add it back.
	LoginOutcomeRemovedByAnotherUser
)

// LoginVerdict is the daemon's answer to "did that login actually leave me
// able to talk upstream?". ReloginReason is one of the enumerated persisted
// re-login reasons and is set only for LoginOutcomeCredentialsFlagged; it is
// never token material.
type LoginVerdict struct {
	Outcome       LoginOutcome
	ReloginReason string
}

// loginVerdictToProto maps a LoginVerdict onto the wire response. An outcome
// this function does not recognise maps to OUTCOME_UNSPECIFIED rather than to
// a guess, keeping "we do not know" distinguishable from "OK".
func loginVerdictToProto(v LoginVerdict) *pb.NotifyAuthChangeResponse {
	resp := &pb.NotifyAuthChangeResponse{}
	switch v.Outcome {
	case LoginOutcomeOK:
		resp.Outcome = pb.NotifyAuthChangeResponse_OUTCOME_OK
	case LoginOutcomeCredentialsFlagged:
		resp.Outcome = pb.NotifyAuthChangeResponse_OUTCOME_CREDENTIALS_FLAGGED
		// relogin_reason is documented as empty for every other outcome, and
		// this mapper is the single wire boundary, so it is the only place
		// that invariant can actually be enforced rather than merely relied on.
		resp.ReloginReason = v.ReloginReason
	case LoginOutcomeCredentialsMissing:
		resp.Outcome = pb.NotifyAuthChangeResponse_OUTCOME_CREDENTIALS_MISSING
	case LoginOutcomeRegisterFailed:
		resp.Outcome = pb.NotifyAuthChangeResponse_OUTCOME_REGISTER_FAILED
	case LoginOutcomeRemovedByAnotherUser:
		resp.Outcome = pb.NotifyAuthChangeResponse_OUTCOME_REMOVED_BY_ANOTHER_USER
	default:
		resp.Outcome = pb.NotifyAuthChangeResponse_OUTCOME_UNSPECIFIED
	}
	return resp
}
