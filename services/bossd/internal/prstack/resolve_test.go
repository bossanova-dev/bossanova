package prstack

import (
	"reflect"
	"testing"
)

func TestResolve(t *testing.T) {
	pr := func(n int, head, base string) PR {
		return PR{Number: n, Title: head + " title", URL: "https://example.test/pull/" + head, HeadBranch: head, BaseBranch: base}
	}
	parent := func(p PR, shared, commits int, isBase bool) Parent {
		return Parent{Number: p.Number, Title: p.Title, URL: p.URL, HeadBranch: p.HeadBranch, SharedCommits: shared, Commits: commits, IsBaseBranch: isBase}
	}
	a, b, c, d := pr(10, "feat-a", "main"), pr(11, "feat-b", "main"), pr(12, "feat-c", "main"), pr(9, "feat-d", "main")
	onA := pr(12, "feat-c", "feat-a")
	onD := pr(12, "feat-c", "feat-d")
	release := pr(5, "main", "production")
	for _, tc := range []struct {
		name     string
		prs      []PR
		patchIDs map[int][]string
		want     map[int][]Parent
	}{
		{
			name:     "independent PRs are not stacked",
			prs:      []PR{a, c},
			patchIDs: map[int][]string{10: {"p1"}, 12: {"p2"}},
			want:     map[int][]Parent{},
		},
		{
			name:     "a parent's commits count as shared and the PR's own do not",
			prs:      []PR{a, c},
			patchIDs: map[int][]string{10: {"p1", "p2"}, 12: {"p1", "p2", "p3"}},
			want:     map[int][]Parent{12: {parent(a, 2, 2, false)}},
		},
		{
			name:     "each commit belongs to its oldest carrier, nearest parent first",
			prs:      []PR{c, a, b},
			patchIDs: map[int][]string{10: {"p1"}, 11: {"p1", "p2"}, 12: {"p1", "p2", "p3"}},
			want: map[int][]Parent{
				11: {parent(a, 1, 1, false)},
				12: {parent(b, 1, 2, false), parent(a, 1, 1, false)},
			},
		},
		{
			name:     "the base-branch parent comes first, ahead of a newer content parent",
			prs:      []PR{d, b, onD},
			patchIDs: map[int][]string{9: {"p1"}, 11: {"p2"}, 12: {"p1", "p2", "p3"}},
			want:     map[int][]Parent{12: {parent(d, 1, 1, true), parent(b, 1, 1, false)}},
		},
		{
			name:     "the base-branch parent counts even with no shared or known commits",
			prs:      []PR{a, onA},
			patchIDs: map[int][]string{12: {"p3"}},
			want:     map[int][]Parent{12: {parent(a, 0, 0, true)}},
		},
		{
			name:     "a PR whose patch ids are unknown carries no commits",
			prs:      []PR{a, c},
			patchIDs: map[int][]string{12: {"p1"}},
			want:     map[int][]Parent{},
		},
		{
			name:     "a base that is no open PR's head is not a stack",
			prs:      []PR{a, pr(12, "feat-c", "production")},
			patchIDs: map[int][]string{10: {"p1"}, 12: {"p2"}},
			want:     map[int][]Parent{},
		},
		{
			name:     "an open PR from the default branch never parents a PR into it",
			prs:      []PR{release, c},
			patchIDs: map[int][]string{5: {}, 12: {"p2"}},
			want:     map[int][]Parent{},
		},
		{
			name:     "a child retargeted to the default branch after its parent merged",
			prs:      []PR{c},
			patchIDs: map[int][]string{12: {"p3"}},
			want:     map[int][]Parent{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Resolve("main", tc.prs, tc.patchIDs); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Resolve = %+v\nwant      %+v", got, tc.want)
			}
		})
	}
}
