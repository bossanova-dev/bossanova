package vcs

import (
	"fmt"
	"strings"
)

// ConstructPRURL constructs a GitHub PR URL from an origin URL and PR number.
// Returns empty string if the origin URL cannot be parsed.
func ConstructPRURL(originURL string, prNumber int) string {
	s := originURL
	// Handle SSH format: git@github.com:owner/repo.git → github.com/owner/repo.git
	// Detect SSH by finding ":" not followed by "/" (excludes "https://").
	if idx := strings.Index(s, ":"); idx > 0 && !strings.Contains(s[:idx], "/") && (idx+1 >= len(s) || s[idx+1] != '/') {
		host := s[:idx]
		// Strip user@ prefix (e.g. "git@github.com" → "github.com").
		if at := strings.Index(host, "@"); at >= 0 {
			host = host[at+1:]
		}
		s = host + "/" + s[idx+1:]
	}
	// Strip protocol prefix.
	for _, prefix := range []string{"https://", "http://", "ssh://", "git://"} {
		s = strings.TrimPrefix(s, prefix)
	}
	// Strip .git suffix.
	s = strings.TrimSuffix(s, ".git")
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 || parts[1] == "" {
		return ""
	}
	return fmt.Sprintf("https://%s/%s/pull/%d", parts[0], parts[1], prNumber)
}
