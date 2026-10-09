package views

import (
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestRepoNameAndBranch(t *testing.T) {
	for _, tc := range []struct {
		name, origin, want, branch string
		github                     bool
	}{
		{"github", "git@github.com:owner/project.git", "@owner/project", "develop", true},
		{"other", "https://example.org/owner/project.git", "project", "", false},
		{"none", "", "checkout", "", false},
		{"invalid github", "", "checkout", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name, branch := RepoNameAndBranch("/tmp/checkout", &pb.ValidateRepoPathResponse{OriginUrl: tc.origin, IsGithub: tc.github, DefaultBranch: tc.branch})
			wantBranch := tc.branch
			if wantBranch == "" {
				wantBranch = "main"
			}
			if name != tc.want || branch != wantBranch {
				t.Fatalf("got %s/%s", name, branch)
			}
		})
	}
}
