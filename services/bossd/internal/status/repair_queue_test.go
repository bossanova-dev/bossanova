package status

import (
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestRepairQueueTTLForSweepInterval(t *testing.T) {
	cases := []struct {
		name  string
		sweep time.Duration
		want  time.Duration
	}{
		{name: "zero_uses_floor", sweep: 0, want: 5 * time.Minute},
		{name: "negative_uses_floor", sweep: -time.Minute, want: 5 * time.Minute},
		{name: "one_minute_uses_floor", sweep: time.Minute, want: 5 * time.Minute},
		{name: "two_minutes_uses_3x", sweep: 2 * time.Minute, want: 6 * time.Minute},
		{name: "five_minutes_uses_3x", sweep: 5 * time.Minute, want: 15 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RepairQueueTTLForSweepInterval(tc.sweep); got != tc.want {
				t.Fatalf("RepairQueueTTLForSweepInterval(%v) = %v, want %v", tc.sweep, got, tc.want)
			}
		})
	}
}

func TestRepairQueueMarkAndRefreshPreservesFirstQueuedAt(t *testing.T) {
	base := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	clock, cur := fixedClock(base)
	m := NewRepairLeaseManager().withClock(0, clock)

	m.MarkQueued("s1")
	at, ok := m.QueuedSince("s1")
	if !ok || !at.Equal(base) {
		t.Fatalf("QueuedSince after initial mark = (%v, %v), want (%v, true)", at, ok, base)
	}

	*cur = base.Add(3 * time.Minute)
	m.MarkQueued("s1")

	*cur = base.Add(7 * time.Minute)
	at, ok = m.QueuedSince("s1")
	if !ok || !at.Equal(base) {
		t.Fatalf("QueuedSince after refresh at +7m = (%v, %v), want (%v, true)", at, ok, base)
	}
}

func TestRepairQueueLapsesAfterTTLWithoutRefresh(t *testing.T) {
	base := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	clock, cur := fixedClock(base)
	m := NewRepairLeaseManager().withClock(0, clock)

	m.MarkQueued("s1")

	*cur = base.Add(MinRepairQueueTTL - time.Second)
	if at, ok := m.QueuedSince("s1"); !ok || !at.Equal(base) {
		t.Fatalf("QueuedSince just before TTL = (%v, %v), want (%v, true)", at, ok, base)
	}

	*cur = base.Add(MinRepairQueueTTL)
	if at, ok := m.QueuedSince("s1"); ok || !at.IsZero() {
		t.Fatalf("QueuedSince at TTL expiry = (%v, %v), want (zero, false)", at, ok)
	}

	m.MarkQueued("s2")
	*cur = base.Add(MinRepairQueueTTL + 10*time.Minute)
	freshAt := *cur
	m.MarkQueued("s2")
	if at, ok := m.QueuedSince("s2"); !ok || !at.Equal(freshAt) {
		t.Fatalf("QueuedSince after re-marking expired entry = (%v, %v), want (%v, true)", at, ok, freshAt)
	}
}

func TestRepairQueueClearedByAcquireAndRelease(t *testing.T) {
	base := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	clock, _ := fixedClock(base)
	m := NewRepairLeaseManager().withClock(time.Hour, clock)

	m.MarkQueued("s1")
	m.MarkQueued("s2")
	m.MarkQueued("s3")

	if err := m.Acquire("s1", "plugin-A"); err != nil {
		t.Fatalf("Acquire s1: %v", err)
	}
	if _, ok := m.QueuedSince("s1"); ok {
		t.Fatal("Acquire must clear queued mark for s1")
	}

	m.Release("s2", "plugin-A")
	if _, ok := m.QueuedSince("s2"); ok {
		t.Fatal("Release must clear queued mark for s2")
	}

	if at, ok := m.QueuedSince("s3"); !ok || !at.Equal(base) {
		t.Fatalf("unrelated session s3 = (%v, %v), want (%v, true)", at, ok, base)
	}
}

func TestRepairQueueDynamicTTLFn(t *testing.T) {
	base := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	clock, cur := fixedClock(base)
	ttl := 5 * time.Minute
	m := NewRepairLeaseManager().withClock(0, clock).SetQueueTTLFn(func() time.Duration { return ttl })

	m.MarkQueued("s1")

	ttl = 9 * time.Minute
	*cur = base.Add(6 * time.Minute)
	if at, ok := m.QueuedSince("s1"); !ok || !at.Equal(base) {
		t.Fatalf("QueuedSince with raised 9m TTL at +6m = (%v, %v), want (%v, true)", at, ok, base)
	}

	ttl = 5 * time.Minute
	if _, ok := m.QueuedSince("s1"); ok {
		t.Fatal("QueuedSince should lapse once live TTL drops below elapsed silence")
	}

	ttl = 0
	*cur = base.Add(20 * time.Minute)
	m.MarkQueued("s2")
	*cur = base.Add(24 * time.Minute)
	if _, ok := m.QueuedSince("s2"); !ok {
		t.Fatal("non-positive TTL func must fall back to MinRepairQueueTTL")
	}
}

func TestRepairQueueHydrateSession(t *testing.T) {
	base := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	clock, cur := fixedClock(base)
	m := NewRepairLeaseManager().withClock(time.Hour, clock)

	m.MarkQueued("s1")
	p := &pb.Session{}
	m.HydrateSession(p, "s1", "", time.Time{})
	if p.GetRepairActive() {
		t.Fatal("queued unleased session must have repair_active=false")
	}
	if p.GetRepairQueuedAt() == nil || !p.GetRepairQueuedAt().AsTime().Equal(base) {
		t.Fatalf("repair_queued_at = %v, want %v", p.GetRepairQueuedAt(), base)
	}

	if err := m.Acquire("s1", "plugin-A"); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	m.MarkQueued("s1")
	pLeased := &pb.Session{}
	m.HydrateSession(pLeased, "s1", "", time.Time{})
	if !pLeased.GetRepairActive() {
		t.Fatal("leased session must have repair_active=true")
	}
	if pLeased.GetRepairQueuedAt() != nil {
		t.Fatalf("leased session must not hydrate repair_queued_at, got %v", pLeased.GetRepairQueuedAt())
	}

	m.Release("s1", "plugin-A")
	m.MarkQueued("s2")
	*cur = base.Add(MinRepairQueueTTL)
	pExpired := &pb.Session{}
	m.HydrateSession(pExpired, "s2", "", time.Time{})
	if pExpired.GetRepairQueuedAt() != nil {
		t.Fatalf("expired queue mark must not hydrate repair_queued_at, got %v", pExpired.GetRepairQueuedAt())
	}
}

func TestRepairQueueNilAndEmptySafety(t *testing.T) {
	var nilMgr *RepairLeaseManager
	nilMgr.SetQueueTTLFn(func() time.Duration { return time.Minute })
	nilMgr.SetOnQueueChange(func(string) {})
	nilMgr.MarkQueued("s1")
	if _, ok := nilMgr.QueuedSince("s1"); ok {
		t.Fatal("nil manager must report not queued")
	}

	m := NewRepairLeaseManager()
	m.MarkQueued("")
	if _, ok := m.QueuedSince(""); ok {
		t.Fatal("empty sessionID must report not queued")
	}
}

func TestRepairQueueOnChangeCallback(t *testing.T) {
	base := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	clock, cur := fixedClock(base)
	m := NewRepairLeaseManager().withClock(time.Hour, clock)

	var events []string
	var queuedStates []bool
	m.SetOnQueueChange(func(sessionID string) {
		events = append(events, sessionID)
		// Re-entering the manager from the callback must not deadlock.
		_, queued := m.QueuedSince(sessionID)
		_ = m.Active(sessionID)
		queuedStates = append(queuedStates, queued)
	})

	// Initial mark notifies once.
	m.MarkQueued("s1")
	if len(events) != 1 || events[0] != "s1" || !queuedStates[0] {
		t.Fatalf("after initial MarkQueued: events=%v queuedStates=%v, want [s1]/[true]", events, queuedStates)
	}

	// In-TTL refresh does not notify.
	*cur = base.Add(2 * time.Minute)
	m.MarkQueued("s1")
	if len(events) != 1 {
		t.Fatalf("in-TTL refresh fired callback: events=%v", events)
	}

	// Acquire clears queued mark and notifies once.
	if err := m.Acquire("s1", "plugin-A"); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if len(events) != 2 || events[1] != "s1" || queuedStates[1] {
		t.Fatalf("after Acquire: events=%v queuedStates=%v, want 2nd event [s1]/[false]", events, queuedStates)
	}

	// Acquire / Release on an unqueued session do not notify.
	if err := m.Acquire("s1", "plugin-A"); err != nil {
		t.Fatalf("re-entrant Acquire: %v", err)
	}
	m.Release("s1", "plugin-A")
	if len(events) != 2 {
		t.Fatalf("unqueued Acquire/Release fired callback: events=%v", events)
	}

	// Release on a queued session notifies once.
	m.MarkQueued("s2")
	m.Release("s2", "plugin-A")
	if len(events) != 4 || events[2] != "s2" || !queuedStates[2] || events[3] != "s2" || queuedStates[3] {
		t.Fatalf("after MarkQueued+Release s2: events=%v queuedStates=%v", events, queuedStates)
	}

	// Lazy TTL eviction notifies once; a second read does not.
	m.MarkQueued("s3")
	*cur = cur.Add(MinRepairQueueTTL)
	if _, ok := m.QueuedSince("s3"); ok {
		t.Fatal("expected s3 to expire")
	}
	if _, ok := m.QueuedSince("s3"); ok {
		t.Fatal("expected s3 to stay expired")
	}
	if len(events) != 6 || events[4] != "s3" || !queuedStates[4] || events[5] != "s3" || queuedStates[5] {
		t.Fatalf("after TTL eviction s3: events=%v queuedStates=%v", events, queuedStates)
	}

	// Clearing callback stops notifications.
	m.SetOnQueueChange(nil)
	m.MarkQueued("s4")
	if len(events) != 6 {
		t.Fatalf("cleared callback fired: events=%v", events)
	}
}
