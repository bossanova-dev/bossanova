package displaystatus

import (
	"slices"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestVerifyParkSummaryRoundTrips(t *testing.T) {
	summary := VerifyParkSummary("ledger-open,human-review:ui")
	if summary != "needs human: ledger-open,human-review:ui" {
		t.Fatalf("VerifyParkSummary = %q", summary)
	}
	if got := VerifyParkSummaryCodes(summary); !slices.Equal(got, []string{"ledger-open", "human-review:ui"}) {
		t.Fatalf("VerifyParkSummaryCodes = %v, want the codes in report order", got)
	}
	for _, other := range []string{"", "needs human — parked by the verify stage", "blocked — needs human intervention"} {
		if got := VerifyParkSummaryCodes(other); got != nil {
			t.Errorf("VerifyParkSummaryCodes(%q) = %v, want nil", other, got)
		}
	}
}

// TestPreVerifyParkSplitOutput is the inverse property: for every park the
// cascade can serve, the inverse returns exactly what the pre-split cascade
// served ("needs human" / WARNING, DANGER when errored), and it leaves every
// other row alone.
func TestPreVerifyParkSplitOutput(t *testing.T) {
	serve := func(state pb.SessionState, codes []string) *pb.Session {
		s := &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_NEEDS_HUMAN, State: state}
		out := Compute(Input{Session: s, ParkCodes: codes})
		s.DisplayLabel, s.DisplayIntent, s.DisplaySpinner = out.Label, out.Intent, out.Spinner
		return s
	}
	for _, codes := range [][]string{{"human-review:ui"}, {"ledger-open"}, {"human-review:security,no-receipt"}, nil} {
		for state, want := range map[pb.SessionState]pb.DisplayIntent{
			pb.SessionState_SESSION_STATE_READY_FOR_REVIEW: pb.DisplayIntent_DISPLAY_INTENT_WARNING,
			pb.SessionState_SESSION_STATE_BLOCKED:          pb.DisplayIntent_DISPLAY_INTENT_DANGER,
		} {
			served := serve(state, codes)
			got := PreVerifyParkSplitOutput(served)
			if got != (Output{Label: NeedsHumanLabel, Intent: want}) {
				t.Errorf("park %v in %v served %q: inverse = %+v, want needs human / %v", codes, state, served.GetDisplayLabel(), got, want)
			}
		}
	}

	unchanged := []*pb.Session{
		{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_NEEDS_HUMAN, DisplayLabel: WorkingLabel, DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_SUCCESS, DisplaySpinner: true},
		{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_NEEDS_HUMAN, DisplayLabel: NeedsHumanLabel, DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_WARNING},
		{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING, DisplayLabel: BlockedLabel, DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_DANGER},
		nil,
	}
	for _, s := range unchanged {
		want := Output{Label: s.GetDisplayLabel(), Intent: s.GetDisplayIntent(), Spinner: s.GetDisplaySpinner()}
		if got := PreVerifyParkSplitOutput(s); got != want {
			t.Errorf("PreVerifyParkSplitOutput(%v %q) = %+v, want it unchanged", s.GetDisplayStatus(), s.GetDisplayLabel(), got)
		}
	}
}
