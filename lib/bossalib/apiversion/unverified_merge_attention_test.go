package apiversion

import (
	"slices"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"google.golang.org/protobuf/proto"
)

// unverifiedMergeSession is a merge-ready row nothing has verified, linked to
// a tracker, as V20260920 serves its attention.
func unverifiedMergeSession() *pb.Session {
	return &pb.Session{
		Id: "sess-unverified",
		Attention: &pb.SessionAttention{
			Kind:     pb.AttentionKind_ATTENTION_KIND_REVIEW,
			Sentence: "Not verified",
			Actions: []pb.AttentionAction{
				pb.AttentionAction_ATTENTION_ACTION_GITHUB,
				pb.AttentionAction_ATTENTION_ACTION_MERGE,
				pb.AttentionAction_ATTENTION_ACTION_TRACKER,
			},
		},
	}
}

// TestUnverifiedMergeAttention_OlderPinsGetTheMergeBack checks every version:
// from V20260920 the unverified review is served as is, and every older pin
// that carries attention gets the MERGE kind, the "Ready to merge" sentence and
// MERGE first, exactly as it was served before, without touching the cached
// source.
func TestUnverifiedMergeAttention_OlderPinsGetTheMergeBack(t *testing.T) {
	reg := DefaultRegistry()
	for _, v := range reg.All() {
		t.Run(v.String(), func(t *testing.T) {
			source := unverifiedMergeSession()
			pristine := proto.Clone(source)
			resp := &pb.ProxyGetSessionResponse{Session: source}
			ProductionChanges().Apply(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, resp, v)
			if !proto.Equal(source, pristine) {
				t.Fatal("mutated cached source")
			}
			got := resp.GetSession().GetAttention()
			switch {
			case !reg.Newer(V20260920, v):
				if !proto.Equal(resp.GetSession(), pristine) {
					t.Fatalf("attention = %v, want the unverified review unchanged", got)
				}
			case reg.Newer(V20260917, v):
				if got != nil {
					t.Fatalf("attention = %v, want none before V20260917", got)
				}
			default:
				want := []pb.AttentionAction{
					pb.AttentionAction_ATTENTION_ACTION_MERGE,
					pb.AttentionAction_ATTENTION_ACTION_GITHUB,
					pb.AttentionAction_ATTENTION_ACTION_TRACKER,
				}
				if got.GetKind() != pb.AttentionKind_ATTENTION_KIND_MERGE || !slices.Equal(got.GetActions(), want) {
					t.Fatalf("attention = %v %v, want MERGE %v", got.GetKind(), got.GetActions(), want)
				}
				if got.GetSentence() != "Ready to merge" {
					t.Fatalf("sentence = %q, want %q", got.GetSentence(), "Ready to merge")
				}
			}
		})
	}
}

// TestUnverifiedMergeAttention_LeavesOtherAnswersAlone checks that a verify
// park's REVIEW (park codes, no MERGE) and a verified MERGE pass through.
func TestUnverifiedMergeAttention_LeavesOtherAnswersAlone(t *testing.T) {
	for name, source := range map[string]*pb.Session{
		"park review": {Attention: &pb.SessionAttention{
			Kind:      pb.AttentionKind_ATTENTION_KIND_REVIEW,
			ParkCodes: []string{"human-review:ui"},
			Actions:   []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_APPROVE, pb.AttentionAction_ATTENTION_ACTION_SEND_BACK, pb.AttentionAction_ATTENTION_ACTION_GITHUB},
		}},
		"verified merge": {Attention: &pb.SessionAttention{
			Kind:     pb.AttentionKind_ATTENTION_KIND_MERGE,
			Sentence: "Ready to merge",
			Actions:  []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_MERGE, pb.AttentionAction_ATTENTION_ACTION_GITHUB},
		}},
		"no attention": {},
	} {
		if got := downconvertUnverifiedMergeAttentionSession(source); got != source {
			t.Errorf("%s rewritten to %v", name, got)
		}
	}
}
