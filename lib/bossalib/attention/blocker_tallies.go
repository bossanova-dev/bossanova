package attention

import (
	"math"
	"slices"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// BlockerKindReviewFinding is the verify blocker kind of a review finding,
// the only kind that carries a severity and a file.
const BlockerKindReviewFinding = "review-finding"

// MaxBlockerFiles caps the review-finding file paths a session attention
// serves.
const MaxBlockerFiles = 3

// Blocker is one standing verify blocker on a PR head, as the verify stage's
// blockers file records it.
type Blocker struct {
	Kind     string
	Severity string
	File     string
}

// BlockerTally counts a PR head's standing blockers of one severity and kind.
// Severity is empty for every kind but a review finding.
type BlockerTally struct {
	Severity string
	Kind     string
	Count    int
}

// BlockerTallies groups blockers by severity and kind, most severe first: P0,
// P1 and P2 review findings, any other review severity, then check, conflict
// and the remaining kinds in first-seen order. files are the distinct paths of
// the review findings in blocker order, at most MaxBlockerFiles.
func BlockerTallies(blockers []Blocker) (tallies []BlockerTally, files []string) {
	for _, b := range blockers {
		severity := ""
		if b.Kind == BlockerKindReviewFinding {
			severity = b.Severity
			if b.File != "" && len(files) < MaxBlockerFiles && !slices.Contains(files, b.File) {
				files = append(files, b.File)
			}
		}
		i := slices.IndexFunc(tallies, func(t BlockerTally) bool { return t.Kind == b.Kind && t.Severity == severity })
		if i < 0 {
			tallies = append(tallies, BlockerTally{Severity: severity, Kind: b.Kind})
			i = len(tallies) - 1
		}
		tallies[i].Count++
	}
	slices.SortStableFunc(tallies, func(a, b BlockerTally) int { return blockerRank(a) - blockerRank(b) })
	return tallies, files
}

func blockerRank(t BlockerTally) int {
	switch t.Kind {
	case BlockerKindReviewFinding:
		if i := slices.Index([]string{"P0", "P1", "P2"}, t.Severity); i >= 0 {
			return i
		}
		return 3
	case "check":
		return 4
	case "conflict":
		return 5
	default:
		return 6
	}
}

func blockerTalliesProto(tallies []BlockerTally) []*pb.BlockerTally {
	if len(tallies) == 0 {
		return nil
	}
	out := make([]*pb.BlockerTally, 0, len(tallies))
	for _, t := range tallies {
		out = append(out, &pb.BlockerTally{Severity: t.Severity, Kind: t.Kind, BlockerCount: int32(min(t.Count, math.MaxInt32))})
	}
	return out
}
