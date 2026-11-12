// Package prstack finds the open pull requests each open pull request is
// stacked on, from the repository's open-PR listing and each PR's commits over
// the default branch.
package prstack

import (
	"cmp"
	"slices"
)

// PR is one open pull request as Resolve sees it.
type PR struct {
	Number     int
	Title      string
	URL        string
	HeadBranch string
	BaseBranch string
}

// Parent is an open pull request another one is stacked on.
type Parent struct {
	Number     int
	Title      string
	URL        string
	HeadBranch string
	// SharedCommits counts the child's commits whose oldest open carrier is
	// this parent.
	SharedCommits int
	// Commits counts the parent's own commits over the default branch.
	Commits int
	// IsBaseBranch is set when the parent's head branch is the child's base.
	IsBaseBranch bool
}

// Resolve returns each stacked PR's parents, nearest first: the PR whose head
// branch is its base branch, then the owners of its other commits by
// descending number. patchIDs holds each PR's patch ids over the default
// branch; a PR missing from it carries no commits. Each patch id belongs to
// the lowest-numbered PR carrying it, so a rebased copy of a parent's commits
// still points at the parent. The default branch never makes a parent by
// name, since it is what every range is measured from.
func Resolve(defaultBranch string, prs []PR, patchIDs map[int][]string) map[int][]Parent {
	owner := make(map[string]int)
	sets := make(map[int]map[string]struct{}, len(patchIDs))
	byNumber := make(map[int]PR, len(prs))
	byHead := make(map[string]PR, len(prs))
	for _, pr := range prs {
		byNumber[pr.Number] = pr
		if head, taken := byHead[pr.HeadBranch]; pr.HeadBranch != "" && pr.HeadBranch != defaultBranch && (!taken || pr.Number < head.Number) {
			byHead[pr.HeadBranch] = pr
		}
		ids, ok := patchIDs[pr.Number]
		if !ok {
			continue
		}
		set := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			set[id] = struct{}{}
			if o, seen := owner[id]; !seen || pr.Number < o {
				owner[id] = pr.Number
			}
		}
		sets[pr.Number] = set
	}
	stacked := make(map[int][]Parent)
	for _, pr := range prs {
		shared := make(map[int]int)
		for id := range sets[pr.Number] {
			if o := owner[id]; o != pr.Number {
				shared[o]++
			}
		}
		base, hasBase := byHead[pr.BaseBranch]
		hasBase = hasBase && base.Number != pr.Number
		parents := make([]Parent, 0, len(shared)+1)
		for n, count := range shared {
			if !hasBase || n != base.Number {
				parents = append(parents, newParent(byNumber[n], count, len(sets[n]), false))
			}
		}
		slices.SortFunc(parents, func(a, b Parent) int { return cmp.Compare(b.Number, a.Number) })
		if hasBase {
			parents = slices.Insert(parents, 0, newParent(base, shared[base.Number], len(sets[base.Number]), true))
		}
		if len(parents) > 0 {
			stacked[pr.Number] = parents
		}
	}
	return stacked
}

func newParent(pr PR, shared, commits int, isBase bool) Parent {
	return Parent{Number: pr.Number, Title: pr.Title, URL: pr.URL, HeadBranch: pr.HeadBranch, SharedCommits: shared, Commits: commits, IsBaseBranch: isBase}
}
