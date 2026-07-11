package session

import (
	"errors"
	"fmt"
	"strings"

	"github.com/recurser/bossalib/vcs"
)

// AlreadyShippedPRError reports that a tracker ticket already has a pull request
// carrying its "[<tracker>]" title tag in an open or merged state, so creating a
// fresh session would duplicate work already shipped or in flight elsewhere.
// Unlike the BOS-236 duplicate-active-session guard, this is derived from a
// GitHub scan and catches the sibling-branch case where no live session row
// exists. It is bypassable with force, mirroring BOS-236.
type AlreadyShippedPRError struct {
	TrackerID string
	PRNumber  int
	State     string
}

func (e *AlreadyShippedPRError) Error() string {
	return fmt.Sprintf("ticket %s already shipped via PR #%d (%s); pass force to create another", e.TrackerID, e.PRNumber, e.State)
}

// IsAlreadyShippedError reports whether err is an AlreadyShippedPRError.
func IsAlreadyShippedError(err error) bool {
	var shipped *AlreadyShippedPRError
	return errors.As(err, &shipped)
}

// firstShippedTaggedPR returns the first PR whose title carries the exact
// bracketed tracker tag "[<trackerID>]" and whose state is open or merged
// (the states that indicate the ticket is shipped or in flight). Closed-but-
// unmerged PRs are skipped: an abandoned PR is a legitimate re-attempt target.
// The exact bracket match rejects near-misses such as "BOS-2890" for "BOS-289".
func firstShippedTaggedPR(prs []vcs.PRSummary, trackerID string) (vcs.PRSummary, bool) {
	tag := "[" + trackerID + "]"
	for _, pr := range prs {
		if !strings.Contains(pr.Title, tag) {
			continue
		}
		if pr.State == vcs.PRStateOpen || pr.State == vcs.PRStateMerged {
			return pr, true
		}
	}
	return vcs.PRSummary{}, false
}

// AlreadyShippedTaggedPR scans prs for a PR that blocks a new session for
// trackerID (open or merged, exact "[<trackerID>]" title tag) and, when found,
// returns a typed AlreadyShippedPRError ready to map to CodeAlreadyExists. The
// boolean reports whether such a PR was found.
func AlreadyShippedTaggedPR(prs []vcs.PRSummary, trackerID string) (*AlreadyShippedPRError, bool) {
	pr, ok := firstShippedTaggedPR(prs, trackerID)
	if !ok {
		return nil, false
	}
	return &AlreadyShippedPRError{
		TrackerID: trackerID,
		PRNumber:  pr.Number,
		State:     prStateLabel(pr.State),
	}, true
}

// prStateLabel renders a vcs.PRState as a lowercase human label for the error
// message ("open" / "merged" / "closed").
func prStateLabel(state vcs.PRState) string {
	switch state {
	case vcs.PRStateOpen:
		return "open"
	case vcs.PRStateMerged:
		return "merged"
	case vcs.PRStateClosed:
		return "closed"
	default:
		return "unknown"
	}
}
