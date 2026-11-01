package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/kit/vcs"

	"github.com/recurser/bossd/internal/ghusage"
)

// GitHub meters the gh CLI against two independent hourly quotas: `gh api
// repos/...` spends the REST "core" quota, while `gh pr ...` and `gh api
// graphql` spend the GraphQL one. Exhausting one leaves the other usable, so
// the breaker tracks them separately.
const (
	quotaCore    = ghusage.QuotaCore
	quotaGraphQL = ghusage.QuotaGraphQL
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
	// quotaReadingTTL is how long one `gh api rate_limit` reading is reused
	// before a refusal may pay for another probe.
	quotaReadingTTL = 60 * time.Second
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

func (e *rateLimitedError) ResetAt() time.Time { return e.resetAt }

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
// reports the quota exhausted it decides, from the cached quota reading, how
// long to pause the quota for every caller. Every call it sends is metered
// against the label the context carries; a refused call and the free
// rate_limit probe are not.
func (p *Provider) gh(ctx context.Context, args ...string) (string, error) {
	quota := quotaFor(args)
	if until, paused := p.limits.pausedUntil(quota); paused {
		return "", &rateLimitedError{quota: quota, resetAt: until}
	}
	callStart := p.nowFn()
	if p.usage != nil {
		p.usage.Record(ghusage.LabelFrom(ctx), quota)
	}
	out, err := p.runGH(ctx, args...)
	if err == nil || !isPrimaryRateLimit(err) {
		return out, err
	}
	p.quotas.markRefused(quota, callStart)
	// Concurrent callers all see the same exhaustion; only the first needs to
	// decide the pause.
	if until, paused := p.limits.pausedUntil(quota); paused {
		return "", &rateLimitedError{quota: quota, resetAt: until, cause: err}
	}
	resetAt := p.quotaResetAt(ctx, quota, callStart)
	if resetAt.After(p.nowFn()) {
		p.limits.pause(quota, resetAt)
	}
	return "", &rateLimitedError{quota: quota, resetAt: resetAt, cause: err}
}

// quotaResetAt decides when calls to quota may resume after a refusal of a
// call that started at callStart. It falls back to a short fixed pause when no
// reading is available. A reading that shows headroom and was taken after the
// call started means the reset landed between the refusal and the probe, so it
// returns now and nothing pauses. A headroom reading older than the call has
// been contradicted by GitHub and cannot be re-probed until it expires, so the
// quota pauses until then.
func (p *Provider) quotaResetAt(ctx context.Context, quota string, callStart time.Time) time.Time {
	reading, ok := p.readQuotas(ctx)
	now := p.nowFn()
	if !ok {
		return now.Add(rateLimitFallbackPause)
	}
	res, ok := reading.status(quota)
	if !ok {
		p.logger.Warn().Str("quota", quota).Msg("github rate limit: reset time missing from rate_limit response; using fallback pause")
		return now.Add(rateLimitFallbackPause)
	}
	if res.Remaining > 0 || res.refused {
		if !reading.readAt.Before(callStart) {
			return now
		}
		return reading.readAt.Add(quotaReadingTTL)
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
	Limit     int   `json:"limit"`
	Remaining int   `json:"remaining"`
	Reset     int64 `json:"reset"`
	// refused is set when a refusal zeroed a reported headroom figure, so the
	// breaker can tell GitHub's own reading of 0 from one it inferred.
	refused bool
}

// quotaReading is one parsed `gh api rate_limit` response: every quota's
// figures and when the probe that read them started.
type quotaReading struct {
	readAt    time.Time
	resources map[string]quotaStatus
}

// status reports quota's entry, when the reading has a usable one.
func (r quotaReading) status(quota string) (quotaStatus, bool) {
	res, ok := r.resources[quota]
	return res, ok && res.Reset > 0
}

// parseQuotaReading decodes a `gh api rate_limit` response.
func parseQuotaReading(out string, readAt time.Time) (quotaReading, bool) {
	var resp struct {
		Resources map[string]quotaStatus `json:"resources"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil || len(resp.Resources) == 0 {
		return quotaReading{}, false
	}
	return quotaReading{readAt: readAt, resources: resp.Resources}, true
}

// quotaCache holds the latest quota reading for one Provider (one daemon, one
// credential) and serialises the probes that refresh it, so concurrent
// refusals share one `gh api rate_limit` call and a burst of refusals inside
// quotaReadingTTL shares none.
type quotaCache struct {
	now func() time.Time

	// probeMu is held for the whole of one probe.
	probeMu sync.Mutex

	mu      sync.Mutex
	reading quotaReading
	// probes counts finished probes, so a caller that waited on probeMu can
	// tell that a probe finished meanwhile and share its outcome.
	probes uint64
	// waiting counts callers inside readQuotas' probe path (waiting on or
	// holding probeMu).
	waiting int
}

// fresh returns the cached reading while it is younger than quotaReadingTTL,
// and the count of finished probes.
func (c *quotaCache) fresh() (quotaReading, bool, uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reading.resources == nil || c.now().Sub(c.reading.readAt) >= quotaReadingTTL {
		return quotaReading{}, false, c.probes
	}
	return c.reading, true, c.probes
}

// finish records a probe's outcome; only a successful reading replaces the
// cached one.
func (c *quotaCache) finish(reading quotaReading, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.probes++
	if ok {
		c.reading = reading
	}
}

func (c *quotaCache) addWaiting(n int) {
	c.mu.Lock()
	c.waiting += n
	c.mu.Unlock()
}

// markRefused records that GitHub refused a call to quota that started at
// callStart: a cached reading taken before then that still shows headroom now
// shows the quota spent, keeping its reset and readAt. A reading taken after
// the call started is newer than the refusal and stays as GitHub reported it.
func (c *quotaCache) markRefused(quota string, callStart time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	res, ok := c.reading.resources[quota]
	if !ok || res.Remaining == 0 || !c.reading.readAt.Before(callStart) {
		return
	}
	resources := maps.Clone(c.reading.resources)
	res.Remaining, res.refused = 0, true
	resources[quota] = res
	c.reading.resources = resources
}

// readQuotas returns the GitHub quota reading, probing `gh api rate_limit` only
// when the cached one is older than quotaReadingTTL. Concurrent callers share
// one probe, including a failed one. A failed or unreadable probe is not
// cached. Later headroom checks (a quota floor, a back-off) should read through
// here rather than adding a second rate_limit read.
func (p *Provider) readQuotas(ctx context.Context) (quotaReading, bool) {
	c := &p.quotas
	reading, ok, seen := c.fresh()
	if ok {
		return reading, true
	}
	c.addWaiting(1)
	defer c.addWaiting(-1)
	c.probeMu.Lock()
	defer c.probeMu.Unlock()
	if reading, ok, done := c.fresh(); ok || done != seen {
		return reading, ok
	}
	readAt := c.now()
	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rateLimitProbeTimeout)
	defer cancel()
	out, err := p.runGH(probeCtx, "api", "rate_limit")
	if err != nil {
		p.logger.Warn().Err(err).Msg("github rate limit: could not read quotas; using fallback pause")
		c.finish(quotaReading{}, false)
		return quotaReading{}, false
	}
	reading, ok = parseQuotaReading(out, readAt)
	if !ok {
		p.logger.Warn().Msg("github rate limit: unreadable rate_limit response; using fallback pause")
	}
	c.finish(reading, ok)
	return reading, ok
}

// waitingCount reports how many callers are in readQuotas' probe path.
func (c *quotaCache) waitingCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.waiting
}

// QuotaStatus reports the daemon token's REST core and GraphQL quotas, in that
// order, from `gh api rate_limit`, which spends neither (see
// rateLimitProbeTimeout). It reads through readQuotas, so it shares the
// breaker's cached reading: one probe per quotaReadingTTL however many crons a
// tick fires. A quota missing from the response, or reporting no limit, is
// omitted; a failed or unreadable read is an error.
func (p *Provider) QuotaStatus(ctx context.Context) ([]vcs.Quota, error) {
	reading, ok := p.readQuotas(ctx)
	if !ok {
		return nil, errors.New("read github rate limit: no usable reading")
	}
	quotas := make([]vcs.Quota, 0, 2)
	for _, name := range []string{quotaCore, quotaGraphQL} {
		if res, ok := reading.status(name); ok && res.Limit > 0 {
			quotas = append(quotas, vcs.Quota{
				Name: name, Limit: res.Limit, Remaining: res.Remaining, ResetAt: time.Unix(res.Reset, 0),
			})
		}
	}
	return quotas, nil
}
