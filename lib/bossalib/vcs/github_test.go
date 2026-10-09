package vcs

import (
	"strings"
	"testing"
)

func TestIsGitHubURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want bool
	}{
		{"HTTPS", "https://github.com/owner/repo.git", true},
		{"SSH", "git@github.com:owner/repo.git", true},
		{"HTTPS no .git", "https://github.com/owner/repo", true},
		{"mixed case", "https://GitHub.COM/owner/repo.git", true},
		{"with whitespace", "  https://github.com/owner/repo.git  ", true},
		{"GitLab HTTPS", "https://gitlab.com/owner/repo.git", false},
		{"GitLab SSH", "git@gitlab.com:owner/repo.git", false},
		{"empty string", "", false},
		{"bare path", "/some/local/path", false},
		{"bitbucket", "https://bitbucket.org/owner/repo.git", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsGitHubURL(tt.url)
			if got != tt.want {
				t.Errorf("IsGitHubURL(%q) = %v, want %v", tt.url, got, tt.want)
			}
		})
	}
}

func TestGitHubNWO(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{"SSH with .git", "git@github.com:owner/repo.git", "owner/repo"},
		{"HTTPS with .git", "https://github.com/owner/repo.git", "owner/repo"},
		{"HTTPS no .git", "https://github.com/owner/repo", "owner/repo"},
		{"with whitespace", "  https://github.com/owner/repo.git  ", "owner/repo"},
		{"extra path segments", "https://github.com/owner/repo/tree/main", "owner/repo"},
		{"SSH extra segments", "git@github.com:owner/repo/extra", "owner/repo"},
		{"owner only", "https://github.com/owner", ""},
		{"non-github", "https://gitlab.com/owner/repo.git", ""},
		{"empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GitHubNWO(tt.url)
			if got != tt.want {
				t.Errorf("GitHubNWO(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}

func TestNormalizeHeadSHA(t *testing.T) {
	lower := strings.Repeat("ab12", 10)
	tests := []struct {
		name   string
		in     string
		want   string
		wantOK bool
	}{
		{"lowercase 40 hex", lower, lower, true},
		{"uppercase normalized", strings.ToUpper(lower), lower, true},
		{"surrounding whitespace trimmed", "  " + lower + "\n", lower, true},
		{"empty", "", "", false},
		{"short", "abc", "", false},
		{"39 hex", lower[:39], "", false},
		{"41 hex", lower + "a", "", false},
		{"non-hex", strings.Repeat("g", 40), "", false},
		{"inner space", lower[:20] + " " + lower[21:], "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := NormalizeHeadSHA(tt.in)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("NormalizeHeadSHA(%q) = (%q, %v), want (%q, %v)", tt.in, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestParseGitHubCheckState(t *testing.T) {
	tests := []struct {
		input          string
		wantStatus     CheckStatus
		wantConclusion CheckConclusion // 0 means nil
		wantRecognized bool
	}{
		{"success", CheckStatusCompleted, CheckConclusionSuccess, true},
		{"SUCCESS", CheckStatusCompleted, CheckConclusionSuccess, true},
		{"failure", CheckStatusCompleted, CheckConclusionFailure, true},
		{"error", CheckStatusCompleted, CheckConclusionFailure, true},
		{"action_required", CheckStatusCompleted, CheckConclusionFailure, true},
		{"stale", CheckStatusCompleted, CheckConclusionFailure, true},
		{"startup_failure", CheckStatusCompleted, CheckConclusionFailure, true},
		{"neutral", CheckStatusCompleted, CheckConclusionNeutral, true},
		{"cancelled", CheckStatusCompleted, CheckConclusionCancelled, true},
		{"skipped", CheckStatusCompleted, CheckConclusionSkipped, true},
		{"timed_out", CheckStatusCompleted, CheckConclusionTimedOut, true},
		{"in_progress", CheckStatusInProgress, 0, true},
		{"queued", CheckStatusQueued, 0, true},
		{"pending", CheckStatusQueued, 0, true},
		{"waiting", CheckStatusQueued, 0, true},
		{"requested", CheckStatusCompleted, 0, false},
		{"", CheckStatusCompleted, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			status, conclusion, recognized := ParseGitHubCheckState(tt.input)
			if status != tt.wantStatus || recognized != tt.wantRecognized {
				t.Fatalf("ParseGitHubCheckState(%q) = %v, recognized %v; want %v, %v", tt.input, status, recognized, tt.wantStatus, tt.wantRecognized)
			}
			switch {
			case tt.wantConclusion == 0 && conclusion != nil:
				t.Fatalf("conclusion = %v, want nil", *conclusion)
			case tt.wantConclusion != 0 && (conclusion == nil || *conclusion != tt.wantConclusion):
				t.Fatalf("conclusion = %v, want %v", conclusion, tt.wantConclusion)
			}
		})
	}
}
