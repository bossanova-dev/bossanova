package displaystatus

import pb "github.com/recurser/bossalib/gen/bossanova/v1"

// keepsBaseIntentWhenErrored preserves legitimate terminal states and the
// in-flight actions that resolve an errored session.
func keepsBaseIntentWhenErrored(out Output) bool {
	return isMutedTerminalPR(out) || IsStateEndingLabel(out.Label)
}

// PreStateEndingErroredOutput restores the served tuple before V20260929.
// It operates on the served fields, never the live cascade, so older pins do
// not inherit later cascade changes. Nil and uncomputed sessions are unchanged.
func PreStateEndingErroredOutput(sess *pb.Session) Output {
	out := Output{Label: sess.GetDisplayLabel(), Intent: sess.GetDisplayIntent(), Spinner: sess.GetDisplaySpinner()}
	if erroredSession(sess) && IsStateEndingLabel(out.Label) {
		out.Intent = pb.DisplayIntent_DISPLAY_INTENT_DANGER
	}
	return out
}
