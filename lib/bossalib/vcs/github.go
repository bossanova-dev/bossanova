package vcs

import "strings"

// IsGitHubURL returns true if the given origin URL points to github.com.
// Supports both HTTPS and SSH formats:
//   - https://github.com/owner/repo.git
//   - git@github.com:owner/repo.git
func IsGitHubURL(originURL string) bool {
	s := strings.ToLower(strings.TrimSpace(originURL))
	if s == "" {
		return false
	}
	return strings.Contains(s, "github.com")
}
