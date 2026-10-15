package displaystatus

import (
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// TestReadyNeverCoversAWorkingChat pins the cascade half of the "✓ ready"
// rule: a receipted settled green PR reads "✓ ready" only while nothing runs,
// so a WORKING chat reads "working" over it whatever the PR says.
func TestReadyNeverCoversAWorkingChat(t *testing.T) {
	for _, ds := range []pb.DisplayStatus{pb.DisplayStatus_DISPLAY_STATUS_PASSING, pb.DisplayStatus_DISPLAY_STATUS_APPROVED, pb.DisplayStatus_DISPLAY_STATUS_REVIEW} {
		sess := &pb.Session{DisplayStatus: ds, HasBuildReceipt: true}
		if got := Compute(Input{Session: sess, ChatStatus: pb.ChatStatus_CHAT_STATUS_WORKING}).Label; got != WorkingLabel {
			t.Errorf("%v with a working chat = %q, want working", ds, got)
		}
		if got := Compute(Input{Session: sess, ChatStatus: pb.ChatStatus_CHAT_STATUS_IDLE}).Label; got != ReadyLabel {
			t.Errorf("%v with an idle chat = %q, want ready", ds, got)
		}
	}
}

// TestPreWorkingOverWaitingOutput is the inverse property: for a marked row,
// the inverse returns exactly what the cascade served when the session folded
// to WAITING over working chats, and the Ready mark that run stamped.
func TestPreWorkingOverWaitingOutput(t *testing.T) {
	for _, tc := range []struct {
		name      string
		sess      func() *pb.Session
		wantLabel string
		wantReady bool
	}{
		{"checking PR reads waiting", func() *pb.Session { return &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_CHECKING} }, WaitingLabel, false},
		{"receipted green PR reads ready over waiting", func() *pb.Session {
			return &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING, HasBuildReceipt: true}
		}, ReadyLabel, true},
		{"errored session keeps the recolor", func() *pb.Session {
			return &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING, State: pb.SessionState_SESSION_STATE_BLOCKED}
		}, WaitingLabel, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			served := tc.sess()
			out := Compute(Input{Session: served, ChatStatus: pb.ChatStatus_CHAT_STATUS_WORKING})
			served.DisplayLabel, served.DisplayIntent, served.DisplaySpinner = out.Label, out.Intent, out.Spinner
			served.IsWorkingOverWaiting = true

			oldIn := Input{Session: tc.sess(), ChatStatus: pb.ChatStatus_CHAT_STATUS_WAITING}
			old := Compute(oldIn)
			got, ready := PreWorkingOverWaitingOutput(served)
			if got != old || got.Label != tc.wantLabel || ready != tc.wantReady || ready != WasReadyOverWaiting(oldIn, old) {
				t.Fatalf("inverse = %+v ready=%v, want the old cascade's %+v ready=%v", got, ready, old, tc.wantReady)
			}
		})
	}

	unmarked := &pb.Session{DisplayLabel: WorkingLabel, DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_SUCCESS, DisplaySpinner: true}
	if got, _ := PreWorkingOverWaitingOutput(unmarked); got.Label != WorkingLabel {
		t.Fatalf("an unmarked working row was rewritten to %q", got.Label)
	}
	if got, ready := PreWorkingOverWaitingOutput(nil); got != (Output{}) || ready {
		t.Fatalf("nil session = %+v ready=%v, want zero", got, ready)
	}
}
