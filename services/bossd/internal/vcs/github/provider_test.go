package github

import (
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

func TestParseCheckStatus(t *testing.T) {
	tests := []struct {
		input string
		want  vcs.CheckStatus
	}{
		{"COMPLETED", vcs.CheckStatusCompleted},
		{"completed", vcs.CheckStatusCompleted},
		{"IN_PROGRESS", vcs.CheckStatusInProgress},
		{"QUEUED", vcs.CheckStatusQueued},
		{"PENDING", vcs.CheckStatusQueued},
		{"WAITING", vcs.CheckStatusQueued},
		{"unknown", vcs.CheckStatusQueued},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := parseCheckStatus(tt.input); got != tt.want {
				t.Errorf("parseCheckStatus(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseCheckConclusion(t *testing.T) {
	tests := []struct {
		input string
		want  *vcs.CheckConclusion
	}{
		{"SUCCESS", ptr(vcs.CheckConclusionSuccess)},
		{"FAILURE", ptr(vcs.CheckConclusionFailure)},
		{"NEUTRAL", ptr(vcs.CheckConclusionNeutral)},
		{"CANCELLED", ptr(vcs.CheckConclusionCancelled)},
		{"SKIPPED", ptr(vcs.CheckConclusionSkipped)},
		{"TIMED_OUT", ptr(vcs.CheckConclusionTimedOut)},
		{"", nil},
		{"unknown", nil},
	}

	for _, tt := range tests {
		name := tt.input
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			got := parseCheckConclusion(tt.input)
			if (got == nil) != (tt.want == nil) {
				t.Errorf("parseCheckConclusion(%q) = %v, want %v", tt.input, got, tt.want)
				return
			}
			if got != nil && *got != *tt.want {
				t.Errorf("parseCheckConclusion(%q) = %v, want %v", tt.input, *got, *tt.want)
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

func ptr[T any](v T) *T { return &v }
