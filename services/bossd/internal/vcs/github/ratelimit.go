package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/vcs"
)

// GitHub meters the gh CLI against two independent hourly quotas: `gh api
// repos/...` spends the REST "core" quota, while `gh pr ...` and `gh api
// graphql` spend the GraphQL one. Exhausting one leaves the other usable, so
// the breaker tracks them separately.
const (
	quotaCore    = "core"
	quotaGraphQL = "graphql"
)

const (
	// rateLimitProbeTimeout bounds the `gh api rate_limit` read that finds the
	// reset time. That endpoint is free: it does not spend either quota.
	rateLimitProbeTimeout = 10 * time.Second
	// rateLimitFallbackPause is how long calls pause when the reset time cannot
	// be read. Short enough that a wrong guess costs little, long enough that a
	// fleet of pollers stops hammering an empty quota.
	rateLimitFallbackPause = 5 * time.Minute
	// rateLimitMaxPause caps a reset time read from GitHub, so a skewed clock or
	// a garbled response cannot park every GitHub call for longer than one
	// quota window.
	rateLimitMaxPause = 65 * time.Minute
)

// quotaFor reports which GitHub quota a gh invocation spends.
func quotaFor(args []string) string {
	if len(args) > 0 && args[0] == "api" && (len(args) < 2 || args[1] != "graphql") {
		return quotaCore
	}
	return quotaGraphQL
}

// isPrimaryRateLimit reports whether err is GitHub refusing an AUTHENTICATED
// request because the account's hourly quota is spent ("API rate limit exceeded
// for user ID N" / "API rate limit already exceeded"). The anonymous form is an
// auth failure (see isGitHubAuthFailure) and a secondary rate limit is a short
// abuse back-off the transient retry ladder handles, so both are excluded.
func isPrimaryRateLimit(err error) bool {
	if err == nil || isGitHubAuthFailure(err) {
		return false
	}
	msg := strings.ToLower(ghResponseText(err))
	return strings.Contains(msg, "api rate limit") && !strings.Contains(msg, "secondary rate limit")
}

// rateLimitedError is returned for a call made while its quota is exhausted:
// either the call that discovered it (cause is GitHub's own refusal) or a call
// the breaker refused without contacting GitHub (cause is nil).
type rateLimitedError struct {
	quota   string
	resetAt time.Time
	cause   error
}

func (e *rateLimitedError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%v (GitHub %s quota exhausted; pausing %s calls until %s)",
			e.cause, e.quota, e.quota, e.resetAt.Format(time.RFC3339))
	}
	return fmt.Sprintf("GitHub %s API rate limit exhausted; call skipped until the quota resets at %s",
		e.quota, e.resetAt.Format(time.RFC3339))
}

func (e *rateLimitedError) Is(target error) bool { return target == vcs.ErrRateLimited }

func (e *rateLimitedError) Unwrap() error { return e.cause }

// rateLimitBreaker remembers, per quota, when an exhausted quota resets. It is
// shared by every caller of one Provider — pollers, reconcile, the callback
// evaluator, plugins via the host service — so the first caller to see the
// quota run out stops all of them, instead of each one re-hitting it on its
// own schedule until the reset.
type rateLimitBreaker struct {
	logger zerolog.Logger
	now    func() time.Time

	mu        sync.Mutex
	openUntil map[string]time.Time
}

// pausedUntil reports the reset time while quota is paused. A pause whose
// reset has passed is cleared here, which is where resumption is logged.
func (b *rateLimitBreaker) pausedUntil(quota string) (time.Time, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	until, ok := b.openUntil[quota]
	if !ok {
		return time.Time{}, false
	}
	if b.now().Before(until) {
		return until, true
	}
	delete(b.openUntil, quota)
	b.logger.Info().Str("quota", quota).Msg("github rate limit reset; resuming calls")
	return time.Time{}, false
}

// pause stops calls to quota until resetAt. An already-open pause is only ever
// extended, so a late probe cannot shorten a pause a later one lengthened.
func (b *rateLimitBreaker) pause(quota string, resetAt time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openUntil == nil {
		b.openUntil = make(map[string]time.Time)
	}
	if prev, ok := b.openUntil[quota]; ok && !resetAt.After(prev) {
		return
	}
	b.openUntil[quota] = resetAt
	b.logger.Warn().
		Str("quota", quota).
		Time("reset_at", resetAt).
		Dur("pause", resetAt.Sub(b.now()).Round(time.Second)).
		Msg("github rate limit exhausted; pausing calls until the quota resets")
}

// gh runs one gh invocation behind the rate-limit breaker. While the call's
// quota is paused it fails immediately without contacting GitHub; when GitHub
// reports the quota exhausted it reads the reset time and pauses the quota for
// every caller until then.
func (p *Provider) gh(ctx context.Context, args ...string) (string, error) {
	quota := quotaFor(args)
	if until, paused := p.limits.pausedUntil(quota); paused {
		return "", &rateLimitedError{quota: quota, resetAt: until}
	}
	out, err := p.runGH(ctx, args...)
	if err == nil || !isPrimaryRateLimit(err) {
		return out, err
	}
	// Concurrent callers all see the same exhaustion; only the first needs to
	// pay for the reset probe.
	if until, paused := p.limits.pausedUntil(quota); paused {
		return "", &rateLimitedError{quota: quota, resetAt: until, cause: err}
	}
	resetAt := p.quotaResetAt(ctx, quota)
	if resetAt.After(p.nowFn()) {
		p.limits.pause(quota, resetAt)
	}
	return "", &rateLimitedError{quota: quota, resetAt: resetAt, cause: err}
}

// quotaResetAt reads when quota resets from `gh api rate_limit`. It falls back
// to a short fixed pause when the probe fails, and returns now when GitHub
// says the quota already has headroom again (the reset landed between the
// refused call and the probe), so the caller does not pause at all.
func (p *Provider) quotaResetAt(ctx context.Context, quota string) time.Time {
	now := p.nowFn()
	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rateLimitProbeTimeout)
	defer cancel()
	out, err := p.runGH(probeCtx, "api", "rate_limit")
	if err != nil {
		p.logger.Warn().Err(err).Str("quota", quota).Msg("github rate limit: could not read reset time; using fallback pause")
		return now.Add(rateLimitFallbackPause)
	}
	res, ok := parseQuotaStatus(out, quota)
	if !ok {
		p.logger.Warn().Str("quota", quota).Msg("github rate limit: reset time missing from rate_limit response; using fallback pause")
		return now.Add(rateLimitFallbackPause)
	}
	if res.Remaining > 0 {
		return now
	}
	resetAt := time.Unix(res.Reset, 0)
	if resetAt.After(now.Add(rateLimitMaxPause)) {
		return now.Add(rateLimitMaxPause)
	}
	if !resetAt.After(now) {
		// Reported as spent but already past its reset: the next call will
		// tell. Pause briefly rather than not at all.
		return now.Add(time.Minute)
	}
	return resetAt
}

// quotaStatus is one quota's entry in a `gh api rate_limit` response.
type quotaStatus struct {
	Remaining int   `json:"remaining"`
	Reset     int64 `json:"reset"`
}

// parseQuotaStatus decodes quota's entry from a `gh api rate_limit` response.
func parseQuotaStatus(out, quota string) (quotaStatus, bool) {
	var resp struct {
		Resources map[string]quotaStatus `json:"resources"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return quotaStatus{}, false
	}
	res, ok := resp.Resources[quota]
	return res, ok && res.Reset > 0
}
