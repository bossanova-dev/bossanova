package vcs

import (
	"fmt"
	"strings"
)

// ConstructPRURL constructs a GitHub PR URL from an origin URL and PR number.
// Returns empty string if the origin URL cannot be parsed.
func ConstructPRURL(originURL string, prNumber int) string {
	host, slug := parseOriginURL(originURL)
	if host == "" || slug == "" {
		return ""
	}
	return fmt.Sprintf("https://%s/%s/pull/%d", host, slug, prNumber)
}

// RepoSlug extracts the "owner/repo" slug from a git origin URL.
// Returns "" if the URL cannot be parsed.
//
// Supports https://, http://, ssh://, git:// protocols and SSH shorthand
// (git@host:owner/repo.git). Strips a trailing ".git" suffix.
func RepoSlug(originURL string) string {
	_, slug := parseOriginURL(originURL)
	return slug
}

// RepoWebLink converts a git origin URL into a provider web URL.
// The provider string lets callers keep provider-specific labels outside
// parsing code. v1 intentionally exposes only GitHub; GitLab can be added
// here without changing each UI surface.
func RepoWebLink(originURL string) (provider, webURL string, ok bool) {
	host, slug := parseOriginURL(originURL)
	if host == "" || slug == "" {
		return "", "", false
	}
	if strings.EqualFold(host, "github.com") {
		return "github", fmt.Sprintf("https://github.com/%s", slug), true
	}
	return "", "", false
}

// parseOriginURL splits an origin URL into (host, "owner/repo").
// Returns ("", "") if the URL cannot be parsed.
func parseOriginURL(originURL string) (host, slug string) {
	s := originURL
	// Handle SSH shorthand: git@github.com:owner/repo.git → github.com/owner/repo.git.
	// Detect by ":" not followed by "/" (excludes "https://").
	if idx := strings.Index(s, ":"); idx > 0 && !strings.Contains(s[:idx], "/") && (idx+1 >= len(s) || s[idx+1] != '/') {
		h := s[:idx]
		// Strip user@ prefix (e.g. "git@github.com" → "github.com").
		if at := strings.Index(h, "@"); at >= 0 {
			h = h[at+1:]
		}
		s = h + "/" + s[idx+1:]
	}
	// Strip protocol prefix.
	for _, prefix := range []string{"https://", "http://", "ssh://", "git://"} {
		s = strings.TrimPrefix(s, prefix)
	}
	// Strip user@ prefix from full URLs (e.g. "ssh://git@github.com/..." → "github.com/...").
	if at := strings.Index(s, "@"); at >= 0 && at < strings.Index(s+"/", "/") {
		s = s[at+1:]
	}
	// Strip .git suffix.
	s = strings.TrimSuffix(s, ".git")
	// Strip trailing slash.
	s = strings.TrimSuffix(s, "/")
	parts := strings.SplitN(s, "/", 4)
	if len(parts) < 3 || parts[1] == "" || parts[2] == "" {
		return "", ""
	}
	return parts[0], parts[1] + "/" + parts[2]
}
