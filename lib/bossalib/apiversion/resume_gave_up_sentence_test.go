package apiversion

import (
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"google.golang.org/protobuf/proto"
)

const (
	resumeGaveUpSentence    = "Resumed automatically at 23:17, 23:19 and 23:27; every attempt got the same 401, so automatic retries stopped. Run claude /login on mac-mini, then press r."
	resumeGaveUpOldSentence = "Resumed automatically at 23:17, 23:19 and 23:27; every attempt got the same 401, so the factory stopped retrying. Run claude /login on mac-mini, then press r."
)

// resumeGaveUpSession is a run whose automatic resumes gave up, as V20260924
// serves its attention.
func resumeGaveUpSession() *pb.Session {
	reason := "automatic resume ledger: attempt budget exhausted"
	return &pb.Session{
		Id:            "sess-resume-gave-up",
		State:         pb.SessionState_SESSION_STATE_BLOCKED,
		BlockedReason: &reason,
		Attention: &pb.SessionAttention{
			Kind:     pb.AttentionKind_ATTENTION_KIND_FIX,
			Sentence: resumeGaveUpSentence,
			AlertKey: "resume:auth_expired",
			Actions: []pb.AttentionAction{
				pb.AttentionAction_ATTENTION_ACTION_DISMISS,
				pb.AttentionAction_ATTENTION_ACTION_RESUME,
			},
		},
	}
}

// TestResumeGaveUpSentence_EveryPinKeepsItsSentence checks every version: from
// V20260924 the sentence says "automatic retries stopped"; V20260917 to
// V20260923 get the factory sentence they were served; older pins carry no
// attention.
func TestResumeGaveUpSentence_EveryPinKeepsItsSentence(t *testing.T) {
	reg := DefaultRegistry()
	for _, v := range reg.All() {
		t.Run(v.String(), func(t *testing.T) {
			source := resumeGaveUpSession()
			pristine := proto.Clone(source)
			resp := &pb.ProxyGetSessionResponse{Session: source}
			ProductionChanges().Apply(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, resp, v)
			if !proto.Equal(source, pristine) {
				t.Fatal("mutated cached source")
			}
			want := resumeGaveUpOldSentence
			switch {
			case !reg.Newer(V20260924, v):
				want = resumeGaveUpSentence
			case reg.Newer(V20260917, v):
				if got := resp.GetSession().GetAttention(); got != nil {
					t.Fatalf("attention = %v, want none before V20260917", got)
				}
				return
			}
			if got := resp.GetSession().GetAttention().GetSentence(); got != want {
				t.Fatalf("sentence = %q, want %q", got, want)
			}
		})
	}
}

// TestResumeGaveUpSentence_LeavesOtherAnswersAlone checks that a FIX that is
// not a gave-up sentence, a non-FIX row and an already-old sentence pass
// through untouched.
func TestResumeGaveUpSentence_LeavesOtherAnswersAlone(t *testing.T) {
	for name, source := range map[string]*pb.Session{
		"other fix": {Attention: &pb.SessionAttention{
			Kind: pb.AttentionKind_ATTENTION_KIND_FIX, Sentence: "Run blocked: x, so automatic retries stopped. ok",
		}},
		"not a fix": {Attention: &pb.SessionAttention{
			Kind: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, Sentence: resumeGaveUpSentence,
		}},
		"old sentence": {Attention: &pb.SessionAttention{
			Kind: pb.AttentionKind_ATTENTION_KIND_FIX, Sentence: resumeGaveUpOldSentence,
		}},
		"no attention": {},
	} {
		if got := downconvertResumeGaveUpSentenceSession(source); got != source {
			t.Errorf("%s rewritten to %v", name, got)
		}
	}
}
