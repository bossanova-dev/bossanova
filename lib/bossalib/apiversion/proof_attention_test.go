package apiversion

import (
	"slices"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"google.golang.org/protobuf/proto"
)

func TestProof_Sessions_AC208703_ProofAttentionEveryPin(t *testing.T) {
	reg := DefaultRegistry()
	for _, procedure := range []string{bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure} {
		for _, v := range reg.All() {
			for _, video := range []string{"", "https://video.test/clip"} {
				t.Run(procedureMethod(procedure)+"/"+v.String()+"/"+video, func(t *testing.T) {
					source := &pb.Session{Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_REVIEW, ParkCodes: []string{"human-review:database"}, EvidenceUrl: video, ConfidenceLevel: pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE, Asks: []string{"Migrations"}, ProofUrl: "https://proof.test/page", LoweredBy: []string{"Needs migration review"}, Actions: []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_GITHUB, pb.AttentionAction_ATTENTION_ACTION_PROOF, pb.AttentionAction_ATTENTION_ACTION_MERGE}}}
					pristine := proto.Clone(source)
					msg, get := sessionResponse(procedure, source)
					ProductionChanges().Apply(procedure, msg, v)
					if !proto.Equal(source, pristine) {
						t.Fatal("mutated cached source")
					}
					got := get(msg).GetAttention()
					if !reg.Newer(V20261101, v) {
						if get(msg) != source || !proto.Equal(got, source.Attention) {
							t.Fatal("current response changed")
						}
						return
					}
					// Versions preceding SessionAttention receive no attention at all.
					if reg.Newer(V20260917, v) {
						if got != nil {
							t.Fatal("unexpected attention")
						}
						return
					}
					if got.GetConfidenceLevel() != pb.ConfidenceLevel_CONFIDENCE_LEVEL_UNSPECIFIED || len(got.GetAsks()) != 0 || got.GetProofUrl() != "" || len(got.GetLoweredBy()) != 0 || slices.Contains(got.GetActions(), pb.AttentionAction_ATTENTION_ACTION_PROOF) {
						t.Fatalf("proof fields survived: %v", got)
					}
					if slices.Contains(got.GetActions(), pb.AttentionAction_ATTENTION_ACTION_VIDEO) != (video != "") {
						t.Fatalf("video actions: %v", got)
					}
					if got.GetEvidenceUrl() != video || !slices.Equal(got.GetParkCodes(), source.Attention.ParkCodes) {
						t.Fatalf("legacy fields changed: %v", got)
					}
				})
			}
		}
	}
}

func TestProofAttentionNoOpAndDuplicateVideo(t *testing.T) {
	for _, source := range []*pb.Session{nil, {}, {Attention: &pb.SessionAttention{EvidenceUrl: "video"}}} {
		if downconvertProofAttentionSession(source) != source {
			t.Fatal("allocated for unaffected row")
		}
	}
	source := &pb.Session{Attention: &pb.SessionAttention{EvidenceUrl: "video", Actions: []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_GITHUB, pb.AttentionAction_ATTENTION_ACTION_PROOF, pb.AttentionAction_ATTENTION_ACTION_VIDEO, pb.AttentionAction_ATTENTION_ACTION_MERGE}}}
	got := downconvertProofAttentionSession(source).GetAttention().Actions
	want := []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_GITHUB, pb.AttentionAction_ATTENTION_ACTION_VIDEO, pb.AttentionAction_ATTENTION_ACTION_MERGE}
	if !slices.Equal(got, want) {
		t.Fatal(got)
	}
}
