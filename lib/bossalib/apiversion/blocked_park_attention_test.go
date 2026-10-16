package apiversion

import (
	"slices"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"google.golang.org/protobuf/proto"
)

// blockedParkSession is a verify-parked row whose park has a failure code, as
// V20260919 serves its attention.
func blockedParkSession() *pb.Session {
	return &pb.Session{
		Id: "sess-blocked",
		Attention: &pb.SessionAttention{
			Kind:     pb.AttentionKind_ATTENTION_KIND_FIX,
			Sentence: "The PR ledger is open · send it back to close it",
			Actions: []pb.AttentionAction{
				pb.AttentionAction_ATTENTION_ACTION_SEND_BACK,
				pb.AttentionAction_ATTENTION_ACTION_GITHUB,
				pb.AttentionAction_ATTENTION_ACTION_VIDEO,
			},
			ParkCodes:   []string{"ledger-open", "human-review:ui"},
			EvidenceUrl: "https://evidence.example/v.mp4",
		},
	}
}

// TestBlockedParkAttention_OlderPinsGetTheReviewBack checks every version: from
// V20260919 the blocked park is served as is, and every older pin that carries
// attention gets the REVIEW kind with APPROVE first and no RESUME, exactly the
// action list it was served before, without touching the cached source.
func TestBlockedParkAttention_OlderPinsGetTheReviewBack(t *testing.T) {
	reg := DefaultRegistry()
	for _, v := range reg.All() {
		t.Run(v.String(), func(t *testing.T) {
			source := blockedParkSession()
			pristine := proto.Clone(source)
			resp := &pb.ProxyGetSessionResponse{Session: source}
			ProductionChanges().Apply(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, resp, v)
			if !proto.Equal(source, pristine) {
				t.Fatal("mutated cached source")
			}
			got := resp.GetSession().GetAttention()
			switch {
			case !reg.Newer(V20260919, v):
				if !proto.Equal(resp.GetSession(), pristine) {
					t.Fatalf("attention = %v, want the blocked park unchanged", got)
				}
			case reg.Newer(V20260917, v):
				if got != nil {
					t.Fatalf("attention = %v, want none before V20260917", got)
				}
			default:
				want := []pb.AttentionAction{
					pb.AttentionAction_ATTENTION_ACTION_APPROVE,
					pb.AttentionAction_ATTENTION_ACTION_SEND_BACK,
					pb.AttentionAction_ATTENTION_ACTION_GITHUB,
					pb.AttentionAction_ATTENTION_ACTION_VIDEO,
				}
				if got.GetKind() != pb.AttentionKind_ATTENTION_KIND_REVIEW || !slices.Equal(got.GetActions(), want) {
					t.Fatalf("attention = %v %v, want REVIEW %v", got.GetKind(), got.GetActions(), want)
				}
				if !slices.Equal(got.GetParkCodes(), pristine.(*pb.Session).GetAttention().GetParkCodes()) {
					t.Fatalf("park codes = %v, want them kept", got.GetParkCodes())
				}
			}
		})
	}
}

// TestBlockedParkAttention_LeavesAFaultFixAlone checks that a FIX answered
// from a fault (no park codes) is not mistaken for a blocked park.
func TestBlockedParkAttention_LeavesAFaultFixAlone(t *testing.T) {
	source := &pb.Session{Attention: &pb.SessionAttention{
		Kind:     pb.AttentionKind_ATTENTION_KIND_FIX,
		AlertKey: "repair-failed",
		Actions:  []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_RESUME, pb.AttentionAction_ATTENTION_ACTION_DISMISS},
	}}
	if got := downconvertBlockedParkAttentionSession(source); got != source {
		t.Fatalf("fault FIX rewritten to %v", got)
	}
}
