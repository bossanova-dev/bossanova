package vcs

import "testing"

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
