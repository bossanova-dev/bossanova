package github

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/recurser/bossalib/kit/vcs"
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
	// readKindOpenPRSnapshot is the repository's open-PR snapshot. Any PR's
	// InvalidatePR drops it, since the snapshot carries that PR.
	readKindOpenPRSnapshot = "open-pr-snapshot"
	// readKindBranchHead is keyed per branch, not per PR, so no PR webhook
	// invalidates it: a cached base branch tip is at most one TTL stale.
	readKindBranchHead = "branch-head"
)

type readCacheEntry struct {
	value any
	// fetchedAt is when the fetch that produced value began, so entries
	// compare by the age of their data: a slow fetch that completes late is
	// still as old as the moment it asked GitHub.
	fetchedAt time.Time
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
	// failedAt records a key's last failed fetch. Background reads skip
	// refetching it for one TTL, so a repository whose snapshot keeps failing
	// pays for one attempt per TTL rather than one per read.
	failedAt map[string]time.Time
}

func newReadCache(ttl time.Duration) *readCache {
	return &readCache{
		ttl:        ttl,
		now:        time.Now,
		entries:    make(map[string]readCacheEntry),
		generation: make(map[string]uint64),
		failedAt:   make(map[string]time.Time),
	}
}

func prReadKey(kind, repoPath string, prID int) string {
	return fmt.Sprintf("%s|%s#%d", kind, strings.ToLower(repoFlag(repoPath)), prID)
}

func repoReadKey(kind, repoPath string) string {
	return kind + "|" + strings.ToLower(repoFlag(repoPath))
}

// branchReadKey keeps the branch name's case: branch names are case-sensitive.
func branchReadKey(kind, repoPath, branch string) string {
	return repoReadKey(kind, repoPath) + "@" + branch
}

func (c *readCache) get(key string) (any, bool) {
	entry, ok := c.fresh(key)
	return entry.value, ok
}

// fresh returns key's entry while it is still servable.
func (c *readCache) fresh(key string) (readCacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || c.ttl <= 0 || c.now().Sub(entry.fetchedAt) >= c.ttl {
		return readCacheEntry{}, false
	}
	return entry, true
}

// backOff records a failed fetch of key. It reports whether key was not
// already backing off, so only the first failure in a TTL is news.
func (c *readCache) backOff(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if at, ok := c.failedAt[key]; ok && now.Sub(at) < c.ttl {
		return false
	}
	c.failedAt[key] = now
	return true
}

// backingOff reports whether a fetch of key failed within the last TTL.
func (c *readCache) backingOff(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	at, ok := c.failedAt[key]
	return ok && c.now().Sub(at) < c.ttl
}

// begin samples key's generation and the time a fetch of it starts.
func (c *readCache) begin(key string) (uint64, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.generation[key], c.now()
}

// store records value, fetched from at, for key unless key was invalidated
// after gen was read.
func (c *readCache) store(key string, gen uint64, at time.Time, value any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation[key] != gen {
		return
	}
	now := c.now()
	if len(c.entries) >= readCacheSweepAt {
		for k, e := range c.entries {
			if now.Sub(e.fetchedAt) >= c.ttl {
				delete(c.entries, k)
			}
		}
	}
	c.entries[key] = readCacheEntry{value: value, fetchedAt: at}
}

func (c *readCache) invalidate(keys ...string) {
	c.mu.Lock()
	for _, key := range keys {
		delete(c.entries, key)
		delete(c.failedAt, key)
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
// PR list (a webhook can announce a PR that list does not yet contain) and its
// open-PR snapshot, so the next background read of any PR in the repository
// refetches it in one query, even inside a failed snapshot's backoff. The
// webhook dispatcher calls it before anything re-reads the PR, so the reads a
// webhook triggers always go to GitHub. It implements vcs.ReadInvalidator.
func (p *Provider) InvalidatePR(repoPath string, prID int) {
	p.cache.invalidate(
		prReadKey(readKindStatus, repoPath, prID),
		prReadKey(readKindChecks, repoPath, prID),
		prReadKey(readKindReviews, repoPath, prID),
		repoReadKey(readKindOpenPRs, repoPath),
		repoReadKey(readKindOpenPRSnapshot, repoPath),
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
		gen, at := c.begin(key)
		v, err := fetch(ctx)
		if err != nil {
			return zero, err
		}
		c.store(key, gen, at, clone(v))
		return v, nil
	}
	if v, ok := c.get(key); ok {
		if typed, ok := v.(T); ok {
			return clone(typed), nil
		}
	}
	ch := c.flights.DoChan(key, func() (any, error) {
		gen, at := c.begin(key)
		flightCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), readFlightTimeout)
		defer cancel()
		v, err := fetch(flightCtx)
		if err != nil {
			return nil, err
		}
		c.store(key, gen, at, clone(v))
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
	return snapshotRead(ctx, p, repoPath, prReadKey(readKindStatus, repoPath, prID), clonePRStatus,
		func(s *prSnapshot) (*vcs.PRStatus, bool) { return s.status(prID) },
		func(ctx context.Context) (*vcs.PRStatus, error) { return p.fetchPRStatus(ctx, repoPath, prID) })
}

// GetCheckResults returns CI check results for a pull request. Background
// callers (vcs.WithCachedReads) may be served from the read cache.
func (p *Provider) GetCheckResults(ctx context.Context, repoPath string, prID int) ([]vcs.CheckResult, error) {
	set, err := p.GetCheckSet(ctx, repoPath, prID)
	return set.Checks, err
}

func cloneCheckSet(set vcs.CheckSet) vcs.CheckSet {
	set.Checks = slices.Clone(set.Checks)
	return set
}

// GetCheckSet reads CI checks and build provenance together. Background callers
// share the same cache entry as GetCheckResults.
func (p *Provider) GetCheckSet(ctx context.Context, repoPath string, prID int) (vcs.CheckSet, error) {
	return snapshotRead(ctx, p, repoPath, prReadKey(readKindChecks, repoPath, prID), cloneCheckSet,
		func(s *prSnapshot) (vcs.CheckSet, bool) { return s.checkSet(prID) },
		func(ctx context.Context) (vcs.CheckSet, error) { return p.fetchCheckSet(ctx, repoPath, prID) })
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
	return snapshotRead(ctx, p, repoPath, repoReadKey(readKindOpenPRs, repoPath), slices.Clone[[]vcs.PRSummary],
		func(s *prSnapshot) ([]vcs.PRSummary, bool) { return s.openPRs(), true },
		func(ctx context.Context) ([]vcs.PRSummary, error) { return p.fetchOpenPRs(ctx, repoPath) })
}

// snapshotRead serves a read the open-PR snapshot can answer. A ctx without
// vcs.WithCachedReads never consults the snapshot. A background read takes
// whichever of key's own entry and the repository snapshot holds newer data;
// with neither fresh it fetches the snapshot (one query for every open PR) and
// projects from it, unless a per-PR read stored while that fetch was in flight.
// Only a PR the snapshot declines, or a snapshot fetch that failed for any
// reason but an exhausted quota, falls through to the per-PR read. A failed
// snapshot is not refetched for one TTL (or until a webhook invalidates it);
// background reads go straight to the per-PR read meanwhile.
func snapshotRead[T any](ctx context.Context, p *Provider, repoPath, key string, clone func(T) T, project func(*prSnapshot) (T, bool), fetch func(context.Context) (T, error)) (T, error) {
	if !vcs.CachedReadsAllowed(ctx) {
		return cachedRead(ctx, p.cache, key, clone, fetch)
	}
	snapKey := repoReadKey(readKindOpenPRSnapshot, repoPath)
	v, served, snapOK := newestRead(p.cache, key, snapKey, clone, project)
	if served {
		return v, nil
	}
	if !snapOK && !p.cache.backingOff(snapKey) {
		var zero T
		fetched, err := cachedRead(ctx, p.cache, snapKey, func(s *prSnapshot) *prSnapshot { return s },
			func(ctx context.Context) (*prSnapshot, error) {
				s, err := p.fetchOpenPRSnapshot(ctx, repoPath)
				// The flight records its own failure, so one that fails after
				// its caller gave up still backs off the next reads.
				if err != nil && !errors.Is(err, vcs.ErrRateLimited) && p.cache.backOff(snapKey) {
					p.logger.Warn().Err(err).Str("repo", repoFlag(repoPath)).Msg("open PR snapshot failed; reading PRs one at a time")
				}
				return s, err
			})
		switch {
		case errors.Is(err, vcs.ErrRateLimited):
			// Falling back would spend the same exhausted quota per PR.
			return zero, err
		case err != nil && ctx.Err() != nil:
			// The caller gave up; a per-PR read under the same ctx would too.
			return zero, err
		case err == nil:
			// An unmarked read may have stored key while the snapshot was in
			// flight, from newer data than the snapshot's.
			if v, served, _ := newestRead(p.cache, key, snapKey, clone, project); served {
				return v, nil
			}
			if v, ok := project(fetched); ok {
				return clone(v), nil
			}
		}
	}
	return cachedRead(ctx, p.cache, key, clone, fetch)
}

// newestRead serves key from whichever of its own fresh entry and the fresh
// repository snapshot was fetched later; on a tie key's own entry wins. served
// is false when neither answers, and snapOK reports whether a fresh snapshot
// exists.
func newestRead[T any](c *readCache, key, snapKey string, clone func(T) T, project func(*prSnapshot) (T, bool)) (v T, served, snapOK bool) {
	own, ownOK := c.fresh(key)
	cached, ownTyped := own.value.(T)
	ownOK = ownOK && ownTyped
	snapEntry, snapOK := c.fresh(snapKey)
	snap, snapTyped := snapEntry.value.(*prSnapshot)
	snapOK = snapOK && snapTyped
	if snapOK && (!ownOK || snapEntry.fetchedAt.After(own.fetchedAt)) {
		if pv, ok := project(snap); ok {
			return clone(pv), true, true
		}
	}
	if ownOK {
		return clone(cached), true, snapOK
	}
	return v, false, snapOK
}
