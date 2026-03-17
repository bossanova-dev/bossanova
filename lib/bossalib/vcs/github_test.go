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
