package status

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
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

// RepairStallWindow bounds how long a held lease may go with NO observed
// progress before it is treated as stalled and released. 8 minutes sits at the
// top of the 5–8m band the MAD-652 report recommends, and is far under
// DefaultRepairLeaseTTL (45m) — the TTL reclaims a crashed holder, this window
// reclaims a live-but-dead repair round.
//
// The rule is deliberately two-axis: a lease is stalled only when BOTH the
// session's repair head SHA has not advanced AND the repair chat has produced
// no output for the whole window. A healthy repair chat violates the rule on
// one axis or the other within minutes — it either commits or it talks — so it
// can never be stall-released.
const RepairStallWindow = 8 * time.Minute

// ErrRepairLeaseHeld is returned by RepairLeaseManager.Acquire when a different,
// still-live dispatcher already holds the session's repair lease. It is the
// single-repairer invariant made explicit: a second repairer gets a clean
// refusal instead of racing onto the same worktree.
var ErrRepairLeaseHeld = errors.New("repair already active on this session")

// repairLease is one held lease.
type repairLease struct {
	holder    string
	expiresAt time.Time

	// Progress bookkeeping for the stall rule (BOS-515). headSHA and
	// lastOutputAt are the most recent evidence a caller fed via
	// MarkRepairProgress; progressAt is the last time progress was actually
	// OBSERVED (not the timestamp of the progress itself), which is what the
	// stall window is measured against.
	headSHA      string
	lastOutputAt time.Time
	progressAt   time.Time
}

// repairStall records why and when a held lease was stall-released. It outlives
// the lease so the hydration layer can surface the reason after the release,
// and is cleared by the next Acquire (a new round) or by observed progress.
type repairStall struct {
	at     time.Time
	reason string
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
//
// BOS-515 adds a second lazy release path on top of the TTL: a held lease with
// no observed progress for RepairStallWindow is STALL-released. That is a new
// RELEASE path, not a second source of truth — the invariant is unchanged
// (repair_active true ⇔ lease held), and the recorded stall is surfaced purely
// as transport metadata (Session.repair_stalled_at + the blocked reason).
type RepairLeaseManager struct {
	mu     sync.Mutex
	leases map[string]repairLease
	stalls map[string]repairStall
	ttl    time.Duration
	now    func() time.Time
}

// NewRepairLeaseManager returns a manager with the default TTL and clock.
func NewRepairLeaseManager() *RepairLeaseManager {
	return &RepairLeaseManager{
		leases: make(map[string]repairLease),
		stalls: make(map[string]repairStall),
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

// WithClock is withClock exported for deterministic tests in the packages that
// consume the manager (the hydration layers in internal/server and
// internal/plugin need a stalled lease without sleeping). A zero ttl keeps the
// default.
func (m *RepairLeaseManager) WithClock(ttl time.Duration, now func() time.Time) *RepairLeaseManager {
	return m.withClock(ttl, now)
}

// Acquire claims the session's repair lease for holder. It succeeds when the
// session has no live lease, or when the existing lease is already held by
// holder (re-entrant — a repairer may refresh its own lease across passes), or
// when the existing lease has TTL-expired (the stale lease is reclaimed).
// Otherwise it returns ErrRepairLeaseHeld and leaves the incumbent untouched.
// An empty holder is treated as "no enforcement": it never takes a lease and
// never refuses, so legacy callers that omit a dispatcher id keep working.
//
// Every successful acquire — fresh or re-entrant refresh — restarts the stall
// clock and clears any recorded stall: a new round is progress by definition.
func (m *RepairLeaseManager) Acquire(sessionID, holder string) error {
	if m == nil || sessionID == "" || holder == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	cur, held := m.leases[sessionID]
	if held && !m.expired(cur, now) && cur.holder != holder {
		return ErrRepairLeaseHeld
	}
	next := repairLease{holder: holder, expiresAt: now.Add(m.ttl), progressAt: now}
	if held && cur.holder == holder {
		// A re-entrant refresh keeps the observed evidence so the next
		// MarkRepairProgress compares against real history, not a blank slate.
		next.headSHA = cur.headSHA
		next.lastOutputAt = cur.lastOutputAt
	}
	m.leases[sessionID] = next
	delete(m.stalls, sessionID)
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

// MarkRepairProgress feeds the stall heartbeat with the CURRENT evidence for a
// session: the repair head SHA the dispatcher last recorded, and the most
// recent chat output timestamp the tracker knows about. Callers pass whatever
// they have on every hydration pass; only a genuine advance on either axis
// counts as progress, and either one restarts the stall clock and clears a
// recorded stall. When nothing advanced, progressAt stays frozen — that is what
// eventually trips RepairStallWindow.
//
// It is a no-op for a nil manager, an empty sessionID, or a session with no
// live lease (nothing to keep alive).
func (m *RepairLeaseManager) MarkRepairProgress(sessionID, headSHA string, lastOutputAt time.Time) {
	if m == nil || sessionID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	cur, ok := m.leases[sessionID]
	// Deliberately only the TTL check here, NOT the full lazy evaluation: a
	// sparse caller that goes quiet for longer than the window and then reports
	// fresh output must be able to record that progress rather than have the
	// lease stall-released out from under it first.
	if !ok || m.expired(cur, now) {
		return
	}
	progressed := false
	if headSHA != "" && headSHA != cur.headSHA {
		cur.headSHA = headSHA
		progressed = true
	}
	if lastOutputAt.After(cur.lastOutputAt) {
		cur.lastOutputAt = lastOutputAt
		progressed = true
	}
	if !progressed {
		return
	}
	cur.progressAt = now
	m.leases[sessionID] = cur
	delete(m.stalls, sessionID)
}

// Active reports whether a live (non-expired, non-stalled) repair lease is held
// for the session. This is the value hydrated onto Session.repair_active.
func (m *RepairLeaseManager) Active(sessionID string) bool {
	if m == nil || sessionID == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.evaluateLocked(sessionID, m.now())
	return ok
}

// HolderOf returns the current live lease holder for the session, or "" if none
// (expired or stalled). Used to build the FailedPrecondition refusal message.
func (m *RepairLeaseManager) HolderOf(sessionID string) string {
	if m == nil || sessionID == "" {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.evaluateLocked(sessionID, m.now())
	if !ok {
		return ""
	}
	return cur.holder
}

// StalledAt returns when the session's repair lease was stall-released, and
// whether such a record is currently live. It runs the same lazy evaluation as
// Active, so a caller that reads StalledAt first still sees a stall detected on
// this very call. The record is cleared by the next Acquire (new round) or by
// observed progress.
func (m *RepairLeaseManager) StalledAt(sessionID string) (time.Time, bool) {
	if m == nil || sessionID == "" {
		return time.Time{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.evaluateLocked(sessionID, m.now())
	st, ok := m.stalls[sessionID]
	if !ok {
		return time.Time{}, false
	}
	return st.at, true
}

// StallReason returns the human-readable reason a session's repair lease was
// stall-released, or "" when there is no live stall record. Like StalledAt it
// runs the lazy evaluation first.
func (m *RepairLeaseManager) StallReason(sessionID string) string {
	if m == nil || sessionID == "" {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.evaluateLocked(sessionID, m.now())
	return m.stalls[sessionID].reason
}

// HydrateSession applies the whole BOS-515 read-side contract to one proto
// Session in the single order every hydration site must use: feed the heartbeat
// with the current evidence, THEN read the (possibly just-released) lease, then
// overlay the stall metadata.
//
// The blocked-reason overwrite is deliberate. last_repair_blocked_reason is
// DB-persisted and this package cannot write the DB, so while a stall record is
// live the stall reason wins at the transport level — that overlay IS the
// intended plumbing for surfacing the stall to dispatchers.
//
// Nil-safe on both the manager (not wired) and p.
func (m *RepairLeaseManager) HydrateSession(p *pb.Session, sessionID, headSHA string, lastOutputAt time.Time) {
	if p == nil {
		return
	}
	m.MarkRepairProgress(sessionID, headSHA, lastOutputAt)
	p.RepairActive = m.Active(sessionID)
	if at, ok := m.StalledAt(sessionID); ok {
		p.RepairStalledAt = timestamppb.New(at)
		p.LastRepairBlockedReason = m.StallReason(sessionID)
	}
}

// evaluateLocked applies both lazy release rules to the session's lease — TTL
// expiry first, then the stall rule — and returns the surviving live lease.
// Every read path funnels through it so Active, HolderOf, StalledAt and
// StallReason can never disagree about whether a lease is live. Callers must
// hold m.mu.
func (m *RepairLeaseManager) evaluateLocked(sessionID string, now time.Time) (repairLease, bool) {
	cur, ok := m.leases[sessionID]
	if !ok {
		return repairLease{}, false
	}
	if m.expired(cur, now) {
		// A TTL reclaim means the holder crashed, not that a repair stalled:
		// no stall record, so repair_stalled_at stays absent.
		delete(m.leases, sessionID)
		return repairLease{}, false
	}
	if !cur.progressAt.IsZero() && now.Sub(cur.progressAt) > RepairStallWindow {
		if m.stalls == nil {
			m.stalls = make(map[string]repairStall)
		}
		m.stalls[sessionID] = repairStall{at: now, reason: repairStallReason()}
		delete(m.leases, sessionID)
		return repairLease{}, false
	}
	return cur, true
}

func (m *RepairLeaseManager) expired(l repairLease, now time.Time) bool {
	return !now.Before(l.expiresAt)
}

// repairStallReason renders the stall reason from RepairStallWindow so the text
// can never drift from the constant.
func repairStallReason() string {
	return fmt.Sprintf("repair stalled: no output and no head advance for %s", stallWindowLabel())
}

// stallWindowLabel renders RepairStallWindow compactly ("8m" rather than
// "8m0s"). The trims are anchored on the whole zero component ("m0s", "h0m")
// rather than a bare "0s"/"0m" so retuning the constant stays safe: a blind
// TrimSuffix would render 10m as "1".
func stallWindowLabel() string {
	s := RepairStallWindow.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
