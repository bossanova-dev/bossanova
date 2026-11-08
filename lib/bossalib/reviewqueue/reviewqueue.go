// Package reviewqueue builds the review queue: the PRs verify has parked for a
// person, ranked by tracker priority, confidence level or age, with per-level header counts.
//
// bossd and bosso serve the same queue from this one rule set, and clients may
// reuse Sort to merge queues across repositories. The builder never mutates the
// sessions it is given: bosso shares cached session pointers, so rows point at
// the input sessions unchanged.
package reviewqueue

import (
	"cmp"
	"slices"
	"strings"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
)

// Build returns waiting rows followed by recent merged High audit samples,
// and one header count
// per ConfidenceLevel in enum order. Audit rows never contribute to counts.
//
// A session is a row when it is open, not archived, has a PR, and its attention
// is a verify park: REVIEW or FIX carrying park codes. A code-less FIX is an
// ordinary fault, not a park. An open, non-archived PR session that is merging
// now counts as HIGH without becoming a row, even while its park codes linger,
// since nobody is waiting on it.
func Build(sessions []*pb.Session, sort pb.ReviewQueueSort, now time.Time) ([]*pb.ReviewQueueRow, []*pb.ReviewQueueLevelCount) {
	rows := make([]*pb.ReviewQueueRow, 0)
	counts := make(map[pb.ConfidenceLevel]int32)
	for _, sess := range sessions {
		if isAuditRow(sess, now) {
			rows = append(rows, &pb.ReviewQueueRow{Session: sess, ConfidenceLevel: pb.ConfidenceLevel_CONFIDENCE_LEVEL_HIGH, IsAuditSample: true})
			continue
		}
		if !isOpenPR(sess) {
			continue
		}
		if sess.GetDisplayMerging() {
			counts[pb.ConfidenceLevel_CONFIDENCE_LEVEL_HIGH]++
			continue
		}
		if isVerifyPark(sess.GetAttention()) {
			level := rowLevel(sess.GetAttention())
			rows = append(rows, &pb.ReviewQueueRow{Session: sess, ConfidenceLevel: level})
			counts[level]++
		}
	}
	Sort(rows, sort)
	return rows, levelCounts(counts)
}

// Sort puts audits after waiting rows, newest merge first. Waiting rows use
// the requested sort, with a total tie-break: the
// oldest park, then the normalized origin URL, then the PR number, then the
// session id. UNSPECIFIED sorts as MOST_CONFIDENT, and rows without a level sort
// last under both confidence sorts. PRIORITY orders tracker tier, deadline and
// tickets unblocked, with unranked sessions after ranked ones.
func Sort(rows []*pb.ReviewQueueRow, sort pb.ReviewQueueSort) {
	slices.SortStableFunc(rows, func(left, right *pb.ReviewQueueRow) int {
		if order := compareAudit(left, right); order != 0 {
			return order
		}
		if left.GetIsAuditSample() && right.GetIsAuditSample() {
			return 0
		}
		if order := compareBySort(left, right, sort); order != 0 {
			return order
		}
		return compareTieBreak(left, right)
	})
}

// MatchesOrigin reports whether the session's repository is originURL. Both
// sides are canonicalized, so the SSH and HTTPS forms of one repository match,
// and so do GitHub remotes that differ only in case across daemons; a side that
// does not canonicalize never matches. The origin, not repo_id, is the
// cross-machine repository identity.
func MatchesOrigin(s *pb.Session, originURL string) bool {
	want := vcs.CanonicalRepoURL(originURL)
	if want == "" {
		return false
	}
	return vcs.CanonicalRepoURL(s.GetRepoOriginUrl()) == want
}

func isOpenPR(sess *pb.Session) bool {
	if sess == nil || sess.GetArchivedAt() != nil || sess.PrNumber == nil {
		return false
	}
	st := sess.GetState()
	return st != pb.SessionState_SESSION_STATE_MERGED && st != pb.SessionState_SESSION_STATE_CLOSED
}

func isVerifyPark(att *pb.SessionAttention) bool {
	if len(att.GetParkCodes()) == 0 {
		return false
	}
	k := att.GetKind()
	return k == pb.AttentionKind_ATTENTION_KIND_REVIEW || k == pb.AttentionKind_ATTENTION_KIND_FIX
}

func rowLevel(att *pb.SessionAttention) pb.ConfidenceLevel {
	if level := att.GetConfidenceLevel(); level != pb.ConfidenceLevel_CONFIDENCE_LEVEL_UNSPECIFIED {
		return level
	}
	if att.GetKind() == pb.AttentionKind_ATTENTION_KIND_FIX {
		return pb.ConfidenceLevel_CONFIDENCE_LEVEL_BLOCKED
	}
	return pb.ConfidenceLevel_CONFIDENCE_LEVEL_UNSPECIFIED
}

func levelCounts(counts map[pb.ConfidenceLevel]int32) []*pb.ReviewQueueLevelCount {
	levels := make([]pb.ConfidenceLevel, 0, len(pb.ConfidenceLevel_name))
	for value := range pb.ConfidenceLevel_name {
		levels = append(levels, pb.ConfidenceLevel(value))
	}
	slices.Sort(levels)
	out := make([]*pb.ReviewQueueLevelCount, 0, len(levels))
	for _, level := range levels {
		out = append(out, &pb.ReviewQueueLevelCount{ConfidenceLevel: level, SessionCount: counts[level]})
	}
	return out
}

// confidenceRank is how confident a level is; zero (UNSPECIFIED) is unrated.
var confidenceRank = map[pb.ConfidenceLevel]int{
	pb.ConfidenceLevel_CONFIDENCE_LEVEL_HIGH:          5,
	pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE:      4,
	pb.ConfidenceLevel_CONFIDENCE_LEVEL_LOW:           3,
	pb.ConfidenceLevel_CONFIDENCE_LEVEL_CANNOT_ASSESS: 2,
	pb.ConfidenceLevel_CONFIDENCE_LEVEL_BLOCKED:       1,
}

func compareBySort(left, right *pb.ReviewQueueRow, sort pb.ReviewQueueSort) int {
	switch sort {
	case pb.ReviewQueueSort_REVIEW_QUEUE_SORT_PRIORITY:
		return CompareTrackerRank(left.GetSession().GetTrackerRank(), right.GetSession().GetTrackerRank())
	case pb.ReviewQueueSort_REVIEW_QUEUE_SORT_SMALLEST:
		l, r := left.GetSession(), right.GetSession()
		lKnown, rKnown := l.GetPrChangedFileCount() > 0, r.GetPrChangedFileCount() > 0
		switch {
		case lKnown && !rKnown:
			return -1
		case !lKnown && rKnown:
			return 1
		case !lKnown:
			return 0
		default:
			return cmp.Or(
				cmp.Compare(int64(l.GetPrAddedLineCount())+int64(l.GetPrDeletedLineCount()), int64(r.GetPrAddedLineCount())+int64(r.GetPrDeletedLineCount())),
				cmp.Compare(l.GetPrChangedFileCount(), r.GetPrChangedFileCount()),
			)
		}
	case pb.ReviewQueueSort_REVIEW_QUEUE_SORT_OLDEST:
		return compareSince(left, right)
	case pb.ReviewQueueSort_REVIEW_QUEUE_SORT_LEAST_CONFIDENT:
		return compareRank(left, right, false)
	default:
		return compareRank(left, right, true)
	}
}

// compareRank orders by confidence, descending when mostFirst, with unrated
// rows last either way.
func compareRank(left, right *pb.ReviewQueueRow, mostFirst bool) int {
	leftRank := confidenceRank[left.GetConfidenceLevel()]
	rightRank := confidenceRank[right.GetConfidenceLevel()]
	switch {
	case leftRank == rightRank:
		return 0
	case leftRank == 0:
		return 1
	case rightRank == 0:
		return -1
	case mostFirst:
		return cmp.Compare(rightRank, leftRank)
	default:
		return cmp.Compare(leftRank, rightRank)
	}
}

func compareSince(left, right *pb.ReviewQueueRow) int {
	leftSince := left.GetSession().GetAttention().GetSince()
	rightSince := right.GetSession().GetAttention().GetSince()
	return cmp.Or(
		cmp.Compare(leftSince.GetSeconds(), rightSince.GetSeconds()),
		cmp.Compare(leftSince.GetNanos(), rightSince.GetNanos()),
	)
}

func compareTieBreak(left, right *pb.ReviewQueueRow) int {
	leftSession, rightSession := left.GetSession(), right.GetSession()
	return cmp.Or(
		compareSince(left, right),
		strings.Compare(
			vcs.NormalizeRepoURL(leftSession.GetRepoOriginUrl()),
			vcs.NormalizeRepoURL(rightSession.GetRepoOriginUrl()),
		),
		cmp.Compare(leftSession.GetPrNumber(), rightSession.GetPrNumber()),
		strings.Compare(leftSession.GetId(), rightSession.GetId()),
	)
}

// CompareTrackerRank is the shared rank order used by the review queue and the
// repair sweep: tier, earliest deadline, then most tickets unblocked. Nil is last.
func CompareTrackerRank(left, right *pb.TrackerRank) int {
	switch {
	case left == nil && right == nil:
		return 0
	case left == nil:
		return 1
	case right == nil:
		return -1
	}
	if order := cmp.Compare(trackerTier(left.GetTier()), trackerTier(right.GetTier())); order != 0 {
		return order
	}
	leftDate, rightDate := left.GetDeadlineOn(), right.GetDeadlineOn()
	switch {
	case leftDate == "" && rightDate != "":
		return 1
	case leftDate != "" && rightDate == "":
		return -1
	}
	return cmp.Or(strings.Compare(leftDate, rightDate), cmp.Compare(right.GetUnblockedTicketCount(), left.GetUnblockedTicketCount()))
}

func trackerTier(tier int32) int32 {
	if tier < 1 || tier > 4 {
		return 5
	}
	return tier
}
