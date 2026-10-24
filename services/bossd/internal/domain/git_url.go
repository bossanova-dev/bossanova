package domain

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
)

// cloneTransportPrefix matches git's `<transport>::<address>` remote-helper
// syntax (e.g. `ext::`, `fd::`), which can run arbitrary commands.
var cloneTransportPrefix = regexp.MustCompile(`^[A-Za-z0-9+.\-]+::`)

// cloneURLScheme matches an RFC 3986 scheme followed by "://".
var cloneURLScheme = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9+.\-]*)://`)

// cloneSCPLike matches git's scp-style `[user@]host:path` form: no '/' before
// the first ':', a non-empty host that does not start with '-', and a path.
var cloneSCPLike = regexp.MustCompile(`^(?:[^@/:\s]+@)?[^@/:\s\-][^@/:\s]*:[^\s].*$`)

// ValidateCloneURL rejects clone URLs git could misread as an option or a
// command-running transport. It accepts only http(s)://, ssh://, git:// and
// file:// URLs, scp-style `[user@]host:path`, and absolute local paths, and
// rejects anything starting with '-', any `<transport>::` form, and any URL
// containing control characters. The returned error wraps ErrUnsafeCloneURL
// and never echoes the URL (it may carry credentials).
func ValidateCloneURL(cloneURL string) error {
	reject := func(reason string) error {
		return fmt.Errorf("%w: %s", ErrUnsafeCloneURL, reason)
	}
	switch {
	case cloneURL == "":
		return reject("clone URL is empty")
	case strings.HasPrefix(cloneURL, "-"):
		return reject("clone URL must not start with '-'")
	case strings.IndexFunc(cloneURL, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0:
		return reject("clone URL contains control characters")
	case cloneTransportPrefix.MatchString(cloneURL):
		return reject("remote-helper transports (<transport>::address) are not allowed")
	}

	if m := cloneURLScheme.FindStringSubmatch(cloneURL); m != nil {
		scheme := strings.ToLower(m[1])
		u, err := url.Parse(cloneURL)
		if err != nil {
			return reject("clone URL does not parse")
		}
		switch scheme {
		case "http", "https", "ssh", "git":
			if u.Host == "" {
				return reject("clone URL has no host")
			}
			if strings.HasPrefix(u.Hostname(), "-") {
				return reject("clone URL host must not start with '-'")
			}
			// ssh is handed the user as well as the host; git blocks a
			// dash-led one today, but do not rely on that.
			if strings.HasPrefix(u.User.Username(), "-") {
				return reject("clone URL user must not start with '-'")
			}
			return nil
		case "file":
			if u.Host != "" || !filepath.IsAbs(u.Path) {
				return reject("file:// clone URL must name an absolute local path")
			}
			return nil
		default:
			return reject("unsupported clone URL scheme " + scheme + " (use http(s), ssh, git or file)")
		}
	}

	if filepath.IsAbs(cloneURL) {
		return nil
	}
	if cloneSCPLike.MatchString(cloneURL) {
		return nil
	}
	return reject("clone URL must be an http(s)://, ssh://, git:// or file:// URL, scp-style user@host:path, or an absolute local path")
}

// CloneURLWithoutCredentials returns raw as Clone records it for origin:
// http(s) password userinfo and token-shaped usernames are removed (BOS-1657).
// Plain usernames and other transports are unchanged. The token heuristic errs
// toward storing ambiguous long usernames safely; short unprefixed tokens may
// still pass through.
func CloneURLWithoutCredentials(raw string) string {
	plainURL, _, _ := SplitCloneCredentials(raw)
	return plainURL
}

// SplitCloneCredentials separates http(s) password userinfo or token-shaped
// usernames into a clean URL and a credential-store line carrying path, query
// and fragment. Bare tokens use an empty password (BOS-1657). Plain usernames
// keep reaching configured helpers; misclassified long usernames fail auth
// safely, while short unprefixed tokens may pass through. Other transports and
// unparseable URLs are left alone.
func SplitCloneCredentials(raw string) (plainURL, credential string, ok bool) {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil || u.Host == "" {
		return raw, "", false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return raw, "", false
	}
	user := u.User
	if _, hasPassword := user.Password(); !hasPassword {
		if !IsTokenShapedUsername(user.Username()) {
			return raw, "", false
		}
		user = url.UserPassword(user.Username(), "")
	}
	// The stored line carries everything after the host: git's
	// credential-store only matches an entry that has every field the lookup
	// carries, and with credential.useHttpPath=true the lookup's path is all
	// of the URL after the host — path, query string and fragment (git 2.53
	// passes path=o/r.git?x=1#frag to the helper on a real clone). A lookup
	// without a path (the default) still matches an entry that has one.
	cred := url.URL{
		Scheme: strings.ToLower(u.Scheme), User: user, Host: u.Host,
		Path: u.Path, RawPath: u.RawPath, RawQuery: u.RawQuery,
		Fragment: u.Fragment, RawFragment: u.RawFragment,
	}
	stripped := *u
	stripped.User = nil
	return stripped.String(), cred.String(), true
}

// IsTokenShapedUsername recognizes decoded usernames that should be kept in
// the credential store rather than exposed in argv or origin URLs.
func IsTokenShapedUsername(name string) bool {
	for _, prefix := range []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_", "glpat-", "gldt-"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	if len(name) < 40 {
		return false
	}
	for _, c := range name {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// urlUserinfoRE matches the userinfo of an absolute URL (scheme://user[:pass]@)
// anywhere in a string. The userinfo may not contain '/', '?', '#',
// whitespace, '"', '<' or '>', so a path segment containing '@' is never
// mistaken for it, and scp-style "git@host:o/r" (no scheme) never matches.
// The class admits the apostrophe (an RFC 3986 sub-delim, legal in a
// password) and '@', so greedy matching consumes up to the LAST '@' before
// the host — a raw '@' or apostrophe inside a password is redacted along with
// the rest.
var urlUserinfoRE = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.\-]*://)[^/?#\s"<>]+@`)

// credentialQueryRE matches a query parameter whose name says it carries a
// credential. Names are matched case-insensitively and exactly, so "keyring="
// or "monkey=" are left alone. The value runs to the next '&', '#', ';',
// whitespace, '"', '<' or '>'; it admits the apostrophe so a quote inside a
// token cannot cut the redaction short (a closing quote right after the value
// is redacted with it — over-redaction, never a leak).
var credentialQueryRE = regexp.MustCompile(`(?i)([?&;])(token|access_token|password|key|sig)=[^&#;\s"<>]*`)

// RedactURLCredentials returns s with every credential an embedded URL can
// carry replaced by "***": the userinfo of any absolute URL
// (https://user:token@host → https://***@host) and the values of query
// parameters named token, access_token, password, key or sig. It operates on
// arbitrary text — a joined git argv, git's stderr, a log field — so a URL
// anywhere inside it is covered. scp-style remotes (git@github.com:o/r.git)
// and plain text pass through unchanged.
//
// It is for what bossd reports, never for what it runs: the URL handed to git
// itself is unchanged (BOS-1563).
func RedactURLCredentials(s string) string {
	if !strings.Contains(s, "://") && !strings.ContainsAny(s, "?&;") {
		return s
	}
	s = urlUserinfoRE.ReplaceAllString(s, "${1}***@")
	return credentialQueryRE.ReplaceAllString(s, "${1}${2}=***")
}
