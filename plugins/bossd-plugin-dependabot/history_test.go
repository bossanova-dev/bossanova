package main

import (
	"testing"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestParseDependabotLibrary(t *testing.T) {
	tests := []struct {
		name   string
		branch string
		title  string
		want   string
	}{
		{
			name:   "npm library from branch",
			branch: "dependabot/npm_and_yarn/lodash-4.17.21",
			title:  "Bump lodash from 4.17.20 to 4.17.21",
			want:   "lodash",
		},
		{
			name:   "go module from branch",
			branch: "dependabot/go_modules/golang.org/x/net-0.38.0",
			title:  "Bump golang.org/x/net from 0.37.0 to 0.38.0",
			want:   "golang.org/x/net",
		},
		{
			name:   "scoped npm package from branch",
			branch: "dependabot/npm_and_yarn/@types/node-20.0.0",
			title:  "Bump @types/node from 19.0.0 to 20.0.0",
			want:   "@types/node",
		},
		{
			name:   "fallback to title when branch doesn't match",
			branch: "some-other-branch",
			title:  "Bump lodash from 4.17.20 to 4.17.21",
			want:   "lodash",
		},
		{
			name:   "update requirement title format",
			branch: "not-dependabot",
			title:  "Update lodash requirement from ~4.17.20 to ~4.17.21",
			want:   "lodash",
		},
		{
			name:   "no version in branch",
			branch: "dependabot/npm_and_yarn/lodash",
			title:  "",
			want:   "lodash",
		},
		{
			name:   "empty inputs",
			branch: "",
			title:  "",
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pr := &bossanovav1.PRSummary{
				HeadBranch: tt.branch,
				Title:      tt.title,
			}
			got := parseDependabotLibrary(pr)
			if got != tt.want {
				t.Errorf("parseDependabotLibrary() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsPreviouslyRejected(t *testing.T) {
	tests := []struct {
		name      string
		pr        *bossanovav1.PRSummary
		closedPRs []*bossanovav1.PRSummary
		want      bool
	}{
		{
			name: "same library closed → rejected",
			pr: &bossanovav1.PRSummary{
				HeadBranch: "dependabot/npm_and_yarn/lodash-4.17.22",
			},
			closedPRs: []*bossanovav1.PRSummary{
				{
					HeadBranch: "dependabot/npm_and_yarn/lodash-4.17.21",
					State:      bossanovav1.PRState_PR_STATE_CLOSED,
				},
			},
			want: true,
		},
		{
			name: "different library closed → not rejected",
			pr: &bossanovav1.PRSummary{
				HeadBranch: "dependabot/npm_and_yarn/lodash-4.17.22",
			},
			closedPRs: []*bossanovav1.PRSummary{
				{
					HeadBranch: "dependabot/npm_and_yarn/express-5.0.0",
					State:      bossanovav1.PRState_PR_STATE_CLOSED,
				},
			},
			want: false,
		},
		{
			name: "same library merged → not rejected",
			pr: &bossanovav1.PRSummary{
				HeadBranch: "dependabot/npm_and_yarn/lodash-4.17.22",
			},
			closedPRs: []*bossanovav1.PRSummary{
				{
					HeadBranch: "dependabot/npm_and_yarn/lodash-4.17.21",
					State:      bossanovav1.PRState_PR_STATE_MERGED,
				},
			},
			want: false,
		},
		{
			name:      "no closed PRs → not rejected",
			pr:        &bossanovav1.PRSummary{HeadBranch: "dependabot/npm_and_yarn/lodash-4.17.22"},
			closedPRs: nil,
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isPreviouslyRejected(tt.pr, tt.closedPRs)
			if got != tt.want {
				t.Errorf("isPreviouslyRejected() = %v, want %v", got, tt.want)
			}
		})
	}
}
