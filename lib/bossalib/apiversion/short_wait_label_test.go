package apiversion

import (
	"testing"

	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"google.golang.org/protobuf/proto"
)

func TestShortWaitLabel_EveryPin(t *testing.T) {
	reg := DefaultRegistry()
	cases := []struct{ label, before string }{
		{attention.WaitingForVerifyLabel, "waiting for verify"},
		{attention.WaitingWords([]string{"base-ci-red"})[0], "waiting for main CI"},
		{attention.WaitingWords([]string{"stack-busy"})[0], "waiting for the test stack"},
	}
	for _, tc := range cases {
		for _, procedure := range []string{bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure} {
			for _, v := range reg.All() {
				if reg.Newer(V20260926, v) {
					continue
				}
				t.Run(tc.label+"/"+procedureMethod(procedure)+"/"+v.String(), func(t *testing.T) {
					source := &pb.Session{Id: "s", DisplayLabel: "✓ passing", Attention: &pb.SessionAttention{
						Kind:          pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS,
						ProgressLabel: tc.label,
					}}
					pristine := proto.Clone(source)
					msg, get := sessionResponse(procedure, source)
					ProductionChanges().Apply(procedure, msg, v)
					if !proto.Equal(source, pristine) {
						t.Fatal("mutated cached source")
					}
					want := tc.label
					if reg.Newer(V20260928, v) {
						want = tc.before
					}
					if got := get(msg).GetAttention().GetProgressLabel(); got != want {
						t.Fatalf("progress = %q, want %q", got, want)
					}
				})
			}
		}
	}
}

func TestShortWaitLabel_LeavesOtherLabelsAlone(t *testing.T) {
	for _, source := range []*pb.Session{nil, {}, {Attention: &pb.SessionAttention{}}, {Attention: &pb.SessionAttention{ProgressLabel: "working"}}, {Attention: &pb.SessionAttention{ProgressLabel: "waiting"}}, {Attention: &pb.SessionAttention{ProgressLabel: "waiting for verify"}}} {
		if got := downconvertShortWaitLabelSession(source); got != source {
			t.Fatalf("rewritten to %v", got)
		}
	}
}
