package displaystatus

import (
	"testing"

	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func verifyingSession() *pb.Session {
	return &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_VERIFYING}
}

func TestVerifyingLabelIsTheAttentionLabel(t *testing.T) {
	if VerifyingLabel != attention.VerifyingLabel {
		t.Fatalf("VerifyingLabel = %q, attention.VerifyingLabel = %q", VerifyingLabel, attention.VerifyingLabel)
	}
}

func TestComputeVerifyWaitLabel(t *testing.T) {
	info := Output{Label: "waiting: merge", Intent: pb.DisplayIntent_DISPLAY_INTENT_INFO, Spinner: true}
	tests := []struct {
		name string
		in   Input
		want Output
	}{
		{name: "wait shows its short label", in: Input{Session: verifyingSession(), VerifyWaitReason: "merge-state-unknown"}, want: info},
		{name: "reused wait label", in: Input{Session: verifyingSession(), VerifyWaitReason: "base-ci-red"}, want: Output{Label: "main red", Intent: pb.DisplayIntent_DISPLAY_INTENT_INFO, Spinner: true}},
		{name: "claim shows verifying", in: Input{Session: verifyingSession()}, want: Output{Label: VerifyingLabel, Intent: pb.DisplayIntent_DISPLAY_INTENT_INFO, Spinner: true}},
		{
			name: "wait with changes requested is danger",
			in:   Input{Session: &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_VERIFYING, DisplayHasChangesRequested: true}, VerifyWaitReason: "merge-state-unknown"},
			want: Output{Label: "waiting: merge", Intent: pb.DisplayIntent_DISPLAY_INTENT_DANGER, Spinner: true},
		},
		{
			name: "reason is ignored off VERIFYING",
			in:   Input{Session: &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING}, VerifyWaitReason: "merge-state-unknown"},
			want: Output{Label: "✓ passing", Intent: pb.DisplayIntent_DISPLAY_INTENT_SUCCESS},
		},
		{
			name: "a working chat outranks the wait",
			in:   Input{Session: verifyingSession(), VerifyWaitReason: "merge-state-unknown", ChatStatus: pb.ChatStatus_CHAT_STATUS_WORKING},
			want: Output{Label: WorkingLabel, Intent: pb.DisplayIntent_DISPLAY_INTENT_SUCCESS, Spinner: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Compute(tt.in); got != tt.want {
				t.Fatalf("Compute() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// The errored recolor's inverse must recognise a wait label as the VERIFYING
// branch's label, whose base intent is INFO.
func TestUnderlyingIntentOfAVerifyWait(t *testing.T) {
	sess := &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_VERIFYING, State: pb.SessionState_SESSION_STATE_BLOCKED}
	out := Compute(Input{Session: sess, VerifyWaitReason: "ci-not-settled"})
	if out.Label != "waiting: CI" || out.Intent != pb.DisplayIntent_DISPLAY_INTENT_DANGER {
		t.Fatalf("errored wait composite = %+v, want DANGER waiting: CI", out)
	}
	sess.DisplayLabel, sess.DisplayIntent, sess.DisplaySpinner = out.Label, out.Intent, out.Spinner
	if got := UnderlyingIntent(sess); got != pb.DisplayIntent_DISPLAY_INTENT_INFO {
		t.Fatalf("UnderlyingIntent = %v, want INFO", got)
	}
}

func TestPreVerifyStatusOutputRestoresCheckingForAWait(t *testing.T) {
	checkingWarning := Output{Label: "checking", Intent: pb.DisplayIntent_DISPLAY_INTENT_WARNING, Spinner: true}
	for _, label := range attention.VerifyWaitLabels() {
		sess := &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_VERIFYING, DisplayLabel: label, DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_INFO, DisplaySpinner: true}
		if got := PreVerifyStatusOutput(sess); got != checkingWarning {
			t.Errorf("label %q: PreVerifyStatusOutput() = %+v, want %+v", label, got, checkingWarning)
		}
	}
	parked := Output{Label: WaitingLabel, Intent: pb.DisplayIntent_DISPLAY_INTENT_INFO, Spinner: true}
	sess := &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_VERIFYING, DisplayLabel: parked.Label, DisplayIntent: parked.Intent, DisplaySpinner: parked.Spinner}
	if got := PreVerifyStatusOutput(sess); got != parked {
		t.Fatalf("chat-parked composite: PreVerifyStatusOutput() = %+v, want it untouched", got)
	}
	other := &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_CHECKING, DisplayLabel: "waiting: merge", DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_INFO, DisplaySpinner: true}
	if got := PreVerifyStatusOutput(other); got.Label != "waiting: merge" {
		t.Fatalf("wait label off VERIFYING: PreVerifyStatusOutput() = %+v, want it untouched", got)
	}
}

// A verify wait replaces "verifying", which is not live activity; neither is
// the wait, so a hint beside it stays bright.
func TestVerifyWaitLabelsAreNotLiveActivity(t *testing.T) {
	for _, label := range append(attention.VerifyWaitLabels(), VerifyingLabel) {
		if IsLiveActivityLabel(label) {
			t.Errorf("IsLiveActivityLabel(%q) = true, want false", label)
		}
	}
}
