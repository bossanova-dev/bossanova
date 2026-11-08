package main

import (
	"slices"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

func TestSessionsInRankOrder_Sessions_AC239501(t *testing.T) {
	sessions := []*pb.Session{
		{Id: "unranked-first"}, {Id: "low", TrackerRank: &pb.TrackerRank{Tier: 4}},
		{Id: "high-late", TrackerRank: &pb.TrackerRank{Tier: 2, DeadlineOn: proto.String("2026-11-02")}},
		{Id: "unranked-second"}, {Id: "urgent", TrackerRank: &pb.TrackerRank{Tier: 1}},
		{Id: "high-less", TrackerRank: &pb.TrackerRank{Tier: 2, DeadlineOn: proto.String("2026-11-01"), UnblockedTicketCount: 1}},
		{Id: "high-more", TrackerRank: &pb.TrackerRank{Tier: 2, DeadlineOn: proto.String("2026-11-01"), UnblockedTicketCount: 3}},
		{Id: "high-equal", TrackerRank: &pb.TrackerRank{Tier: 2, DeadlineOn: proto.String("2026-11-01"), UnblockedTicketCount: 3}},
	}
	before := slices.Clone(sessions)
	got := sessionsInRankOrder(sessions)
	want := []string{"urgent", "high-more", "high-equal", "high-less", "high-late", "low", "unranked-first", "unranked-second"}
	for i, s := range got {
		if s.GetId() != want[i] {
			t.Fatalf("row %d=%s want %s", i, s.GetId(), want[i])
		}
	}
	if !slices.Equal(sessions, before) {
		t.Fatal("input slice reordered")
	}
}

func TestSweepRepairsInRankOrder_Sessions_AC239501(t *testing.T) {
	mock := newTestMock()
	mock.branchHeadSHA = "base"
	for _, tc := range []struct {
		id   string
		rank *pb.TrackerRank
	}{
		{"unranked-first", nil}, {"low", &pb.TrackerRank{Tier: 4}}, {"urgent", &pb.TrackerRank{Tier: 1}}, {"unranked-second", nil},
	} {
		mock.sessions = append(mock.sessions, &pb.Session{Id: tc.id, TrackerRank: tc.rank,
			State: pb.SessionState_SESSION_STATE_AWAITING_CHECKS, DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_FAILING,
			RepoOriginUrl: "https://github.com/test/" + tc.id, BaseBranch: "main", PrNumber: proto.Int32(1),
			PrDisplayHeadSha: "head", LastRepairHeadSha: "head", LastRepairBaseSha: "base", LastRepairDisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_FAILING,
		})
	}
	rm := newTestMonitor(mock)
	// Repeated-head guards stop before spawning repair goroutines. The live base
	// read is synchronous inside maybeRepair, so this records sweep evaluation order.
	rm.sweepExistingSessions(t.Context())
	want := []string{"urgent", "low", "unranked-first", "unranked-second"}
	if len(mock.branchHeadReqs) != len(want) {
		t.Fatalf("base reads=%d want %d", len(mock.branchHeadReqs), len(want))
	}
	for i, req := range mock.branchHeadReqs {
		if req.GetRepoOriginUrl() != "https://github.com/test/"+want[i] {
			t.Fatalf("read %d=%s want %s", i, req.GetRepoOriginUrl(), want[i])
		}
	}
	if mock.startCalls != 0 {
		t.Fatal("duplicate repairs should not start a goroutine")
	}
}
