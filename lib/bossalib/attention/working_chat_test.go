package attention_test

import (
	"testing"

	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// A chat at work on a session holds the operator's move: the row reads in
// progress, with a spinner, until the turn ends and the FIX, REVIEW or MERGE
// it would otherwise show comes back. A question and an admission hold stay
// the operator's however busy the chat is.
func TestCompute_WorkingChatReadsInProgress(t *testing.T) {
	blocked := readyPR()
	blocked.Fix = &attention.FixFact{AlertKey: "blocked:Blocked", Sentence: "Run blocked: verify parked", Since: stateSince}
	exhausted := readyPR()
	exhausted.ChecksFailing = 1
	exhausted.Fix = &attention.FixFact{AlertKey: "repair-exhausted", Sentence: "Automatic repair exhausted its attempts", Since: stateSince}
	partial := unverifiedPR()
	partial.IsPartial = true
	question := readyPR()
	question.QuestionChatID, question.QuestionText = "c", "Which base?"
	admission := unverifiedPR()
	admission.IsFactoryRepo = true
	admission.VerifyAdmission = attention.Admission{State: attention.AdmissionPending, Reason: "ticket-not-selected"}

	for _, tc := range []struct {
		name       string
		in         attention.Input
		idle, busy pb.AttentionKind
		idlePhrase string
	}{
		{"run stuck", blocked, pb.AttentionKind_ATTENTION_KIND_FIX, pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, "⨯ run stuck"},
		{"repair failed", exhausted, pb.AttentionKind_ATTENTION_KIND_FIX, pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, "⨯ repair failed"},
		{"partial build", partial, pb.AttentionKind_ATTENTION_KIND_FIX, pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, "⨯ partial build"},
		{"sign-off review", withParks(readyPR(), "human-review:ui"), pb.AttentionKind_ATTENTION_KIND_REVIEW, pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, attention.ReviewPhrase},
		{"merge", readyPR(), pb.AttentionKind_ATTENTION_KIND_MERGE, pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, attention.MergePhrase},
		{"question", question, pb.AttentionKind_ATTENTION_KIND_QUESTION, pb.AttentionKind_ATTENTION_KIND_QUESTION, "? question"},
		{"admission hold", admission, pb.AttentionKind_ATTENTION_KIND_FIX, pb.AttentionKind_ATTENTION_KIND_FIX, "⨯ ticket not selected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idle := tc.in
			idle.ChatStatus = pb.ChatStatus_CHAT_STATUS_WORKING
			if got := attention.Compute(idle); got.GetKind() != tc.idle || got.GetStatusPhrase() != tc.idlePhrase {
				t.Fatalf("no chat at work: %v %q, want %v %q", got.GetKind(), got.GetStatusPhrase(), tc.idle, tc.idlePhrase)
			}
			busy := tc.in
			busy.ChatStatus, busy.ProgressPhase, busy.ChatAtWork = pb.ChatStatus_CHAT_STATUS_WORKING, attention.RepairingLabel, true
			got := attention.Compute(busy)
			if got.GetKind() != tc.busy {
				t.Fatalf("chat at work: kind %v, want %v", got.GetKind(), tc.busy)
			}
			if tc.busy == pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS &&
				(got.GetProgressLabel() != attention.RepairingLabel || got.GetStatusPhrase() != attention.RepairingLabel) {
				t.Fatalf("chat at work: label %q phrase %q, want %q", got.GetProgressLabel(), got.GetStatusPhrase(), attention.RepairingLabel)
			}
		})
	}
}
