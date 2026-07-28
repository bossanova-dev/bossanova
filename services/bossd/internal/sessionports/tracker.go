package sessionports

import (
	"context"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

const (
	// freshnessTTL is how long a completed refresh stays fresh before a read
	// schedules the next one.
	freshnessTTL = 3 * time.Second
	// staleRetention is the longest a previously verified endpoint set is kept
	// while discovery is only ever coming back incomplete.
	staleRetention = 15 * time.Second
)

// backoffSchedule is the escalating delay applied after a failed or incomplete
// refresh, capped at its final value.
var backoffSchedule = []time.Duration{
	2 * time.Second,
	4 * time.Second,
	8 * time.Second,
	16 * time.Second,
	30 * time.Second,
}

// listenerDetector is the subset of *Detector the tracker depends on, injected
// so tests substitute a fake scan.
type listenerDetector interface {
	Scan(ctx context.Context, rootsBySession map[string][]Root) map[string]SessionResult
}

// Tracker turns BOS-471 listener discovery into verified, canonical local web
// endpoints without ever blocking a session read. Reads return an immutable copy
// of the cached endpoints immediately and schedule a background refresh when the
// cache is stale; a single lifecycle-owned worker performs the scan-probe-publish
// cycle. Close cancels all outstanding work and joins the worker.
type Tracker struct {
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	wake      chan struct{}
	closeOnce sync.Once

	logger   zerolog.Logger
	clock    clock
	detector listenerDetector
	roots    RootProvider
	prober   prober

	mu         sync.Mutex
	requested  map[string]requestedSession
	cache      map[string]cacheEntry
	epochSeq   uint64
	nextDue    time.Time
	hasRun     bool
	backoffIdx int

	// refreshHook, when set (tests only), is invoked after each refresh cycle
	// completes so tests can synchronize with the async worker.
	refreshHook func()
}

// requestedSession is a caller-requested session plus the identity epoch assigned
// when it (re)appeared under a given worktree path. Stale in-flight work whose
// captured epoch no longer matches is rejected at publish.
type requestedSession struct {
	snap  SessionSnapshot
	epoch uint64
}

// cacheEntry is a session's last-published endpoints plus the timestamp they were
// last confirmed non-empty (for bounded stale retention).
type cacheEntry struct {
	endpoints    []Endpoint
	lastPositive time.Time
}

// staleExpired reports whether a non-empty endpoint set has gone unconfirmed
// past the retention ceiling, so it must neither be surfaced on a read nor
// retained on an incomplete publish. Single-sourcing the ceiling here keeps the
// read path (visibleEndpointsLocked) and the publish path (publishLocked) from
// drifting apart.
func (e cacheEntry) staleExpired(now time.Time) bool {
	return len(e.endpoints) > 0 && now.Sub(e.lastPositive) >= staleRetention
}

// refreshResult is one session's outcome from a refresh cycle.
type refreshResult struct {
	endpoints []Endpoint
	complete  bool
}

// Option configures a Tracker.
type Option func(*Tracker)

// WithLogger sets the tracker's logger (aggregate debug diagnostics only).
func WithLogger(l zerolog.Logger) Option { return func(t *Tracker) { t.logger = l } }

func withClock(c clock) Option               { return func(t *Tracker) { t.clock = c } }
func withDetector(d listenerDetector) Option { return func(t *Tracker) { t.detector = d } }
func withRootProvider(r RootProvider) Option { return func(t *Tracker) { t.roots = r } }
func withProber(p prober) Option             { return func(t *Tracker) { t.prober = p } }
func withRefreshHook(fn func()) Option       { return func(t *Tracker) { t.refreshHook = fn } }

// New builds a production Tracker wired to live discovery: the BOS-471 detector,
// a tmux-root provider over the injected pane lister, a raw-socket prober, and
// the wall clock. The tracker owns a lifecycle context derived from parent, so
// daemon shutdown (cancelling parent) or Close cancels every scan, probe, and
// timer.
func New(parent context.Context, panePID PaneLister, opts ...Option) *Tracker {
	proc := newCommandProcessSource()
	base := make([]Option, 0, 4+len(opts))
	base = append(base,
		withDetector(NewDetector(zerolog.Nop())),
		withRootProvider(newTmuxRootProvider(panePID, proc)),
		withProber(newSocketProber()),
		withClock(realClock{}),
	)
	return newTracker(parent, append(base, opts...)...)
}

// newTracker applies options over defaults and starts the background worker. All
// injectable collaborators must be supplied (New does so for production; tests
// pass fakes).
func newTracker(parent context.Context, opts ...Option) *Tracker {
	// The cancel func is retained on the Tracker and invoked by Close (and by
	// parent cancellation); it is not leaked despite gosec's flow-insensitive check.
	ctx, cancel := context.WithCancel(parent) //nolint:gosec // cancel is stored and called in Close
	t := &Tracker{
		ctx:       ctx,
		cancel:    cancel,
		done:      make(chan struct{}),
		wake:      make(chan struct{}, 1),
		logger:    zerolog.Nop(),
		clock:     realClock{},
		requested: make(map[string]requestedSession),
		cache:     make(map[string]cacheEntry),
	}
	for _, o := range opts {
		o(t)
	}
	go t.run()
	return t
}

// Close cancels the lifecycle context and joins the worker. It is idempotent.
func (t *Tracker) Close() {
	t.closeOnce.Do(func() {
		t.cancel()
		<-t.done
	})
}

// Get returns the cached endpoints for a single session, upserting it into the
// tracked set and scheduling a refresh if due. It never blocks on scanning or
// probing and does not prune other sessions.
func (t *Tracker) Get(snap SessionSnapshot) []Endpoint {
	return t.getBatch([]SessionSnapshot{snap}, false)[snap.ID]
}

// GetBatch returns the cached endpoints for the given sessions, treating snaps as
// the full current set: sessions absent from snaps are pruned. Reads never block;
// a refresh is scheduled when the cache is stale.
func (t *Tracker) GetBatch(snaps []SessionSnapshot) map[string][]Endpoint {
	return t.getBatch(snaps, true)
}

func (t *Tracker) getBatch(snaps []SessionSnapshot, prune bool) map[string][]Endpoint {
	now := t.clock.Now()
	t.mu.Lock()
	defer t.mu.Unlock()

	present := make(map[string]bool, len(snaps))
	for _, s := range snaps {
		present[s.ID] = true
		t.upsertLocked(s)
	}
	if prune {
		for id := range t.requested {
			if !present[id] {
				delete(t.requested, id)
				delete(t.cache, id)
			}
		}
	}

	if len(t.requested) > 0 && t.dueLocked(now) {
		t.signalLocked()
	}

	out := make(map[string][]Endpoint, len(snaps))
	for _, s := range snaps {
		out[s.ID] = t.visibleEndpointsLocked(s.ID, now)
	}
	return out
}

// visibleEndpointsLocked returns a copy of a session's cached endpoints while
// enforcing the stale-retention ceiling on the read path. publishLocked only
// drops a stale positive set on a later refresh, and incomplete-refresh backoff
// can defer that refresh past lastPositive+staleRetention; without this guard a
// read in that gap would return an endpoint retained longer than the ceiling
// promises. Enforcing it here bounds the observable retention regardless of when
// the next refresh actually runs. Under normal polling a live endpoint is
// re-confirmed every freshness cycle, so lastPositive stays recent and only a
// genuinely stale (unconfirmable) set is suppressed.
func (t *Tracker) visibleEndpointsLocked(id string, now time.Time) []Endpoint {
	e := t.cache[id]
	if e.staleExpired(now) {
		return nil
	}
	return append([]Endpoint(nil), e.endpoints...)
}

// upsertLocked records or refreshes a requested session. A new session, or one
// whose worktree path changed, gets a fresh identity epoch and its stale cache
// dropped; a same-identity re-read only refreshes the snapshot (tmux names may
// change) and keeps the epoch.
func (t *Tracker) upsertLocked(s SessionSnapshot) {
	// The snapshot's TmuxNames slice is caller-owned and read later on the worker
	// goroutine (compute -> RootProvider.Roots). Clone it so a caller that reuses
	// or mutates the slice after Get/GetBatch returns cannot race with discovery.
	s.TmuxNames = append([]string(nil), s.TmuxNames...)
	cur, ok := t.requested[s.ID]
	if !ok || cur.snap.WorktreePath != s.WorktreePath {
		t.epochSeq++
		t.requested[s.ID] = requestedSession{snap: s, epoch: t.epochSeq}
		delete(t.cache, s.ID)
		return
	}
	cur.snap = s
	t.requested[s.ID] = cur
}

// dueLocked reports whether a refresh should be scheduled: the tracker has never
// refreshed, or the freshness/backoff deadline has elapsed.
func (t *Tracker) dueLocked(now time.Time) bool {
	return !t.hasRun || !now.Before(t.nextDue)
}

// signalLocked wakes the worker without blocking; a pending wake coalesces
// concurrent reads into a single refresh.
func (t *Tracker) signalLocked() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

// run is the single background worker: it refreshes on each wake until the
// lifecycle context is cancelled.
func (t *Tracker) run() {
	defer close(t.done)
	for {
		select {
		case <-t.ctx.Done():
			return
		case <-t.wake:
			t.refreshOnce()
		}
	}
}

// refreshOnce runs one refresh cycle if still due, capturing the latest merged
// session inputs, computing verified endpoints outside the lock, then publishing
// under the lock. A wake that is no longer due (a prior refresh already ran)
// no-ops, which is how coalesced wakes collapse to a single refresh.
func (t *Tracker) refreshOnce() {
	t.mu.Lock()
	now := t.clock.Now()
	if !t.dueLocked(now) {
		t.mu.Unlock()
		return
	}
	captured := make(map[string]requestedSession, len(t.requested))
	for id, rs := range t.requested {
		captured[id] = rs
	}
	t.mu.Unlock()

	var results map[string]refreshResult
	if len(captured) > 0 {
		results = t.compute(captured)
	}

	t.mu.Lock()
	t.publishLocked(captured, results, t.clock.Now())
	t.mu.Unlock()

	if t.refreshHook != nil {
		t.refreshHook()
	}
}

// compute resolves roots, scans for listeners, probes the derived loopback
// targets, and canonicalizes the verified endpoints per session. It holds no
// lock and is bounded by the lifecycle context plus the detector's and prober's
// own timeouts.
func (t *Tracker) compute(captured map[string]requestedSession) map[string]refreshResult {
	rootsBySession := make(map[string][]Root, len(captured))
	for id, rs := range captured {
		rootsBySession[id] = t.roots.Roots(t.ctx, rs.snap)
	}
	scan := t.detector.Scan(t.ctx, rootsBySession)

	perSession := make(map[string][]probeCandidate, len(scan))
	targetSet := make(map[string]struct{})
	for id, res := range scan {
		for _, l := range res.Listeners {
			c, ok := deriveCandidate(l)
			if !ok {
				continue
			}
			perSession[id] = append(perSession[id], c)
			targetSet[c.target()] = struct{}{}
		}
	}
	targets := make([]string, 0, len(targetSet))
	for tg := range targetSet {
		targets = append(targets, tg)
	}
	probed, probeComplete := probeBatch(t.ctx, t.prober, targets)

	out := make(map[string]refreshResult, len(captured))
	verifiedCount := 0
	for id := range captured {
		res := scan[id]
		var verified []verifiedEndpoint
		for _, c := range perSession[id] {
			if scheme, ok := probed[c.target()]; ok {
				verified = append(verified, verifiedEndpoint{port: c.port, family: c.family, host: c.host, scheme: scheme})
			}
		}
		endpoints := canonicalize(verified)
		verifiedCount += len(endpoints)
		// A refresh is only authoritative when BOTH listener discovery and the
		// probe batch completed: a truncated probe batch leaves some targets
		// unverified, which must not be read as "endpoint gone".
		out[id] = refreshResult{endpoints: endpoints, complete: res.Complete && probeComplete}
	}
	t.logger.Debug().
		Int("sessions", len(captured)).
		Int("targets", len(targets)).
		Int("verified_endpoints", verifiedCount).
		Msg("sessionports: refresh complete")
	return out
}

// publishLocked reconciles a refresh into the cache. Results are rejected for any
// session whose identity epoch changed while the refresh was in flight (deleted,
// resurrected, or path-changed), so stale work can never repopulate a changed
// session. A complete scan is authoritative — it replaces the endpoints, removing
// them on empty. An incomplete scan publishes any positives it did find, else
// retains the prior endpoints until they exceed the stale ceiling. The
// freshness/backoff deadline advances based on whether every session was complete.
func (t *Tracker) publishLocked(captured map[string]requestedSession, results map[string]refreshResult, now time.Time) {
	incomplete := false
	for id, capt := range captured {
		res := results[id]
		if !res.complete {
			incomplete = true
		}
		cur, ok := t.requested[id]
		if !ok || cur.epoch != capt.epoch {
			continue
		}
		entry := t.cache[id]
		switch {
		case res.complete:
			entry.endpoints = res.endpoints
			if len(res.endpoints) > 0 {
				entry.lastPositive = now
			}
		case len(res.endpoints) > 0:
			entry.endpoints = res.endpoints
			entry.lastPositive = now
		case entry.staleExpired(now):
			entry.endpoints = nil
		}
		t.cache[id] = entry
	}

	t.hasRun = true
	if len(captured) == 0 || !incomplete {
		t.backoffIdx = 0
		t.nextDue = now.Add(freshnessTTL)
		return
	}
	t.nextDue = now.Add(t.nextBackoffLocked())
}

// nextBackoffLocked returns the current backoff delay and advances the index,
// clamping at the final schedule entry.
func (t *Tracker) nextBackoffLocked() time.Duration {
	d := backoffSchedule[t.backoffIdx]
	if t.backoffIdx < len(backoffSchedule)-1 {
		t.backoffIdx++
	}
	return d
}
