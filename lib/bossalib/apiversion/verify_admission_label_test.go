package apiversion

import (
	"testing"

	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"google.golang.org/protobuf/proto"
)

func TestVerifyAdmissionLabel_EveryPin(t *testing.T) {
	verifyAdmissionLabelsEveryPin(t, map[string]string{
		"verify unknown":                       "verify unknown",
		"outside verify":                       "outside verify",
		"outside verify: ticket not in review": "outside verify: ticket not in review",
	}, V20260928)
}

func verifyAdmissionLabelsEveryPin(t *testing.T, labels map[string]string, introduced Version) {
	t.Helper()
	reg := DefaultRegistry()
	for label, previous := range labels {
		for _, display := range []string{"✓ passing", "idle", "waiting", "usage-limited (resets soon)"} {
			for _, procedure := range []string{bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure} {
				for _, v := range reg.All() {
					t.Run(label+"/"+display+"/"+procedureMethod(procedure)+"/"+v.String(), func(t *testing.T) {
						source := waitingForVerifySession()
						source.Attention.ProgressLabel = label
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
								t.Fatalf("attention = %v, want none", got.GetAttention())
							}
							return
						}
						want := label
						if reg.Newer(introduced, v) {
							want = previous
						}
						if reg.Newer(V20260928, v) {
							want = "waiting for verify"
						}
						if reg.Newer(V20260926, v) {
							want = "stopped"
							if display == "idle" || display == "waiting" || display == "usage-limited (resets soon)" {
								want = "idle"
							}
						}
						if got.GetAttention().GetProgressLabel() != want {
							t.Fatalf("progress = %q, want %q", got.GetAttention().GetProgressLabel(), want)
						}
						if v == reg.Current() && got != source {
							t.Fatal("current pin cloned source")
						}
					})
				}
			}
		}
	}
}

func TestVerifyAdmissionLabel_LeavesOtherLabelsAlone(t *testing.T) {
	for _, source := range []*pb.Session{nil, {}, {Attention: &pb.SessionAttention{}}, {Attention: &pb.SessionAttention{ProgressLabel: "working"}}, {Attention: &pb.SessionAttention{ProgressLabel: attention.WaitingForVerifyLabel}}, {Attention: &pb.SessionAttention{ProgressLabel: "outside verify unrelated"}}} {
		if got := downconvertVerifyAdmissionLabelSession(source); got != source {
			t.Fatalf("rewritten to %v", got)
		}
	}
}

func TestVerifyAdmissionLabel_MethodAndPayload(t *testing.T) {
	source := waitingForVerifySession()
	source.Attention.ProgressLabel = "verify unknown"
	pristine := proto.Clone(source)
	msg, get := sessionResponse(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, source)
	VerifyAdmissionLabelChange{}.TransformResponse("/other/method", msg)
	if get(msg) != source || !proto.Equal(source, pristine) {
		t.Fatal("wrong method changed source")
	}
	VerifyAdmissionLabelChange{}.TransformResponse(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, &pb.ProxyListSessionsResponse{})
	converted := downconvertVerifyAdmissionLabelSession(source)
	if converted == source || converted.GetAttention().GetProgressLabel() != attention.WaitingForVerifyLabel || !proto.Equal(source, pristine) {
		t.Fatal("conversion did not preserve source and clone new label")
	}
}
