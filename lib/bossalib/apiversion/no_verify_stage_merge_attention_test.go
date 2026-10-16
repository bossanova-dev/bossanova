package apiversion

import (
	"slices"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"google.golang.org/protobuf/proto"
)

// noVerifyStageMergeSession is a merge-ready row nothing has verified, linked
// to a tracker, as V20260921 serves its attention.
func noVerifyStageMergeSession() *pb.Session {
	return &pb.Session{
		Id: "sess-no-verify-stage",
		Attention: &pb.SessionAttention{
			Kind:     pb.AttentionKind_ATTENTION_KIND_MERGE,
			Sentence: "Ready to merge",
			Actions: []pb.AttentionAction{
				pb.AttentionAction_ATTENTION_ACTION_MERGE,
				pb.AttentionAction_ATTENTION_ACTION_GITHUB,
				pb.AttentionAction_ATTENTION_ACTION_TRACKER,
			},
			IsUnverified: true,
		},
	}
}

// TestNoVerifyStageMergeAttention_EveryPinKeepsItsAnswer checks every version:
// from V20260921 the MERGE is served as is, V20260920 gets the unverified
// REVIEW it was served, and older pins that carry attention end on the MERGE
// UnverifiedMergeAttentionChange restores from that REVIEW, all without
// touching the cached source.
func TestNoVerifyStageMergeAttention_EveryPinKeepsItsAnswer(t *testing.T) {
	reg := DefaultRegistry()
	mergeFirst := []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_MERGE, pb.AttentionAction_ATTENTION_ACTION_GITHUB, pb.AttentionAction_ATTENTION_ACTION_TRACKER}
	githubFirst := []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_GITHUB, pb.AttentionAction_ATTENTION_ACTION_MERGE, pb.AttentionAction_ATTENTION_ACTION_TRACKER}
	for _, v := range reg.All() {
		t.Run(v.String(), func(t *testing.T) {
			source := noVerifyStageMergeSession()
			pristine := proto.Clone(source)
			resp := &pb.ProxyGetSessionResponse{Session: source}
			ProductionChanges().Apply(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, resp, v)
			if !proto.Equal(source, pristine) {
				t.Fatal("mutated cached source")
			}
			got := resp.GetSession().GetAttention()
			kind, sentence, actions := pb.AttentionKind_ATTENTION_KIND_MERGE, "Ready to merge", mergeFirst
			switch {
			case !reg.Newer(V20260921, v):
				if !proto.Equal(resp.GetSession(), pristine) {
					t.Fatalf("attention = %v, want the MERGE unchanged", got)
				}
				return
			case reg.Newer(V20260917, v):
				if got != nil {
					t.Fatalf("attention = %v, want none before V20260917", got)
				}
				return
			case v == V20260920:
				kind, sentence, actions = pb.AttentionKind_ATTENTION_KIND_REVIEW, "Not verified", githubFirst
			}
			if got.GetKind() != kind || got.GetSentence() != sentence || !slices.Equal(got.GetActions(), actions) || len(got.GetParkCodes()) != 0 {
				t.Fatalf("attention = %v %q %v, want %v %q %v", got.GetKind(), got.GetSentence(), got.GetActions(), kind, sentence, actions)
			}
		})
	}
}

// TestNoVerifyStageMergeAttention_LeavesOtherAnswersAlone checks that a
// verified MERGE (or one from a daemon that predates is_unverified), the
// unverified REVIEW a V20260920 daemon still serves and a park's REVIEW pass
// through.
func TestNoVerifyStageMergeAttention_LeavesOtherAnswersAlone(t *testing.T) {
	merge := []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_MERGE, pb.AttentionAction_ATTENTION_ACTION_GITHUB}
	for name, source := range map[string]*pb.Session{
		"verified merge": {Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_MERGE, Sentence: "Ready to merge", Actions: merge}},
		"unverified review": {Attention: &pb.SessionAttention{
			Kind:     pb.AttentionKind_ATTENTION_KIND_REVIEW,
			Sentence: "Not verified",
			Actions:  []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_GITHUB, pb.AttentionAction_ATTENTION_ACTION_MERGE},
		}},
		"park review": {Attention: &pb.SessionAttention{
			Kind:      pb.AttentionKind_ATTENTION_KIND_REVIEW,
			ParkCodes: []string{"human-review:ui"},
			Actions:   []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_GITHUB, pb.AttentionAction_ATTENTION_ACTION_MERGE},
		}},
		"no attention": {},
	} {
		if got := downconvertNoVerifyStageMergeAttentionSession(source); got != source {
			t.Errorf("%s rewritten to %v", name, got)
		}
	}
}
