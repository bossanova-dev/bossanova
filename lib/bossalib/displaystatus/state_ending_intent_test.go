package displaystatus

import (
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestComputeErroredStateEndingIntent_ChatPicker_AC15_AC16(t *testing.T) {
	for _, state := range []pb.SessionState{pb.SessionState_SESSION_STATE_BLOCKED, pb.SessionState_SESSION_STATE_ORPHANED} {
		for _, tc := range []struct {
			label              string
			merging, archiving bool
			intent             pb.DisplayIntent
		}{
			{"merging", true, false, pb.DisplayIntent_DISPLAY_INTENT_INFO},
			{"archiving", false, true, pb.DisplayIntent_DISPLAY_INTENT_WARNING},
			{"✓ passing", false, false, pb.DisplayIntent_DISPLAY_INTENT_DANGER},
		} {
			t.Run(state.String()+"/"+tc.label, func(t *testing.T) {
				in := Input{Session: &pb.Session{State: state, DisplayMerging: tc.merging, ArchivePending: tc.archiving, DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING}, ChatStatus: pb.ChatStatus_CHAT_STATUS_IDLE}
				want := Output{Label: tc.label, Intent: tc.intent, Spinner: tc.merging || tc.archiving}
				if got := Compute(in); got != want {
					t.Fatalf("Compute() = %+v, want %+v", got, want)
				}
			})
		}
	}
}

func TestPreStateEndingErroredOutput(t *testing.T) {
	for _, state := range []pb.SessionState{pb.SessionState_SESSION_STATE_BLOCKED, pb.SessionState_SESSION_STATE_ORPHANED, pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN} {
		for _, in := range []Input{
			{Session: &pb.Session{DisplayMerging: true}},
			{Session: &pb.Session{ArchivePending: true}},
			{Session: &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING}, ChatStatus: pb.ChatStatus_CHAT_STATUS_IDLE},
			{Session: &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_MERGED}},
			{Session: &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_CLOSED}},
		} {
			in.Session.State = state
			want := ComputeBase(in)
			if errored(in) && !isMutedTerminalPR(want) {
				want.Intent = pb.DisplayIntent_DISPLAY_INTENT_DANGER
			}
			served := Compute(in)
			in.Session.DisplayLabel, in.Session.DisplayIntent, in.Session.DisplaySpinner = served.Label, served.Intent, served.Spinner
			if got := PreStateEndingErroredOutput(in.Session); got != want {
				t.Errorf("state %s label %q: got %+v, want %+v", state, served.Label, got, want)
			}
		}
	}
	for _, s := range []*pb.Session{nil, {}, {State: pb.SessionState_SESSION_STATE_BLOCKED}} {
		if got := PreStateEndingErroredOutput(s); got != (Output{}) {
			t.Errorf("empty session = %+v", got)
		}
	}
}
