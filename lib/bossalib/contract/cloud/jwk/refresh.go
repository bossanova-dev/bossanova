package jwk

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
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

// KeyLookup is a cache's view of one kid at one instant.
type KeyLookup struct {
	Key      *rsa.PublicKey // nil when the cache does not hold kid
	Expired  bool           // the cache's TTL has lapsed
	Rejected string         // reason a published key failed validation
}

// ResolveKey returns the signing key for kid, refreshing missing or expired keys.
// lookup is called under g.mu and must be cheap and never call back into the gate.
// Lock order is
// g.mu followed by the caller's cache lock; fetch runs without g.mu and must
// commit its cache updates before returning.
func (g *RefreshGate) ResolveKey(ctx context.Context, kid string,
	lookup func(string) KeyLookup, fetch func(context.Context) error) (*rsa.PublicKey, error) {
	snapshot := lookup(kid)
	if snapshot.Key != nil && !snapshot.Expired {
		return snapshot.Key, nil
	}
	stale := snapshot.Key
	g.mu.Lock()
	snapshot = lookup(kid)
	if snapshot.Key != nil {
		stale = snapshot.Key
	}
	if snapshot.Key != nil && !snapshot.Expired {
		g.mu.Unlock()
		return snapshot.Key, nil
	}
	expired := snapshot.Expired
	err := g.refreshLocked(ctx, kid, expired, fetch)
	throttledUnknown := !expired && errors.Is(err, ErrRefreshThrottled)
	if err != nil && !throttledUnknown {
		if stale != nil {
			return stale, nil
		}
		return nil, fmt.Errorf("fetch jwks: %w", err)
	}
	snapshot = lookup(kid)
	if snapshot.Key != nil {
		return snapshot.Key, nil
	}
	if !throttledUnknown {
		g.markUnknown(kid)
	}
	if snapshot.Rejected != "" {
		return nil, fmt.Errorf("signing key %q was published but rejected: %s", kid, snapshot.Rejected)
	}
	return nil, fmt.Errorf("unknown kid %q", kid)
}

// refreshLocked chooses a fetch from the cache view read while holding g.mu.
// It releases g.mu before fetching or waiting, including on throttled paths.
func (g *RefreshGate) refreshLocked(ctx context.Context, kid string, expired bool, fetch func(context.Context) error) error {
	now := g.now()
	if !expired {
		if exp, ok := g.unknown[kid]; ok && now.Before(exp) {
			g.mu.Unlock()
			return ErrRefreshThrottled
		}
	}
	if call := g.inflight; call != nil {
		g.mu.Unlock()
		return wait(ctx, call)
	}
	if g.inFailureBackoff() || (!expired && !g.lastUnknownFetch.IsZero() && now.Sub(g.lastUnknownFetch) < g.unknownInterval) {
		// Interval-throttled kids were never fetched and must not grow the map.
		g.mu.Unlock()
		return ErrRefreshThrottled
	}
	if !expired {
		g.lastUnknownFetch = now
	}
	call := g.begin()
	g.mu.Unlock()
	return g.run(ctx, call, fetch)
}

// markUnknown negatively caches kid for the unknown-kid interval.
func (g *RefreshGate) markUnknown(kid string) {
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
