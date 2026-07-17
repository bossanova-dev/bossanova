package agenterr

// ErrRateLimited is the typed error surfaced via Runner.ExitError when a
// headless run exits because the provider returned a 429 / rate limit (BOS-406).
// Unlike ErrUsageLimited it carries no ResetAt: a rate limit's reset is resolved
// authoritatively from the bossd usage probe, not parsed from the banner.
//
// It lives host-side of the plugin boundary (lib/bossalib) so both the agent
// plugins and the daemon can construct and errors.As it without violating the
// plugins-no-host-imports depguard. The Error() method has a value receiver so
// errors.As(err, &ErrRateLimited{}) matches an ErrRateLimited value returned
// from a PostExit hook.
type ErrRateLimited struct{}

// Error implements error. The message contains "429" so that when it is
// flattened into exit_error and re-classified on the bossd round-trip it lands
// back in rateLimitedPatterns and re-derives KindRateLimited — the property that
// routes it into the usage-limit rotation intercept.
func (e ErrRateLimited) Error() string {
	return "agent rate limit reached (429)"
}
