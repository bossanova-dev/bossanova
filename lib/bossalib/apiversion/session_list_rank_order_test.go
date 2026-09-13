package apiversion

import (
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func rankedSessionListFixture() []*pb.Session {
	low, mid, high := int64(100), int64(200), int64(300)
	at := func(day int) *timestamppb.Timestamp {
		return timestamppb.New(time.Date(2026, time.January, day, 12, 0, 0, 0, time.UTC))
	}
	// Already in the CURRENT (rank-aware) order the server produces: the three
	// ranked rows lead by rank ascending even though they are the oldest, then
	// the unranked rows by created_at descending with an id-ascending tie-break.
	return []*pb.Session{
		{Id: "r-low", ListRank: &low, CreatedAt: at(1)},
		{Id: "r-mid", ListRank: &mid, CreatedAt: at(5)},
		{Id: "r-high", ListRank: &high, CreatedAt: at(8)},
		{Id: "u-new", CreatedAt: at(9)},
		{Id: "u-tie-a", CreatedAt: at(3)},
		{Id: "u-tie-b", CreatedAt: at(3)},
		{Id: "u-old", CreatedAt: at(2)},
	}
}

// legacySessionListOrder is what a pre-V20260915 server returned for
// rankedSessionListFixture: created_at descending, then id ascending, with the
// rank ignored entirely. It shares not one leading element with the current
// order, so "the transform ran" and "the transform did nothing" can never look
// alike.
var legacySessionListOrder = []string{"u-new", "r-high", "r-mid", "u-tie-a", "u-tie-b", "u-old", "r-low"}

var currentSessionListOrder = []string{"r-low", "r-mid", "r-high", "u-new", "u-tie-a", "u-tie-b", "u-old"}

func sessionIDs(sessions []*pb.Session) []string {
	ids := make([]string, 0, len(sessions))
	for _, sess := range sessions {
		ids = append(ids, sess.GetId())
	}
	return ids
}

func assertOrder(t *testing.T, got []*pb.Session, want []string) {
	t.Helper()
	ids := sessionIDs(got)
	if len(ids) != len(want) {
		t.Fatalf("session order = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("session order = %v, want %v", ids, want)
		}
	}
}

// TestSessionListRankOrderChange_AppliesOneVersionBack drives the change
// through the real Changes chain rather than calling TransformResponse
// directly, so what it proves is what a pinned client actually receives: at one
// version below the new constant the response comes back in the legacy
// created_at/id order, and at the new version it is untouched.
func TestSessionListRankOrderChange_AppliesOneVersionBack(t *testing.T) {
	changes := ProductionChanges()

	for _, tc := range []struct {
		name    string
		version Version
		want    []string
	}{
		{name: "one version back sees the legacy order", version: V20260914, want: legacySessionListOrder},
		{name: "baseline sees the legacy order", version: Baseline, want: legacySessionListOrder},
		{name: "the newest version is untouched", version: V20260915, want: currentSessionListOrder},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := &pb.ProxyListSessionsResponse{Sessions: rankedSessionListFixture()}
			changes.Apply(bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, msg, tc.version)
			assertOrder(t, msg.GetSessions(), tc.want)
		})
	}
}

// TestSessionListRankOrderChange_DoesNotStripTheRank pins the decision the plan
// called out as the trap: the down-convert RE-SORTS and leaves list_rank
// populated. Clearing the field instead would hide the cause while leaving the
// response in the server's order, which is not the order any older client saw.
func TestSessionListRankOrderChange_DoesNotStripTheRank(t *testing.T) {
	msg := &pb.ProxyListSessionsResponse{Sessions: rankedSessionListFixture()}
	ProductionChanges().Apply(bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, msg, Baseline)

	ranked := 0
	for _, sess := range msg.GetSessions() {
		if sess.ListRank != nil {
			ranked++
		}
	}
	if ranked != 3 {
		t.Fatalf("down-converted response carries %d ranked sessions, want 3; the transform must re-sort, not strip", ranked)
	}
}

// TestSessionListRankOrderChange_IsANoopForOtherMethods proves the scope
// claim. ProxyGetSession is the sharpest case available: it carries a Session,
// so a transform that matched on the payload type rather than the procedure
// would reach it — and it has no list order to restore.
func TestSessionListRankOrderChange_IsANoopForOtherMethods(t *testing.T) {
	t.Run("a single-session response is untouched", func(t *testing.T) {
		rank := int64(100)
		msg := &pb.ProxyGetSessionResponse{Session: &pb.Session{Id: "s-1", ListRank: &rank}}
		SessionListRankOrderChange{}.TransformResponse(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, msg)
		if msg.GetSession().GetId() != "s-1" || msg.GetSession().ListRank == nil {
			t.Fatal("ProxyGetSession must be untouched by the session-list ordering transform")
		}
	})

	t.Run("a non-session list response is untouched", func(t *testing.T) {
		msg := &pb.ProxyListAccountsResponse{Accounts: []*pb.Account{{Id: "b"}, {Id: "a"}}}
		SessionListRankOrderChange{}.TransformResponse(bossanovav1connect.OrchestratorServiceProxyListAccountsProcedure, msg)
		if msg.GetAccounts()[0].GetId() != "b" || msg.GetAccounts()[1].GetId() != "a" {
			t.Fatal("ProxyListAccounts must keep its own order; the session-list transform is procedure-scoped")
		}
	})

	t.Run("the session-list procedure with a foreign payload is untouched", func(t *testing.T) {
		// A type mismatch on the targeted procedure must be a silent no-op, not
		// a panic: the transform runs inside the response path of every request.
		msg := &pb.ProxyListAccountsResponse{Accounts: []*pb.Account{{Id: "b"}, {Id: "a"}}}
		SessionListRankOrderChange{}.TransformResponse(bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, msg)
		if msg.GetAccounts()[0].GetId() != "b" {
			t.Fatal("a payload the transform does not recognise must pass through unchanged")
		}
	})
}

// TestSessionListRankOrderChange_ToleratesANilSession keeps the comparator
// total. A malformed response must not turn a transform into the thing that
// fails the request.
func TestSessionListRankOrderChange_ToleratesANilSession(t *testing.T) {
	msg := &pb.ProxyListSessionsResponse{Sessions: []*pb.Session{
		nil,
		{Id: "s-1", CreatedAt: timestamppb.New(time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC))},
		nil,
	}}

	SessionListRankOrderChange{}.TransformResponse(bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, msg)

	if got := msg.GetSessions()[0].GetId(); got != "s-1" {
		t.Fatalf("present session sorted to index %v, want first (nil entries sort last in the legacy comparator)", sessionIDs(msg.GetSessions()))
	}
}

// TestSessionListRankOrderChange_CoversTheDeprecatedUnionProcedure pins the
// second targeted procedure. ProxyListSessionsAcrossOrganizations is the
// deprecated spelling of the same union read, so a client still pinned to it
// observes the same reordering and is owed the same restoration.
func TestSessionListRankOrderChange_CoversTheDeprecatedUnionProcedure(t *testing.T) {
	//nolint:staticcheck // The deprecated RPC remains supported for pinned clients.
	msg := &pb.ProxyListSessionsAcrossOrganizationsResponse{Sessions: rankedSessionListFixture()}
	ProductionChanges().Apply(bossanovav1connect.OrchestratorServiceProxyListSessionsAcrossOrganizationsProcedure, msg, V20260914)
	assertOrder(t, msg.GetSessions(), legacySessionListOrder)
}
