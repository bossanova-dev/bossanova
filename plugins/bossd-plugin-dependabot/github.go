package main

import (
	"context"

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

func filterDependabotPRs(prs []*bossanovav1.PRSummary) []*bossanovav1.PRSummary {
	var filtered []*bossanovav1.PRSummary
	for _, pr := range prs {
		if pr.GetAuthor() == dependabotAuthor {
			filtered = append(filtered, pr)
		}
	}
	return filtered
}
