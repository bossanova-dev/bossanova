package apiversion

import (
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

func attentionSession() *pb.Session {
	return &pb.Session{
		Id: "sess-attention",
		AttentionStatus: &pb.AttentionStatus{
			NeedsAttention: true,
			Reason:         pb.AttentionReason_ATTENTION_REASON_AWAITING_HUMAN_INPUT,
		},
		Attention: &pb.SessionAttention{
			Kind:      pb.AttentionKind_ATTENTION_KIND_REVIEW,
			Sentence:  "Review the migration, then approve",
			Actions:   []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_APPROVE},
			ParkCodes: []string{"human-review:database"},
		},
	}
}

func TestSessionAttentionChange_Version(t *testing.T) {
	if got := (SessionAttentionChange{}).Version(); got != V20260917 {
		t.Errorf("SessionAttentionChange.Version() = %q, want %q", got, V20260917)
	}
}

// TestSessionAttentionChange_ProductionChainByVersion pushes a session with
// attention through the full production chain: Current serves it untouched,
// one version back and older clears attention and leaves attention_status.
func TestSessionAttentionChange_ProductionChainByVersion(t *testing.T) {
	reg := DefaultRegistry()
	if got := reg.Current(); got != V20260917 {
		t.Fatalf("DefaultRegistry().Current() = %q, want %q", got, V20260917)
	}

	current := &pb.ProxyListSessionsResponse{Sessions: []*pb.Session{attentionSession()}}
	ProductionChanges().Apply(bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, current, reg.Current())
	if got := current.GetSessions()[0].GetAttention(); got.GetKind() != pb.AttentionKind_ATTENTION_KIND_REVIEW {
		t.Fatalf("attention at Current = %v, want the REVIEW answer untouched", got)
	}

	for _, v := range []Version{V20260916, V20260812, Baseline} {
		t.Run(string(v), func(t *testing.T) {
			msg := &pb.ProxyListSessionsResponse{Sessions: []*pb.Session{attentionSession()}}
			ProductionChanges().Apply(bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, msg, v)
			got := msg.GetSessions()[0]
			if got.GetAttention() != nil {
				t.Errorf("attention = %v, want nil", got.GetAttention())
			}
			if !got.GetAttentionStatus().GetNeedsAttention() {
				t.Errorf("attention_status = %v, want it untouched", got.GetAttentionStatus())
			}
		})
	}
}

func TestSessionAttentionChange_DoesNotMutateSharedSession(t *testing.T) {
	shared := attentionSession()
	msg := &pb.ProxyGetSessionResponse{Session: shared}
	(SessionAttentionChange{}).TransformResponse(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, msg)
	if msg.GetSession().GetAttention() != nil {
		t.Fatal("response session still carries attention")
	}
	if shared.GetAttention() == nil {
		t.Fatal("transform mutated the shared (cached) session in place")
	}
}

func TestSessionAttentionChange_NoAllocationWithoutAttention(t *testing.T) {
	s := &pb.Session{Id: "plain"}
	msg := &pb.ProxyGetSessionResponse{Session: s}
	(SessionAttentionChange{}).TransformResponse(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, msg)
	if msg.GetSession() != s {
		t.Error("a session without attention was replaced; want the same pointer")
	}
}

func TestSessionAttentionChange_StreamedCreatedSession(t *testing.T) {
	msg := &pb.ProxyCreateSessionResponse{
		Body: &pb.ProxyCreateSessionResponse_Created{Created: attentionSession()},
	}
	(SessionAttentionChange{}).TransformResponse(bossanovav1connect.OrchestratorServiceProxyCreateSessionProcedure, msg)
	if got := msg.GetCreated().GetAttention(); got != nil {
		t.Errorf("created session attention = %v, want nil", got)
	}
}
