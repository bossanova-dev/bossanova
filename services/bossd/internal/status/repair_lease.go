package status

import (
	"errors"
	"sync"
	"time"
)

// DefaultRepairLeaseTTL bounds how long a single-repairer lease survives without
// an explicit release. It must exceed a single repair attempt's wait deadline
// (the repair plugin's WaitChatRun is bounded at 30m) so a still-working
// attempt is never evicted under itself; the lease is normally released
// promptly on post-repair assessment, and the TTL only reclaims a lease whose
// holder crashed or was killed without ever clearing its repair status. The
// lease is re-acquired per attempt (refreshing the expiry), so the whole round
// stays covered as long as each attempt refreshes within the TTL.
const DefaultRepairLeaseTTL = 45 * time.Minute

// ErrRepairLeaseHeld is returned by RepairLeaseManager.Acquire when a different,
// still-live dispatcher already holds the session's repair lease. It is the
// single-repairer invariant made explicit: a second repairer gets a clean
// refusal instead of racing onto the same worktree.
var ErrRepairLeaseHeld = errors.New("repair already active on this session")

// repairLease is one held lease.
type repairLease struct {
	holder    string
	expiresAt time.Time
}

// RepairLeaseManager enforces a per-session single-repairer invariant. At most
// one dispatcher (the repair plugin sweep, a /boss-repair watch chat, a boss-epic
// driver) may hold a session's repair lease at a time; a second acquirer is
// refused with ErrRepairLeaseHeld until the holder releases it or the lease
// TTL-expires. Expiry is lazy — evaluated on each read/acquire against the
// injected clock — so a crashed holder's stale lease is reclaimed by the next
// acquirer without a background sweep. All methods are safe for concurrent use.
//
// It is the authoritative source for the transport-only Session.repair_active
// field: repair_active is true for exactly the window a lease is held (from
// repair-chat start until post-repair assessment or TTL reclaim), which is why
// it is more trustworthy than the advisory display_is_repairing flag — the
// latter can stick "true" forever if a repairer dies before clearing it.
type RepairLeaseManager struct {
	mu     sync.Mutex
	leases map[string]repairLease
	ttl    time.Duration
	now    func() time.Time
}

// NewRepairLeaseManager returns a manager with the default TTL and clock.
func NewRepairLeaseManager() *RepairLeaseManager {
	return &RepairLeaseManager{
		leases: make(map[string]repairLease),
		ttl:    DefaultRepairLeaseTTL,
		now:    time.Now,
	}
}

// withClock overrides the TTL and clock for deterministic tests. A zero ttl
// keeps the default.
func (m *RepairLeaseManager) withClock(ttl time.Duration, now func() time.Time) *RepairLeaseManager {
	if ttl > 0 {
		m.ttl = ttl
	}
	if now != nil {
		m.now = now
	}
	return m
}

// Acquire claims the session's repair lease for holder. It succeeds when the
// session has no live lease, or when the existing lease is already held by
// holder (re-entrant — a repairer may refresh its own lease across passes), or
// when the existing lease has TTL-expired (the stale lease is reclaimed).
// Otherwise it returns ErrRepairLeaseHeld and leaves the incumbent untouched.
// An empty holder is treated as "no enforcement": it never takes a lease and
// never refuses, so legacy callers that omit a dispatcher id keep working.
func (m *RepairLeaseManager) Acquire(sessionID, holder string) error {
	if m == nil || sessionID == "" || holder == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if cur, ok := m.leases[sessionID]; ok && !m.expired(cur, now) && cur.holder != holder {
		return ErrRepairLeaseHeld
	}
	m.leases[sessionID] = repairLease{holder: holder, expiresAt: now.Add(m.ttl)}
	return nil
}

// Release drops the session's repair lease, but only if holder currently owns
// it (a losing dispatcher's late release must not evict the winner). A holder
// mismatch or missing lease is a no-op. An empty holder is a no-op.
func (m *RepairLeaseManager) Release(sessionID, holder string) {
	if m == nil || sessionID == "" || holder == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.leases[sessionID]; ok && cur.holder == holder {
		delete(m.leases, sessionID)
	}
}

// Active reports whether a live (non-expired) repair lease is held for the
// session. This is the value hydrated onto Session.repair_active.
func (m *RepairLeaseManager) Active(sessionID string) bool {
	if m == nil || sessionID == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.leases[sessionID]
	if !ok {
		return false
	}
	if m.expired(cur, m.now()) {
		delete(m.leases, sessionID)
		return false
	}
	return true
}

// HolderOf returns the current live lease holder for the session, or "" if none
// (or expired). Used to build the FailedPrecondition refusal message.
func (m *RepairLeaseManager) HolderOf(sessionID string) string {
	if m == nil || sessionID == "" {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.leases[sessionID]
	if !ok || m.expired(cur, m.now()) {
		return ""
	}
	return cur.holder
}

func (m *RepairLeaseManager) expired(l repairLease, now time.Time) bool {
	return !now.Before(l.expiresAt)
}
