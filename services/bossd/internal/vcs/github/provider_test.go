package github

import (
	"fmt"
	"testing"

	"github.com/recurser/bossalib/vcs"
)

func TestParsePRNumberFromURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		want    int
		wantErr bool
	}{
		{
			name: "standard PR URL",
			url:  "https://github.com/owner/repo/pull/42",
			want: 42,
		},
		{
			name: "trailing slash",
			url:  "https://github.com/owner/repo/pull/7/",
			want: 7,
		},
		{
			name:    "not a PR URL",
			url:     "https://github.com/owner/repo/issues/5",
			wantErr: true,
		},
		{
			name:    "empty string",
			url:     "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePRNumberFromURL(tt.url)
			if (err != nil) != tt.wantErr {
				t.Errorf("parsePRNumberFromURL(%q) error = %v, wantErr %v", tt.url, err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("parsePRNumberFromURL(%q) = %d, want %d", tt.url, got, tt.want)
			}
		})
	}
}

func TestParsePRState(t *testing.T) {
	tests := []struct {
		input string
		want  vcs.PRState
	}{
		{"OPEN", vcs.PRStateOpen},
		{"open", vcs.PRStateOpen},
		{"CLOSED", vcs.PRStateClosed},
		{"closed", vcs.PRStateClosed},
		{"MERGED", vcs.PRStateMerged},
		{"merged", vcs.PRStateMerged},
		{"unknown", vcs.PRStateOpen},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := parsePRState(tt.input); got != tt.want {
				t.Errorf("parsePRState(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseCheckState(t *testing.T) {
	tests := []struct {
		input          string
		wantStatus     vcs.CheckStatus
		wantConclusion *vcs.CheckConclusion
	}{
		// Terminal states — completed with conclusion.
		{"SUCCESS", vcs.CheckStatusCompleted, ptr(vcs.CheckConclusionSuccess)},
		{"FAILURE", vcs.CheckStatusCompleted, ptr(vcs.CheckConclusionFailure)},
		{"STARTUP_FAILURE", vcs.CheckStatusCompleted, ptr(vcs.CheckConclusionFailure)},
		{"STALE", vcs.CheckStatusCompleted, ptr(vcs.CheckConclusionFailure)},
		{"NEUTRAL", vcs.CheckStatusCompleted, ptr(vcs.CheckConclusionNeutral)},
		{"CANCELLED", vcs.CheckStatusCompleted, ptr(vcs.CheckConclusionCancelled)},
		{"SKIPPED", vcs.CheckStatusCompleted, ptr(vcs.CheckConclusionSkipped)},
		{"TIMED_OUT", vcs.CheckStatusCompleted, ptr(vcs.CheckConclusionTimedOut)},
		// In-progress states — no conclusion.
		{"IN_PROGRESS", vcs.CheckStatusInProgress, nil},
		{"QUEUED", vcs.CheckStatusQueued, nil},
		{"PENDING", vcs.CheckStatusQueued, nil},
		{"WAITING", vcs.CheckStatusQueued, nil},
		// Unknown defaults to queued.
		{"unknown", vcs.CheckStatusQueued, nil},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			gotStatus, gotConclusion := parseCheckState(tt.input)
			if gotStatus != tt.wantStatus {
				t.Errorf("parseCheckState(%q) status = %v, want %v", tt.input, gotStatus, tt.wantStatus)
			}
			if (gotConclusion == nil) != (tt.wantConclusion == nil) {
				t.Errorf("parseCheckState(%q) conclusion = %v, want %v", tt.input, gotConclusion, tt.wantConclusion)
				return
			}
			if gotConclusion != nil && *gotConclusion != *tt.wantConclusion {
				t.Errorf("parseCheckState(%q) conclusion = %v, want %v", tt.input, *gotConclusion, *tt.wantConclusion)
			}
		})
	}
}

func TestParseReviewState(t *testing.T) {
	tests := []struct {
		input string
		want  vcs.ReviewState
	}{
		{"APPROVED", vcs.ReviewStateApproved},
		{"CHANGES_REQUESTED", vcs.ReviewStateChangesRequested},
		{"COMMENTED", vcs.ReviewStateCommented},
		{"DISMISSED", vcs.ReviewStateDismissed},
		{"unknown", vcs.ReviewStateCommented},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := parseReviewState(tt.input); got != tt.want {
				t.Errorf("parseReviewState(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestIsRepoNotReady(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "unrelated error",
			err:  fmt.Errorf("network timeout"),
			want: false,
		},
		{
			name: "head sha blank",
			err:  fmt.Errorf("gh pr create: exit status 1: pull request create failed: GraphQL: Head sha can't be blank (createPullRequest)"),
			want: true,
		},
		{
			name: "base sha blank",
			err:  fmt.Errorf("gh pr create: exit status 1: pull request create failed: GraphQL: Base sha can't be blank (createPullRequest)"),
			want: true,
		},
		{
			name: "no commits between branches",
			err:  fmt.Errorf("gh pr create: exit status 1: pull request create failed: GraphQL: No commits between main and my-branch (createPullRequest)"),
			want: true,
		},
		{
			name: "combined GitHub error",
			err:  fmt.Errorf("gh pr create: exit status 1: pull request create failed: GraphQL: Head sha can't be blank, Base sha can't be blank, No commits between main and plan-meta-ads-automation, Head ref must be a branch (createPullRequest)"),
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRepoNotReady(tt.err); got != tt.want {
				t.Errorf("isRepoNotReady(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }
