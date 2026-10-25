package apiversion

import (
	"testing"

	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"google.golang.org/protobuf/proto"
)

func waitingForVerifySession() *pb.Session {
	return &pb.Session{Id: "queued", DisplayLabel: "✓ passing", Attention: &pb.SessionAttention{
		Kind:          pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS,
		ProgressLabel: attention.WaitingForVerifyLabel,
	}}
}

func TestWaitingForVerifyLabel_EveryPin(t *testing.T) {
	reg := DefaultRegistry()
	for _, kind := range []pb.AttentionKind{pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, pb.AttentionKind_ATTENTION_KIND_MERGE} {
		for _, display := range []string{"✓ passing", "✓ ready", "", "idle", "waiting", "usage-limited (resets ~10:00)"} {
			for _, procedure := range []string{bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure} {
				for _, v := range reg.All() {
					t.Run(kind.String()+"/"+display+"/"+procedureMethod(procedure)+"/"+v.String(), func(t *testing.T) {
						source := waitingForVerifySession()
						source.Attention.Kind = kind
						source.DisplayLabel = display
						pristine := proto.Clone(source)
						msg, get := sessionResponse(procedure, source)
						ProductionChanges().Apply(procedure, msg, v)
						if !proto.Equal(source, pristine) {
							t.Fatal("mutated cached source")
						}
						got := get(msg)
						if reg.Newer(V20260917, v) {
							if got.GetAttention() != nil {
								t.Fatalf("attention = %v, want none before V20260917", got.GetAttention())
							}
							return
						}
						want := attention.WaitingForVerifyLabel
						if reg.Newer(V20260928, v) {
							want = "waiting for verify"
						}
						if reg.Newer(V20260926, v) {
							want = "stopped"
							if display == "idle" || display == "waiting" || display == "usage-limited (resets ~10:00)" {
								want = "idle"
							}
						}
						if got.GetAttention().GetProgressLabel() != want {
							t.Fatalf("progress = %q, want %q", got.GetAttention().GetProgressLabel(), want)
						}
					})
				}
			}
		}
	}
}

func TestWaitingForVerifyLabel_LeavesOtherLabelsAlone(t *testing.T) {
	for _, source := range []*pb.Session{nil, {}, {Attention: &pb.SessionAttention{}}, {Attention: &pb.SessionAttention{ProgressLabel: "working"}}, {Attention: &pb.SessionAttention{ProgressLabel: "stopped"}}} {
		if got := downconvertWaitingForVerifyLabelSession(source); got != source {
			t.Fatalf("rewritten to %v", got)
		}
	}
}
