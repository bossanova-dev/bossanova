package vcs

import (
	"fmt"
	"strings"
)

type webLinkProvider struct {
	name           string
	matchesHost    func(string) bool
	repoURL        func(host, slug string) string
	pullRequestURL func(host, slug string, prNumber int) string
}

var webLinkProviders = []webLinkProvider{
	{
		name: "github",
		matchesHost: func(host string) bool {
			return strings.EqualFold(host, "github.com")
		},
		repoURL: func(_ string, slug string) string {
			return fmt.Sprintf("https://github.com/%s", slug)
		},
		pullRequestURL: func(_ string, slug string, prNumber int) string {
			if prNumber <= 0 {
				return ""
			}
			return fmt.Sprintf("https://github.com/%s/pull/%d", slug, prNumber)
		},
	},
}

func webLinkProviderForHost(host string) *webLinkProvider {
	for i := range webLinkProviders {
		provider := &webLinkProviders[i]
		if provider.matchesHost(host) {
			return provider
		}
	}
	return nil
}

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

// NormalizeRepoURL converts any supported git origin URL form into the
// canonical https://<host>/<owner>/<repo> form. Returns "" if the input
// cannot be parsed (empty, malformed, no owner/repo path).
//
// Unlike RepoWebLink, this function is host-agnostic: it relies on
// mechanical SSH-shorthand → HTTPS conversion (the same convention
// GitHub, GitLab, Gitea, Bitbucket, and self-hosted instances all
// follow) rather than a registry of known providers. That makes it
// safe to use as the single canonical identifier shared between bossd's
// repo snapshot and bosso's webhook dispatcher: a new git host works
// without registering it anywhere, as long as both ends route through
// this helper.
func NormalizeRepoURL(originURL string) string {
	host, slug := parseCanonicalOriginURL(originURL)
	if host == "" || slug == "" {
		return ""
	}
	return fmt.Sprintf("https://%s/%s", host, slug)
}

// RepoWebLink converts a git origin URL into a provider web URL.
// The provider string lets callers keep provider-specific labels outside
// parsing code. v1 intentionally exposes only GitHub; GitLab can be added
// here without changing each UI surface.
//
// For repo-identity normalization without per-provider gating, prefer
// NormalizeRepoURL — it produces the same https URL for unknown hosts too.
func RepoWebLink(originURL string) (provider, webURL string, ok bool) {
	host, slug := parseOriginURL(originURL)
	if host == "" || slug == "" {
		return "", "", false
	}
	providerSpec := webLinkProviderForHost(host)
	if providerSpec == nil {
		return "", "", false
	}
	webURL = providerSpec.repoURL(host, slug)
	if webURL == "" {
		return "", "", false
	}
	return providerSpec.name, webURL, true
}

// PullRequestWebLink converts a git origin URL and PR number into a provider
// pull request web URL. Add providers here once the UI supports their labels.
func PullRequestWebLink(originURL string, prNumber int) (provider, webURL string, ok bool) {
	host, slug := parseOriginURL(originURL)
	if host == "" || slug == "" || prNumber <= 0 {
		return "", "", false
	}
	providerSpec := webLinkProviderForHost(host)
	if providerSpec == nil {
		return "", "", false
	}
	webURL = providerSpec.pullRequestURL(host, slug, prNumber)
	if webURL == "" {
		return "", "", false
	}
	return providerSpec.name, webURL, true
}

// parseOriginURL splits an origin URL into (host, "owner/repo").
// Returns ("", "") if the URL cannot be parsed.
func parseOriginURL(originURL string) (host, slug string) {
	host, parts := parseOriginURLParts(originURL)
	if host == "" || len(parts) < 2 {
		return "", ""
	}
	return host, parts[0] + "/" + parts[1]
}

func parseCanonicalOriginURL(originURL string) (host, slug string) {
	host, parts := parseOriginURLParts(originURL)
	if host == "" || len(parts) < 2 {
		return "", ""
	}
	return host, strings.Join(parts, "/")
}

func parseOriginURLParts(originURL string) (host string, parts []string) {
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
	parts = strings.Split(s, "/")
	if len(parts) < 3 || parts[1] == "" || parts[2] == "" {
		return "", nil
	}
	return parts[0], parts[1:]
}

// MaxCanonicalRepoURLPasses bounds CanonicalRepoURL's iteration. It is exported
// so a caller that reports its own refusal message can name the same bound.
const MaxCanonicalRepoURLPasses = 8

// CanonicalRepoURL iterates NormalizeRepoURL to its fixed point and returns
// that canonical form, or "" when the input has no one.
//
// One pass of NormalizeRepoURL is not a canonical form. It strips ".git"
// before the trailing slash, so it is not idempotent on inputs it accepts:
// ".../alpha.git/" settles on ".../alpha.git", which normalizes again to
// ".../alpha". A caller that stops after one pass produces a value this
// function does not reproduce, so two spellings of one repository become two
// distinct keys -- which is how a globally unique index stops separating two
// organizations from one repository.
//
// The iteration is bounded rather than trusted to converge. Today it does:
// after the first pass the value is already in https://host/slug form, so
// every later pass only removes a ".git" or one trailing slash and the string
// strictly shrinks. The bound is what keeps that a property of this loop
// rather than a property borrowed from parsing code that nothing stops from
// changing. An input that has not settled within it, and one whose own
// normalization collapses to unparseable (".../.git" does), reports "": both
// would otherwise be used in a spelling this function does not reproduce. Real
// spellings settle in at most two extra passes.
//
// This is the single definition of that fixed point. Two callers derived it
// independently once and disagreed -- bosso wrote the fixed point into
// repo_organization_mappings while bossd published a single pass on the same
// origin -- so any join between those two string spaces has to re-canonicalize
// through here rather than assume the agreement already exists.
func CanonicalRepoURL(originURL string) string {
	canonical := NormalizeRepoURL(strings.TrimSpace(originURL))
	if canonical == "" {
		return ""
	}
	for pass := 0; ; pass++ {
		next := NormalizeRepoURL(canonical)
		if next == canonical {
			return canonical
		}
		if next == "" || pass >= MaxCanonicalRepoURLPasses {
			return ""
		}
		canonical = next
	}
}
