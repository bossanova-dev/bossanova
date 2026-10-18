package apiversion

import (
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"google.golang.org/protobuf/proto"
)

func TestAttentionActionSessionCarriers_VersionPins(t *testing.T) {
	for _, procedure := range []string{
		bossanovav1connect.OrchestratorServiceProxyDismissSessionAlertProcedure,
		bossanovav1connect.OrchestratorServiceProxyResumeSessionRunProcedure,
		bossanovav1connect.OrchestratorServiceProxyOperatorMergeSessionProcedure,
	} {
		t.Run(procedureMethod(procedure), func(t *testing.T) {
			for _, version := range []Version{Baseline, DefaultRegistry().Current()} {
				t.Run(string(version), func(t *testing.T) {
					original := &pb.Session{Id: "session-attention", State: pb.SessionState_SESSION_STATE_ORPHANED}
					msg, get := sessionResponse(procedure, original)
					ProductionChanges().Apply(procedure, msg, version)
					want := pb.SessionState_SESSION_STATE_ORPHANED
					if version == Baseline {
						want = pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN
					}
					if got := get(msg).GetState(); got != want {
						t.Fatalf("state = %v, want %v", got, want)
					}
					if original.GetState() != pb.SessionState_SESSION_STATE_ORPHANED {
						t.Fatal("transform mutated shared daemon session")
					}
					if version == DefaultRegistry().Current() && !proto.Equal(get(msg), original) {
						t.Fatal("current response changed")
					}
				})
			}
		})
	}
}

func TestOperatorMergeRefusal_KeepsAnswerWithoutSession(t *testing.T) {
	for _, version := range []Version{Baseline, DefaultRegistry().Current()} {
		t.Run(string(version), func(t *testing.T) {
			refused := &pb.ProxyOperatorMergeSessionResponse{Approval: &pb.VerifyApproveResult{Reason: "head-moved"}, TrackerWhy: "kept"}
			msg := proto.Clone(refused).(*pb.ProxyOperatorMergeSessionResponse)
			ProductionChanges().Apply(bossanovav1connect.OrchestratorServiceProxyOperatorMergeSessionProcedure, msg, version)
			if !proto.Equal(msg, refused) {
				t.Fatalf("refused merge = %v, want %v", msg, refused)
			}
		})
	}
}
