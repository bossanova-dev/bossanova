package github

import (
	"context"
	"fmt"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	// writePointCost is what GitHub's secondary limits charge a write: a
	// read costs 1 point and a mutating request 5. A gh shape
	// isReadInvocation does not recognise is charged as a write.
	writePointCost = 5
	// pacerMaxBackgroundWait bounds how long a background read waits for its
	// slot. Past it the bucket is in debt for longer than one window, so the
	// read is refused, like a floor refusal, rather than parking a goroutine.
	pacerMaxBackgroundWait = 60 * time.Second
	// pacerBurstWindow sizes each bucket: ten seconds' worth of points.
	pacerBurstWindow = 10 * time.Second
)

// CallPacing sets the points a minute each quota's sent calls may spend. A
// value of zero or less leaves that quota unpaced.
type CallPacing struct {
	CorePointsPerMinute    int
	GraphQLPointsPerMinute int
}

// WithCallPacing paces the calls gh sends with one token bucket per quota
// pool: each identity (the user's login and every installation) gets its own
// bucket at these rates, since GitHub limits each one separately.
// Foreground calls and writes always take a slot and send at once, driving
// the bucket into debt; background reads wait for their slot, or are refused
// when it is further away than pacerMaxBackgroundWait or their deadline. A
// Provider built without this option never paces or waits.
func WithCallPacing(c CallPacing) ProviderOption {
	return func(p *Provider) {
		p.pacer.perMinute = map[string]int{}
		for quota, perMinute := range map[string]int{quotaCore: c.CorePointsPerMinute, quotaGraphQL: c.GraphQLPointsPerMinute} {
			if perMinute > 0 {
				p.pacer.perMinute[quota] = perMinute
			}
		}
	}
}

// WithAfterFunc overrides the timer a paced background read waits on (for
// testing). It defaults to time.After.
func WithAfterFunc(f func(time.Duration) <-chan time.Time) ProviderOption {
	return func(p *Provider) { p.afterFn = f }
}

// callPacer holds one token bucket per paced quota pool. It only meters the
// daemon's own sends: it never writes into the quota reading, so QuotaStatus
// and the floors see only GitHub's figures.
type callPacer struct {
	// perMinute is the configured rate of each paced quota.
	perMinute map[string]int

	mu sync.Mutex
	// buckets holds each pool's bucket, made on its first call.
	buckets map[quotaPool]*quotaPacer
	// refusing records the pools whose background reads are being refused,
	// so the start and the end of a refusal log once each.
	refusing map[quotaPool]bool
}

// bucket returns pool's token bucket, or nil when its quota is unpaced.
func (c *callPacer) bucket(pool quotaPool) *quotaPacer {
	perMinute := c.perMinute[pool.quota]
	if perMinute <= 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.buckets == nil {
		c.buckets = make(map[quotaPool]*quotaPacer)
	}
	q := c.buckets[pool]
	if q == nil {
		q = newQuotaPacer(perMinute)
		c.buckets[pool] = q
	}
	return q
}

type quotaPacer struct {
	limiter *rate.Limiter
	// slot is the interval between two points at the paced rate; it bounds
	// the jitter added to a wait.
	slot time.Duration
}

func newQuotaPacer(perMinute int) *quotaPacer {
	burst := max(perMinute*int(pacerBurstWindow/time.Second)/60, writePointCost)
	return &quotaPacer{
		limiter: rate.NewLimiter(rate.Limit(float64(perMinute)/60), burst),
		slot:    time.Minute / time.Duration(perMinute),
	}
}

// pointCost is what one gh invocation spends against the per-minute limit.
func pointCost(args []string) int {
	if isReadInvocation(args) {
		return 1
	}
	return writePointCost
}

// admit takes pool's pacer slot for one call and reports whether it waited
// for it. Only the limiter's explicit-time API is used, with p.nowFn, so a
// fake clock drives it. A background read (one the floors would gate) waits
// for its slot plus jitter, or is refused without contacting GitHub; every
// other call reserves its points and returns at once. A refusal pauses
// nothing, so foreground calls keep running.
func (p *Provider) admit(ctx context.Context, pool quotaPool, args []string) (bool, error) {
	q := p.pacer.bucket(pool)
	if q == nil {
		return false, nil
	}
	now := p.nowFn()
	if _, tier := p.floors.floorFor(ctx); tier == "" || !isReadInvocation(args) {
		q.limiter.ReserveN(now, pointCost(args))
		return false, nil
	}
	r := q.limiter.ReserveN(now, 1)
	if !r.OK() {
		return false, nil
	}
	delay := r.DelayFrom(now)
	if delay > 0 {
		delay += p.pacerJitter(q.slot)
	}
	if delay > pacerMaxBackgroundWait || pastDeadline(ctx, now, delay) {
		r.CancelAt(now)
		p.pacer.transition(p, pool, true)
		return false, &rateLimitedError{quota: pool.quota, identity: pool.identity, resetAt: now.Add(delay), paced: true}
	}
	p.pacer.transition(p, pool, false)
	if delay <= 0 {
		return false, nil
	}
	select {
	case <-p.afterFn(delay):
		return true, nil
	case <-ctx.Done():
		r.CancelAt(p.nowFn())
		return false, ctx.Err()
	}
}

// pacerJitter returns a random duration in [0, max) from the breaker's jitter
// source, so daemons started together do not send in lockstep.
func (p *Provider) pacerJitter(max time.Duration) time.Duration {
	if p.limits.jitter != nil {
		return p.limits.jitter(max)
	}
	return defaultJitter(max)
}

// pastDeadline reports whether ctx's deadline arrives before a wait of delay
// from now ends. It measures from p.nowFn's now, which is time.Now in
// production.
func pastDeadline(ctx context.Context, now time.Time, delay time.Duration) bool {
	deadline, ok := ctx.Deadline()
	return ok && delay > 0 && deadline.Sub(now) <= delay
}

// transition logs one Info line when pool starts or stops refusing paced
// background reads.
func (c *callPacer) transition(p *Provider, pool quotaPool, refusing bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.refusing[pool] == refusing {
		return
	}
	if c.refusing == nil {
		c.refusing = make(map[quotaPool]bool)
	}
	c.refusing[pool] = refusing
	if refusing {
		p.logger.Info().Str("quota", pool.quota).Str("identity", pool.identity).Msg("github calls paced below the per-minute limit; refusing background reads")
		return
	}
	p.logger.Info().Str("quota", pool.quota).Str("identity", pool.identity).Msg("github pacer has slots again; resuming background reads")
}

// pacedMessage is a paced rateLimitedError's text. It names the per-minute
// pacing, never an exhausted quota.
func (e *rateLimitedError) pacedMessage() string {
	return fmt.Sprintf("GitHub %s background read for %s paced below the per-minute limit; retry after %s",
		e.quota, e.identityName(), e.resetAt.Format(time.RFC3339))
}
