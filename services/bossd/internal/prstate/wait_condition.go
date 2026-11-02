package prstate

import "github.com/recurser/bossalib/kit/vcs"

// WaitCondition names what a wait blocks on. The values share the
// PRWaitCondition proto enum's numbering.
type WaitCondition int

const (
	// WaitChecksSettled is met when the check verdict is failing, or green
	// with every head workflow run settled.
	WaitChecksSettled WaitCondition = iota + 1
	// WaitMergeable is met when GitHub reports the PR mergeable, and
	// unreachable when GitHub reports a conflict.
	WaitMergeable
	// WaitHeadChanged is met when the head SHA differs from the baseline.
	WaitHeadChanged
)

// WaitOutcome is how a wait ended. The values share the PRWaitOutcome proto
// enum's numbering.
type WaitOutcome int

const (
	// waitPending means the condition neither holds nor became unreachable.
	waitPending WaitOutcome = iota
	// WaitMet means the condition holds.
	WaitMet
	// WaitUnreachable means the condition can no longer hold without a new
	// push, or at all.
	WaitUnreachable
	// WaitTimedOut means the timeout passed first.
	WaitTimedOut
)

// evaluateWait judges req's condition against snap. sinceHead is the
// head-changed baseline; an empty baseline never meets it. A closed or merged
// PR is unreachable for every condition.
func evaluateWait(req WaitRequest, snap Snapshot, sinceHead string) WaitOutcome {
	if snap.Status.State == vcs.PRStateClosed || snap.Status.State == vcs.PRStateMerged {
		return WaitUnreachable
	}
	switch req.Condition {
	case WaitChecksSettled:
		if checksSettled(snap.Verdict, snap.RunsSettled, req.AcceptNoGateRan) {
			return WaitMet
		}
	case WaitMergeable:
		if m := snap.Status.Mergeable; m != nil {
			if *m {
				return WaitMet
			}
			return WaitUnreachable
		}
	case WaitHeadChanged:
		if head := snap.Status.HeadSHA; sinceHead != "" && head != "" && head != sinceHead {
			return WaitMet
		}
	}
	return waitPending
}

// checksSettled reports a failing verdict, or a green one whose head workflow
// runs have all completed. The verdict carries the daemon's checks_passed
// meaning: a verify-stage hold is already out of it, and a required check that
// has not run reads unknown, which keeps waiting. A green head where no gate
// ran (every check skipped or neutral) settles only when acceptNoGateRan.
func checksSettled(v vcs.CheckVerdict, runsSettled *bool, acceptNoGateRan bool) bool {
	if v.State == vcs.CheckVerdictFailing {
		return true
	}
	if !v.IsGreen() || runsSettled == nil || !*runsSettled {
		return false
	}
	return v.Reason != vcs.CheckVerdictReasonNoGateRan || acceptNoGateRan
}
