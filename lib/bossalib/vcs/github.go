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
// Only the first two path segments are returned, so extra path or query parts
// (e.g. /tree/main) are discarded. Returns empty string if owner/repo cannot be
// determined.
//
// Examples:
//   - "git@github.com:owner/repo.git"          → "owner/repo"
//   - "https://github.com/owner/repo.git"      → "owner/repo"
//   - "https://github.com/owner/repo"          → "owner/repo"
//   - "https://github.com/owner/repo/tree/main" → "owner/repo"
func GitHubNWO(originURL string) string {
	s := strings.TrimSpace(originURL)
	if s == "" {
		return ""
	}

	// SSH format: git@github.com:owner/repo.git
	if _, after, ok := strings.Cut(s, "github.com:"); ok {
		return ownerRepo(after)
	}

	// HTTPS format: https://github.com/owner/repo.git
	if _, after, ok := strings.Cut(s, "github.com/"); ok {
		return ownerRepo(after)
	}

	return ""
}

// ownerRepo returns the "owner/repo" prefix of a GitHub path, trimming a
// trailing .git and any deeper path segments. Returns "" unless both an owner
// and repo segment are present.
func ownerRepo(path string) string {
	path = strings.TrimSuffix(path, ".git")
	parts := strings.SplitN(path, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return parts[0] + "/" + strings.TrimSuffix(parts[1], ".git")
}
