package vcs

import (
	"fmt"
	"net/netip"
	"net/url"
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
//
// A GitHub origin's port is dropped, so ssh://git@github.com:22/o/r keys the
// same as the https://github.com/o/r a GitHub webhook carries. Other hosts
// keep their port.
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
	r, ok := parseRemote(originURL)
	if !ok {
		return "", nil
	}
	host = r.host
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	// A GitHub repository has one web address whatever transport port the
	// origin names, so the port is dropped there. Elsewhere it is kept, because
	// a self-hosted https port can be part of the repository's address.
	if r.port != "" && !IsGitHubHost(r.host) {
		host += ":" + r.port
	}
	s := strings.TrimSuffix(r.path, ".git")
	s = strings.TrimSuffix(s, "/")
	parts = strings.Split(s, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", nil
	}
	return host, parts
}

// remote is a git origin reduced to the parts repository identity depends on.
type remote struct {
	host string // without userinfo, port or IPv6 brackets; ssh.github.com reads as github.com
	port string // decimal, or "" when the origin names none
	path string // without a leading slash, still percent-encoded; a URL's query and fragment are dropped
}

var remoteSchemes = map[string]bool{
	"http": true, "https": true, "ssh": true, "git": true, "git+ssh": true, "ssh+git": true,
}

// gitHubSSHHost is GitHub's SSH-over-443 endpoint. It serves github.com's
// repositories, so an origin naming it keys and links as github.com.
const gitHubSSHHost = "ssh.github.com"

// parseRemote is this package's one git origin parser. It accepts a URL whose
// scheme is in remoteSchemes, git's scp-like [user@]host:path, and a
// scheme-less host/path. The URL forms go through net/url, so the authority
// ends at the first "/", "?" or "#", userinfo runs to the last "@", and a port
// must be decimal or empty. ok is false when no valid host is found.
func parseRemote(origin string) (r remote, ok bool) {
	s := strings.TrimSpace(origin)
	if hasDOSDrivePrefix(s) {
		return remote{}, false
	}
	if r, ok = splitRemote(s); !ok || !validHost(r.host) {
		return remote{}, false
	}
	if strings.EqualFold(strings.TrimSuffix(r.host, "."), gitHubSSHHost) {
		r.host = "github.com"
	}
	return r, true
}

func splitRemote(s string) (remote, bool) {
	if userHost, path, found := cutSCPLike(s); found {
		host := userHost[strings.LastIndex(userHost, "@")+1:]
		if strings.HasPrefix(host, "[") {
			var ok bool
			if host, ok = bracketedIP(host); !ok {
				return remote{}, false
			}
		}
		return remote{host: host, path: strings.TrimPrefix(path, "/")}, true
	}
	var u *url.URL
	var err error
	if strings.Contains(s, "://") {
		u, err = url.Parse(s)
		if err != nil || !remoteSchemes[u.Scheme] {
			return remote{}, false
		}
	} else if u, err = url.Parse("//" + s); err != nil {
		return remote{}, false
	}
	return remote{host: u.Hostname(), port: u.Port(), path: strings.TrimPrefix(u.EscapedPath(), "/")}, true
}

// cutSCPLike splits git's scp-like syntax, [user@]host:path, where host may be
// a bracketed IPv6 literal. Git reads an origin that way when a colon comes
// before any slash and does not start "://", so "host:22/repo" names the path
// "22/repo", not a port, and "host:/srv/repo" names the absolute path
// "/srv/repo".
func cutSCPLike(s string) (userHost, path string, ok bool) {
	i := strings.Index(s, ":")
	if lb := strings.Index(s, "["); lb >= 0 && (i < 0 || lb < i) {
		rb := strings.Index(s[lb:], "]:")
		if rb < 0 || strings.ContainsAny(s[:lb], ":/") || (lb > 0 && s[lb-1] != '@') {
			return "", "", false
		}
		i = lb + rb + 1
	}
	if i <= 0 || strings.Contains(s[:i], "/") || strings.HasPrefix(s[i:], "://") {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

// bracketedIP unwraps a "[addr]" host and reports whether addr is an IP
// address. A zone's "%" fails validHost afterwards.
func bracketedIP(host string) (string, bool) {
	inner, ok := strings.CutPrefix(host, "[")
	if !ok {
		return "", false
	}
	inner, ok = strings.CutSuffix(inner, "]")
	if !ok {
		return "", false
	}
	_, err := netip.ParseAddr(inner)
	return inner, err == nil
}

// hasDOSDrivePrefix reports a Windows path such as C:/src/repo or C:\src,
// which git reads as a local path. A letter and colon followed by anything
// else is an scp-like host with a one-letter name, as in the ssh alias
// "a:team/widgets.git".
func hasDOSDrivePrefix(s string) bool {
	if len(s) < 3 || s[1] != ':' || (s[2] != '/' && s[2] != '\\') {
		return false
	}
	return 'a' <= s[0] && s[0] <= 'z' || 'A' <= s[0] && s[0] <= 'Z'
}

func validHost(host string) bool {
	return host != "" &&
		!strings.HasPrefix(host, ".") &&
		!strings.Contains(host, "..") &&
		!strings.ContainsAny(host, `/?#\@[]% `)
}

// MaxCanonicalRepoURLPasses bounds CanonicalRepoURL's iteration. It is exported
// so a caller that reports its own refusal message can name the same bound.
const MaxCanonicalRepoURLPasses = 8

// CanonicalRepoURL iterates NormalizeRepoURL to its fixed point and returns
// that canonical form, or "" when the input has no one. GitHub origins are
// lowercased because their owner and repository names are case insensitive;
// other hosts retain their case.
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
		if parsed, err := url.Parse(next); err == nil && IsGitHubHost(parsed.Hostname()) {
			next = strings.ToLower(next)
		}
		if next == canonical {
			return canonical
		}
		if next == "" || pass >= MaxCanonicalRepoURLPasses {
			return ""
		}
		canonical = next
	}
}

// IsGitHubHost reports whether host names github.com or one of its subdomains.
// Host names are case insensitive; a decimal port and a trailing dot do not
// affect identity. A non-decimal port or an empty label is not a host.
func IsGitHubHost(host string) bool {
	if before, port, ok := strings.Cut(host, ":"); ok {
		if !isDecimal(port) {
			return false
		}
		host = before
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if strings.HasPrefix(host, ".") || strings.Contains(host, "..") {
		return false
	}
	return host == "github.com" || strings.HasSuffix(host, ".github.com")
}

func isDecimal(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
