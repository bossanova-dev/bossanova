package git

import (
	"regexp"
	"strings"
)

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

// redactURLCredentials returns s with every credential an embedded URL can
// carry replaced by "***": the userinfo of any absolute URL
// (https://user:token@host → https://***@host) and the values of query
// parameters named token, access_token, password, key or sig. It operates on
// arbitrary text — a joined git argv, git's stderr, a log field — so a URL
// anywhere inside it is covered. scp-style remotes (git@github.com:o/r.git)
// and plain text pass through unchanged.
//
// It is for what bossd reports, never for what it runs: the URL handed to git
// itself is unchanged (BOS-1563).
func redactURLCredentials(s string) string {
	if !strings.Contains(s, "://") && !strings.ContainsAny(s, "?&;") {
		return s
	}
	s = urlUserinfoRE.ReplaceAllString(s, "${1}***@")
	return credentialQueryRE.ReplaceAllString(s, "${1}${2}=***")
}

// RedactURLCredentials is redactURLCredentials for callers outside package
// git that log a URL bossd did not build itself — a repo's origin as read back
// from `git remote get-url origin`, which keeps any userinfo the clone URL
// carried. For logging or error text — never pass its result to git or store it.
func RedactURLCredentials(s string) string {
	return redactURLCredentials(s)
}

// redactedGitArgv renders a git argv for an error or log line with any URL
// credentials redacted.
func redactedGitArgv(args []string) string {
	return redactURLCredentials(strings.Join(args, " "))
}
