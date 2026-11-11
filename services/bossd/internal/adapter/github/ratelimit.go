package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	// by QuotaStatus and the floors. A refusal re-probes any reading older than
	// the refused call (see readingAfter); the pause it opens bounds those
	// probes to about one per quota per minute.
	quotaReadingTTL = 60 * time.Second
)

// quotaFor reports which GitHub quota a gh invocation spends.
func quotaFor(args []string) string {
	if len(args) > 0 && args[0] == "api" && (len(args) < 2 || args[1] != "graphql") {
		return quotaCore
	}
	return quotaGraphQL
}

// isRateLimitRefusal reports whether err is GitHub refusing an AUTHENTICATED
// request for a rate limit: "API rate limit exceeded for user ID N", "API rate
// limit already exceeded" or "You have exceeded a secondary rate limit". The
// anonymous form is an auth failure (see isGitHubAuthFailure) and is excluded.
// The text only says GitHub refused the call; it does not say which limit,
// because GitHub words its per-minute GraphQL refusal exactly like a spent
// hourly quota. refusalPause decides that from a quota reading.
func isRateLimitRefusal(err error) bool {
	if err == nil || isGitHubAuthFailure(err) {
		return false
	}
	msg := strings.ToLower(ghResponseText(err))
	return strings.Contains(msg, "api rate limit") || strings.Contains(msg, "secondary rate limit")
}

// rateLimitedError is returned for a call made while its quota is paused:
// either the call that discovered it (cause is GitHub's own refusal) or a call
// the breaker refused without contacting GitHub (cause is nil).
type rateLimitedError struct {
	quota string
	// identity is whose pool ran out: ghusage.IdentityUser or an
	// installation identity.
	identity string
	resetAt  time.Time
	cause    error
	// burst is set when the pause is a short per-minute or secondary back-off
	// rather than a spent hourly quota; see refusalPause.
	burst bool
	// belowFloor is set when the background floor refused the call while the
	// quota still had headroom; see backgroundFloorRefusal.
	belowFloor *quotaShortfall
	// paced is set when the call pacer refused a background read; see admit.
	paced bool
}

func (e *rateLimitedError) Error() string {
	if e.belowFloor != nil {
		return e.belowFloor.message(e.quota, e.resetAt)
	}
	if e.burst {
		return e.burstMessage()
	}
	if e.paced {
		return e.pacedMessage()
	}
	if e.cause != nil {
		return fmt.Sprintf("%v (GitHub %s quota exhausted for %s; pausing %s calls until %s)",
			e.cause, e.quota, e.identityName(), e.quota, e.resetAt.Format(time.RFC3339))
	}
	return fmt.Sprintf("GitHub %s API rate limit exhausted for %s; call skipped until the quota resets at %s",
		e.quota, e.identityName(), e.resetAt.Format(time.RFC3339))
}

// identityName is the pool owner the messages name; unset means the user.
func (e *rateLimitedError) identityName() string {
	if e.identity == "" {
		return ghusage.IdentityUser
	}
	return e.identity
}

func (e *rateLimitedError) Is(target error) bool { return target == vcs.ErrRateLimited }

func (e *rateLimitedError) ResetAt() time.Time { return e.resetAt }

func (e *rateLimitedError) Unwrap() error { return e.cause }

// quotaPool is one identity's GitHub quota: the user's login and each
// installation token have their own pools, so exhausting one never pauses
// another.
type quotaPool struct {
	identity string
	quota    string
}

// rateLimitBreaker remembers, per quota pool, when a paused pool may be called
// again. It is shared by every caller of one Provider — pollers, reconcile,
// the callback evaluator, plugins via the host service — so the first caller
// to see a refusal stops all of them, instead of each one re-hitting the limit
// on its own schedule.
type rateLimitBreaker struct {
	logger zerolog.Logger
	now    func() time.Time
	// jitter returns a random duration in [0, max); nil uses rand.N. See
	// WithJitterFunc.
	jitter func(max time.Duration) time.Duration

	mu   sync.Mutex
	open map[quotaPool]pauseWindow
	// streak counts burst refusals per pool since its last successful call;
	// see openBurst.
	streak map[quotaPool]int
}

// pauseWindow is one open pause: when it ends and whether it is a burst
// back-off rather than a spent hourly quota.
type pauseWindow struct {
	until time.Time
	burst bool
}

// pausedUntil reports the open pause while pool is paused. A pause that has
// ended is cleared here, which is where resumption is logged.
func (b *rateLimitBreaker) pausedUntil(pool quotaPool) (pauseWindow, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	w, ok := b.open[pool]
	if !ok {
		return pauseWindow{}, false
	}
	if b.now().Before(w.until) {
		return w, true
	}
	delete(b.open, pool)
	b.logger.Info().Str("quota", pool.quota).Str("identity", pool.identity).Msg("github rate limit reset; resuming calls")
	return pauseWindow{}, false
}

// pause stops calls to quota until resetAt because its hourly quota is spent
// (or unreadable). An already-open pause is only ever extended, so a late probe
// cannot shorten a pause a later one lengthened.
func (b *rateLimitBreaker) pause(pool quotaPool, resetAt time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.open == nil {
		b.open = make(map[quotaPool]pauseWindow)
	}
	if prev, ok := b.open[pool]; ok && !resetAt.After(prev.until) {
		return
	}
	b.open[pool] = pauseWindow{until: resetAt}
	b.logger.Warn().
		Str("quota", pool.quota).
		Str("identity", pool.identity).
		Time("reset_at", resetAt).
		Dur("pause", resetAt.Sub(b.now()).Round(time.Second)).
		Msg("github rate limit exhausted; pausing calls until the quota resets")
}

// ghAs runs one gh invocation as id behind the rate-limit breaker; ctx
// already carries id (see withIdentity). While the call's quota pool is
// paused it fails immediately without contacting GitHub; when GitHub refuses
// the call for a rate limit, refusalPause decides how long to pause that pool
// for every caller. A background read below its pool's floor also fails
// without contacting GitHub, but pauses nothing (see backgroundFloorRefusal).
// The pool's call pacer may then hold a background read for its slot, and the
// breaker is checked again after that wait (see admit). Every call it sends is
// metered against the label the context carries and id, and so is every call
// the breaker or the floor refuses, as a skip; a pacer refusal and the free
// rate_limit probe are not metered.
func (p *Provider) ghAs(ctx context.Context, id ghIdentity, args ...string) (string, error) {
	pool := quotaPool{identity: id.key, quota: quotaFor(args)}
	if err := p.breakerRefusal(pool); err != nil {
		p.recordSkip(ctx, pool, err)
		return "", err
	}
	if err := p.backgroundFloorRefusal(ctx, pool, args); err != nil {
		p.recordSkip(ctx, pool, err)
		return "", err
	}
	waited, err := p.admit(ctx, pool, args)
	if err != nil {
		return "", err
	}
	if waited {
		if err := p.breakerRefusal(pool); err != nil {
			p.recordSkip(ctx, pool, err)
			return "", err
		}
	}
	callStart := p.nowFn()
	out, err := p.runGH(ctx, args...)
	p.recordResult(ctx, pool, out, err)
	if err == nil {
		p.limits.succeeded(pool)
		return out, nil
	}
	if !isRateLimitRefusal(err) {
		return out, err
	}
	// Concurrent callers all see the same refusal; only the first needs to
	// decide the pause.
	w, paused := p.limits.pausedUntil(pool)
	if !paused {
		w = p.refusalPause(ctx, pool, callStart, err)
	}
	return "", &rateLimitedError{quota: pool.quota, identity: pool.identity, resetAt: w.until, burst: w.burst, cause: err}
}

// breakerRefusal returns the breaker's error while pool is paused.
func (p *Provider) breakerRefusal(pool quotaPool) error {
	if w, paused := p.limits.pausedUntil(pool); paused {
		return &rateLimitedError{quota: pool.quota, identity: pool.identity, resetAt: w.until, burst: w.burst}
	}
	return nil
}

// refusalPause decides, and opens, the pause after GitHub refused a call to
// quota that started at callStart. A secondary rate limit is a burst back-off
// by its own wording. Anything else is judged by a quota reading taken after
// the call started, because GitHub words the per-minute and hourly refusals
// alike: remaining 0 is a spent hourly quota, paused until its reset; a full
// quota means the reset landed mid-call, so nothing pauses; any other headroom
// is a per-minute burst (see openBurst). No usable reading falls back to
// rateLimitFallbackPause.
func (p *Provider) refusalPause(ctx context.Context, pool quotaPool, callStart time.Time, err error) pauseWindow {
	if isSecondaryRateLimit(err) {
		return p.limits.openBurst(pool, nil)
	}
	res, ok := p.readingAfter(ctx, pool.quota, callStart)
	now := p.nowFn()
	switch {
	case !ok:
		w := pauseWindow{until: now.Add(rateLimitFallbackPause)}
		p.limits.pause(pool, w.until)
		return w
	case res.Remaining == 0:
		w := pauseWindow{until: hourlyResetAt(res, now)}
		p.limits.pause(pool, w.until)
		return w
	case res.Remaining >= res.limitOrDefault():
		return pauseWindow{until: now}
	}
	return p.limits.openBurst(pool, &res)
}

// readingAfter returns quota's figures, for the identity ctx carries, from a
// reading taken no earlier than callStart, expiring and re-probing a cached
// reading older than the refused call. Probes go through readQuotas, so
// concurrent refusals share one.
func (p *Provider) readingAfter(ctx context.Context, quota string, callStart time.Time) (quotaStatus, bool) {
	reading, ok := p.readQuotas(ctx)
	if ok && reading.readAt.Before(callStart) {
		p.quotaCacheFor(ctx).expire(reading.readAt)
		reading, ok = p.readQuotas(ctx)
	}
	if !ok {
		return quotaStatus{}, false
	}
	res, ok := reading.status(quota)
	if !ok {
		p.logger.Warn().Str("quota", quota).Msg("github rate limit: reset time missing from rate_limit response; using fallback pause")
	}
	return res, ok
}

// hourlyResetAt is when a spent hourly quota resumes: GitHub's reset, capped
// at rateLimitMaxPause, or one minute away when the reset has already passed.
func hourlyResetAt(res quotaStatus, now time.Time) time.Time {
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

// defaultQuotaLimit is GitHub's documented authenticated hourly limit, assumed
// when a rate_limit entry omits its limit.
const defaultQuotaLimit = 5000

// quotaStatus is one quota's entry in a `gh api rate_limit` response.
type quotaStatus struct {
	Limit     int   `json:"limit"`
	Remaining int   `json:"remaining"`
	Reset     int64 `json:"reset"`
}

// limitOrDefault reports the quota's limit, or defaultQuotaLimit when the
// response gave none.
func (s quotaStatus) limitOrDefault() int {
	if s.Limit > 0 {
		return s.Limit
	}
	return defaultQuotaLimit
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

// quotaCache holds the latest quota reading for one identity of one Provider
// (see quotaCacheFor) and serialises the probes that refresh it, so concurrent
// refusals share one `gh api rate_limit` call. It only ever holds GitHub's own
// figures: a refusal never writes an inferred reading into it.
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

// readQuotas returns the quota reading of the identity ctx carries (the user
// when none), probing `gh api rate_limit` as that identity only when the
// cached one is older than quotaReadingTTL. Concurrent callers share one
// probe, including a failed one. A failed or unreadable probe is not cached.
// Later headroom checks (a quota floor, a back-off) should read through here
// rather than adding a second rate_limit read.
func (p *Provider) readQuotas(ctx context.Context) (quotaReading, bool) {
	c := p.quotaCacheFor(ctx)
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

// quotaCacheFor returns the quota cache of the identity ctx carries: p.quotas
// for the user, one per installation otherwise.
func (p *Provider) quotaCacheFor(ctx context.Context) *quotaCache {
	id := identityFrom(ctx)
	if id.isUser() {
		return &p.quotas
	}
	p.installQuotasMu.Lock()
	defer p.installQuotasMu.Unlock()
	c, ok := p.installQuotas[id.key]
	if !ok {
		if p.installQuotas == nil {
			p.installQuotas = make(map[string]*quotaCache)
		}
		c = &quotaCache{now: p.nowFn}
		p.installQuotas[id.key] = c
	}
	return c
}

// waitingCount reports how many callers are in readQuotas' probe path.
func (c *quotaCache) waitingCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.waiting
}

// QuotaStatus reports the user login's REST core and GraphQL quotas, in that
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

func refusedQuota(err error) (string, bool) {
	var limited *rateLimitedError
	if errors.As(err, &limited) {
		return limited.quota, true
	}
	return "", false
}
