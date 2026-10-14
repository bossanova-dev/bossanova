package jwk

import (
	"context"
	"errors"
	"sync"
	"time"
)

const (
	// DefaultUnknownKIDInterval is the minimum spacing between JWKS fetches
	// triggered by a token naming a key id the cache does not hold. golang-jwt
	// resolves the key before verifying the signature, so without this bound
	// every forged token with a fresh kid costs one outbound JWKS request.
	DefaultUnknownKIDInterval = 30 * time.Second
	// DefaultFailureBackoff is how long a failed fetch suppresses the next one.
	// Callers keep serving the keys they already hold meanwhile, so a JWKS
	// outage degrades to stale keys instead of one outbound request per token.
	DefaultFailureBackoff = 5 * time.Second
	// maxUnknownKIDs bounds the negative cache. Kids past the cap are simply
	// not cached; the unknown-kid interval still bounds outbound fetches.
	maxUnknownKIDs = 1024
)

// ErrRefreshThrottled reports that the gate declined to fetch: the kid is in
// the negative cache, an unknown-kid fetch ran within the interval, or the
// last fetch failed within the failure backoff.
var ErrRefreshThrottled = errors.New("jwks refresh throttled")

// errRefreshPanicked is the shared result waiters see when the leader's fetch
// panicked; the panic itself still propagates on the leader's goroutine.
var errRefreshPanicked = errors.New("jwks refresh panicked")

// RefreshGate bounds how often a JWKS cache refetches its keys. It collapses
// concurrent fetches into one (singleflight), spaces unknown-kid fetches by a
// minimum interval, remembers kids that a fetch did not resolve for that same
// interval, and backs off after a failed fetch so callers can keep serving
// stale keys. The zero value is not usable; construct with NewRefreshGate.
type RefreshGate struct {
	unknownInterval time.Duration
	failureBackoff  time.Duration
	now             func() time.Time

	mu               sync.Mutex
	inflight         *refreshCall
	lastUnknownFetch time.Time
	lastFailure      time.Time
	lastSweep        time.Time
	unknown          map[string]time.Time // kid -> negative-cache expiry
}

type refreshCall struct {
	done chan struct{}
	err  error
}

// RefreshGateConfig tunes a RefreshGate. Zero fields take the defaults.
type RefreshGateConfig struct {
	UnknownKIDInterval time.Duration
	FailureBackoff     time.Duration
	// Now is injectable so tests can step time without sleeping.
	Now func() time.Time
}

// NewRefreshGate returns a gate configured by cfg.
func NewRefreshGate(cfg RefreshGateConfig) *RefreshGate {
	g := &RefreshGate{
		unknownInterval: cfg.UnknownKIDInterval,
		failureBackoff:  cfg.FailureBackoff,
		now:             cfg.Now,
		unknown:         make(map[string]time.Time),
	}
	if g.unknownInterval <= 0 {
		g.unknownInterval = DefaultUnknownKIDInterval
	}
	if g.failureBackoff <= 0 {
		g.failureBackoff = DefaultFailureBackoff
	}
	if g.now == nil {
		g.now = time.Now
	}
	return g
}

// Refresh runs a TTL-driven refetch of a cache whose keys have expired. It is
// not spaced by the unknown-kid interval — known-key rotation is unchanged —
// but concurrent callers share one fetch, and a fetch within the failure
// backoff of a failed one returns ErrRefreshThrottled.
func (g *RefreshGate) Refresh(ctx context.Context, fetch func(context.Context) error) error {
	g.mu.Lock()
	if call := g.inflight; call != nil {
		g.mu.Unlock()
		return wait(ctx, call)
	}
	if g.inFailureBackoff() {
		g.mu.Unlock()
		return ErrRefreshThrottled
	}
	call := g.begin()
	g.mu.Unlock()
	return g.run(ctx, call, fetch)
}

// RefreshUnknown runs a refetch triggered by a token naming kid, a key id the
// unexpired cache does not hold. It joins an in-flight fetch when there is
// one; otherwise it fetches only when kid is not negatively cached and no
// unknown-kid fetch ran within the interval. When it declines it returns
// ErrRefreshThrottled. A caller that still cannot resolve kid after a nil
// return should call MarkUnknown.
func (g *RefreshGate) RefreshUnknown(ctx context.Context, kid string, fetch func(context.Context) error) error {
	g.mu.Lock()
	now := g.now()
	if exp, ok := g.unknown[kid]; ok && now.Before(exp) {
		g.mu.Unlock()
		return ErrRefreshThrottled
	}
	if call := g.inflight; call != nil {
		g.mu.Unlock()
		return wait(ctx, call)
	}
	if g.inFailureBackoff() ||
		(!g.lastUnknownFetch.IsZero() && now.Sub(g.lastUnknownFetch) < g.unknownInterval) {
		// Do not negatively cache kids that were never fetched: the interval
		// already throttles them, and caching each forged kid would grow the
		// map per request.
		g.mu.Unlock()
		return ErrRefreshThrottled
	}
	g.lastUnknownFetch = now
	call := g.begin()
	g.mu.Unlock()
	return g.run(ctx, call, fetch)
}

// MarkUnknown negatively caches kid for the unknown-kid interval, so repeated
// tokens naming it do not trigger a refetch.
func (g *RefreshGate) MarkUnknown(kid string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.markUnknownLocked(kid, g.now())
}

func (g *RefreshGate) markUnknownLocked(kid string, now time.Time) {
	// Sweep expired entries at most once per interval, so the cost stays
	// amortised O(1) per insert, and cap the map so a burst of distinct
	// forged kids cannot grow it without bound.
	if now.Sub(g.lastSweep) >= g.unknownInterval {
		for k, exp := range g.unknown {
			if !now.Before(exp) {
				delete(g.unknown, k)
			}
		}
		g.lastSweep = now
	}
	if _, ok := g.unknown[kid]; !ok && len(g.unknown) >= maxUnknownKIDs {
		return
	}
	g.unknown[kid] = now.Add(g.unknownInterval)
}

// inFailureBackoff reports whether the last fetch failed recently. Callers
// hold g.mu.
func (g *RefreshGate) inFailureBackoff() bool {
	return !g.lastFailure.IsZero() && g.now().Sub(g.lastFailure) < g.failureBackoff
}

// begin registers a new in-flight call. Callers hold g.mu.
func (g *RefreshGate) begin() *refreshCall {
	call := &refreshCall{done: make(chan struct{})}
	g.inflight = call
	return call
}

// run executes fetch on the leader's goroutine and releases every waiter. The
// fetch runs detached from the leader's cancellation so a cancelled leader
// still settles the shared result; the fetcher's own HTTP timeout bounds it.
// The deferred settle releases waiters even if fetch panics.
func (g *RefreshGate) run(ctx context.Context, call *refreshCall, fetch func(context.Context) error) (err error) {
	err = errRefreshPanicked
	defer func() {
		g.mu.Lock()
		call.err = err
		g.inflight = nil
		if err != nil {
			g.lastFailure = g.now()
		} else {
			g.lastFailure = time.Time{}
			// A successful fetch may have published kids that were negatively
			// cached; let them resolve on their next use.
			clear(g.unknown)
		}
		g.mu.Unlock()
		close(call.done)
	}()
	err = fetch(context.WithoutCancel(ctx))
	return err
}

func wait(ctx context.Context, call *refreshCall) error {
	select {
	case <-call.done:
		return call.err
	case <-ctx.Done():
		return ctx.Err()
	}
}
