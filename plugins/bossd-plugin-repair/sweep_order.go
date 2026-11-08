package main

import (
	"slices"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/reviewqueue"
)

// sessionsInRankOrder leaves equal ranks in ListSessions order without changing
// the caller's slice; unranked sessions follow every ranked session.
func sessionsInRankOrder(sessions []*pb.Session) []*pb.Session {
	ordered := slices.Clone(sessions)
	slices.SortStableFunc(ordered, func(left, right *pb.Session) int {
		return reviewqueue.CompareTrackerRank(left.GetTrackerRank(), right.GetTrackerRank())
	})
	return ordered
}
