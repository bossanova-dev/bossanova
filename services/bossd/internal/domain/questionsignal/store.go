// Package questionsignal holds the structured "a question is pending" signal
// that lets an explicit agent-emitted event (e.g. a Claude Code Notification
// hook) drive CHAT_STATUS_QUESTION, instead of inferring it by scraping the
// rendered terminal pane with regexes.
//
// The store is deliberately agent-agnostic: it is keyed by agent_session_id
// and knows nothing about which agent set the record, so future agents
// (opencode, codex) can feed the same seam without a further change here.
//
// Key discipline (matters for children B/C): the poller READS with
// chatResumeSessionID(chat) — ProviderSessionID when set, else AgentSessionID
// (see tmux_poller.go). A writer must SetPending under that SAME value or the
// read silently misses. For claude the two coincide (its ResolveInteractiveSessionID
// echoes the requested id back as the provider id), so the Notification hook can
// POST the bossd AgentSessionID directly. An agent whose provider id differs
// from the bossd AgentSessionID (e.g. codex's distinct rollout id) must key the
// record on the provider id instead.
//
// Records self-heal. A pending record is a hint, not authoritative — the
// poller clears it when the user has answered (LastTurnIsUser), and a stale
// record ages out via the TTL so a missed clear can never pin a chat in
// QUESTION forever.
package questionsignal

import (
	"sync"
	"time"
)

// DefaultTTL bounds how long a pending record survives without being cleared.
// A Notification hook that fires but is never reconciled (missed clear, daemon
// restart racing the transcript write) must not leave a chat stuck showing
// "? question", so Get treats any record older than this as not-pending.
const DefaultTTL = 10 * time.Minute

// Record is a single pending-question signal for one agent session.
type Record struct {
	// Pending is always true for a record returned by Get; it exists so the
	// stored shape matches the {pending, at, source} contract and reads
	// self-documentingly at call sites.
	Pending bool
	// At is when SetPending recorded the signal (store clock).
	At time.Time
	// Source labels who set it, e.g. "claude-notification".
	Source string
}

// Store is a thread-safe, TTL-bounded map of agent_session_id -> pending
// question record. The zero value is not usable; construct with NewStore.
type Store struct {
	mu      sync.Mutex
	records map[string]Record
	ttl     time.Duration
	now     func() time.Time
}

// NewStore returns a Store whose records age out after ttl using the wall
// clock. A non-positive ttl falls back to DefaultTTL so a misconfigured
// caller can't disable expiry entirely.
func NewStore(ttl time.Duration) *Store {
	return NewStoreWithClock(ttl, time.Now)
}

// NewStoreWithClock is NewStore with an injectable clock for deterministic
// TTL tests. A nil now falls back to time.Now.
func NewStoreWithClock(ttl time.Duration, now func() time.Time) *Store {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if now == nil {
		now = time.Now
	}
	return &Store{
		records: make(map[string]Record),
		ttl:     ttl,
		now:     now,
	}
}

// SetPending records (or refreshes) a pending question for agentSessionID.
// The recorded time is advanced to now on every call so a repeated
// Notification hook keeps the record fresh rather than letting it age out
// mid-conversation. An empty agentSessionID is ignored.
func (s *Store) SetPending(agentSessionID, source string) {
	if agentSessionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[agentSessionID] = Record{Pending: true, At: s.now(), Source: source}
}

// Clear removes any pending record for agentSessionID. It is a no-op when no
// record exists.
func (s *Store) Clear(agentSessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.records, agentSessionID)
}

// Get returns the fresh pending record for agentSessionID, or ok=false when
// there is none or the record has aged out. An expired record is purged
// lazily on read so the map self-heals without a background sweeper. A record
// whose age is exactly the TTL is still considered fresh; only strictly older
// records expire.
func (s *Store) Get(agentSessionID string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[agentSessionID]
	if !ok {
		return Record{}, false
	}
	if s.now().Sub(rec.At) > s.ttl {
		delete(s.records, agentSessionID)
		return Record{}, false
	}
	return rec, true
}

// HasPending reports whether agentSessionID has a fresh pending record. It is
// the narrow read seam for consumers that only need the pending state and
// should not depend on Record's storage shape.
func (s *Store) HasPending(agentSessionID string) bool {
	_, ok := s.Get(agentSessionID)
	return ok
}

// len reports how many records are currently held. Test-only helper.
func (s *Store) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}
