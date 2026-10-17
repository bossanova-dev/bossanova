package apiversion

import (
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossalib/sessionreason"
	"google.golang.org/protobuf/proto"
)

// runBlockedSession is a blocked run whose reason finalize stored with its
// own "run blocked:" label, as V20260923 serves its attention.
func runBlockedSession() *pb.Session {
	reason := sessionreason.RunBlocked("MAD-1751 implemented")
	return &pb.Session{
		Id:            "sess-run-blocked",
		BlockedReason: &reason,
		Attention: &pb.SessionAttention{
			Kind:     pb.AttentionKind_ATTENTION_KIND_FIX,
			Sentence: "Run blocked: MAD-1751 implemented",
			AlertKey: "blocked:blocked",
			Actions: []pb.AttentionAction{
				pb.AttentionAction_ATTENTION_ACTION_DISMISS,
				pb.AttentionAction_ATTENTION_ACTION_OPEN_CHAT,
			},
		},
	}
}

// TestRunBlockedLabel_EveryPinKeepsItsSentence checks every version: from
// V20260923 the sentence says "run blocked" once; V20260917 to V20260922 get
// the doubled sentence they were served; older pins carry no attention.
func TestRunBlockedLabel_EveryPinKeepsItsSentence(t *testing.T) {
	reg := DefaultRegistry()
	for _, v := range reg.All() {
		t.Run(v.String(), func(t *testing.T) {
			source := runBlockedSession()
			pristine := proto.Clone(source)
			resp := &pb.ProxyGetSessionResponse{Session: source}
			ProductionChanges().Apply(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, resp, v)
			if !proto.Equal(source, pristine) {
				t.Fatal("mutated cached source")
			}
			want := "Run blocked: run blocked: MAD-1751 implemented"
			switch {
			case !reg.Newer(V20260923, v):
				want = "Run blocked: MAD-1751 implemented"
			case reg.Newer(V20260917, v):
				if got := resp.GetSession().GetAttention(); got != nil {
					t.Fatalf("attention = %v, want none before V20260917", got)
				}
				return
			}
			if got := resp.GetSession().GetAttention().GetSentence(); got != want {
				t.Fatalf("sentence = %q, want %q", got, want)
			}
		})
	}
}

// TestRunBlockedLabel_RebuildsTheExactOldSentence checks reasons whose old
// sentence differed from today's in more than the label: surrounding space
// and repeated labels in any case.
func TestRunBlockedLabel_RebuildsTheExactOldSentence(t *testing.T) {
	for reason, old := range map[string]string{
		"  Blocked:  RUN BLOCKED: twice  ": "Run blocked:   Blocked:  RUN BLOCKED: twice",
		"   leading space":                 "Run blocked:    leading space",
	} {
		s := &pb.Session{BlockedReason: &reason, Attention: &pb.SessionAttention{
			Kind:     pb.AttentionKind_ATTENTION_KIND_FIX,
			Sentence: "Run blocked: " + sessionreason.WithoutBlockedLabel(reason),
		}}
		if got := downconvertRunBlockedLabelSession(s).GetAttention().GetSentence(); got != old {
			t.Errorf("reason %q: sentence = %q, want %q", reason, got, old)
		}
	}
}

// TestRunBlockedLabel_LeavesOtherAnswersAlone checks that a reason with no
// label, a FIX whose sentence did not come from the reason, a non-FIX row and
// a row with no reason pass through untouched.
func TestRunBlockedLabel_LeavesOtherAnswersAlone(t *testing.T) {
	labelled := sessionreason.RunBlocked("MAD-1751 implemented")
	plain := "draft PR creation failed: boom"
	for name, source := range map[string]*pb.Session{
		"no label": {BlockedReason: &plain, Attention: &pb.SessionAttention{
			Kind: pb.AttentionKind_ATTENTION_KIND_FIX, Sentence: "Run blocked: " + plain,
		}},
		"other sentence": {BlockedReason: &labelled, Attention: &pb.SessionAttention{
			Kind: pb.AttentionKind_ATTENTION_KIND_FIX, Sentence: "Automatic repair exhausted its attempts",
		}},
		"not a fix": {BlockedReason: &labelled, Attention: &pb.SessionAttention{
			Kind: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, Sentence: "Run blocked: MAD-1751 implemented",
		}},
		"no reason": {Attention: &pb.SessionAttention{
			Kind: pb.AttentionKind_ATTENTION_KIND_FIX, Sentence: "Run stopped",
		}},
	} {
		if got := downconvertRunBlockedLabelSession(source); got != source {
			t.Errorf("%s rewritten to %v", name, got)
		}
	}
}
