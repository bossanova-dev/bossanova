package main

import (
	"context"
	"slices"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// The gh CLI returns app/dependabot as the author login.
const dependabotAuthor = "app/dependabot"

func listDependabotPRs(ctx context.Context, host hostClient, repoOriginURL string) ([]*bossanovav1.PRSummary, error) {
	prs, err := host.ListOpenPRs(ctx, repoOriginURL)
	if err != nil {
		return nil, err
	}
	return filterDependabotPRs(prs), nil
}

func listClosedDependabotPRs(ctx context.Context, host hostClient, repoOriginURL string) ([]*bossanovav1.PRSummary, error) {
	prs, err := host.ListClosedPRs(ctx, repoOriginURL)
	if err != nil {
		return nil, err
	}
	return filterDependabotPRs(prs), nil
}

// closedHistoryMaxAge bounds how long a repo's closed-PR history is reused
// while its open dependabot set is unchanged. It matches the daemon's
// TerminalRetryCooldown, so a rejection is noticed no later than a retry.
const closedHistoryMaxAge = 30 * time.Minute

// closedHistoryEntry is one repo's cached closed dependabot PR history, with
// the open dependabot PR numbers seen when it was fetched.
type closedHistoryEntry struct {
	prs       []*bossanovav1.PRSummary
	fetchedAt time.Time
	openSet   []int32
}

// closedHistoryFor returns the repo's closed dependabot PRs, reusing the cached
// history while it is younger than closedHistoryMaxAge and the open dependabot
// set still matches the one it was fetched against. A PR leaving the open set
// (merged or closed) refetches, so a fresh rejection is seen on the next poll.
// A failed fetch is not cached: it returns the error together with the last
// cached history (nil when there is none), so a refused refetch — such as a
// background quota-floor refusal while the open-PR snapshot keeps serving —
// still checks against the most recent rejections it knows.
func (s *server) closedHistoryFor(ctx context.Context, repoOriginURL string, open []*bossanovav1.PRSummary) ([]*bossanovav1.PRSummary, error) {
	openSet := prNumberSet(open)
	now := s.now()

	s.closedMu.Lock()
	entry, ok := s.closedHistory[repoOriginURL]
	s.closedMu.Unlock()
	if ok && now.Sub(entry.fetchedAt) < closedHistoryMaxAge && slices.Equal(entry.openSet, openSet) {
		return entry.prs, nil
	}

	prs, err := listClosedDependabotPRs(ctx, s.host, repoOriginURL)
	if err != nil {
		return entry.prs, err
	}

	s.closedMu.Lock()
	defer s.closedMu.Unlock()
	if s.closedHistory == nil {
		s.closedHistory = make(map[string]closedHistoryEntry)
	}
	s.closedHistory[repoOriginURL] = closedHistoryEntry{prs: prs, fetchedAt: now, openSet: openSet}
	return prs, nil
}

// prNumberSet returns the sorted PR numbers of prs.
func prNumberSet(prs []*bossanovav1.PRSummary) []int32 {
	set := make([]int32, 0, len(prs))
	for _, pr := range prs {
		set = append(set, pr.GetNumber())
	}
	slices.Sort(set)
	return set
}

func filterDependabotPRs(prs []*bossanovav1.PRSummary) []*bossanovav1.PRSummary {
	var filtered []*bossanovav1.PRSummary
	for _, pr := range prs {
		if pr.GetAuthor() == dependabotAuthor {
			filtered = append(filtered, pr)
		}
	}
	return filtered
}
