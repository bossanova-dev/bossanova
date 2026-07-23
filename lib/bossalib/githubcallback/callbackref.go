// Package githubcallback holds the surface-neutral parsing and validation logic
// shared by every client that registers GitHub PR callbacks — the boss CLI
// (`boss callback add|list|remove`) and the MCP tools
// (register/list/delete_github_callback). It has no dependency on cobra, the
// MCP SDK, or any services/* package, so both surfaces reuse one implementation
// of PR-reference parsing, trigger validation, and expiry bounds.
//
// The daemon store (services/bossd/internal/db) is the authority that ultimately
// enforces these rules; the helpers here fail fast with agent-correctable error
// text so a caller never has to round-trip to the daemon to learn its input was
// malformed. The DefaultExpiry/MaxExpiry constants mirror the store's
// GithubCallbackDefaultExpiry/GithubCallbackMaxExpiry and are covered by a test
// asserting they stay in lockstep.
package githubcallback

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/recurser/bossalib/models"
)

// DefaultExpiry is applied when a caller registers a callback without an
// explicit expiry. It is the canonical models constant, also used by the
// daemon store, so the surfaces cannot drift.
const DefaultExpiry = models.GithubCallbackDefaultExpiry

// MaxExpiry caps how far in the future a callback may be set to expire. It is
// the canonical models constant, also used by the daemon store.
const MaxExpiry = models.GithubCallbackMaxExpiry

// PRRef is a fully-resolved, normalized pull-request identity: a lowercased
// owner/repo plus a positive PR number. Producing one is the only sanctioned
// way to turn user input into the repo_owner/repo_name/pr_number a callback
// registration carries.
type PRRef struct {
	Owner    string
	Repo     string
	PRNumber int32
}

// ParsePRRef resolves a PR reference into a normalized PRRef. It accepts either
// form:
//
//   - a bare positive integer ("123") — requires repository context via
//     repoOwner+repoName (both non-empty); the number alone is ambiguous.
//   - a canonical GitHub PR URL ("https://github.com/<owner>/<repo>/pull/<n>").
//
// Owner and repo are lowercased so the stored identity is stable regardless of
// how the caller cased them. When both a URL and repo context are supplied, the
// URL's owner/repo must agree with the context (case-insensitively); a
// disagreement is rejected rather than silently preferring one.
//
// It rejects non-GitHub hosts, malformed or extra path segments, and any
// identity-changing URL fragment (userinfo, query, or fragment), so a reference
// that looks canonical but points somewhere else can never be registered.
func ParsePRRef(ref, repoOwner, repoName string) (PRRef, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return PRRef{}, fmt.Errorf("PR reference is required (a PR number like 123 or a URL like https://github.com/owner/repo/pull/123)")
	}

	// Bare positive integer: needs repo context to be unambiguous.
	if n, err := strconv.ParseInt(ref, 10, 32); err == nil {
		if n <= 0 {
			return PRRef{}, fmt.Errorf("PR number must be a positive integer, got %q", ref)
		}
		owner, repo, ok := normalizeRepo(repoOwner, repoName)
		if !ok {
			return PRRef{}, fmt.Errorf("PR number %d needs repository context: pass --repo owner/repo or run inside a repository, or use a full https://github.com/owner/repo/pull/%d URL", n, n)
		}
		return PRRef{Owner: owner, Repo: repo, PRNumber: int32(n)}, nil
	}

	// Otherwise it must be a canonical GitHub PR URL.
	u, err := url.Parse(ref)
	if err != nil {
		return PRRef{}, fmt.Errorf("invalid PR reference %q: not a number and not a parseable URL", ref)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return PRRef{}, fmt.Errorf("invalid PR URL %q: expected an https://github.com/owner/repo/pull/N URL", ref)
	}
	if !strings.EqualFold(u.Host, "github.com") {
		return PRRef{}, fmt.Errorf("invalid PR URL %q: only github.com pull-request URLs are supported, got host %q", ref, u.Host)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return PRRef{}, fmt.Errorf("invalid PR URL %q: unexpected userinfo, query, or fragment — use a plain https://github.com/owner/repo/pull/N URL", ref)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 4 || parts[2] != "pull" || parts[0] == "" || parts[1] == "" {
		return PRRef{}, fmt.Errorf("invalid PR URL %q: expected path /owner/repo/pull/N", ref)
	}
	n, err := strconv.ParseInt(parts[3], 10, 32)
	if err != nil || n <= 0 {
		return PRRef{}, fmt.Errorf("invalid PR URL %q: PR number must be a positive integer", ref)
	}
	owner := strings.ToLower(parts[0])
	repo := strings.ToLower(parts[1])

	// If the caller ALSO supplied repo context, it must not disagree with the URL.
	if ctxOwner, ctxRepo, ok := normalizeRepo(repoOwner, repoName); ok {
		if ctxOwner != owner || ctxRepo != repo {
			return PRRef{}, fmt.Errorf("repository %s/%s disagrees with PR URL repository %s/%s: pass a matching --repo or drop it", ctxOwner, ctxRepo, owner, repo)
		}
	}
	return PRRef{Owner: owner, Repo: repo, PRNumber: int32(n)}, nil
}

// normalizeRepo lowercases and validates an owner/repo pair. ok is false when
// either component is blank after trimming (i.e. no usable repo context).
func normalizeRepo(owner, repo string) (normOwner, normRepo string, ok bool) {
	owner = strings.ToLower(strings.TrimSpace(owner))
	repo = strings.ToLower(strings.TrimSpace(repo))
	if owner == "" || repo == "" {
		return "", "", false
	}
	return owner, repo, true
}

// SplitRepo parses a "owner/repo" slug (e.g. from --repo) into its components,
// lowercasing both. It rejects anything that is not exactly two non-empty
// segments so a stray "owner" or "owner/repo/extra" fails loudly.
func SplitRepo(slug string) (owner, repo string, err error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return "", "", fmt.Errorf("repository must be in owner/repo form")
	}
	parts := strings.Split(slug, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("repository %q must be in owner/repo form", slug)
	}
	return strings.ToLower(parts[0]), strings.ToLower(parts[1]), nil
}

// ValidTriggers lists the accepted PR-event triggers in a stable order, for
// help text, schema enums, and error messages.
func ValidTriggers() []models.GithubCallbackTrigger {
	return []models.GithubCallbackTrigger{
		models.GithubCallbackTriggerMerged,
		models.GithubCallbackTriggerClosed,
		models.GithubCallbackTriggerChecksPassed,
		models.GithubCallbackTriggerChecksFailed,
	}
}

// ValidTriggerStrings is ValidTriggers rendered as plain strings.
func ValidTriggerStrings() []string {
	triggers := ValidTriggers()
	out := make([]string, len(triggers))
	for i, t := range triggers {
		out[i] = string(t)
	}
	return out
}

// ValidateTrigger checks that trigger is one of the known PR-event triggers and
// returns the typed value. The error lists the accepted set so an agent can
// self-correct.
func ValidateTrigger(trigger string) (models.GithubCallbackTrigger, error) {
	t := models.GithubCallbackTrigger(strings.TrimSpace(trigger))
	for _, valid := range ValidTriggers() {
		if t == valid {
			return valid, nil
		}
	}
	return "", fmt.Errorf("invalid trigger %q: expected one of %s", trigger, strings.Join(ValidTriggerStrings(), ", "))
}

// ParseExpiresIn turns a human duration ("30m", "24h", "7d", "2w") into an
// absolute expiry timestamp measured from now. An empty string yields the
// 24-hour default. The result must be positive and within MaxExpiry (30 days);
// anything else is rejected so a caller can never register a callback that
// outlives the store's cap.
func ParseExpiresIn(expiresIn string, now time.Time) (time.Time, error) {
	d, err := ParseDuration(expiresIn)
	if err != nil {
		return time.Time{}, err
	}
	return now.Add(d), nil
}

// ParseDuration parses a callback expiry duration, honoring day ("d") and week
// ("w") suffixes on top of Go's time.ParseDuration units. An empty string is
// the 24-hour default. The duration must be positive and ≤ MaxExpiry.
func ParseDuration(expiresIn string) (time.Duration, error) {
	s := strings.TrimSpace(expiresIn)
	if s == "" {
		return DefaultExpiry, nil
	}
	d, err := parseExtendedDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid expiry %q: use a duration like 30m, 24h, 7d, or 2w", expiresIn)
	}
	if d <= 0 {
		return 0, fmt.Errorf("invalid expiry %q: must be a positive duration", expiresIn)
	}
	if d > MaxExpiry {
		return 0, fmt.Errorf("invalid expiry %q: must not exceed %s (30 days)", expiresIn, MaxExpiry)
	}
	return d, nil
}

// parseExtendedDuration extends time.ParseDuration with "d" (day) and "w"
// (week) suffixes on a single leading number. Compound forms with those units
// (e.g. "1w2d") are intentionally unsupported — callbacks take one simple span.
func parseExtendedDuration(s string) (time.Duration, error) {
	if len(s) >= 2 {
		unit := s[len(s)-1]
		if unit == 'd' || unit == 'w' {
			n, err := strconv.ParseFloat(s[:len(s)-1], 64)
			if err != nil {
				return 0, err
			}
			base := 24 * time.Hour
			if unit == 'w' {
				base = 7 * 24 * time.Hour
			}
			return time.Duration(n * float64(base)), nil
		}
	}
	return time.ParseDuration(s)
}
