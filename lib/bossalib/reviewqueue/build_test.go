package reviewqueue

import (
	"maps"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const testOrigin = "git@github.com:acme/widgets.git"

var parkBase = time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)

type sessionOpt func(*pb.Session)

func newSession(id string, opts ...sessionOpt) *pb.Session {
	sess := &pb.Session{
		Id:            id,
		RepoOriginUrl: testOrigin,
		State:         pb.SessionState_SESSION_STATE_READY_FOR_REVIEW,
		PrNumber:      proto.Int32(1),
		Attention: &pb.SessionAttention{
			Kind:      pb.AttentionKind_ATTENTION_KIND_REVIEW,
			ParkCodes: []string{"human-review:ui"},
			Since:     timestamppb.New(parkBase),
		},
	}
	for _, opt := range opts {
		opt(sess)
	}
	return sess
}

func kind(k pb.AttentionKind) sessionOpt {
	return func(s *pb.Session) { s.Attention.Kind = k }
}

func noCodes() sessionOpt { return func(s *pb.Session) { s.Attention.ParkCodes = nil } }

func level(l pb.ConfidenceLevel) sessionOpt {
	return func(s *pb.Session) { s.Attention.ConfidenceLevel = l }
}

func parkedAt(minutes int) sessionOpt {
	return func(s *pb.Session) {
		s.Attention.Since = timestamppb.New(parkBase.Add(time.Duration(minutes) * time.Minute))
	}
}

func pr(n int32) sessionOpt { return func(s *pb.Session) { s.PrNumber = proto.Int32(n) } }

func origin(url string) sessionOpt { return func(s *pb.Session) { s.RepoOriginUrl = url } }

func state(st pb.SessionState) sessionOpt { return func(s *pb.Session) { s.State = st } }

func archived() sessionOpt {
	return func(s *pb.Session) { s.ArchivedAt = timestamppb.New(parkBase) }
}

func merging() sessionOpt { return func(s *pb.Session) { s.DisplayMerging = true } }

func rowIDs(rows []*pb.ReviewQueueRow) []string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.GetSession().GetId())
	}
	return ids
}

func countsByLevel(counts []*pb.ReviewQueueLevelCount) map[pb.ConfidenceLevel]int32 {
	out := make(map[pb.ConfidenceLevel]int32, len(counts))
	for _, c := range counts {
		out[c.GetConfidenceLevel()] = c.GetSessionCount()
	}
	return out
}

func TestBuildListsOnlyVerifyParks_Sessions_AC209001(t *testing.T) {
	cases := []struct {
		name    string
		session *pb.Session
		isRow   bool
	}{
		{"review park", newSession("review"), true},
		{"blocked park with codes", newSession("fix", kind(pb.AttentionKind_ATTENTION_KIND_FIX)), true},
		{"code-less fix fault", newSession("fault", kind(pb.AttentionKind_ATTENTION_KIND_FIX), noCodes()), false},
		{"code-less review", newSession("review-nocode", noCodes()), false},
		{"question", newSession("question", kind(pb.AttentionKind_ATTENTION_KIND_QUESTION)), false},
		{"merge", newSession("merge", kind(pb.AttentionKind_ATTENTION_KIND_MERGE)), false},
		{"in progress", newSession("progress", kind(pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS)), false},
		{"done", newSession("done", kind(pb.AttentionKind_ATTENTION_KIND_DONE)), false},
		{"archived park", newSession("archived", archived()), false},
		{"merged with stale codes", newSession("merged", state(pb.SessionState_SESSION_STATE_MERGED)), false},
		{"closed with stale codes", newSession("closed", state(pb.SessionState_SESSION_STATE_CLOSED)), false},
		{"no PR", newSession("nopr", func(s *pb.Session) { s.PrNumber = nil }), false},
		{"no attention", newSession("noatt", func(s *pb.Session) { s.Attention = nil }), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, _ := Build([]*pb.Session{tc.session}, pb.ReviewQueueSort_REVIEW_QUEUE_SORT_UNSPECIFIED)
			if got := len(rows) == 1; got != tc.isRow {
				t.Fatalf("row = %v, want %v (rows %v)", got, tc.isRow, rowIDs(rows))
			}
		})
	}
	rows, _ := Build([]*pb.Session{nil}, pb.ReviewQueueSort_REVIEW_QUEUE_SORT_UNSPECIFIED)
	if len(rows) != 0 {
		t.Fatalf("nil session produced rows %v", rowIDs(rows))
	}
}

func TestBuildRowLevelAndInputUntouched_Sessions_AC209001(t *testing.T) {
	cases := []struct {
		name    string
		session *pb.Session
		want    pb.ConfidenceLevel
	}{
		{"attention level wins", newSession("moderate", level(pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE)), pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE},
		{"leveled fix keeps its level", newSession("fixlow", kind(pb.AttentionKind_ATTENTION_KIND_FIX), level(pb.ConfidenceLevel_CONFIDENCE_LEVEL_LOW)), pb.ConfidenceLevel_CONFIDENCE_LEVEL_LOW},
		{"level-less fix park is blocked", newSession("fix", kind(pb.AttentionKind_ATTENTION_KIND_FIX)), pb.ConfidenceLevel_CONFIDENCE_LEVEL_BLOCKED},
		{"level-less review is unrated", newSession("review"), pb.ConfidenceLevel_CONFIDENCE_LEVEL_UNSPECIFIED},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := proto.Clone(tc.session)
			rows, _ := Build([]*pb.Session{tc.session}, pb.ReviewQueueSort_REVIEW_QUEUE_SORT_UNSPECIFIED)
			if len(rows) != 1 {
				t.Fatalf("rows = %v, want one", rowIDs(rows))
			}
			if got := rows[0].GetConfidenceLevel(); got != tc.want {
				t.Fatalf("level = %v, want %v", got, tc.want)
			}
			if rows[0].GetSession() != tc.session {
				t.Fatal("row session is not the input pointer")
			}
			if !proto.Equal(before, tc.session) {
				t.Fatal("Build mutated its input session")
			}
		})
	}
}

// sortFixture is one queue whose orders differ under every sort.
func sortFixture() []*pb.Session {
	return []*pb.Session{
		newSession("high-new", level(pb.ConfidenceLevel_CONFIDENCE_LEVEL_HIGH), parkedAt(50)),
		newSession("high-old", level(pb.ConfidenceLevel_CONFIDENCE_LEVEL_HIGH), parkedAt(10)),
		newSession("moderate", level(pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE), parkedAt(40)),
		newSession("low", level(pb.ConfidenceLevel_CONFIDENCE_LEVEL_LOW), parkedAt(5)),
		newSession("cannot", level(pb.ConfidenceLevel_CONFIDENCE_LEVEL_CANNOT_ASSESS), parkedAt(30)),
		newSession("blocked", kind(pb.AttentionKind_ATTENTION_KIND_FIX), parkedAt(20)),
		newSession("unrated", parkedAt(0)),
	}
}

func TestSortOrdersRows_Sessions_AC209002(t *testing.T) {
	cases := []struct {
		sort pb.ReviewQueueSort
		want []string
	}{
		{pb.ReviewQueueSort_REVIEW_QUEUE_SORT_UNSPECIFIED, []string{"high-old", "high-new", "moderate", "low", "cannot", "blocked", "unrated"}},
		{pb.ReviewQueueSort_REVIEW_QUEUE_SORT_MOST_CONFIDENT, []string{"high-old", "high-new", "moderate", "low", "cannot", "blocked", "unrated"}},
		{pb.ReviewQueueSort_REVIEW_QUEUE_SORT_LEAST_CONFIDENT, []string{"blocked", "cannot", "low", "moderate", "high-old", "high-new", "unrated"}},
		{pb.ReviewQueueSort_REVIEW_QUEUE_SORT_OLDEST, []string{"unrated", "low", "high-old", "blocked", "cannot", "moderate", "high-new"}},
	}
	for _, tc := range cases {
		t.Run(tc.sort.String(), func(t *testing.T) {
			rows, _ := Build(sortFixture(), tc.sort)
			if got := rowIDs(rows); !slices.Equal(got, tc.want) {
				t.Fatalf("order = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSortTieBreakIsTotal_Sessions_AC209002(t *testing.T) {
	same := level(pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE)
	sessions := []*pb.Session{
		newSession("z-newer", same, parkedAt(9), origin("https://github.com/acme/aaa"), pr(1)),
		newSession("b", same, parkedAt(1), origin("git@github.com:acme/zzz.git"), pr(1)),
		newSession("c", same, parkedAt(1), origin("https://github.com/acme/aaa"), pr(7)),
		newSession("e", same, parkedAt(1), origin("https://github.com/acme/aaa"), pr(2)),
		newSession("d", same, parkedAt(1), origin("https://github.com/acme/aaa"), pr(2)),
	}
	// Oldest park, then normalized origin, then PR number, then id.
	want := []string{"d", "e", "c", "b", "z-newer"}
	rng := rand.New(rand.NewPCG(1, 2))
	for range 20 {
		shuffled := slices.Clone(sessions)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		for _, sort := range []pb.ReviewQueueSort{
			pb.ReviewQueueSort_REVIEW_QUEUE_SORT_MOST_CONFIDENT,
			pb.ReviewQueueSort_REVIEW_QUEUE_SORT_LEAST_CONFIDENT,
			pb.ReviewQueueSort_REVIEW_QUEUE_SORT_OLDEST,
		} {
			rows, _ := Build(shuffled, sort)
			if got := rowIDs(rows); !slices.Equal(got, want) {
				t.Fatalf("%v order = %v, want %v", sort, got, want)
			}
		}
	}
}

func TestLevelCounts_Sessions_AC209002(t *testing.T) {
	sessions := append(sortFixture(),
		newSession("merging-now", kind(pb.AttentionKind_ATTENTION_KIND_MERGE), noCodes(), merging()),
		newSession("merging-parked", level(pb.ConfidenceLevel_CONFIDENCE_LEVEL_LOW), merging()),
		newSession("merging-archived", kind(pb.AttentionKind_ATTENTION_KIND_MERGE), noCodes(), merging(), archived()),
		newSession("merging-merged", kind(pb.AttentionKind_ATTENTION_KIND_MERGE), noCodes(), merging(), state(pb.SessionState_SESSION_STATE_MERGED)),
		newSession("fault", kind(pb.AttentionKind_ATTENTION_KIND_FIX), noCodes()),
	)
	rows, counts := Build(sessions, pb.ReviewQueueSort_REVIEW_QUEUE_SORT_UNSPECIFIED)
	for _, id := range []string{"merging-now", "merging-parked"} {
		if slices.Contains(rowIDs(rows), id) {
			t.Fatalf("merging session %q became a row", id)
		}
	}
	gotLevels := make([]pb.ConfidenceLevel, 0, len(counts))
	for _, c := range counts {
		gotLevels = append(gotLevels, c.GetConfidenceLevel())
	}
	wantLevels := []pb.ConfidenceLevel{
		pb.ConfidenceLevel_CONFIDENCE_LEVEL_UNSPECIFIED,
		pb.ConfidenceLevel_CONFIDENCE_LEVEL_HIGH,
		pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE,
		pb.ConfidenceLevel_CONFIDENCE_LEVEL_LOW,
		pb.ConfidenceLevel_CONFIDENCE_LEVEL_BLOCKED,
		pb.ConfidenceLevel_CONFIDENCE_LEVEL_CANNOT_ASSESS,
	}
	if !slices.Equal(gotLevels, wantLevels) {
		t.Fatalf("count levels = %v, want %v", gotLevels, wantLevels)
	}
	want := map[pb.ConfidenceLevel]int32{
		pb.ConfidenceLevel_CONFIDENCE_LEVEL_UNSPECIFIED:   1,
		pb.ConfidenceLevel_CONFIDENCE_LEVEL_HIGH:          4, // two rows + merging-now + merging-parked
		pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE:      1,
		pb.ConfidenceLevel_CONFIDENCE_LEVEL_LOW:           1,
		pb.ConfidenceLevel_CONFIDENCE_LEVEL_BLOCKED:       1,
		pb.ConfidenceLevel_CONFIDENCE_LEVEL_CANNOT_ASSESS: 1,
	}
	if got := countsByLevel(counts); !maps.Equal(got, want) {
		t.Fatalf("counts = %v, want %v", got, want)
	}

	_, empty := Build(nil, pb.ReviewQueueSort_REVIEW_QUEUE_SORT_UNSPECIFIED)
	if len(empty) != len(wantLevels) {
		t.Fatalf("empty queue lists %d levels, want %d", len(empty), len(wantLevels))
	}
	for _, c := range empty {
		if c.GetSessionCount() != 0 {
			t.Fatalf("empty queue counts %v", c)
		}
	}
}

func TestMatchesOrigin_Sessions_AC209001(t *testing.T) {
	sess := newSession("s", origin("git@github.com:acme/widgets.git"))
	cases := []struct {
		name   string
		origin string
		want   bool
	}{
		{"https form of the same repo", "https://github.com/acme/widgets", true},
		{"ssh form of the same repo", "git@github.com:acme/widgets.git", true},
		{"case-differing remote of the same repo", "https://github.com/Acme/Widgets", true},
		{"another repo", "https://github.com/acme/gadgets", false},
		{"empty origin", "", false},
		{"unparseable origin", "not a url", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MatchesOrigin(sess, tc.origin); got != tc.want {
				t.Fatalf("MatchesOrigin(%q) = %v, want %v", tc.origin, got, tc.want)
			}
		})
	}
	if MatchesOrigin(newSession("blank", origin("")), "") {
		t.Fatal("two empty origins matched")
	}
	if MatchesOrigin(newSession("bad", origin("not a url")), "not a url") {
		t.Fatal("two unparseable origins matched")
	}
}
