package apiversion

import (
	"slices"
	"testing"

	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"google.golang.org/protobuf/proto"
)

// noReceiptParkSession is a no-receipt verify park as attention.Compute serves
// it today.
func noReceiptParkSession() *pb.Session {
	return &pb.Session{Id: "sess-no-receipt", Attention: attention.Compute(attention.Input{ParkCodes: []string{"no-receipt"}})}
}

// TestFactoryReviewParkSentence_EveryPinKeepsItsSentence runs today's
// no-receipt park through every pinned version. Each pin must read the exact
// sentence it was served: the new one from V20260922, "Built by hand, no
// factory review" at V20260920 and V20260921, and the send-back form before
// that. Pins older than V20260917 carry no attention at all.
func TestFactoryReviewParkSentence_EveryPinKeepsItsSentence(t *testing.T) {
	reg := DefaultRegistry()
	if got := noReceiptParkSession().GetAttention().GetSentence(); got != "No review of the latest commit" {
		t.Fatalf("today's no-receipt sentence = %q, want the V20260922 sentence", got)
	}
	for _, v := range reg.All() {
		t.Run(v.String(), func(t *testing.T) {
			source := noReceiptParkSession()
			pristine := proto.Clone(source)
			resp := &pb.ProxyGetSessionResponse{Session: source}
			ProductionChanges().Apply(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, resp, v)
			if !proto.Equal(source, pristine) {
				t.Fatal("mutated cached source")
			}
			got := resp.GetSession().GetAttention()
			var want string
			switch {
			case reg.Newer(V20260917, v):
				if got != nil {
					t.Fatalf("attention = %v, want none before V20260917", got)
				}
				return
			case !reg.Newer(V20260922, v):
				want = "No review of the latest commit"
			case v == V20260921 || v == V20260920:
				want = "Built by hand, no factory review"
			default:
				want = "Built by hand, no factory review · send it back through the factory"
			}
			if got.GetSentence() != want {
				t.Fatalf("sentence = %q, want %q", got.GetSentence(), want)
			}
			if !slices.Equal(got.GetParkCodes(), []string{"no-receipt"}) {
				t.Fatalf("park codes = %q, want no-receipt kept", got.GetParkCodes())
			}
		})
	}
}

// TestFactoryReviewParkSentence_LeavesOtherAnswersAlone checks that only a
// park row carrying the new sentence is rewritten: another park, the old
// sentence an older daemon still serves, and the same text on a row with no
// park codes all pass through as the same pointer.
func TestFactoryReviewParkSentence_LeavesOtherAnswersAlone(t *testing.T) {
	for name, source := range map[string]*pb.Session{
		"other park":   {Attention: attention.Compute(attention.Input{ParkCodes: []string{"ledger-open"}})},
		"old sentence": {Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_FIX, Sentence: "Built by hand, no factory review", ParkCodes: []string{"no-receipt"}}},
		"no park codes": {Attention: &pb.SessionAttention{
			Kind:     pb.AttentionKind_ATTENTION_KIND_REVIEW,
			Sentence: "No review of the latest commit",
		}},
		"no attention": {},
	} {
		if got := downconvertFactoryReviewParkSentenceSession(source); got != source {
			t.Errorf("%s rewritten to %v", name, got)
		}
	}
}
