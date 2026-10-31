package apiversion

import (
	"slices"
	"testing"

	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"google.golang.org/protobuf/proto"
)

// signOffParkSession is a UI sign-off park linked to a tracker, as V20260920
// serves its attention.
func signOffParkSession() *pb.Session {
	return &pb.Session{Id: "sess-signoff", Attention: attention.Compute(attention.Input{
		ParkCodes:      []string{"human-review:ui"},
		Evidence:       &attention.Evidence{URL: "https://evidence.example/v.mp4"},
		HasTrackerLink: true,
	})}
}

// TestParkMergeAttention_OlderPinsGetApproveAndSendBackBack runs what
// attention.Compute serves today for a sign-off park and a blocked park through
// every pinned version. From V20260920 both are served as is; V20260919 gets
// APPROVE and SEND_BACK (SEND_BACK alone on the FIX) and the old sentences;
// older pins then get BlockedParkAttentionChange's REVIEW on top, exactly the
// pre-V20260919 answer.
func TestParkMergeAttention_OlderPinsGetApproveAndSendBackBack(t *testing.T) {
	cases := map[string]struct {
		source       func() *pb.Session
		wantSentence string
		wantKind0919 pb.AttentionKind
		want0919     []pb.AttentionAction
		wantOlder    []pb.AttentionAction
	}{
		"sign-off park": {
			source:       signOffParkSession,
			wantSentence: "Watch the evidence video, then approve · UI change",
			wantKind0919: pb.AttentionKind_ATTENTION_KIND_REVIEW,
			want0919: []pb.AttentionAction{
				pb.AttentionAction_ATTENTION_ACTION_APPROVE, pb.AttentionAction_ATTENTION_ACTION_SEND_BACK,
				pb.AttentionAction_ATTENTION_ACTION_GITHUB, pb.AttentionAction_ATTENTION_ACTION_VIDEO,
				pb.AttentionAction_ATTENTION_ACTION_TRACKER,
			},
		},
		"blocked park": {
			source: func() *pb.Session {
				return &pb.Session{Id: "sess-blocked", Attention: attention.Compute(attention.Input{ParkCodes: []string{"ledger-open"}})}
			},
			wantSentence: "The PR ledger is open · send it back to close it",
			wantKind0919: pb.AttentionKind_ATTENTION_KIND_FIX,
			want0919:     []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_SEND_BACK, pb.AttentionAction_ATTENTION_ACTION_GITHUB},
			wantOlder: []pb.AttentionAction{
				pb.AttentionAction_ATTENTION_ACTION_APPROVE, pb.AttentionAction_ATTENTION_ACTION_SEND_BACK,
				pb.AttentionAction_ATTENTION_ACTION_GITHUB,
			},
		},
	}
	reg := DefaultRegistry()
	for name, tc := range cases {
		for _, v := range reg.All() {
			t.Run(name+"/"+v.String(), func(t *testing.T) {
				source := tc.source()
				pristine := proto.Clone(source)
				resp := &pb.ProxyGetSessionResponse{Session: source}
				ProductionChanges().Apply(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, resp, v)
				if !proto.Equal(source, pristine) {
					t.Fatal("mutated cached source")
				}
				got := resp.GetSession().GetAttention()
				switch {
				case !reg.Newer(V20260920, v):
					if !proto.Equal(resp.GetSession(), func() *pb.Session {
						if reg.Newer(V20261031, v) {
							return downconvertParkVerdictSentence(source)
						}
						return source
					}()) {
						t.Fatalf("attention = %v, want today's answer unchanged", got)
					}
					if slices.Contains(got.GetActions(), pb.AttentionAction_ATTENTION_ACTION_APPROVE) || slices.Contains(got.GetActions(), pb.AttentionAction_ATTENTION_ACTION_SEND_BACK) {
						t.Fatalf("actions = %v, want neither APPROVE nor SEND_BACK at %s", got.GetActions(), v)
					}
				case reg.Newer(V20260917, v):
					if got != nil {
						t.Fatalf("attention = %v, want none before V20260917", got)
					}
				case v == V20260919:
					if got.GetKind() != tc.wantKind0919 || !slices.Equal(got.GetActions(), tc.want0919) {
						t.Fatalf("attention = %v %v, want %v %v", got.GetKind(), got.GetActions(), tc.wantKind0919, tc.want0919)
					}
					if got.GetSentence() != tc.wantSentence {
						t.Fatalf("sentence = %q, want %q", got.GetSentence(), tc.wantSentence)
					}
				case tc.wantOlder != nil && v == V20260918:
					if got.GetKind() != pb.AttentionKind_ATTENTION_KIND_REVIEW || !slices.Equal(got.GetActions(), tc.wantOlder) {
						t.Fatalf("attention = %v %v, want REVIEW %v", got.GetKind(), got.GetActions(), tc.wantOlder)
					}
				}
			})
		}
	}
}

// TestParkMergeAttention_RebuildsEveryParkSentence pins the sentence map
// against what attention.Compute renders, taken back to V20260921 first, so a
// reworded park sentence without a rewrite entry fails here.
func TestParkMergeAttention_RebuildsEveryParkSentence(t *testing.T) {
	for code, want := range map[string]string{
		"human-review:database": "Review the migration, then approve",
		"human-review:unproven": "Some checks have no evidence · send it back to prove them",
		"ledger-open":           "The PR ledger is open · send it back to close it",
		"no-receipt":            "Built by hand, no factory review · send it back through the factory",
		"repair-exhausted":      "Verify is blocked · send it back or fix the PR",
		"human-review:ui":       "Review the UI change, then approve",
	} {
		a := downconvertFactoryReviewParkSentenceSession(downconvertParkVerdictSentence(&pb.Session{Attention: attention.Compute(attention.Input{ParkCodes: []string{code}})})).GetAttention()
		if got := previousParkSentence(a.GetSentence()); got != want {
			t.Errorf("%s: %q rebuilt as %q, want %q", code, a.GetSentence(), got, want)
		}
	}
	withVideo := attention.Compute(attention.Input{ParkCodes: []string{"human-review:ui"}, Evidence: &attention.Evidence{URL: "u", VideoDuration: 160e9}})
	if got, want := previousParkSentence(downconvertParkVerdictSentence(&pb.Session{Attention: withVideo}).GetAttention().GetSentence()), "Watch the evidence video (2:40), then approve · UI change"; got != want {
		t.Errorf("UI park with a video: rebuilt as %q, want %q", got, want)
	}
}

// TestParkMergeAttention_LeavesOtherAnswersAlone checks that answers without
// park codes, and a park row already carrying the old actions, pass through.
func TestParkMergeAttention_LeavesOtherAnswersAlone(t *testing.T) {
	for name, source := range map[string]*pb.Session{
		"unverified review": unverifiedMergeSession(),
		"fault fix": {Attention: &pb.SessionAttention{
			Kind:    pb.AttentionKind_ATTENTION_KIND_FIX,
			Actions: []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_RESUME, pb.AttentionAction_ATTENTION_ACTION_DISMISS},
		}},
		"old-shaped park":  blockedParkSession(),
		"no attention":     {},
		"in-progress park": {Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, ParkCodes: []string{"base-ci-red"}}},
	} {
		if got := downconvertParkMergeAttentionSession(source); got != source {
			t.Errorf("%s rewritten to %v", name, got)
		}
	}
}
