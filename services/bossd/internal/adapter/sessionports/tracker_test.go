package sessionports

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// --- tracker test doubles ---------------------------------------------------

// fakeClock is a manually advanced clock.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1_700_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fakeDetector serves a per-session result and can block its first Scan so tests
// can interleave state changes with an in-flight refresh.
type fakeDetector struct {
	mu      sync.Mutex
	results map[string]SessionResult
	calls   int

	entered chan struct{} // signalled when the first Scan enters (if set)
	release chan struct{} // first Scan blocks until closed (if set)
}

func (d *fakeDetector) Scan(ctx context.Context, roots map[string][]Root) map[string]SessionResult {
	d.mu.Lock()
	d.calls++
	first := d.calls == 1
	d.mu.Unlock()

	if first && d.entered != nil {
		d.entered <- struct{}{}
		select {
		case <-d.release:
		case <-ctx.Done():
		}
	}

	out := make(map[string]SessionResult, len(roots))
	for id := range roots {
		if r, ok := d.results[id]; ok {
			out[id] = r
		} else {
			out[id] = SessionResult{Complete: true}
		}
	}
	return out
}

func (d *fakeDetector) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

// staticRoots returns a single fixed root per session (contents are irrelevant;
// the fake detector keys off session ID).
type staticRoots struct{}

func (staticRoots) Roots(_ context.Context, snap SessionSnapshot) []Root {
	return []Root{{PID: 1, Identity: Identity{StartTime: 1, Present: true}}}
}

// fakeProber returns a fixed scheme for configured targets.
type fakeProber struct {
	schemes map[string]string
}

func (p *fakeProber) Probe(_ context.Context, target string) (string, bool) {
	s, ok := p.schemes[target]
	return s, ok
}

// trackerHarness bundles a tracker with its fakes and a refresh signal.
type trackerHarness struct {
	tr        *Tracker
	clock     *fakeClock
	det       *fakeDetector
	refreshed chan struct{}
}

func TestCacheEntryStaleExpiredBoundaries(t *testing.T) {
	now := time.Unix(100, 0)
	tests := []struct {
		name  string
		entry cacheEntry
		want  bool
	}{
		{
			name:  "empty entry never expires",
			entry: cacheEntry{lastPositive: now.Add(-staleRetention)},
		},
		{
			name: "non-empty entry before retention ceiling",
			entry: cacheEntry{
				endpoints:    []Endpoint{{Port: 8080}},
				lastPositive: now.Add(-staleRetention + time.Nanosecond),
			},
		},
		{
			name: "non-empty entry at retention ceiling",
			entry: cacheEntry{
				endpoints:    []Endpoint{{Port: 8080}},
				lastPositive: now.Add(-staleRetention),
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.entry.staleExpired(now); got != tt.want {
				t.Fatalf("staleExpired() = %v, want %v", got, tt.want)
			}
		})
	}
}

func newHarness(t *testing.T, det *fakeDetector, prober prober) *trackerHarness {
	t.Helper()
	clk := newFakeClock()
	refreshed := make(chan struct{}, 32)
	tr := newTracker(context.Background(),
		withDetector(det),
		withRootProvider(staticRoots{}),
		withProber(prober),
		withClock(clk),
		WithLogger(zerolog.Nop()),
		withRefreshHook(func() { refreshed <- struct{}{} }),
	)
	t.Cleanup(tr.Close)
	return &trackerHarness{tr: tr, clock: clk, det: det, refreshed: refreshed}
}

// waitRefresh blocks until one refresh cycle finishes.
func (h *trackerHarness) waitRefresh(t *testing.T) {
	t.Helper()
	select {
	case <-h.refreshed:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for refresh")
	}
}

const wtA = "/wt/a"

func snapA() SessionSnapshot {
	return SessionSnapshot{ID: "a", WorktreePath: wtA, TmuxNames: []string{"sess-a"}}
}

// --- tests ------------------------------------------------------------------

func TestTrackerEmptyCacheReturnsImmediatelyAndSchedules(t *testing.T) {
	det := &fakeDetector{results: map[string]SessionResult{
		"a": {Complete: true, Listeners: []Listener{{PID: 1, Address: "127.0.0.1", Port: 8080, Family: FamilyIPv4}}},
	}}
	h := newHarness(t, det, &fakeProber{schemes: map[string]string{"127.0.0.1:8080": "http"}})

	got := h.tr.GetBatch([]SessionSnapshot{snapA()})
	if len(got["a"]) != 0 {
		t.Fatalf("expected empty initial cache, got %+v", got["a"])
	}
	h.waitRefresh(t)
	if h.det.callCount() != 1 {
		t.Fatalf("expected exactly one scan scheduled, got %d", h.det.callCount())
	}
}

func TestTrackerEmptyBatchDoesNotScheduleRefresh(t *testing.T) {
	tr := &Tracker{
		clock:     newFakeClock(),
		wake:      make(chan struct{}, 1),
		requested: make(map[string]requestedSession),
		cache:     make(map[string]cacheEntry),
	}

	if got := tr.GetBatch(nil); len(got) != 0 {
		t.Fatalf("GetBatch(nil) = %v, want empty result", got)
	}
	if got := len(tr.wake); got != 0 {
		t.Fatalf("GetBatch(nil) queued %d refreshes, want none", got)
	}
}

func TestTrackerRefreshOnceSkipsComputeForEmptySet(t *testing.T) {
	det := &fakeDetector{}
	h := newHarness(t, det, &fakeProber{})

	h.tr.refreshOnce()

	if got := det.callCount(); got != 0 {
		t.Fatalf("empty refresh called detector %d times, want none", got)
	}
}

func TestTrackerCacheHitDoesNotBlockOnScan(t *testing.T) {
	det := &fakeDetector{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
		results: map[string]SessionResult{"a": {Complete: true}},
	}
	h := newHarness(t, det, &fakeProber{})

	// First read schedules a refresh whose scan blocks. The read must still
	// return immediately (this call returning at all proves non-blocking).
	_ = h.tr.GetBatch([]SessionSnapshot{snapA()})
	<-det.entered // worker is now blocked inside Scan

	done := make(chan []Endpoint, 1)
	go func() { done <- h.tr.GetBatch([]SessionSnapshot{snapA()})["a"] }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cache read blocked while a scan was in flight")
	}
	close(det.release)
	h.waitRefresh(t)
}

func TestTrackerPublishesVerifiedEndpoint(t *testing.T) {
	det := &fakeDetector{results: map[string]SessionResult{
		"a": {Complete: true, Listeners: []Listener{{PID: 1, Address: "*", Port: 8080, Family: FamilyIPv4}}},
	}}
	h := newHarness(t, det, &fakeProber{schemes: map[string]string{"127.0.0.1:8080": "http"}})

	h.tr.GetBatch([]SessionSnapshot{snapA()})
	h.waitRefresh(t)

	got := h.tr.GetBatch([]SessionSnapshot{snapA()})["a"]
	want := []Endpoint{{Port: 8080, URL: "http://127.0.0.1:8080"}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("endpoints = %+v, want %+v", got, want)
	}
}

func TestTrackerConcurrentCallersCollapseToOneRefresh(t *testing.T) {
	det := &fakeDetector{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
		results: map[string]SessionResult{"a": {Complete: true}},
	}
	h := newHarness(t, det, &fakeProber{})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.tr.GetBatch([]SessionSnapshot{snapA()})
		}()
	}
	wg.Wait()
	<-det.entered // exactly one worker is inside Scan
	close(det.release)
	h.waitRefresh(t)

	// Drain any coalesced extra wake; it must not launch a second scan because
	// the clock has not advanced past the freshness deadline.
	time.Sleep(50 * time.Millisecond)
	if c := h.det.callCount(); c != 1 {
		t.Fatalf("expected exactly one refresh, got %d", c)
	}
}

func TestTrackerCompleteNegativeRemovesEndpoints(t *testing.T) {
	det := &fakeDetector{results: map[string]SessionResult{
		"a": {Complete: true, Listeners: []Listener{{PID: 1, Address: "127.0.0.1", Port: 8080, Family: FamilyIPv4}}},
	}}
	h := newHarness(t, det, &fakeProber{schemes: map[string]string{"127.0.0.1:8080": "http"}})

	h.tr.GetBatch([]SessionSnapshot{snapA()})
	h.waitRefresh(t)
	if got := h.tr.GetBatch([]SessionSnapshot{snapA()})["a"]; len(got) != 1 {
		t.Fatalf("expected one endpoint after first refresh, got %+v", got)
	}
	h.tr.mu.Lock()
	lastPositive := h.tr.cache["a"].lastPositive
	h.tr.mu.Unlock()

	// The port is gone now: a complete scan with no listeners removes it.
	det.mu.Lock()
	det.results["a"] = SessionResult{Complete: true}
	det.mu.Unlock()
	h.clock.Advance(freshnessTTL + time.Second)
	h.tr.GetBatch([]SessionSnapshot{snapA()})
	h.waitRefresh(t)

	if got := h.tr.GetBatch([]SessionSnapshot{snapA()})["a"]; len(got) != 0 {
		t.Fatalf("expected endpoints removed by complete-negative scan, got %+v", got)
	}
	h.tr.mu.Lock()
	gotLastPositive := h.tr.cache["a"].lastPositive
	h.tr.mu.Unlock()
	if !gotLastPositive.Equal(lastPositive) {
		t.Fatalf("complete-negative scan changed last-positive time from %v to %v", lastPositive, gotLastPositive)
	}
}

func TestTrackerIncompleteRetainsThenDropsStale(t *testing.T) {
	det := &fakeDetector{results: map[string]SessionResult{
		"a": {Complete: true, Listeners: []Listener{{PID: 1, Address: "127.0.0.1", Port: 8080, Family: FamilyIPv4}}},
	}}
	h := newHarness(t, det, &fakeProber{schemes: map[string]string{"127.0.0.1:8080": "http"}})

	h.tr.GetBatch([]SessionSnapshot{snapA()})
	h.waitRefresh(t)

	// Now discovery goes incomplete with no listeners: retain within 15s.
	det.mu.Lock()
	det.results["a"] = SessionResult{Complete: false}
	det.mu.Unlock()
	h.clock.Advance(5 * time.Second)
	h.tr.GetBatch([]SessionSnapshot{snapA()})
	h.waitRefresh(t)
	if got := h.tr.GetBatch([]SessionSnapshot{snapA()})["a"]; len(got) != 1 {
		t.Fatalf("expected stale endpoint retained within 15s, got %+v", got)
	}

	// Past the 15s ceiling (measured from last positive): drop it.
	h.clock.Advance(20 * time.Second)
	h.tr.GetBatch([]SessionSnapshot{snapA()})
	h.waitRefresh(t)
	if got := h.tr.GetBatch([]SessionSnapshot{snapA()})["a"]; len(got) != 0 {
		t.Fatalf("expected stale endpoint dropped after 15s, got %+v", got)
	}
}

func TestTrackerBackoffAdvancesOnRepeatedIncomplete(t *testing.T) {
	det := &fakeDetector{results: map[string]SessionResult{"a": {Complete: false}}}
	h := newHarness(t, det, &fakeProber{})

	want := []time.Duration{2, 4, 8, 16, 30, 30}
	for i, w := range want {
		h.tr.GetBatch([]SessionSnapshot{snapA()})
		h.waitRefresh(t)

		h.tr.mu.Lock()
		got := h.tr.nextDue.Sub(h.clock.Now())
		h.tr.mu.Unlock()
		if got != w*time.Second {
			t.Fatalf("refresh %d: backoff = %v, want %v", i, got, w*time.Second)
		}
		// Advance just past this deadline so the next read is due.
		h.clock.Advance(w*time.Second + time.Millisecond)
	}
}

func TestTrackerReadSuppressesEndpointsPastStaleCeiling(t *testing.T) {
	det := &fakeDetector{results: map[string]SessionResult{
		"a": {Complete: true, Listeners: []Listener{{PID: 1, Address: "127.0.0.1", Port: 8080, Family: FamilyIPv4}}},
	}}
	h := newHarness(t, det, &fakeProber{schemes: map[string]string{"127.0.0.1:8080": "http"}})

	// Publish a verified endpoint at T0 (lastPositive = T0).
	h.tr.GetBatch([]SessionSnapshot{snapA()})
	h.waitRefresh(t)
	if got := h.tr.GetBatch([]SessionSnapshot{snapA()})["a"]; len(got) != 1 {
		t.Fatalf("expected endpoint published, got %+v", got)
	}

	// Discovery goes incomplete. Drive incomplete refreshes so backoff pushes the
	// next refresh deadline (nextDue) out to T17 — past the 15s stale ceiling —
	// while the endpoint is still retained by publishLocked (age < 15s each time).
	det.mu.Lock()
	det.results["a"] = SessionResult{Complete: false}
	det.mu.Unlock()
	for _, adv := range []time.Duration{3 * time.Second, 2 * time.Second, 4 * time.Second} {
		h.clock.Advance(adv)
		h.tr.GetBatch([]SessionSnapshot{snapA()})
		h.waitRefresh(t)
	}
	// At T9 the endpoint is still legitimately retained (age 9 < 15).
	if got := h.tr.GetBatch([]SessionSnapshot{snapA()})["a"]; len(got) != 1 {
		t.Fatalf("expected stale endpoint retained at T9, got %+v", got)
	}

	// Advance to T16: past the 15s ceiling but before nextDue (T17), so no refresh
	// runs to prune it. The read path must still suppress it — retention may not
	// exceed the ceiling just because backoff deferred the pruning refresh.
	h.clock.Advance(7 * time.Second)
	if got := h.tr.GetBatch([]SessionSnapshot{snapA()})["a"]; len(got) != 0 {
		t.Fatalf("expected stale endpoint suppressed on read past 15s ceiling, got %+v", got)
	}
}

func TestTrackerClonesCallerTmuxNames(t *testing.T) {
	det := &fakeDetector{results: map[string]SessionResult{"a": {Complete: true}}}
	h := newHarness(t, det, &fakeProber{})

	names := []string{"sess-a"}
	h.tr.GetBatch([]SessionSnapshot{{ID: "a", WorktreePath: wtA, TmuxNames: names}})
	// Mutating the caller-owned slice after the call returns must not affect the
	// snapshot the tracker retained for async discovery.
	names[0] = "mutated"

	h.tr.mu.Lock()
	stored := h.tr.requested["a"].snap.TmuxNames[0]
	h.tr.mu.Unlock()
	if stored != "sess-a" {
		t.Fatalf("tracker retained caller slice by reference: stored=%q, want sess-a", stored)
	}
}

func TestTrackerIdentityEpochAdvancesOnWorktreeChange(t *testing.T) {
	tr := &Tracker{
		requested: make(map[string]requestedSession),
		cache:     make(map[string]cacheEntry),
	}

	tr.upsertLocked(snapA())
	firstEpoch := tr.requested["a"].epoch

	sameIdentity := snapA()
	sameIdentity.TmuxNames = []string{"renamed"}
	tr.upsertLocked(sameIdentity)
	if got := tr.requested["a"].epoch; got != firstEpoch {
		t.Fatalf("same worktree changed identity epoch from %d to %d", firstEpoch, got)
	}

	moved := snapA()
	moved.WorktreePath = "/wt/a-moved"
	tr.upsertLocked(moved)
	if got := tr.requested["a"].epoch; got <= firstEpoch {
		t.Fatalf("changed worktree identity epoch = %d, want greater than %d", got, firstEpoch)
	}
}

func TestTrackerStaleGenerationCannotRepopulateChangedSession(t *testing.T) {
	det := &fakeDetector{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
		results: map[string]SessionResult{
			"a": {Complete: true, Listeners: []Listener{{PID: 1, Address: "127.0.0.1", Port: 8080, Family: FamilyIPv4}}},
		},
	}
	h := newHarness(t, det, &fakeProber{schemes: map[string]string{"127.0.0.1:8080": "http"}})

	// First refresh captures session "a" at worktree /wt/a, then blocks in Scan.
	h.tr.GetBatch([]SessionSnapshot{snapA()})
	<-det.entered

	// While the refresh is in flight, "a" changes its worktree path (a new
	// identity epoch). The stale in-flight result must not publish to it.
	h.tr.GetBatch([]SessionSnapshot{{ID: "a", WorktreePath: "/wt/a-moved", TmuxNames: []string{"sess-a"}}})
	close(det.release)
	h.waitRefresh(t)

	got := h.tr.GetBatch([]SessionSnapshot{{ID: "a", WorktreePath: "/wt/a-moved", TmuxNames: []string{"sess-a"}}})["a"]
	if len(got) != 0 {
		t.Fatalf("stale generation repopulated a changed session: %+v", got)
	}
}

func TestTrackerPrunesUnrequestedSessions(t *testing.T) {
	det := &fakeDetector{results: map[string]SessionResult{
		"a": {Complete: true, Listeners: []Listener{{PID: 1, Address: "127.0.0.1", Port: 8080, Family: FamilyIPv4}}},
	}}
	h := newHarness(t, det, &fakeProber{schemes: map[string]string{"127.0.0.1:8080": "http"}})

	h.tr.GetBatch([]SessionSnapshot{snapA()})
	h.waitRefresh(t)

	// A batch that no longer includes "a" prunes its cache entry.
	h.tr.GetBatch([]SessionSnapshot{{ID: "b", WorktreePath: "/wt/b", TmuxNames: []string{"sess-b"}}})
	h.tr.mu.Lock()
	_, stillCached := h.tr.cache["a"]
	_, stillRequested := h.tr.requested["a"]
	h.tr.mu.Unlock()
	if stillCached || stillRequested {
		t.Fatalf("expected session a pruned, cached=%v requested=%v", stillCached, stillRequested)
	}
}

func TestTrackerCloseIsIdempotent(t *testing.T) {
	det := &fakeDetector{results: map[string]SessionResult{"a": {Complete: true}}}
	h := newHarness(t, det, &fakeProber{})
	h.tr.GetBatch([]SessionSnapshot{snapA()})
	h.waitRefresh(t)
	h.tr.Close()
	h.tr.Close() // second call must not panic or block (t.Cleanup calls a third)
}
