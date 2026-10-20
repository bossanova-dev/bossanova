package main

import (
	"strings"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// matchPR attempts to match a Linear issue to an existing PR.
// It first tries to match by branch name (exact match), then falls back
// to matching by title (contains [ENG-123]). Returns (prNumber, branch)
// if a match is found, or (0, "") if no match.
func matchPR(issue linearIssue, prs []*bossanovav1.PRSummary) (prNumber int32, branch string) {
	// Primary: branch name match
	if issue.BranchName != "" {
		for _, pr := range prs {
			if pr.HeadBranch == issue.BranchName {
				return pr.Number, pr.HeadBranch
			}
		}
	}

	// Fallback: title contains [ENG-123]
	tag := "[" + issue.Identifier + "]"
	for _, pr := range prs {
		if strings.Contains(pr.Title, tag) {
			return pr.Number, pr.HeadBranch
		}
	}

	return 0, ""
}
