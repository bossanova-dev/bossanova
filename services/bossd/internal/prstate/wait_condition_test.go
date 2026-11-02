package prstate

import (
	"testing"

	"github.com/recurser/bossalib/kit/vcs"
)

func TestEvaluateWait(t *testing.T) {
	const other = "fedcba9876543210fedcba9876543210fedcba98"
	snapshot := func(state vcs.PRState, verdict vcs.CheckVerdictState, settled, mergeable *bool, headSHA string) Snapshot {
		return Snapshot{
			Status:      vcs.PRStatus{State: state, HeadSHA: headSHA, Mergeable: mergeable},
			Verdict:     vcs.CheckVerdict{State: verdict, Reason: vcs.CheckVerdictReasonOK},
			RunsSettled: settled,
		}
	}
	withReason := func(s Snapshot, reason string) Snapshot {
		s.Verdict.Reason = reason
		return s
	}
	open := vcs.PRStateOpen
	tests := []struct {
		name  string
		cond  WaitCondition
		snap  Snapshot
		since string
		want  WaitOutcome
		// accept is the no-gate-ran opt-in.
		accept bool
	}{
		{"checks: failing verdict is met", WaitChecksSettled, snapshot(open, vcs.CheckVerdictFailing, nil, nil, head), head, WaitMet, false},
		{"checks: green with settled runs is met", WaitChecksSettled, snapshot(open, vcs.CheckVerdictGreen, ptr(true), nil, head), head, WaitMet, false},
		{"checks: green with held runs waits", WaitChecksSettled, snapshot(open, vcs.CheckVerdictGreen, ptr(false), nil, head), head, waitPending, false},
		{"checks: green with runs not computed waits", WaitChecksSettled, snapshot(open, vcs.CheckVerdictGreen, nil, nil, head), head, waitPending, false},
		{"checks: pending waits", WaitChecksSettled, snapshot(open, vcs.CheckVerdictPending, nil, nil, head), head, waitPending, false},
		{"checks: unknown waits", WaitChecksSettled, snapshot(open, vcs.CheckVerdictUnknown, nil, nil, head), head, waitPending, false},
		{"checks: green where no gate ran waits by default", WaitChecksSettled, withReason(snapshot(open, vcs.CheckVerdictGreen, ptr(true), nil, head), vcs.CheckVerdictReasonNoGateRan), head, waitPending, false},
		{"checks: green where no gate ran is met on opt-in", WaitChecksSettled, withReason(snapshot(open, vcs.CheckVerdictGreen, ptr(true), nil, head), vcs.CheckVerdictReasonNoGateRan), head, WaitMet, true},
		{"checks: no gate ran with held runs waits even on opt-in", WaitChecksSettled, withReason(snapshot(open, vcs.CheckVerdictGreen, ptr(false), nil, head), vcs.CheckVerdictReasonNoGateRan), head, waitPending, true},
		{"checks: a required check not yet run waits", WaitChecksSettled, withReason(snapshot(open, vcs.CheckVerdictUnknown, nil, nil, head), vcs.CheckVerdictReasonNotRequested), head, waitPending, false},
		{"checks: merged is unreachable", WaitChecksSettled, snapshot(vcs.PRStateMerged, vcs.CheckVerdictGreen, ptr(true), nil, head), head, WaitUnreachable, false},
		{"mergeable: true is met", WaitMergeable, snapshot(open, vcs.CheckVerdictPending, nil, ptr(true), head), head, WaitMet, false},
		{"mergeable: conflict is unreachable", WaitMergeable, snapshot(open, vcs.CheckVerdictGreen, ptr(true), ptr(false), head), head, WaitUnreachable, false},
		{"mergeable: unknown waits", WaitMergeable, snapshot(open, vcs.CheckVerdictGreen, ptr(true), nil, head), head, waitPending, false},
		{"mergeable: closed is unreachable", WaitMergeable, snapshot(vcs.PRStateClosed, vcs.CheckVerdictPending, nil, ptr(true), head), head, WaitUnreachable, false},
		{"head: a new head is met", WaitHeadChanged, snapshot(open, vcs.CheckVerdictPending, nil, nil, other), head, WaitMet, false},
		{"head: the same head waits", WaitHeadChanged, snapshot(open, vcs.CheckVerdictPending, nil, nil, head), head, waitPending, false},
		{"head: an empty baseline never meets", WaitHeadChanged, snapshot(open, vcs.CheckVerdictPending, nil, nil, other), "", waitPending, false},
		{"head: an empty head waits", WaitHeadChanged, snapshot(open, vcs.CheckVerdictPending, nil, nil, ""), head, waitPending, false},
		{"head: merged is unreachable", WaitHeadChanged, snapshot(vcs.PRStateMerged, vcs.CheckVerdictPending, nil, nil, other), head, WaitUnreachable, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := evaluateWait(WaitRequest{Condition: tc.cond, AcceptNoGateRan: tc.accept}, tc.snap, tc.since); got != tc.want {
				t.Fatalf("evaluateWait = %d, want %d", got, tc.want)
			}
		})
	}
}
