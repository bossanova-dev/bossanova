package reviewqueue

import (
	"slices"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// signedOff is a session whose current head verify parked for a sign-off at
// level, parked minutes after parkBase, and whose attention now says what its
// chat is doing.
func signedOff(id string, lvl pb.ConfidenceLevel, minutes int, now pb.AttentionKind) *pb.Session {
	sess := newSession(id, level(lvl), parkedAt(minutes))
	sess.VerifyPark = proto.CloneOf(sess.GetAttention())
	sess.Attention = &pb.SessionAttention{Kind: now, Since: timestamppb.New(parkBase.Add(999 * time.Minute))}
	return sess
}

func TestBuildFollowsTheVerifyParkWhateverTheChatDoes_Sessions_AC209001(t *testing.T) {
	moderate := pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE
	newHead := newSession("new-head", kind(pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS), noCodes())
	merged := signedOff("merged", moderate, 0, pb.AttentionKind_ATTENTION_KIND_DONE)
	merged.State = pb.SessionState_SESSION_STATE_MERGED
	closed := signedOff("closed", moderate, 0, pb.AttentionKind_ATTENTION_KIND_DONE)
	closed.State = pb.SessionState_SESSION_STATE_CLOSED
	cases := []struct {
		name    string
		session *pb.Session
		isRow   bool
	}{
		{"chat asks a question", signedOff("question", moderate, 0, pb.AttentionKind_ATTENTION_KIND_QUESTION), true},
		{"chat at work", signedOff("working", moderate, 0, pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS), true},
		{"fault outranks the park", signedOff("fault", moderate, 0, pb.AttentionKind_ATTENTION_KIND_FIX), true},
		{"idle park", signedOff("idle", moderate, 0, pb.AttentionKind_ATTENTION_KIND_REVIEW), true},
		{"new head verify has not parked", newHead, false},
		{"merged", merged, false},
		{"closed", closed, false},
		{"blocked park while a chat works", newSession("blocked-working", kind(pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS), noCodes()), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := proto.Clone(tc.session)
			rows, counts := Build([]*pb.Session{tc.session}, pb.ReviewQueueSort_REVIEW_QUEUE_SORT_UNSPECIFIED, parkBase)
			if got := len(rows) == 1; got != tc.isRow {
				t.Fatalf("row = %v, want %v (rows %v)", got, tc.isRow, rowIDs(rows))
			}
			if !proto.Equal(before, tc.session) {
				t.Fatal("Build mutated its input session")
			}
			if !tc.isRow {
				return
			}
			if got := rows[0].GetConfidenceLevel(); got != moderate {
				t.Fatalf("level = %v, want the park's %v", got, moderate)
			}
			if got := countsByLevel(counts)[moderate]; got != 1 {
				t.Fatalf("moderate count = %d, want 1", got)
			}
		})
	}
}

// A row's age is its park's, not the newer time its chat activity began, so
// the oldest park still comes first.
func TestSortUsesTheParkTime_Sessions_AC209002(t *testing.T) {
	high := pb.ConfidenceLevel_CONFIDENCE_LEVEL_HIGH
	sessions := []*pb.Session{
		newSession("idle-newer", level(high), parkedAt(20)),
		signedOff("asking-older", high, 10, pb.AttentionKind_ATTENTION_KIND_QUESTION),
	}
	for _, sort := range []pb.ReviewQueueSort{pb.ReviewQueueSort_REVIEW_QUEUE_SORT_MOST_CONFIDENT, pb.ReviewQueueSort_REVIEW_QUEUE_SORT_OLDEST} {
		rows, _ := Build(sessions, sort, parkBase)
		if got, want := rowIDs(rows), []string{"asking-older", "idle-newer"}; !slices.Equal(got, want) {
			t.Fatalf("%v order = %v, want %v", sort, got, want)
		}
	}
}
