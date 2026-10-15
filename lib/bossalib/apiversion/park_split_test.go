package apiversion

import (
	"testing"

	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

// parkSplitSession is a verify-parked row as V20260918 serves it, showing
// label ("needs sign-off" WARNING or "blocked" DANGER).
func parkSplitSession(label string) *pb.Session {
	intent := pb.DisplayIntent_DISPLAY_INTENT_WARNING
	if label == displaystatus.BlockedLabel {
		intent = pb.DisplayIntent_DISPLAY_INTENT_DANGER
	}
	return &pb.Session{
		Id:            "sess-parked",
		State:         pb.SessionState_SESSION_STATE_READY_FOR_REVIEW,
		DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_NEEDS_HUMAN,
		DisplayLabel:  label,
		DisplayIntent: intent,
		MergeBlock:    &pb.MergeBlock{Gate: pb.MergeBlock_GATE_PENDING, DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_NEEDS_HUMAN},
		AttentionStatus: &pb.AttentionStatus{
			NeedsAttention: true,
			Reason:         pb.AttentionReason_ATTENTION_REASON_AWAITING_HUMAN_INPUT,
			Summary:        displaystatus.VerifyParkSummary("ledger-open"),
		},
	}
}

// TestVerifyParkSplit_ChainMatchesThePreChangeRowAtEveryOlderVersion is the
// exact-inverse property over the whole production chain: at every version
// older than V20260918 both new park rows down-convert to what the same session
// served before the split (the "needs human" row) down-converts to, and Current
// leaves them alone.
func TestVerifyParkSplit_ChainMatchesThePreChangeRowAtEveryOlderVersion(t *testing.T) {
	for _, label := range []string{displaystatus.NeedsSignOffLabel, displaystatus.BlockedLabel} {
		t.Run(label, func(t *testing.T) {
			if got := applyToSession(t, parkSplitSession(label), DefaultRegistry().Current()); !proto.Equal(got, parkSplitSession(label)) {
				t.Fatalf("Current rewrote the row: %v", got)
			}
			preChange := func() *pb.Session {
				s := parkSplitSession(label)
				s.DisplayLabel, s.DisplayIntent = displaystatus.NeedsHumanLabel, pb.DisplayIntent_DISPLAY_INTENT_WARNING
				return s
			}
			older := 0
			for _, v := range DefaultRegistry().All() {
				if v >= V20260918 {
					continue
				}
				older++
				if got, want := applyToSession(t, parkSplitSession(label), v), applyToSession(t, preChange(), v); !proto.Equal(got, want) {
					t.Errorf("at %s: row down-converts to\n%v\nwant the pre-change row's\n%v", v, got, want)
				}
			}
			if older == 0 {
				t.Fatal("no registry version older than V20260918")
			}
			if got := applyToSession(t, parkSplitSession(label), V20260917); got.GetDisplayLabel() != displaystatus.NeedsHumanLabel ||
				got.GetDisplayIntent() != pb.DisplayIntent_DISPLAY_INTENT_WARNING {
				t.Fatalf("one version back = %q / %v, want needs human / WARNING", got.GetDisplayLabel(), got.GetDisplayIntent())
			}
		})
	}
}

// TestVerifyParkSplit_ErroredParkKeepsTheRecolorAtV20260917 pins the errored
// overlay through the inverse: a blocked session's park was DANGER before the
// split and stays DANGER one version back.
func TestVerifyParkSplit_ErroredParkKeepsTheRecolorAtV20260917(t *testing.T) {
	s := parkSplitSession(displaystatus.NeedsSignOffLabel)
	s.State = pb.SessionState_SESSION_STATE_BLOCKED
	s.DisplayIntent = pb.DisplayIntent_DISPLAY_INTENT_DANGER
	if got := applyToSession(t, s, V20260917); got.GetDisplayLabel() != displaystatus.NeedsHumanLabel ||
		got.GetDisplayIntent() != pb.DisplayIntent_DISPLAY_INTENT_DANGER {
		t.Fatalf("errored park one version back = %q / %v, want needs human / DANGER", got.GetDisplayLabel(), got.GetDisplayIntent())
	}
}

func TestVerifyParkSplit_LeavesOtherRowsAlone(t *testing.T) {
	working := parkSplitSession(displaystatus.BlockedLabel)
	working.DisplayLabel, working.DisplayIntent, working.DisplaySpinner = displaystatus.WorkingLabel, pb.DisplayIntent_DISPLAY_INTENT_SUCCESS, true
	passing := &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING, DisplayLabel: "✓ passing", DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_SUCCESS}
	for _, s := range []*pb.Session{working, passing, nil} {
		before := proto.Clone(s)
		if got := downconvertVerifyParkSplitSession(s); got != s || !proto.Equal(s, before) {
			t.Errorf("downconvertVerifyParkSplitSession(%v) changed the row", s)
		}
	}
}
