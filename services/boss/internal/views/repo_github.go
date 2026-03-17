package views

import (
	"path"
	"strings"
)

// parseRepoNameFromURL extracts the repository name from a git URL.
// Supports:
//   - https://github.com/owner/repo
//   - https://github.com/owner/repo.git
//   - git@github.com:owner/repo.git
//   - owner/repo (bare shorthand)
func parseRepoNameFromURL(rawURL string) string {
	s := strings.TrimSpace(rawURL)
	if s == "" {
		return ""
	}

	// SSH style: git@host:owner/repo.git
	if idx := strings.Index(s, ":"); idx > 0 && !strings.Contains(s[:idx], "/") {
		s = s[idx+1:]
	}

	// Strip .git suffix.
	s = strings.TrimSuffix(s, ".git")

	// Take just the last path component (the repo name).
	return path.Base(s)
}
