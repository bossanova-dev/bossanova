// Package sessionorder orders and caps a session list by created or updated
// time. bosso applies it to ProxyListSessions and the MCP list_sessions tool
// applies it after its backend returns, so both answer the same way.
package sessionorder

import (
	"cmp"
	"slices"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Known reports whether o is one of the declared SessionListOrdering values,
// including unspecified.
func Known(o pb.SessionListOrdering) bool {
	_, ok := pb.SessionListOrdering_name[int32(o)]
	return ok
}

// Apply returns sessions ordered by o and capped at limit when limit > 0.
// Ordering is stable by the chosen timestamp and direction, with ties by id
// ascending; a nil timestamp counts as the zero time and a nil session sorts
// last. Unspecified ordering keeps the input order. Apply never reorders the
// input slice or mutates a session, so it is safe on shared reads.
func Apply(sessions []*pb.Session, o pb.SessionListOrdering, limit int32) []*pb.Session {
	out := sessions
	if key, newestFirst, ok := sortKey(o); ok {
		out = slices.Clone(sessions)
		slices.SortStableFunc(out, func(a, b *pb.Session) int {
			return compare(a, b, key, newestFirst)
		})
	}
	if limit > 0 && int(limit) < len(out) {
		out = out[:limit:limit]
	}
	return out
}

func sortKey(o pb.SessionListOrdering) (key func(*pb.Session) *timestamppb.Timestamp, newestFirst, ok bool) {
	switch o {
	case pb.SessionListOrdering_SESSION_LIST_ORDERING_CREATED_OLDEST_FIRST:
		return (*pb.Session).GetCreatedAt, false, true
	case pb.SessionListOrdering_SESSION_LIST_ORDERING_CREATED_NEWEST_FIRST:
		return (*pb.Session).GetCreatedAt, true, true
	case pb.SessionListOrdering_SESSION_LIST_ORDERING_UPDATED_OLDEST_FIRST:
		return (*pb.Session).GetUpdatedAt, false, true
	case pb.SessionListOrdering_SESSION_LIST_ORDERING_UPDATED_NEWEST_FIRST:
		return (*pb.Session).GetUpdatedAt, true, true
	default:
		return nil, false, false
	}
}

func compare(a, b *pb.Session, key func(*pb.Session) *timestamppb.Timestamp, newestFirst bool) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	}
	byTime := timeOf(key(a)).Compare(timeOf(key(b)))
	if newestFirst {
		byTime = -byTime
	}
	if byTime != 0 {
		return byTime
	}
	return cmp.Compare(a.GetId(), b.GetId())
}

func timeOf(ts *timestamppb.Timestamp) time.Time {
	if ts == nil {
		return time.Time{}
	}
	return ts.AsTime()
}
