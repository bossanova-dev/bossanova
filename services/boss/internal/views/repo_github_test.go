package views

import "testing"

func TestParseRepoNameFromURL(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		// HTTPS URLs
		{"https://github.com/owner/repo", "repo"},
		{"https://github.com/owner/repo.git", "repo"},

		// SSH URLs
		{"git@github.com:owner/repo.git", "repo"},
		{"git@gitlab.com:group/subgroup/repo.git", "repo"},

		// Bare shorthand
		{"owner/repo", "repo"},

		// Edge cases
		{"", ""},
		{"  ", ""},
		{"https://github.com/owner/my-cool-repo.git", "my-cool-repo"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseRepoNameFromURL(tt.input)
			if got != tt.want {
				t.Errorf("parseRepoNameFromURL(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
