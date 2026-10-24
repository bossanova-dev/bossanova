package ui

import (
	"testing"
	"time"

	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestHintDemoted_DemotedRowRendersTheAttentionHintBright asserts KTD-7 as a
// DECISION rather than leaving it as an emergent consequence. hintDemoted fades
// a demotable hint when the row's own composite is a live-activity label;
// "✓ passing" is not one, so on a demoted row the attention hint goes from faded
// to bright. That is the documented fail-loud direction for this predicate, and
// the row genuinely is no longer live, so it is accepted rather than repaired.
// If it ever reads wrong in use, the remedy is the recorded follow-up (teaching
// IsLiveActivityLabel about a demoted row), not a quiet widening here.

func TestHintDemoted_DemotedRowRendersTheAttentionHintBright(t *testing.T) {
	demotable := SessionHint{Text: "stalled", demotable: true}

	waiting := &pb.Session{
		State:        pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
		DisplayLabel: displaystatus.WaitingLabel,
	}
	if !hintDemoted(waiting, demotable) {
		t.Fatal("hintDemoted(waiting row) = false, want true — waiting IS a live-activity label")
	}

	demoted := &pb.Session{
		State:            pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
		DisplayStatus:    pb.DisplayStatus_DISPLAY_STATUS_PASSING,
		DisplayLabel:     "✓ passing",
		IsWaitingDemoted: true,
	}
	if hintDemoted(demoted, demotable) {
		t.Fatal("hintDemoted(demoted row) = true, want false — the hint renders bright (KTD-7)")
	}

	// The repair hint is NOT part of the flip: it is already suppressed for a
	// passing PR at the source, so it never reaches the style gate here.
	//
	// Asserting that on the bare `demoted` session above would prove nothing —
	// repairFailureHint returns early on LastRepairAttemptCount == 0, so the
	// empty result would come from "this session never had a repair attempt",
	// not from repairFailureResolved. Give it a real repair-failure history so
	// the count and error guards both pass and repairFailureResolved is the
	// only thing left that can empty the hint.
	withFailures := func(status pb.DisplayStatus) *pb.Session {
		return &pb.Session{
			State:                  pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
			DisplayStatus:          status,
			DisplayLabel:           "✓ passing",
			IsWaitingDemoted:       true,
			LastRepairAttemptCount: 3,
			LastRepairRunnerError:  "runner exited 1",
			LastRepairStartedAt:    timestamppb.New(time.Now().Add(-time.Hour)),
		}
	}
	if got := repairHint(withFailures(pb.DisplayStatus_DISPLAY_STATUS_PASSING)); got != "" {
		t.Fatalf("repairHint on a passing PR = %q, want empty — it is suppressed at the source, not by the fade", got)
	}
	// The negative control: identical session, a status repairFailureResolved
	// does NOT resolve. A non-empty hint here is what proves the emptiness above
	// came from repairFailureResolved rather than from an earlier guard.
	if got := repairHint(withFailures(pb.DisplayStatus_DISPLAY_STATUS_FAILING)); got == "" {
		t.Fatal("repairHint on a FAILING PR is empty — the passing-PR assertion above is vacuous")
	}
}
