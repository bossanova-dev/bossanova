package github

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/recurser/bossalib/vcs"
)

const (
	// defaultReadCacheTTL is how long a read stays servable to background
	// callers. Long enough that the daemon's independent 2-minute loops (the
	// state-machine poller, display poller, callback reconcile, cron-title
	// repair) share one GitHub read per PR instead of each paying for its own;
	// short enough that a repo without webhooks still sees changes within a
	// poll cycle. A webhook for the PR drops the entry immediately.
	defaultReadCacheTTL = 60 * time.Second
	// readFlightTimeout bounds one coalesced fetch. The fetch runs detached from
	// whichever caller started it, so one caller's short deadline cannot fail
	// every caller that joined.
	readFlightTimeout = 60 * time.Second
	// readCacheSweepAt is the entry count above which a store also evicts
	// expired entries, bounding memory without a background goroutine.
	readCacheSweepAt = 256
)

// Cache key kinds. One PR's kinds are dropped together by InvalidatePR.
const (
	readKindStatus  = "status"
	readKindChecks  = "checks"
	readKindReviews = "reviews"
	readKindOpenPRs = "open-prs"
)

type readCacheEntry struct {
	value    any
	storedAt time.Time
}

// readCache holds recent successful reads for callers that opted in with
// vcs.WithCachedReads, and coalesces identical concurrent reads. Errors are
// never cached: a failed read is retried by the next caller.
type readCache struct {
	ttl time.Duration
	now func() time.Time

	mu      sync.Mutex
	entries map[string]readCacheEntry
	// generation is bumped by invalidation. A fetch that started before an
	// invalidation must not store its (possibly pre-webhook) result after it.
	generation map[string]uint64
	flights    singleflight.Group
}

func newReadCache(ttl time.Duration) *readCache {
	return &readCache{
		ttl:        ttl,
		now:        time.Now,
		entries:    make(map[string]readCacheEntry),
		generation: make(map[string]uint64),
	}
}

func prReadKey(kind, repoPath string, prID int) string {
	return fmt.Sprintf("%s|%s#%d", kind, strings.ToLower(repoFlag(repoPath)), prID)
}

func repoReadKey(kind, repoPath string) string {
	return kind + "|" + strings.ToLower(repoFlag(repoPath))
}

func (c *readCache) get(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || c.ttl <= 0 || c.now().Sub(entry.storedAt) >= c.ttl {
		return nil, false
	}
	return entry.value, true
}

func (c *readCache) generationOf(key string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.generation[key]
}

// store records value for key unless key was invalidated after gen was read.
func (c *readCache) store(key string, gen uint64, value any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation[key] != gen {
		return
	}
	now := c.now()
	if len(c.entries) >= readCacheSweepAt {
		for k, e := range c.entries {
			if now.Sub(e.storedAt) >= c.ttl {
				delete(c.entries, k)
			}
		}
	}
	c.entries[key] = readCacheEntry{value: value, storedAt: now}
}

func (c *readCache) invalidate(keys ...string) {
	c.mu.Lock()
	for _, key := range keys {
		delete(c.entries, key)
		c.generation[key]++
	}
	c.mu.Unlock()
	// A cached read arriving after this must not join a fetch that started
	// before it.
	for _, key := range keys {
		c.flights.Forget(key)
	}
}

// InvalidatePR drops every cached read for one PR, plus the repository's open
// PR list (a webhook can announce a PR that list does not yet contain). The
// webhook dispatcher calls it before anything re-reads the PR, so the reads a
// webhook triggers always go to GitHub. It implements vcs.ReadInvalidator.
func (p *Provider) InvalidatePR(repoPath string, prID int) {
	p.cache.invalidate(
		prReadKey(readKindStatus, repoPath, prID),
		prReadKey(readKindChecks, repoPath, prID),
		prReadKey(readKindReviews, repoPath, prID),
		repoReadKey(readKindOpenPRs, repoPath),
	)
}

// cachedRead serves one read. A ctx marked with vcs.WithCachedReads gets a
// fresh-enough cached value, or joins an identical read already in flight. Any
// other ctx always reads GitHub, and its result refreshes the cache for the
// background callers. clone keeps callers from sharing (and mutating) one
// cached value.
func cachedRead[T any](ctx context.Context, c *readCache, key string, clone func(T) T, fetch func(context.Context) (T, error)) (T, error) {
	var zero T
	if !vcs.CachedReadsAllowed(ctx) {
		gen := c.generationOf(key)
		v, err := fetch(ctx)
		if err != nil {
			return zero, err
		}
		c.store(key, gen, clone(v))
		return v, nil
	}
	if v, ok := c.get(key); ok {
		if typed, ok := v.(T); ok {
			return clone(typed), nil
		}
	}
	ch := c.flights.DoChan(key, func() (any, error) {
		gen := c.generationOf(key)
		flightCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), readFlightTimeout)
		defer cancel()
		v, err := fetch(flightCtx)
		if err != nil {
			return nil, err
		}
		c.store(key, gen, clone(v))
		return v, nil
	})
	select {
	case <-ctx.Done():
		return zero, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return zero, res.Err
		}
		// Unreachable: every flight for a key is produced by the same typed
		// getter. Reported rather than panicked so a future key collision
		// surfaces as a failed read, not a daemon crash.
		typed, ok := res.Val.(T)
		if !ok {
			return zero, fmt.Errorf("read cache flight %q returned %T", key, res.Val)
		}
		return clone(typed), nil
	}
}

func clonePRStatus(s *vcs.PRStatus) *vcs.PRStatus {
	if s == nil {
		return nil
	}
	c := *s
	return &c
}

// GetPRStatus returns the current status of a pull request. Background callers
// (vcs.WithCachedReads) may be served from the read cache.
func (p *Provider) GetPRStatus(ctx context.Context, repoPath string, prID int) (*vcs.PRStatus, error) {
	return cachedRead(ctx, p.cache, prReadKey(readKindStatus, repoPath, prID), clonePRStatus,
		func(ctx context.Context) (*vcs.PRStatus, error) { return p.fetchPRStatus(ctx, repoPath, prID) })
}

// GetCheckResults returns CI check results for a pull request. Background
// callers (vcs.WithCachedReads) may be served from the read cache.
func (p *Provider) GetCheckResults(ctx context.Context, repoPath string, prID int) ([]vcs.CheckResult, error) {
	return cachedRead(ctx, p.cache, prReadKey(readKindChecks, repoPath, prID), slices.Clone[[]vcs.CheckResult],
		func(ctx context.Context) ([]vcs.CheckResult, error) { return p.fetchCheckResults(ctx, repoPath, prID) })
}

// GetReviewComments returns review comments on a pull request. Background
// callers (vcs.WithCachedReads) may be served from the read cache.
func (p *Provider) GetReviewComments(ctx context.Context, repoPath string, prID int) ([]vcs.ReviewComment, error) {
	return cachedRead(ctx, p.cache, prReadKey(readKindReviews, repoPath, prID), slices.Clone[[]vcs.ReviewComment],
		func(ctx context.Context) ([]vcs.ReviewComment, error) {
			return p.fetchReviewComments(ctx, repoPath, prID)
		})
}

// ListOpenPRs returns all open pull requests for a repository. Background
// callers (vcs.WithCachedReads) may be served from the read cache.
func (p *Provider) ListOpenPRs(ctx context.Context, repoPath string) ([]vcs.PRSummary, error) {
	return cachedRead(ctx, p.cache, repoReadKey(readKindOpenPRs, repoPath), slices.Clone[[]vcs.PRSummary],
		func(ctx context.Context) ([]vcs.PRSummary, error) { return p.fetchOpenPRs(ctx, repoPath) })
}
