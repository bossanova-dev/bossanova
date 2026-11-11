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
	// repair) share one GitHub read per PR instead of each paying for its own.
	// A webhook for the PR drops the entry immediately. The open-PR snapshot
	// is served longer instead (snapshotMaxAge).
	defaultReadCacheTTL = 60 * time.Second
	// readFlightTimeout bounds one coalesced fetch. The fetch runs detached from
	// whichever caller started it, so one caller's short deadline cannot fail
	// every caller that joined.
	readFlightTimeout = 60 * time.Second
	// readCacheSweepAt is the entry count above which a store also evicts
	// expired entries, bounding memory without a background goroutine.
	readCacheSweepAt = 256
	// reviewCommentsMaxAge bounds how long a review-comments entry whose
	// review-activity fingerprint still matches the open-PR snapshot's stays
	// servable to background callers. The fingerprint cannot see an edit to an
	// existing inline comment's body; a webhook catches that on connected
	// repositories, and this bound catches it everywhere else.
	reviewCommentsMaxAge = 10 * time.Minute
	// snapshotSafetyNetMaxAge is how long the open-PR snapshot serves
	// background reads while the repo's webhook delivery is healthy: webhooks
	// invalidate it on every change, so the age only bounds a dropped delivery.
	// It mirrors the pollers' webhookSafetyNetInterval
	// (services/bossd/internal/session/webhook_polling.go), so a lost webhook
	// is caught on the same scale as their own safety net.
	snapshotSafetyNetMaxAge = 10 * time.Minute
	// snapshotFallbackMaxAge is how long the snapshot serves background reads
	// for a repo without healthy webhook delivery (no Boss Cloud login, a quiet
	// or broken webhook pipe, or a fresh reconnect): such a repo still catches
	// every change within this age.
	snapshotFallbackMaxAge = 5 * time.Minute
)

// WebhookDeliveryHealth reports whether GitHub webhooks are currently reaching
// this daemon for a repo. upstream.WebhookHealth satisfies it.
type WebhookDeliveryHealth interface {
	WebhookDeliveryHealthy(repo string) bool
}

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
	// fingerprint is the review-activity fingerprint of an open-PR snapshot
	// fetched no later than this entry's fetch began, or "" when there was
	// none. Only review-comments entries carry one; see GetReviewComments.
	fingerprint string
	// retain is how long the size sweep keeps the entry.
	retain time.Duration
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
	// repoEpoch is bumped by invalidateRepo. It counts toward the generation
	// of every key of that repo it covers, so one bump stops every in-flight
	// fetch of them from storing.
	repoEpoch map[string]uint64
	// failedAt records a key's last failed fetch. Background reads skip
	// refetching it for one TTL, so a repository whose snapshot keeps failing
	// pays for one attempt per TTL rather than one per read.
	failedAt map[string]time.Time
	// health decides the snapshot's max age per read; nil (no Boss Cloud
	// login) is never healthy.
	health WebhookDeliveryHealth
}

func newReadCache(ttl time.Duration) *readCache {
	return &readCache{
		ttl:        ttl,
		now:        time.Now,
		entries:    make(map[string]readCacheEntry),
		generation: make(map[string]uint64),
		repoEpoch:  make(map[string]uint64),
		failedAt:   make(map[string]time.Time),
	}
}

// SetWebhookHealth lets the open-PR snapshot serve a repo whose webhooks are
// healthy for snapshotSafetyNetMaxAge instead of snapshotFallbackMaxAge.
// Safe to call while reads are in flight.
func (p *Provider) SetWebhookHealth(h WebhookDeliveryHealth) {
	p.cache.mu.Lock()
	defer p.cache.mu.Unlock()
	p.cache.health = h
}

// snapshotMaxAge is how old repoPath's snapshot may be and still serve a
// background read, decided per read so a lapse in webhook health takes effect
// on the next one.
func (c *readCache) snapshotMaxAge(repoPath string) time.Duration {
	c.mu.Lock()
	h := c.health
	c.mu.Unlock()
	if h != nil && h.WebhookDeliveryHealthy(repoPath) {
		return snapshotSafetyNetMaxAge
	}
	return snapshotFallbackMaxAge
}

// keyRepo splits a read key into its kind and its lower-cased repository.
// Repository names hold neither '#' nor '@', so the first of them ends it.
func keyRepo(key string) (kind, repo string) {
	kind, rest, _ := strings.Cut(key, "|")
	if i := strings.IndexAny(rest, "#@"); i >= 0 {
		rest = rest[:i]
	}
	return kind, rest
}

// repoScoped reports whether invalidateRepo covers a kind. Reviews are left out:
// pushes, statuses and checks never change them.
func repoScoped(kind string) bool { return kind != readKindReviews }

// generationOf is key's own generation plus its repository's epoch when
// invalidateRepo covers it. Both only grow, so either bump changes the sum.
// Callers hold c.mu.
func (c *readCache) generationOf(key string) uint64 {
	gen := c.generation[key]
	if kind, repo := keyRepo(key); repoScoped(kind) {
		gen += c.repoEpoch[repo]
	}
	return gen
}

// retention is how long the size sweep keeps key's entry: the snapshot serves
// for up to snapshotSafetyNetMaxAge and a fingerprinted entry for up to
// reviewCommentsMaxAge, so both outlive the TTL.
func (c *readCache) retention(key, fingerprint string) time.Duration {
	if kind, _ := keyRepo(key); kind == readKindOpenPRSnapshot {
		return max(c.ttl, snapshotSafetyNetMaxAge)
	}
	if fingerprint != "" {
		return max(c.ttl, reviewCommentsMaxAge)
	}
	return c.ttl
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

// fresh returns key's entry while it is younger than the TTL.
func (c *readCache) fresh(key string) (readCacheEntry, bool) {
	return c.freshWithin(key, c.ttl)
}

// freshWithin returns key's entry while it is younger than maxAge. A
// non-positive TTL disables caching for every kind.
func (c *readCache) freshWithin(key string, maxAge time.Duration) (readCacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || c.ttl <= 0 || c.now().Sub(entry.fetchedAt) >= maxAge {
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
	return c.generationOf(key), c.now()
}

// matching returns key's entry while it carries fingerprint and is younger
// than reviewCommentsMaxAge. An empty fingerprint never matches.
func (c *readCache) matching(key, fingerprint string) (any, bool) {
	if fingerprint == "" || c.ttl <= 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || entry.fingerprint != fingerprint || c.now().Sub(entry.fetchedAt) >= reviewCommentsMaxAge {
		return nil, false
	}
	return entry.value, true
}

// peek returns key's entry whatever its age.
func (c *readCache) peek(key string) (readCacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	return entry, ok
}

// store records value, fetched from at and carrying fingerprint, for key
// unless key was invalidated after gen was read.
func (c *readCache) store(key string, gen uint64, at time.Time, value any, fingerprint string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generationOf(key) != gen {
		return
	}
	now := c.now()
	if len(c.entries) >= readCacheSweepAt {
		for k, e := range c.entries {
			if now.Sub(e.fetchedAt) >= e.retain {
				delete(c.entries, k)
			}
		}
	}
	c.entries[key] = readCacheEntry{value: value, fetchedAt: at, fingerprint: fingerprint, retain: c.retention(key, fingerprint)}
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
		c.flights.Forget(key + reserveFlightSuffix)
	}
}

// reserveFlightSuffix keeps a reserve-tier read (vcs.WithReserveQuotaReads)
// from joining a standard-tier flight for the same key, which the background
// floor may refuse while the reserve tier still has quota to spend. Both
// tiers share the cached entry.
const reserveFlightSuffix = "|reserve"

func flightKey(ctx context.Context, key string) string {
	if vcs.ReserveQuotaReads(ctx) {
		return key + reserveFlightSuffix
	}
	return key
}

// invalidateRepo drops every cached read invalidateRepo covers for repoPath
// and stops every in-flight fetch of them from storing.
func (c *readCache) invalidateRepo(repoPath string) {
	// Derived the way generationOf derives a key's repo, so both always agree.
	_, repo := keyRepo(repoReadKey(readKindOpenPRSnapshot, repoPath))
	c.mu.Lock()
	c.repoEpoch[repo]++
	for key := range c.entries {
		if kind, r := keyRepo(key); r == repo && repoScoped(kind) {
			delete(c.entries, key)
		}
	}
	for key := range c.failedAt {
		if kind, r := keyRepo(key); r == repo && repoScoped(kind) {
			delete(c.failedAt, key)
		}
	}
	c.mu.Unlock()
	// A background read arriving after this must not join a snapshot or list
	// fetch that started before it. A per-PR flight may still answer a caller
	// that joins it, but it cannot store.
	for _, key := range []string{repoReadKey(readKindOpenPRSnapshot, repoPath), repoReadKey(readKindOpenPRs, repoPath)} {
		c.flights.Forget(key)
		c.flights.Forget(key + reserveFlightSuffix)
	}
}

// InvalidateRepo marks repoPath's open-PR snapshot, open-PR list, per-PR
// status and checks, and branch heads stale, for a webhook that names no PR (a
// push, a commit status, a check on a branch): the next background read of
// any of them goes to GitHub, and a fetch begun before the call never stores.
// Review comments are kept. The webhook dispatcher calls it.
func (p *Provider) InvalidateRepo(repoPath string) {
	p.cache.invalidateRepo(repoPath)
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
// background callers. The stored entry carries fingerprint(at), at being when
// its fetch began; noFingerprint for every kind but review comments. clone
// keeps callers from sharing (and mutating) one cached value.
func cachedRead[T any](ctx context.Context, c *readCache, key string, fingerprint func(time.Time) string, clone func(T) T, fetch func(context.Context) (T, error)) (T, error) {
	var zero T
	if !vcs.CachedReadsAllowed(ctx) {
		gen, at := c.begin(key)
		v, err := fetch(ctx)
		if err != nil {
			return zero, err
		}
		c.store(key, gen, at, clone(v), fingerprint(at))
		return v, nil
	}
	if v, ok := c.get(key); ok {
		if typed, ok := v.(T); ok {
			return clone(typed), nil
		}
	}
	ch := c.flights.DoChan(flightKey(ctx, key), func() (any, error) {
		gen, at := c.begin(key)
		flightCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), readFlightTimeout)
		defer cancel()
		v, err := fetch(flightCtx)
		if err != nil {
			return nil, err
		}
		c.store(key, gen, at, clone(v), fingerprint(at))
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
	ctx = withRoute(ctx, "GetPRStatus", vcs.GitHubNWO(repoPath))
	return snapshotRead(ctx, p, repoPath, prReadKey(readKindStatus, repoPath, prID), clonePRStatus,
		func(s *prSnapshot) (*vcs.PRStatus, bool) { return s.status(prID) },
		func(ctx context.Context) (*vcs.PRStatus, error) { return p.fetchPRStatus(ctx, repoPath, prID) })
}

// GetCheckResults returns CI check results for a pull request. Background
// callers (vcs.WithCachedReads) may be served from the read cache.
func (p *Provider) GetCheckResults(ctx context.Context, repoPath string, prID int) ([]vcs.CheckResult, error) {
	ctx = withRoute(ctx, "GetCheckResults", vcs.GitHubNWO(repoPath))
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
	ctx = withRoute(ctx, "GetCheckSet", vcs.GitHubNWO(repoPath))
	return snapshotRead(ctx, p, repoPath, prReadKey(readKindChecks, repoPath, prID), cloneCheckSet,
		func(s *prSnapshot) (vcs.CheckSet, bool) { return s.checkSet(prID) },
		func(ctx context.Context) (vcs.CheckSet, error) { return p.fetchCheckSet(ctx, repoPath, prID) })
}

// GetReviewComments returns review comments on a pull request. A background
// caller (vcs.WithCachedReads) is served an entry younger than the TTL, then
// one whose review-activity fingerprint still matches the open-PR snapshot's
// for up to reviewCommentsMaxAge; otherwise it reads GitHub and stores the
// result with the fingerprint it read before the fetch began. Any other caller
// always reads GitHub, never fetches a snapshot, and stores the cached
// snapshot's fingerprint only when that snapshot predates its fetch. So an
// entry never carries a fingerprint taken after its fetch began, and a match
// means the snapshot saw no review activity since the entry's data was read.
func (p *Provider) GetReviewComments(ctx context.Context, repoPath string, prID int) ([]vcs.ReviewComment, error) {
	ctx = withRoute(ctx, "GetReviewComments", vcs.GitHubNWO(repoPath))
	key := prReadKey(readKindReviews, repoPath, prID)
	clone := slices.Clone[[]vcs.ReviewComment]
	fetch := func(ctx context.Context) ([]vcs.ReviewComment, error) {
		return p.fetchReviewComments(ctx, repoPath, prID)
	}
	if !vcs.CachedReadsAllowed(ctx) {
		return cachedRead(ctx, p.cache, key, func(at time.Time) string {
			return p.cachedReviewActivity(repoPath, prID, at)
		}, clone, fetch)
	}
	if v, ok := p.cache.get(key); ok {
		if typed, ok := v.([]vcs.ReviewComment); ok {
			return clone(typed), nil
		}
	}
	fp, err := p.currentReviewActivity(ctx, repoPath, prID)
	if err != nil {
		return nil, err
	}
	if v, ok := p.cache.matching(key, fp); ok {
		if typed, ok := v.([]vcs.ReviewComment); ok {
			return clone(typed), nil
		}
	}
	return cachedRead(ctx, p.cache, key, func(time.Time) string { return fp }, clone, fetch)
}

func noFingerprint(time.Time) string { return "" }

// currentReviewActivity returns prID's fingerprint from the fresh open-PR
// snapshot, fetching it when none is cached. It returns "" (no proof of no
// change) when the snapshot declines the PR, is backing off or failed, and an
// error only for an exhausted quota or a caller that gave up: a per-PR read
// would spend the same quota, or fail the same way.
func (p *Provider) currentReviewActivity(ctx context.Context, repoPath string, prID int) (string, error) {
	snapKey := repoReadKey(readKindOpenPRSnapshot, repoPath)
	entry, fresh := p.cache.fresh(snapKey)
	snap, ok := entry.value.(*prSnapshot)
	if !fresh || !ok {
		if p.cache.backingOff(snapKey) {
			return "", nil
		}
		var err error
		snap, err = p.readOpenPRSnapshot(ctx, repoPath)
		switch {
		case errors.Is(err, vcs.ErrRateLimited), err != nil && ctx.Err() != nil:
			return "", err
		case err != nil:
			return "", nil
		}
	}
	fp, _ := snap.reviewActivity(prID)
	return fp, nil
}

// cachedReviewActivity returns prID's fingerprint from the cached open-PR
// snapshot, of any age, when that snapshot completed no later than at; ""
// otherwise. A snapshot that began before at but read a page after it could
// record activity the fetch begun at at never saw. It never fetches.
func (p *Provider) cachedReviewActivity(repoPath string, prID int, at time.Time) string {
	entry, ok := p.cache.peek(repoReadKey(readKindOpenPRSnapshot, repoPath))
	snap, typed := entry.value.(*prSnapshot)
	if !ok || !typed || snap.completedAt.After(at) {
		return ""
	}
	fp, _ := snap.reviewActivity(prID)
	return fp
}

// ListOpenPRs returns all open pull requests for a repository. Background
// callers (vcs.WithCachedReads) may be served from the read cache.
func (p *Provider) ListOpenPRs(ctx context.Context, repoPath string) ([]vcs.PRSummary, error) {
	ctx = withRoute(ctx, "ListOpenPRs", vcs.GitHubNWO(repoPath))
	return snapshotRead(ctx, p, repoPath, repoReadKey(readKindOpenPRs, repoPath), slices.Clone[[]vcs.PRSummary],
		func(s *prSnapshot) ([]vcs.PRSummary, bool) { return s.openPRs(), true },
		func(ctx context.Context) ([]vcs.PRSummary, error) { return p.fetchOpenPRs(ctx, repoPath) })
}

// snapshotRead serves a read the open-PR snapshot can answer. A ctx without
// vcs.WithCachedReads never consults the snapshot. A background read takes
// whichever of key's own entry and the repository snapshot holds newer data;
// with neither fresh it fetches the snapshot (one query for every open PR) and
// projects from it, unless a per-PR read stored while that fetch was in flight.
// A PR the snapshot declines, or a failed snapshot fetch, falls through
// to the per-PR read. A failed
// snapshot is not refetched for one TTL (or until a webhook invalidates it);
// background reads go straight to the per-PR read meanwhile.
func snapshotRead[T any](ctx context.Context, p *Provider, repoPath, key string, clone func(T) T, project func(*prSnapshot) (T, bool), fetch func(context.Context) (T, error)) (T, error) {
	if !vcs.CachedReadsAllowed(ctx) {
		return cachedRead(ctx, p.cache, key, noFingerprint, clone, fetch)
	}
	snapKey := repoReadKey(readKindOpenPRSnapshot, repoPath)
	maxAge := p.cache.snapshotMaxAge(repoPath)
	v, served, snapOK := newestRead(p.cache, key, snapKey, maxAge, clone, project)
	if served {
		return v, nil
	}
	if !snapOK && !p.cache.backingOff(snapKey) {
		var zero T
		fetched, err := p.readOpenPRSnapshot(ctx, repoPath)
		switch {
		case err != nil && ctx.Err() != nil:
			// The caller gave up; a per-PR read under the same ctx would too.
			return zero, err
		case err == nil:
			// An unmarked read may have stored key while the snapshot was in
			// flight, from newer data than the snapshot's.
			if v, served, _ := newestRead(p.cache, key, snapKey, maxAge, clone, project); served {
				return v, nil
			}
			// A snapshot this read joined may predate key's own entry; it
			// must not answer over it either.
			if own, ok := p.cache.peek(key); ok && !own.fetchedAt.Before(fetched.completedAt) {
				break
			}
			if v, ok := project(fetched); ok {
				return clone(v), nil
			}
		}
	}
	return cachedRead(ctx, p.cache, key, noFingerprint, clone, fetch)
}

// readOpenPRSnapshot reads the repository's open-PR snapshot through the
// cache, coalesced with any identical read in flight. A failed fetch backs the
// snapshot off for one TTL and is logged once per backoff window.
func (p *Provider) readOpenPRSnapshot(ctx context.Context, repoPath string) (*prSnapshot, error) {
	snapKey := repoReadKey(readKindOpenPRSnapshot, repoPath)
	return cachedRead(ctx, p.cache, snapKey, noFingerprint, func(s *prSnapshot) *prSnapshot { return s },
		func(ctx context.Context) (*prSnapshot, error) {
			s, err := p.fetchOpenPRSnapshot(ctx, repoPath)
			// The flight records its own failure, so one that fails after
			// its caller gave up still backs off the next reads.
			if err != nil && !errors.Is(err, vcs.ErrRateLimited) && p.cache.backOff(snapKey) {
				p.logger.Warn().Err(err).Str("repo", repoFlag(repoPath)).Msg("open PR snapshot failed; reading PRs one at a time")
			}
			return s, err
		})
}

// newestRead serves key from whichever of its own fresh entry and the
// repository snapshot younger than snapMaxAge was fetched later; on a tie key's
// own entry wins. The snapshot outlives the TTL, so it never answers while key
// still holds an entry, fresh or expired, fetched no earlier than it: that
// would step back to older data, and key's own read is repeated instead.
// served is false when neither answers, and snapOK reports whether a fresh
// snapshot exists.
func newestRead[T any](c *readCache, key, snapKey string, snapMaxAge time.Duration, clone func(T) T, project func(*prSnapshot) (T, bool)) (v T, served, snapOK bool) {
	own, ownCached := c.peek(key)
	cached, ownTyped := own.value.(T)
	ownCached = ownCached && ownTyped
	_, ownOK := c.fresh(key)
	ownOK = ownOK && ownCached
	snapEntry, snapOK := c.freshWithin(snapKey, snapMaxAge)
	snap, snapTyped := snapEntry.value.(*prSnapshot)
	snapOK = snapOK && snapTyped
	if snapOK && (!ownCached || snapEntry.fetchedAt.After(own.fetchedAt)) {
		if pv, ok := project(snap); ok {
			return clone(pv), true, true
		}
	}
	if ownOK {
		return clone(cached), true, snapOK
	}
	return v, false, snapOK
}
