// Package agenterr implements the shared agent-error taxonomy for BOS-164: a
// classifier that turns raw provider/CLI banner text into a small, closed set
// of usage-cap error kinds, reset-time parsing for those errors, and a
// secret-redacting capture hook for banner snapshots. It is a standalone
// library package (stdlib + appstate only) ported from the lumi-bridge
// project's error-policy classifier so bossd/boss/plugins can share one
// taxonomy instead of re-deriving heuristics independently.
package agenterr

import (
	"regexp"
	"time"
)

// Kind is a closed classification of an agent-facing error banner. The zero
// value, KindNone, is the fail-safe: unrecognized text classifies as "no
// known kind" rather than guessing.
type Kind int

const (
	// KindNone indicates no known error pattern matched. This is the zero
	// value so an unset/zeroed Classification is fail-safe by construction.
	KindNone Kind = iota
	// KindUsageExhausted indicates the provider/CLI reported a usage or
	// billing cap (quota, credits, or a rate/plan limit) has been hit.
	KindUsageExhausted
	// KindRateLimited indicates the provider reported a 429 / rate-limit
	// (short-term throttling), distinct from a genuine infra transient
	// (KindTransientProvider). It is its own kind (BOS-406) so a rotation
	// consumer can fail over on it rather than treating it as a benign retry.
	// It carries no ResetAt — a rate limit's reset is resolved from the bossd
	// usage probe, exactly as a reset-less usage-exhausted match is.
	KindRateLimited
	// KindAuthInvalidated indicates stored credentials were invalidated and
	// require the user to re-authenticate.
	KindAuthInvalidated
	// KindTransientProvider indicates a transient, provider-side failure
	// (timeouts, 5xx, overload) that is expected to clear on retry rather than
	// requiring user action. Rate limiting is deliberately NOT here — a 429 is
	// KindRateLimited (BOS-406) so a rotation consumer can act on it.
	KindTransientProvider
)

// String returns a lower_snake_case name for k, or "unknown" for values
// outside the defined range.
func (k Kind) String() string {
	switch k {
	case KindNone:
		return "none"
	case KindUsageExhausted:
		return "usage_exhausted"
	case KindRateLimited:
		return "rate_limited"
	case KindAuthInvalidated:
		return "auth_invalidated"
	case KindTransientProvider:
		return "transient_provider"
	default:
		return "unknown"
	}
}

// Classification is the result of classifying a banner/error text.
type Classification struct {
	// Kind is the closed classification of the text.
	Kind Kind
	// ResetAt is non-nil only when Kind is KindUsageExhausted and a reset
	// time could be unambiguously parsed from the text. It is nil in every
	// other case, including a usage-exhausted match with no parseable reset.
	ResetAt *time.Time
}

// Regexes ported (verbatim intent) from lumi-bridge's error-policy.ts:50-135.
// context_exceeded, missing_session, and provider_stalled are intentionally
// not ported (out of scope for BOS-164 Task 1).
var (
	reAuthInvalidatedToken = regexp.MustCompile(`(?i)authentication token has been invalidated`)
	reAuthInvalidatedOAuth = regexp.MustCompile(`(?i)invalidated.{0,20}oauth token`)
	reAuthSignInAgain      = regexp.MustCompile(`(?i)please (?:try )?sign(?:ing)? in again`)

	// Suspension patterns identify an org/billing block (e.g. a credit-card
	// limit disabling Claude subscription access), as distinct from a usage cap
	// or a benign scope refusal. They intentionally match the machine code and
	// the org-disabled message but NOT the setup-token "does not have the
	// required scopes" 403, which is an expected healthy path.
	reSuspendOrgNotAllowed  = regexp.MustCompile(`(?i)oauth_org_not_allowed`)
	reSuspendOrgDisabledSub = regexp.MustCompile(`(?i)organization has disabled.{0,40}subscription`)
	reSuspendDisabledAccess = regexp.MustCompile(`(?i)disabled Claude subscription access`)

	reUsageInsufficientQuota = regexp.MustCompile(`(?i)insufficient_quota`)
	reUsageQuotaExceeded     = regexp.MustCompile(`(?i)quota_exceeded`)
	reUsageLimitReached      = regexp.MustCompile(`(?i)usage_limit_reached`)
	reUsageCreditsExhausted  = regexp.MustCompile(`(?i)credits? exhausted`)
	reUsageOutOfCredits      = regexp.MustCompile(`(?i)out of credits`)
	reUsageUsageLimit        = regexp.MustCompile(`(?i)usage limit`)
	reUsagePeriodicLimit     = regexp.MustCompile(`(?i)(?:daily|weekly|monthly) limit`)
	reUsageNHourLimit        = regexp.MustCompile(`(?i)\d+-hour limit`)
	reUsageHitYourLimit      = regexp.MustCompile(`(?i)hit your .{0,40}limit`)
	reUsageBillingLimit      = regexp.MustCompile(`(?i)billing.*(?:limit|quota)`)
	reUsageSubscription      = regexp.MustCompile(`(?i)subscription.*(?:expired|required)`)

	// Rate-limit banners (BOS-406). The two bare patterns are intentionally
	// liberal (they match the headless exit path); the two anchored banner
	// patterns pin the Claude 429 banner shape. All live in rateLimitedPatterns,
	// NOT transientPatterns, so they classify as KindRateLimited.
	reRateLimit429             = regexp.MustCompile(`(?i)\b429\b`)
	reRateLimitRate            = regexp.MustCompile(`(?i)\brate limit\b`)
	reRateLimitRejected        = regexp.MustCompile(`(?i)request rejected \(429\)`)
	reRateLimitWouldExc        = regexp.MustCompile(`(?i)would exceed your .*rate limit`)
	reTransient5xx             = regexp.MustCompile(`(?i)\b5(?:00|02|03|29)\b`)
	reTransientOverloaded      = regexp.MustCompile(`(?i)overloaded`)
	reTransientTimedOut        = regexp.MustCompile(`(?i)timed out`)
	reTransientTimeout         = regexp.MustCompile(`(?i)timeout`)
	reTransientServiceUnavail  = regexp.MustCompile(`(?i)service unavailable`)
	reTransientTempUnavailable = regexp.MustCompile(`(?i)temporarily unavailable`)
	reTransientAtCapacity      = regexp.MustCompile(`(?i)at capacity`)
)

var authPatterns = []*regexp.Regexp{
	reAuthInvalidatedToken,
	reAuthInvalidatedOAuth,
	reAuthSignInAgain,
}

// suspensionPatterns are a subset of the auth bucket: they classify as
// KindAuthInvalidated (permanent health failure), but are also matchable on
// their own via SuspensionReason so callers can log/persist a legible reason
// distinct from a bad-token failure without a new taxonomy Kind.
var suspensionPatterns = []*regexp.Regexp{
	reSuspendOrgNotAllowed,
	reSuspendOrgDisabledSub,
	reSuspendDisabledAccess,
}

var usagePatterns = []*regexp.Regexp{
	reUsageInsufficientQuota,
	reUsageQuotaExceeded,
	reUsageLimitReached,
	reUsageCreditsExhausted,
	reUsageOutOfCredits,
	reUsageUsageLimit,
	reUsagePeriodicLimit,
	reUsageNHourLimit,
	reUsageHitYourLimit,
	reUsageBillingLimit,
	reUsageSubscription,
}

// rateLimitedPatterns classify a 429 / rate-limit banner as KindRateLimited
// (BOS-406), keeping it distinct from the genuine infra transients below so a
// rotation consumer can act on it.
var rateLimitedPatterns = []*regexp.Regexp{
	reRateLimit429,
	reRateLimitRate,
	reRateLimitRejected,
	reRateLimitWouldExc,
}

var transientPatterns = []*regexp.Regexp{
	reTransient5xx,
	reTransientOverloaded,
	reTransientTimedOut,
	reTransientTimeout,
	reTransientServiceUnavail,
	reTransientTempUnavailable,
	reTransientAtCapacity,
}

// Classify inspects text (a raw provider/CLI banner or error message) and
// returns its Classification. now anchors reset-time parsing for a
// KindUsageExhausted match.
//
// Precedence is AUTH, then USAGE, then RATE_LIMITED, then TRANSIENT so a line
// like "please sign in again" classifies as auth even if it also contains
// transient-sounding text, a usage line classifies as usage rather than
// rate-limited (a usage banner mentioning "rate limit" is still a usage cap),
// and a 429 / rate-limit banner classifies as its own KindRateLimited rather
// than a benign infra transient (BOS-406). Text matching none of the patterns
// fails safe to KindNone.
func Classify(text string, now time.Time) Classification {
	if anyMatch(authPatterns, text) || anyMatch(suspensionPatterns, text) {
		return Classification{Kind: KindAuthInvalidated}
	}
	if anyMatch(usagePatterns, text) {
		c := Classification{Kind: KindUsageExhausted}
		if resetAt, ok := ParseResetTime(text, now); ok {
			c.ResetAt = &resetAt
		}
		return c
	}
	if anyMatch(rateLimitedPatterns, text) {
		return Classification{Kind: KindRateLimited}
	}
	if anyMatch(transientPatterns, text) {
		return Classification{Kind: KindTransientProvider}
	}
	return Classification{Kind: KindNone}
}

// SuspensionReason reports whether text carries an account-suspension signature
// (an org/billing block such as a credit-card limit disabling Claude
// subscription access) and, if so, returns a short human-legible reason. It is
// a narrow sub-check of the auth bucket: a suspension already classifies as
// KindAuthInvalidated via Classify, but callers use this to persist/log a
// reason distinct from an ordinary invalidated credential. text may be a raw
// HTTP error body, a decoded error message, or a CLI banner.
func SuspensionReason(text string) (string, bool) {
	if anyMatch(suspensionPatterns, text) {
		return "account suspended: organization disabled Claude subscription access", true
	}
	return "", false
}

func anyMatch(patterns []*regexp.Regexp, text string) bool {
	for _, p := range patterns {
		if p.MatchString(text) {
			return true
		}
	}
	return false
}
