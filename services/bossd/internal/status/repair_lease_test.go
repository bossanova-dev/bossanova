package status

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func fixedClock(t time.Time) (func() time.Time, *time.Time) {
	cur := t
	return func() time.Time { return cur }, &cur
}

func TestRepairLease_AcquireAndRefuse(t *testing.T) {
	base := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	clock, _ := fixedClock(base)
	m := NewRepairLeaseManager().withClock(time.Hour, clock)

	if err := m.Acquire("s1", "plugin"); err != nil {
		t.Fatalf("first acquire: unexpected error %v", err)
	}
	if !m.Active("s1") {
		t.Fatal("session should be active after acquire")
	}
	if got := m.HolderOf("s1"); got != "plugin" {
		t.Fatalf("HolderOf = %q, want plugin", got)
	}

	// A different dispatcher is refused with the clean sentinel.
	if err := m.Acquire("s1", "driver"); !errors.Is(err, ErrRepairLeaseHeld) {
		t.Fatalf("second acquire: got %v, want ErrRepairLeaseHeld", err)
	}
	// The incumbent is untouched.
	if got := m.HolderOf("s1"); got != "plugin" {
		t.Fatalf("HolderOf after refusal = %q, want plugin", got)
	}
}

func TestRepairLease_ReentrantSameHolder(t *testing.T) {
	base := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	clock, cur := fixedClock(base)
	m := NewRepairLeaseManager().withClock(time.Hour, clock)

	if err := m.Acquire("s1", "plugin"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// Same holder re-acquires (refreshing across passes) — never refused, and
	// the refresh pushes the expiry out.
	*cur = base.Add(30 * time.Minute)
	if err := m.Acquire("s1", "plugin"); err != nil {
		t.Fatalf("re-acquire same holder: %v", err)
	}
	// The repairer keeps reporting progress across the round, so the BOS-515
	// stall window never trips and only the TTL is under test here.
	for _, at := range []time.Duration{40, 50, 60, 70, 80} {
		*cur = base.Add(at * time.Minute)
		m.MarkRepairProgress("s1", "", *cur)
	}
	*cur = base.Add(80 * time.Minute) // past the original expiry (base+60m), within the refreshed one (base+90m)
	if !m.Active("s1") {
		t.Fatal("refreshed lease should still be active")
	}
}

func TestRepairLease_TTLReclaim(t *testing.T) {
	base := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	clock, cur := fixedClock(base)
	m := NewRepairLeaseManager().withClock(time.Hour, clock)

	if err := m.Acquire("s1", "plugin"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// Advance past the TTL: the stale lease is reclaimable.
	*cur = base.Add(time.Hour + time.Second)
	if m.Active("s1") {
		t.Fatal("expired lease should not be active")
	}
	if got := m.HolderOf("s1"); got != "" {
		t.Fatalf("HolderOf on expired = %q, want empty", got)
	}
	// A fresh dispatcher takes over the expired lease without refusal.
	if err := m.Acquire("s1", "driver"); err != nil {
		t.Fatalf("takeover of expired lease: %v", err)
	}
	if got := m.HolderOf("s1"); got != "driver" {
		t.Fatalf("HolderOf after takeover = %q, want driver", got)
	}
}

func TestRepairLease_ReleaseByHolder(t *testing.T) {
	m := NewRepairLeaseManager()
	if err := m.Acquire("s1", "plugin"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// A losing dispatcher's release must not evict the winner.
	m.Release("s1", "someone-else")
	if !m.Active("s1") {
		t.Fatal("release by non-holder must be a no-op")
	}
	// The real holder releases; the session is free again.
	m.Release("s1", "plugin")
	if m.Active("s1") {
		t.Fatal("session should be inactive after holder release")
	}
	if err := m.Acquire("s1", "driver"); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
}

func TestRepairLease_PerSessionIsolation(t *testing.T) {
	m := NewRepairLeaseManager()
	if err := m.Acquire("s1", "plugin"); err != nil {
		t.Fatalf("acquire s1: %v", err)
	}
	// A different session is independently claimable.
	if err := m.Acquire("s2", "driver"); err != nil {
		t.Fatalf("acquire s2 must not be blocked by s1: %v", err)
	}
	if !m.Active("s1") || !m.Active("s2") {
		t.Fatal("both sessions should hold independent leases")
	}
}

func TestRepairLease_EmptyHolderNoEnforcement(t *testing.T) {
	m := NewRepairLeaseManager()
	// A legacy caller with no dispatcher id neither takes nor refuses a lease.
	if err := m.Acquire("s1", ""); err != nil {
		t.Fatalf("empty-holder acquire: %v", err)
	}
	if m.Active("s1") {
		t.Fatal("empty-holder acquire must not take a lease")
	}
	// And it cannot displace a real holder.
	if err := m.Acquire("s1", "plugin"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := m.Acquire("s1", ""); err != nil {
		t.Fatalf("empty-holder acquire while held: %v", err)
	}
	if got := m.HolderOf("s1"); got != "plugin" {
		t.Fatalf("HolderOf = %q, want plugin", got)
	}
}

// TestRepairLease_StallRule drives the whole BOS-515 stall lifecycle off the
// injected clock (no sleeps): what counts as progress, what trips the window,
// and what clears the record.
func TestRepairLease_StallRule(t *testing.T) {
	base := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		// ttl overrides the lease TTL; must stay well above RepairStallWindow
		// unless the case is specifically about TTL-vs-stall ordering.
		ttl time.Duration
		// steps advance the clock and feed the heartbeat, in order.
		steps []struct {
			after        time.Duration
			headSHA      string
			lastOutputAt time.Time
		}
		// checkAt is when the assertions below are evaluated.
		checkAt     time.Duration
		wantActive  bool
		wantStalled bool
	}{
		{
			// A chat that keeps talking is never stall-released, even over 3x
			// the window: every pass advances lastOutputAt, which is progress.
			name: "output_advancing_never_stalls",
			ttl:  2 * time.Hour,
			steps: []struct {
				after        time.Duration
				headSHA      string
				lastOutputAt time.Time
			}{
				{after: 4 * time.Minute, lastOutputAt: base.Add(4 * time.Minute)},
				{after: 8 * time.Minute, lastOutputAt: base.Add(8 * time.Minute)},
				{after: 12 * time.Minute, lastOutputAt: base.Add(12 * time.Minute)},
				{after: 16 * time.Minute, lastOutputAt: base.Add(16 * time.Minute)},
				{after: 20 * time.Minute, lastOutputAt: base.Add(20 * time.Minute)},
				{after: 24 * time.Minute, lastOutputAt: base.Add(24 * time.Minute)},
			},
			checkAt:    24 * time.Minute,
			wantActive: true,
		},
		{
			// Same evidence re-reported forever is NOT progress: neither the
			// head SHA nor the output timestamp advanced.
			name: "repeated_identical_evidence_stalls",
			ttl:  2 * time.Hour,
			steps: []struct {
				after        time.Duration
				headSHA      string
				lastOutputAt time.Time
			}{
				{after: 2 * time.Minute, headSHA: "sha-1", lastOutputAt: base},
				{after: 5 * time.Minute, headSHA: "sha-1", lastOutputAt: base},
				{after: 9 * time.Minute, headSHA: "sha-1", lastOutputAt: base},
			},
			// The last real progress was the sha-1 advance at +2m, so the
			// window trips just after +10m.
			checkAt:     10*time.Minute + time.Second,
			wantActive:  false,
			wantStalled: true,
		},
		{
			name:        "no_evidence_at_all_stalls_past_window",
			ttl:         2 * time.Hour,
			checkAt:     RepairStallWindow + time.Second,
			wantActive:  false,
			wantStalled: true,
		},
		{
			name:       "no_evidence_still_active_inside_window",
			ttl:        2 * time.Hour,
			checkAt:    RepairStallWindow - time.Second,
			wantActive: true,
		},
		{
			// A head-SHA advance alone (a silent agent that keeps committing)
			// restarts the clock.
			name: "sha_advance_alone_resets_clock",
			ttl:  2 * time.Hour,
			steps: []struct {
				after        time.Duration
				headSHA      string
				lastOutputAt time.Time
			}{
				{after: 7 * time.Minute, headSHA: "sha-1"},
				{after: 14 * time.Minute, headSHA: "sha-2"},
			},
			checkAt:    21 * time.Minute, // 7m after the last advance
			wantActive: true,
		},
		{
			// …but only until the window elapses past THAT advance.
			name: "sha_advance_then_silence_stalls",
			ttl:  2 * time.Hour,
			steps: []struct {
				after        time.Duration
				headSHA      string
				lastOutputAt time.Time
			}{
				{after: 7 * time.Minute, headSHA: "sha-1"},
			},
			checkAt:     15*time.Minute + time.Second,
			wantActive:  false,
			wantStalled: true,
		},
		{
			// TTL expiry wins and is NOT a stall: a crashed holder must not be
			// reported as a stalled repair round.
			name:        "ttl_expiry_wins_and_records_no_stall",
			ttl:         5 * time.Minute,
			checkAt:     5*time.Minute + time.Second,
			wantActive:  false,
			wantStalled: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock, cur := fixedClock(base)
			m := NewRepairLeaseManager().withClock(tc.ttl, clock)
			if err := m.Acquire("s1", "plugin"); err != nil {
				t.Fatalf("acquire: %v", err)
			}
			for _, step := range tc.steps {
				*cur = base.Add(step.after)
				m.MarkRepairProgress("s1", step.headSHA, step.lastOutputAt)
			}
			*cur = base.Add(tc.checkAt)

			// StalledAt must run the same lazy evaluation as Active, so read it
			// FIRST: a caller that never touches Active still sees the stall.
			at, stalled := m.StalledAt("s1")
			if stalled != tc.wantStalled {
				t.Fatalf("StalledAt ok = %v, want %v", stalled, tc.wantStalled)
			}
			if got := m.Active("s1"); got != tc.wantActive {
				t.Fatalf("Active = %v, want %v", got, tc.wantActive)
			}
			if !tc.wantActive {
				if got := m.HolderOf("s1"); got != "" {
					t.Fatalf("HolderOf on released lease = %q, want empty", got)
				}
			}
			if !tc.wantStalled {
				if reason := m.StallReason("s1"); reason != "" {
					t.Fatalf("StallReason = %q, want empty", reason)
				}
				return
			}
			if want := base.Add(tc.checkAt); !at.Equal(want) {
				t.Fatalf("StalledAt = %v, want %v", at, want)
			}
			reason := m.StallReason("s1")
			if !strings.Contains(reason, "repair stalled") || !strings.Contains(reason, stallWindowLabel()) {
				t.Fatalf("StallReason = %q, want it to name the stall and the %s window", reason, stallWindowLabel())
			}
		})
	}
}

func TestRepairLease_StallWindowLabel(t *testing.T) {
	// The reason text is derived from the constant so the two cannot drift.
	if got, want := stallWindowLabel(), "8m"; got != want {
		t.Fatalf("stallWindowLabel = %q, want %q", got, want)
	}
	if RepairStallWindow >= DefaultRepairLeaseTTL {
		t.Fatalf("RepairStallWindow (%s) must stay well under the crash TTL (%s)", RepairStallWindow, DefaultRepairLeaseTTL)
	}
}

func TestRepairLease_NewAcquireClearsStall(t *testing.T) {
	base := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	clock, cur := fixedClock(base)
	m := NewRepairLeaseManager().withClock(2*time.Hour, clock)

	if err := m.Acquire("s1", "plugin"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	*cur = base.Add(RepairStallWindow + time.Second)
	if m.Active("s1") {
		t.Fatal("lease should be stall-released")
	}
	if _, ok := m.StalledAt("s1"); !ok {
		t.Fatal("expected a stall record")
	}

	// A new round clears the record and restarts the stall clock.
	if err := m.Acquire("s1", "driver"); err != nil {
		t.Fatalf("re-acquire after stall: %v", err)
	}
	if _, ok := m.StalledAt("s1"); ok {
		t.Fatal("a new round must clear the stall record")
	}
	if reason := m.StallReason("s1"); reason != "" {
		t.Fatalf("StallReason after new round = %q, want empty", reason)
	}
	*cur = base.Add(RepairStallWindow + time.Second + RepairStallWindow - time.Second)
	if !m.Active("s1") {
		t.Fatal("the fresh round's stall clock should have restarted at acquire")
	}
}

func TestRepairLease_ProgressClearsStallRecordOnNextRound(t *testing.T) {
	base := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	clock, cur := fixedClock(base)
	m := NewRepairLeaseManager().withClock(2*time.Hour, clock)

	if err := m.Acquire("s1", "plugin"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// A re-entrant refresh preserves the observed evidence, so re-reporting the
	// SAME sha after the refresh is not progress...
	m.MarkRepairProgress("s1", "sha-1", time.Time{})
	*cur = base.Add(time.Minute)
	if err := m.Acquire("s1", "plugin"); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	*cur = base.Add(2 * time.Minute)
	m.MarkRepairProgress("s1", "sha-1", time.Time{})
	*cur = base.Add(time.Minute + RepairStallWindow + time.Second)
	if m.Active("s1") {
		t.Fatal("a refresh + repeated sha must not hold the lease past the window")
	}

	// ...and once released, progress on a dead lease is a no-op (the stall
	// record survives until a new Acquire).
	m.MarkRepairProgress("s1", "sha-2", *cur)
	if _, ok := m.StalledAt("s1"); !ok {
		t.Fatal("progress on a released lease must not clear the stall record")
	}
}

func TestRepairLease_StallNilAndEmptySafety(t *testing.T) {
	var m *RepairLeaseManager
	m.MarkRepairProgress("s1", "sha", time.Now())
	if _, ok := m.StalledAt("s1"); ok {
		t.Fatal("nil manager must report no stall")
	}
	if got := m.StallReason("s1"); got != "" {
		t.Fatalf("nil manager StallReason = %q, want empty", got)
	}
	m.HydrateSession(&pb.Session{}, "s1", "sha", time.Now())

	live := NewRepairLeaseManager()
	live.MarkRepairProgress("", "sha", time.Now())
	if _, ok := live.StalledAt(""); ok {
		t.Fatal("empty sessionID must report no stall")
	}
	if got := live.StallReason(""); got != "" {
		t.Fatalf("empty sessionID StallReason = %q, want empty", got)
	}
	live.HydrateSession(nil, "s1", "sha", time.Now())
	// A session with no lease at all: heartbeat is a no-op, nothing is stalled.
	live.MarkRepairProgress("never-repaired", "sha", time.Now())
	if live.Active("never-repaired") {
		t.Fatal("MarkRepairProgress must not create a lease")
	}
}

// TestRepairLease_HydrateSession covers the read-side contract both hydration
// layers (internal/server, internal/plugin) delegate to.
func TestRepairLease_HydrateSession(t *testing.T) {
	base := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	clock, cur := fixedClock(base)
	m := NewRepairLeaseManager().withClock(2*time.Hour, clock)
	if err := m.Acquire("s1", "plugin"); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// Healthy round: chat output keeps advancing, so repair_stalled_at stays
	// absent and the persisted blocked reason is left alone.
	for i := 1; i <= 5; i++ {
		*cur = base.Add(time.Duration(i) * 4 * time.Minute)
		p := &pb.Session{LastRepairBlockedReason: "persisted reason"}
		m.HydrateSession(p, "s1", "sha-1", *cur)
		if !p.GetRepairActive() {
			t.Fatalf("pass %d: repair_active should stay true while the chat talks", i)
		}
		if p.GetRepairStalledAt() != nil {
			t.Fatalf("pass %d: repair_stalled_at must be absent for a progressing round", i)
		}
		if got := p.GetLastRepairBlockedReason(); got != "persisted reason" {
			t.Fatalf("pass %d: blocked reason overwritten to %q with no stall", i, got)
		}
	}

	// The chat goes quiet: the same evidence is re-reported every pass until
	// the window elapses, then the lease is stall-released.
	frozenOutput := *cur
	stallAt := cur.Add(RepairStallWindow + time.Second)
	*cur = stallAt
	p := &pb.Session{LastRepairBlockedReason: "persisted reason"}
	m.HydrateSession(p, "s1", "sha-1", frozenOutput)
	if p.GetRepairActive() {
		t.Fatal("repair_active must drop once the lease is stall-released")
	}
	if p.GetRepairStalledAt() == nil {
		t.Fatal("repair_stalled_at must be populated once stalled")
	}
	if got := p.GetRepairStalledAt().AsTime(); !got.Equal(stallAt) {
		t.Fatalf("repair_stalled_at = %v, want %v", got, stallAt)
	}
	if got := p.GetLastRepairBlockedReason(); !strings.Contains(got, "repair stalled") {
		t.Fatalf("blocked reason = %q, want the stall reason to win while the record is live", got)
	}
}

func TestRepairLease_ConcurrentAcquireSingleWinner(t *testing.T) {
	m := NewRepairLeaseManager()
	const n = 32
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	wg.Add(n)
	for i := range n {
		holder := "holder-" + time.Duration(i).String()
		go func(h string) {
			defer wg.Done()
			if err := m.Acquire("s1", h); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}(holder)
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("expected exactly one winner, got %d", wins)
	}
}
