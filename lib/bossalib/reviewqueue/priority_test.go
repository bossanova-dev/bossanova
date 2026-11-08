package reviewqueue

import (
	"slices"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

func TestCompareTrackerRank_Sessions_AC239501(t *testing.T) {
	rank := func(tier int32, date string, leverage int32) *pb.TrackerRank {
		r := &pb.TrackerRank{Tier: tier, UnblockedTicketCount: leverage}
		if date != "" {
			r.DeadlineOn = proto.String(date)
		}
		return r
	}
	for _, tc := range []struct {
		name        string
		left, right *pb.TrackerRank
		want        int
	}{
		{"urgent high", rank(1, "", 0), rank(2, "", 0), -1},
		{"high medium", rank(2, "", 0), rank(3, "", 0), -1},
		{"medium low", rank(3, "", 0), rank(4, "", 0), -1},
		{"low none", rank(4, "", 0), rank(0, "", 0), -1},
		{"invalid none", rank(5, "", 0), rank(0, "", 0), 0},
		{"negative none", rank(-1, "", 0), rank(0, "", 0), 0},
		{"deadline", rank(2, "2026-11-01", 0), rank(2, "2026-11-02", 99), -1},
		{"dated", rank(2, "2026-11-01", 0), rank(2, "", 99), -1},
		{"leverage", rank(2, "2026-11-01", 3), rank(2, "2026-11-01", 1), -1},
		{"ranked", rank(0, "", 0), nil, -1},
		{"nil", nil, nil, 0},
		{"metadata ignored", &pb.TrackerRank{Tier: 1, TierSource: "first", IsScheduled: true}, rank(1, "", 0), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CompareTrackerRank(tc.left, tc.right); got != tc.want {
				t.Fatalf("compare=%d want %d", got, tc.want)
			}
			if got := CompareTrackerRank(tc.right, tc.left); got != -tc.want {
				t.Fatalf("reverse=%d want %d", got, -tc.want)
			}
		})
	}
}

func TestPrioritySortMixedRows_Sessions_AC239501(t *testing.T) {
	ranked := func(id string, tier int32, date string, leverage int32) *pb.Session {
		s := newSession(id)
		s.TrackerRank = &pb.TrackerRank{Tier: tier, UnblockedTicketCount: leverage}
		if date != "" {
			s.TrackerRank.DeadlineOn = proto.String(date)
		}
		return s
	}
	sessions := []*pb.Session{
		newSession("unranked-new", parkedAt(3)), ranked("low", 4, "", 0),
		ranked("high-undated", 2, "", 99), ranked("none", 0, "", 0),
		ranked("high-late", 2, "2026-11-02", 99), ranked("high-less", 2, "2026-11-01", 1),
		ranked("high-more", 2, "2026-11-01", 3), ranked("urgent", 1, "", 0),
		newSession("unranked-old", parkedAt(1)),
	}
	rows := make([]*pb.ReviewQueueRow, 0, len(sessions)+1)
	rows = append(rows, &pb.ReviewQueueRow{Session: ranked("audit", 1, "2026-01-01", 100), IsAuditSample: true})
	for _, s := range sessions {
		rows = append(rows, &pb.ReviewQueueRow{Session: s})
	}
	Sort(rows, pb.ReviewQueueSort_REVIEW_QUEUE_SORT_PRIORITY)
	want := []string{"urgent", "high-more", "high-less", "high-late", "high-undated", "low", "none", "unranked-old", "unranked-new", "audit"}
	if got := rowIDs(rows); !slices.Equal(got, want) {
		t.Fatalf("order=%v want %v", got, want)
	}
}
