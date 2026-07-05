package status

import (
	"errors"
	"sync"
	"testing"
	"time"
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
