package reviewqueue

import (
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestAuditSampleStableAndDistributed(t *testing.T) {
	count := 0
	for n := int32(1); n <= 2000; n++ {
		got := IsAuditSample(testOrigin, n)
		for _, origin := range []string{testOrigin, "https://github.com/acme/widgets", "https://github.com/Acme/Widgets.git", "git@github.com:ACME/Widgets.git"} {
			if got != IsAuditSample(origin, n) {
				t.Fatalf("sample changes with origin spelling %q or refresh for PR %d", origin, n)
			}
		}
		if got {
			count++
		}
	}
	if count < 140 || count > 260 {
		t.Fatalf("sample count = %d", count)
	}
	for _, origin := range []string{"", "not a repository"} {
		if IsAuditSample(origin, 1) {
			t.Fatal("invalid origin sampled")
		}
	}
	if IsAuditSample(testOrigin, 0) || IsAuditSample(testOrigin, -1) {
		t.Fatal("invalid PR sampled")
	}
}
func auditSession(id string, ago time.Duration) *pb.Session {
	n := int32(1)
	for !IsAuditSample(testOrigin, n) {
		n++
	}
	return &pb.Session{Id: id, RepoOriginUrl: testOrigin, PrNumber: proto.Int32(n), State: pb.SessionState_SESSION_STATE_MERGED, MergedConfidenceLevel: pb.ConfidenceLevel_CONFIDENCE_LEVEL_HIGH, StateEnteredAt: timestamppb.New(parkBase.Add(-ago)), ArchivedAt: timestamppb.New(parkBase)}
}
func TestBuildAuditConditions_Sessions_AC213601(t *testing.T) {
	cases := []struct {
		name   string
		change func(*pb.Session)
		want   bool
	}{
		{"archived", func(*pb.Session) {}, true},
		{"active", func(s *pb.Session) { s.ArchivedAt = nil }, true},
		{"future", func(s *pb.Session) { s.StateEnteredAt = timestamppb.New(parkBase.Add(time.Hour)) }, true},
		{"unsampled", func(s *pb.Session) {
			for IsAuditSample(testOrigin, s.GetPrNumber()) {
				*s.PrNumber++
			}
		}, false},
		{"moderate", func(s *pb.Session) { s.MergedConfidenceLevel = pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE }, false},
		{"unset", func(s *pb.Session) { s.MergedConfidenceLevel = 0 }, false},
		{"expired", func(s *pb.Session) { s.StateEnteredAt = timestamppb.New(parkBase.Add(-AuditWindow)) }, false},
		{"missing time", func(s *pb.Session) { s.StateEnteredAt = nil }, false},
		{"closed", func(s *pb.Session) { s.State = pb.SessionState_SESSION_STATE_CLOSED }, false},
		{"no PR", func(s *pb.Session) { s.PrNumber = nil }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := auditSession("audit", time.Hour)
			tc.change(sess)
			before := proto.Clone(sess)
			rows, counts := Build([]*pb.Session{sess}, 0, parkBase)
			if (len(rows) == 1) != tc.want {
				t.Fatalf("rows = %v", rows)
			}
			if tc.want && (!rows[0].GetIsAuditSample() || rows[0].GetConfidenceLevel() != pb.ConfidenceLevel_CONFIDENCE_LEVEL_HIGH || rows[0].Session != sess) {
				t.Fatalf("audit row = %v", rows)
			}
			for _, c := range counts {
				if c.SessionCount != 0 {
					t.Fatal("audit counted")
				}
			}
			if !proto.Equal(before, sess) {
				t.Fatal("mutated input")
			}
		})
	}
}
func TestAuditRowsSortAfterWaiting_Sessions_AC213601(t *testing.T) {
	for _, sort := range []pb.ReviewQueueSort{0, 1, 2, 3} {
		input := []*pb.Session{auditSession("old", 2*time.Hour), newSession("waiting"), auditSession("new", time.Hour)}
		for range 10 {
			rand.Shuffle(len(input), func(i, j int) { input[i], input[j] = input[j], input[i] })
			rows, _ := Build(input, sort, parkBase)
			if !slices.Equal(rowIDs(rows), []string{"waiting", "new", "old"}) {
				t.Fatal(rowIDs(rows))
			}
		}
	}
}

func TestAuditTieBreakIgnoresParkAge(t *testing.T) {
	a, b := auditSession("a", time.Hour), auditSession("b", time.Hour)
	a.Attention = &pb.SessionAttention{Since: timestamppb.New(parkBase)}
	b.Attention = &pb.SessionAttention{Since: timestamppb.New(parkBase.Add(-time.Hour))}
	rows, _ := Build([]*pb.Session{b, a}, pb.ReviewQueueSort_REVIEW_QUEUE_SORT_OLDEST, parkBase)
	if !slices.Equal(rowIDs(rows), []string{"a", "b"}) {
		t.Fatal(rowIDs(rows))
	}
}
