package apiversion

import (
	"slices"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossalib/sessionreason"
	"google.golang.org/protobuf/proto"
)

// deadBootstrapSession is a reaped-bootstrap row linked to a tracker, as
// V20260920 serves its attention.
func deadBootstrapSession() *pb.Session {
	reason := sessionreason.BootstrapStranded()
	return &pb.Session{
		Id:            "sess-dead-bootstrap",
		BlockedReason: &reason,
		Attention: &pb.SessionAttention{
			Kind:     pb.AttentionKind_ATTENTION_KIND_FIX,
			Sentence: "Run died before setup finished and left no worktree or chat",
			AlertKey: "blocked:blocked",
			Actions: []pb.AttentionAction{
				pb.AttentionAction_ATTENTION_ACTION_DISMISS,
				pb.AttentionAction_ATTENTION_ACTION_TRACKER,
			},
		},
	}
}

// TestDeadBootstrapAttention_OlderPinsGetOpenChatBack checks every version:
// from V20260920 the dismiss-only answer is served as is; V20260918 and
// V20260919 get OPEN_CHAT after DISMISS and the old sentence; V20260917 also
// gets the RESUME that ResumeAttentionActionsChange prepends to every FIX, so
// each pin sees the list it was served before V20260920.
func TestDeadBootstrapAttention_OlderPinsGetOpenChatBack(t *testing.T) {
	reg := DefaultRegistry()
	oldSentence := "Run blocked: session bootstrap never completed; worktree and branch were cleaned up · open the chat, then resume"
	for _, v := range reg.All() {
		t.Run(v.String(), func(t *testing.T) {
			source := deadBootstrapSession()
			pristine := proto.Clone(source)
			resp := &pb.ProxyGetSessionResponse{Session: source}
			ProductionChanges().Apply(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, resp, v)
			if !proto.Equal(source, pristine) {
				t.Fatal("mutated cached source")
			}
			got := resp.GetSession().GetAttention()
			var want []pb.AttentionAction
			switch {
			case !reg.Newer(V20260920, v):
				if !proto.Equal(resp.GetSession(), pristine) {
					t.Fatalf("attention = %v, want the dismiss-only fix unchanged", got)
				}
				return
			case reg.Newer(V20260917, v):
				if got != nil {
					t.Fatalf("attention = %v, want none before V20260917", got)
				}
				return
			case reg.Newer(V20260918, v):
				want = []pb.AttentionAction{
					pb.AttentionAction_ATTENTION_ACTION_RESUME,
					pb.AttentionAction_ATTENTION_ACTION_DISMISS,
					pb.AttentionAction_ATTENTION_ACTION_OPEN_CHAT,
					pb.AttentionAction_ATTENTION_ACTION_TRACKER,
				}
			default:
				want = []pb.AttentionAction{
					pb.AttentionAction_ATTENTION_ACTION_DISMISS,
					pb.AttentionAction_ATTENTION_ACTION_OPEN_CHAT,
					pb.AttentionAction_ATTENTION_ACTION_TRACKER,
				}
			}
			if got.GetKind() != pb.AttentionKind_ATTENTION_KIND_FIX || !slices.Equal(got.GetActions(), want) {
				t.Fatalf("attention = %v %v, want FIX %v", got.GetKind(), got.GetActions(), want)
			}
			if got.GetSentence() != oldSentence {
				t.Fatalf("sentence = %q, want %q", got.GetSentence(), oldSentence)
			}
		})
	}
}

// TestDeadBootstrapAttention_LeavesOtherAnswersAlone checks that a FIX for any
// other blocked reason, one already offering OPEN_CHAT, and a non-FIX row with
// the reaped reason pass through untouched.
func TestDeadBootstrapAttention_LeavesOtherAnswersAlone(t *testing.T) {
	reaped := sessionreason.BootstrapStranded()
	other := "draft PR creation failed: boom"
	for name, source := range map[string]*pb.Session{
		"other reason": {BlockedReason: &other, Attention: &pb.SessionAttention{
			Kind:    pb.AttentionKind_ATTENTION_KIND_FIX,
			Actions: []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_DISMISS},
		}},
		"already open chat": {BlockedReason: &reaped, Attention: &pb.SessionAttention{
			Kind:    pb.AttentionKind_ATTENTION_KIND_FIX,
			Actions: []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_DISMISS, pb.AttentionAction_ATTENTION_ACTION_OPEN_CHAT},
		}},
		"not a fix":    {BlockedReason: &reaped, Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS}},
		"no attention": {BlockedReason: &reaped},
	} {
		if got := downconvertDeadBootstrapAttentionSession(source); got != source {
			t.Errorf("%s rewritten to %v", name, got)
		}
	}
}
