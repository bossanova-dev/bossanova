package vcs

import (
	"slices"
	"strings"
	"testing"
)

func TestGitHubPRQueryArgs(t *testing.T) {
	for _, repo := range []string{"https://github.com/o/r.git", "git@github.com:o/r.git", "o/r", "https://git.example.com/o/r.git"} {
		t.Run(repo, func(t *testing.T) {
			for _, build := range []func(string, int, string) ([]string, bool){GitHubPRStatusArgs, GitHubPRChecksArgs} {
				args, ok := build(repo, 7, "next")
				if !ok || !slices.Contains(args, "owner=o") || !slices.Contains(args, "name=r") || !strings.Contains(strings.Join(args, " "), "-F number=7") {
					t.Fatalf("invalid query args: %v, %v", args, ok)
				}
				if !slices.Equal(args[len(args)-2:], []string{"-f", "cursor=next"}) {
					t.Fatalf("cursor not last: %v", args)
				}
				host := "github.com"
				if strings.Contains(repo, "git.example.com") {
					host = "git.example.com"
				}
				if repo != "o/r" && !strings.Contains(strings.Join(args, " "), "--hostname "+host) {
					t.Fatalf("host missing: %v", args)
				}
			}
			args, ok := GitHubPRMergeCommitArgs(repo, 7)
			if !ok || !slices.Contains(args, "number=7") || strings.Contains(strings.Join(args, " "), "cursor=") {
				t.Fatalf("merge args: %v", args)
			}
		})
	}
	for _, repo := range []string{"", "repo", "/repo", "owner/"} {
		if args, ok := GitHubPRStatusArgs(repo, 7, ""); ok {
			t.Errorf("accepted %q: %v", repo, args)
		}
	}
}
