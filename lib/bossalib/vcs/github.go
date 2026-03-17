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

// GitHubNWO extracts the "owner/repo" identifier from a GitHub origin URL.
// Returns empty string if the URL cannot be parsed.
//
// Examples:
//   - "git@github.com:owner/repo.git"      → "owner/repo"
//   - "https://github.com/owner/repo.git"  → "owner/repo"
//   - "https://github.com/owner/repo"      → "owner/repo"
func GitHubNWO(originURL string) string {
	s := strings.TrimSpace(originURL)
	if s == "" {
		return ""
	}

	// SSH format: git@github.com:owner/repo.git
	if idx := strings.Index(s, "github.com:"); idx >= 0 {
		s = s[idx+len("github.com:"):]
		s = strings.TrimSuffix(s, ".git")
		return s
	}

	// HTTPS format: https://github.com/owner/repo.git
	if idx := strings.Index(s, "github.com/"); idx >= 0 {
		s = s[idx+len("github.com/"):]
		s = strings.TrimSuffix(s, ".git")
		return s
	}

	return ""
}
