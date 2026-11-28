package status

import "time"

// MinRepairQueueTTL is the floor on how long a queued repair mark stays live
// without a refresh from the repair plugin.
const MinRepairQueueTTL = 5 * time.Minute

// DefaultRepairSweepInterval matches the repair plugin's default 1-minute sweep
// interval when no explicit sweep_interval_minutes is configured.
const DefaultRepairSweepInterval = time.Minute

type repairQueueEntry struct {
	firstQueuedAt time.Time
	lastSeenAt    time.Time
}

// RepairQueueTTLForSweepInterval returns max(5m, 3 * sweepInterval), using
// DefaultRepairSweepInterval when sweepInterval <= 0.
func RepairQueueTTLForSweepInterval(sweepInterval time.Duration) time.Duration {
	if sweepInterval <= 0 {
		sweepInterval = DefaultRepairSweepInterval
	}
	ttl := 3 * sweepInterval
	if ttl < MinRepairQueueTTL {
		return MinRepairQueueTTL
	}
	return ttl
}

// SetQueueTTLFn installs a live TTL provider consulted whenever queued marks
// are refreshed or read. Passing nil restores MinRepairQueueTTL. Returns m for
// chaining.
func (m *RepairLeaseManager) SetQueueTTLFn(fn func() time.Duration) *RepairLeaseManager {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	m.queueTTLFn = fn
	m.mu.Unlock()
	return m
}

// SetOnQueueChange registers a callback invoked after a session enters or
// leaves the repair queue (not on in-TTL refreshes). Invoked without holding
// m.mu. Passing nil clears the callback. Returns m for chaining.
func (m *RepairLeaseManager) SetOnQueueChange(fn func(sessionID string)) *RepairLeaseManager {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	m.onQueueChange = fn
	m.mu.Unlock()
	return m
}

func resolveQueueTTL(fn func() time.Duration) time.Duration {
	if fn != nil {
		if d := fn(); d > 0 {
			return d
		}
	}
	return MinRepairQueueTTL
}

// MarkQueued records or refreshes the queued mark for sessionID. Repeated calls
// while the entry is still live preserve the original first-queued timestamp
// and refresh lastSeenAt; an absent or already-expired entry starts fresh.
func (m *RepairLeaseManager) MarkQueued(sessionID string) {
	if m == nil || sessionID == "" {
		return
	}
	m.mu.Lock()
	fn := m.queueTTLFn
	m.mu.Unlock()

	ttl := resolveQueueTTL(fn)

	m.mu.Lock()
	now := m.now()
	if m.queued == nil {
		m.queued = make(map[string]repairQueueEntry)
	}
	if existing, ok := m.queued[sessionID]; ok && now.Before(existing.lastSeenAt.Add(ttl)) {
		existing.lastSeenAt = now
		m.queued[sessionID] = existing
		m.mu.Unlock()
		return
	}
	m.queued[sessionID] = repairQueueEntry{firstQueuedAt: now, lastSeenAt: now}
	onChange := m.onQueueChange
	m.mu.Unlock()
	if onChange != nil {
		onChange(sessionID)
	}
}

// QueuedSince returns the first-queued timestamp for sessionID while its last
// refresh is within the current queue TTL, lazily evicting an expired mark.
func (m *RepairLeaseManager) QueuedSince(sessionID string) (time.Time, bool) {
	if m == nil || sessionID == "" {
		return time.Time{}, false
	}
	m.mu.Lock()
	entry, ok := m.queued[sessionID]
	fn := m.queueTTLFn
	now := m.now()
	m.mu.Unlock()
	if !ok {
		return time.Time{}, false
	}

	ttl := resolveQueueTTL(fn)
	if !now.Before(entry.lastSeenAt.Add(ttl)) {
		m.mu.Lock()
		evicted := false
		if cur, exists := m.queued[sessionID]; exists && cur.lastSeenAt.Equal(entry.lastSeenAt) {
			delete(m.queued, sessionID)
			evicted = true
		}
		onChange := m.onQueueChange
		m.mu.Unlock()
		if evicted && onChange != nil {
			onChange(sessionID)
		}
		return time.Time{}, false
	}
	return entry.firstQueuedAt, true
}
