package apiversion

import (
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"google.golang.org/protobuf/proto"
)

func TestStatusPhrase_OlderPinsLoseOnlyThePhrase(t *testing.T) {
	reg := DefaultRegistry()
	for _, procedure := range []string{bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure} {
		for _, v := range reg.All() {
			t.Run(procedureMethod(procedure)+"/"+v.String(), func(t *testing.T) {
				source := &pb.Session{Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, ProgressLabel: "repairing", StatusPhrase: "repairing 1 P2 +2", StatusDetail: "1 P2, 2 checks · in a.go"}}
				pristine := proto.Clone(source)
				msg, get := sessionResponse(procedure, source)
				ProductionChanges().Apply(procedure, msg, v)
				if !proto.Equal(source, pristine) {
					t.Fatal("mutated cached source")
				}
				got := get(msg).GetAttention()
				if !reg.Newer(V20261101, v) {
					if !proto.Equal(got, source.Attention) {
						t.Fatalf("attention = %v, want today's answer unchanged", got)
					}
					return
				}
				if reg.Newer(V20260917, v) {
					if got != nil {
						t.Fatal("unexpected attention")
					}
					return
				}
				if got.GetStatusPhrase() != "" || got.GetStatusDetail() != "" {
					t.Fatalf("status survived for %s: %v", v, got)
				}
				if got.GetKind() != source.Attention.Kind || got.GetProgressLabel() != "repairing" {
					t.Fatalf("other fields changed: %v", got)
				}
			})
		}
	}
}

func TestStatusPhraseNoOp(t *testing.T) {
	for _, source := range []*pb.Session{nil, {}, {Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_DONE}}} {
		if downconvertStatusPhraseSession(source) != source {
			t.Fatal("allocated for a row without a status phrase")
		}
	}
}
