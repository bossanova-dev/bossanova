package apiversion

import (
	"slices"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

func TestResumeAttentionActionsCompatibility(t *testing.T) {
	for _, v := range []Version{V20260917, DefaultRegistry().Current()} {
		t.Run(v.String(), func(t *testing.T) {
			original := &pb.Session{Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_FIX, Actions: []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_DISMISS, pb.AttentionAction_ATTENTION_ACTION_OPEN_CHAT}}}
			resp := &pb.ProxyGetSessionResponse{Session: original}
			ProductionChanges().Apply(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, resp, v)
			got := slices.Contains(resp.Session.Attention.Actions, pb.AttentionAction_ATTENTION_ACTION_RESUME)
			if got != (v == V20260917) {
				t.Fatalf("resume=%v version=%s", got, v)
			}
			if slices.Contains(original.Attention.Actions, pb.AttentionAction_ATTENTION_ACTION_RESUME) {
				t.Fatal("mutated cached source")
			}
		})
	}
}
