// Package status provides an in-memory cache for chat status heartbeats
// reported by boss CLI clients. The daemon uses this to share process status
// across multiple CLI instances.
package status

import (
	"sync"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// StaleThreshold is how long since the last heartbeat before a chat is
// considered stale (and thus stopped). Set to 5x the 3s heartbeat interval.
const StaleThreshold = 15 * time.Second

// Entry is a cached status heartbeat for a single Claude chat process.
type Entry struct {
	Status       pb.ChatStatus
	LastOutputAt time.Time
	ReceivedAt   time.Time
	// ResetAt is the usage-limit reset time, set only when Status is
	// CHAT_STATUS_LIMITED and the banner carried a parseable reset time.
	// Zero when unknown or the chat is not limited. Plumbed for the display
	// layer; human-readable rendering is BOS-167.
	ResetAt time.Time
}

// Tracker is a thread-safe in-memory cache of chat process statuses.
type Tracker struct {
	mu      sync.RWMutex
	entries map[string]*Entry // claude_id -> entry

	// authFailed records, per agent session ID, the time the login-required
	// terminal shape ("Not logged in" / "Please run /login") was last observed
	// on the chat's pane. It is kept separate from entries because Update
	// recreates the Entry on every heartbeat (which would otherwise wipe the
	// marker); this map is only written by the poller's dedicated SetAuthFailed
	// path. A marker older than StaleThreshold is treated as absent so a chat
	// that logged back in (or died) stops flagging — fail toward NOT flagging.
	authFailed map[string]time.Time // agent_session_id -> last observed

	// onUpdate, when non-nil, is invoked after every Update with the
	// claude_id whose status changed. The hook resolves claude_id →
	// sessionID and triggers DisplayStatusComputer.Recompute. Kept as a
	// loose function to avoid a status → db dependency on a concrete
	// resolver type, and to keep this package free of cross-package
	// imports for chat lookup.
	onUpdate func(agentSessionID string)

	// onLimitTransition, when non-nil, is fired exactly once when a chat enters
	// CHAT_STATUS_LIMITED and exactly once when it leaves. entered is true on the
	// enter transition (not-limited → limited) and false on the leave transition
	// (limited → not-limited); it never fires on same-limited-state repeated
	// polls. Wired in cmd/main.go to emit a structured audit log per transition;
	// the durable sink is Epic 4.4.
	onLimitTransition func(agentSessionID string, entered bool)

	// onAuthChange, when non-nil, is invoked after SetAuthFailed whenever the
	// chat's EFFECTIVE auth-failed state flips (absent/stale → fresh, or
	// present → cleared). It is separate from onUpdate because the auth-failed
	// overlay is an attention_status change that need not coincide with a chat
	// STATUS change — a WORKING chat can go login-required without Update ever
	// firing. The hook resolves agent_session_id → session and emits a
	// SessionDelta so the cloud/web read model (fed only by the reverse stream)
	// receives ATTENTION_REASON_AGENT_AUTH_FAILED. Fired only on a transition,
	// so the poller's per-tick SetAuthFailed calls don't storm the stream.
	onAuthChange func(agentSessionID string)
}

// NewTracker creates a new empty Tracker.
func NewTracker() *Tracker {
	return &Tracker{
		entries:    make(map[string]*Entry),
		authFailed: make(map[string]time.Time),
	}
}

// Update upserts a heartbeat for the given claude ID. ResetAt is cleared —
// use UpdateLimited for the CHAT_STATUS_LIMITED path that carries a reset time.
func (t *Tracker) Update(agentSessionID string, status pb.ChatStatus, lastOutputAt time.Time) {
	t.update(agentSessionID, status, lastOutputAt, time.Time{})
}

// UpdateLimited upserts a CHAT_STATUS_LIMITED heartbeat carrying the usage-limit
// reset time (zero when the banner had no parseable reset). Separate from Update
// so the common status path stays a two-value call and only the limit path
// threads a reset time. The onUpdate hook fires on transitions into and out of
// LIMITED because those change Status.
func (t *Tracker) UpdateLimited(agentSessionID string, resetAt, lastOutputAt time.Time) {
	t.update(agentSessionID, pb.ChatStatus_CHAT_STATUS_LIMITED, lastOutputAt, resetAt)
}

func (t *Tracker) update(agentSessionID string, status pb.ChatStatus, lastOutputAt, resetAt time.Time) {
	t.mu.Lock()
	prev, hadPrev := t.entries[agentSessionID]
	t.entries[agentSessionID] = &Entry{
		Status:       status,
		LastOutputAt: lastOutputAt,
		ReceivedAt:   time.Now(),
		ResetAt:      resetAt,
	}
	hook := t.onUpdate
	limitHook := t.onLimitTransition
	wasLimited := hadPrev && prev.Status == pb.ChatStatus_CHAT_STATUS_LIMITED
	isLimited := status == pb.ChatStatus_CHAT_STATUS_LIMITED
	t.mu.Unlock()

	// Fire the hook only when the status actually changed — avoids burning
	// a recompute on every 3-second heartbeat when nothing's moved.
	if hook != nil && (!hadPrev || prev.Status != status) {
		hook(agentSessionID)
	}

	// Fire the limit-transition hook exactly once on the false→true flip and
	// once on the true→false flip; never on same-limited-state repeated polls.
	if limitHook != nil && wasLimited != isLimited {
		limitHook(agentSessionID, isLimited)
	}
}

// SetAuthFailed records or clears the login-required marker for a chat. When
// failed is true it stamps the current time; when false it clears any existing
// marker immediately (so a chat that logged back in stops flagging on the next
// poll tick). Called by the tmux poller every tick with the current pane state.
//
// It fires onAuthChange only when the EFFECTIVE auth-failed state flips — a
// fresh marker appearing where there was none (or a stale one), or a fresh
// marker being cleared. Because the poller calls this every tick, gating on the
// transition keeps the hook from re-emitting a SessionDelta on every poll while
// the state holds steady.
func (t *Tracker) SetAuthFailed(agentSessionID string, failed bool) {
	t.mu.Lock()
	prevAt, had := t.authFailed[agentSessionID]
	wasFailed := had && time.Since(prevAt) <= StaleThreshold
	if failed {
		t.authFailed[agentSessionID] = time.Now()
	} else {
		delete(t.authFailed, agentSessionID)
	}
	hook := t.onAuthChange
	shouldFire := (!failed && had) || (failed && !wasFailed)
	t.mu.Unlock()

	if hook != nil && shouldFire {
		hook(agentSessionID)
	}
}

// AuthFailed reports whether the chat's pane currently shows the login-required
// terminal shape and the marker is fresh (observed within StaleThreshold). A
// stale marker — the poller stopped re-observing it (the chat logged in, its
// pane changed, or it died) — is treated as absent so the flag clears itself and
// never sticks. This fails toward NOT flagging.
func (t *Tracker) AuthFailed(agentSessionID string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	at, ok := t.authFailed[agentSessionID]
	if !ok {
		return false
	}
	return time.Since(at) <= StaleThreshold
}

// SetOnUpdate wires a callback fired after Update when the chat's status
// changes. The wiring lives in cmd/main.go and resolves claude_id →
// sessionID before delegating to DisplayStatusComputer.Recompute. Tests
// usually leave this nil.
func (t *Tracker) SetOnUpdate(fn func(agentSessionID string)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.onUpdate = fn
}

// SetOnLimitTransition wires a callback fired once when a chat enters
// CHAT_STATUS_LIMITED and once when it leaves. The wiring lives in cmd/main.go
// and emits a structured audit log per transition. Tests usually leave this nil.
func (t *Tracker) SetOnLimitTransition(fn func(agentSessionID string, entered bool)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.onLimitTransition = fn
}

// SetOnAuthChange wires a callback fired after SetAuthFailed when the chat's
// effective auth-failed state flips. The wiring lives in cmd/main.go and
// resolves agent_session_id → session before emitting a SessionDelta carrying
// the AGENT_AUTH_FAILED overlay. Tests usually leave this nil.
func (t *Tracker) SetOnAuthChange(fn func(agentSessionID string)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.onAuthChange = fn
}

// Get returns the cached entry for the given claude ID, or nil if not found
// or stale (older than StaleThreshold).
func (t *Tracker) Get(agentSessionID string) *Entry {
	t.mu.RLock()
	defer t.mu.RUnlock()
	e, ok := t.entries[agentSessionID]
	if !ok {
		return nil
	}
	if time.Since(e.ReceivedAt) > StaleThreshold {
		return nil
	}
	return e
}

// GetBatch returns entries for multiple claude IDs. Stale entries are
// returned as stopped.
func (t *Tracker) GetBatch(agentSessionIDs []string) map[string]*Entry {
	t.mu.RLock()
	defer t.mu.RUnlock()
	result := make(map[string]*Entry, len(agentSessionIDs))
	now := time.Now()
	for _, id := range agentSessionIDs {
		e, ok := t.entries[id]
		if !ok {
			continue
		}
		if now.Sub(e.ReceivedAt) > StaleThreshold {
			result[id] = &Entry{
				Status:       pb.ChatStatus_CHAT_STATUS_STOPPED,
				LastOutputAt: e.LastOutputAt,
				ReceivedAt:   e.ReceivedAt,
			}
		} else {
			// Return a copy to prevent callers from mutating the cached entry.
			result[id] = &Entry{
				Status:       e.Status,
				LastOutputAt: e.LastOutputAt,
				ReceivedAt:   e.ReceivedAt,
				ResetAt:      e.ResetAt,
			}
		}
	}
	return result
}

// Snapshot returns a copy of every fresh (non-stale) entry, keyed by
// claude_id. Stale entries are filtered out — callers receive only chats
// whose tracker heartbeat is recent enough to trust. Used by the upstream
// stream's DaemonSnapshot path so a freshly-connected orchestrator inherits
// the daemon's current per-chat status without waiting for the next
// status transition (Update suppresses the OnUpdate hook on no-op
// heartbeats, so long-running chats whose state hasn't moved would
// otherwise be invisible until they next change).
func (t *Tracker) Snapshot() map[string]*Entry {
	t.mu.RLock()
	defer t.mu.RUnlock()
	now := time.Now()
	out := make(map[string]*Entry, len(t.entries))
	for id, e := range t.entries {
		if now.Sub(e.ReceivedAt) > StaleThreshold {
			continue
		}
		out[id] = &Entry{
			Status:       e.Status,
			LastOutputAt: e.LastOutputAt,
			ReceivedAt:   e.ReceivedAt,
			ResetAt:      e.ResetAt,
		}
	}
	return out
}

// Remove deletes the entry for the given claude ID.
func (t *Tracker) Remove(agentSessionID string) {
	t.mu.Lock()
	_, hadAuthMarker := t.authFailed[agentSessionID]
	delete(t.entries, agentSessionID)
	delete(t.authFailed, agentSessionID)
	hook := t.onAuthChange
	t.mu.Unlock()

	if hook != nil && hadAuthMarker {
		hook(agentSessionID)
	}
}

// Cleanup removes all stale entries (older than StaleThreshold).
func (t *Tracker) Cleanup() {
	t.mu.Lock()
	now := time.Now()
	for id, e := range t.entries {
		if now.Sub(e.ReceivedAt) > StaleThreshold {
			delete(t.entries, id)
		}
	}
	var clearedAuthMarkers []string
	for id, at := range t.authFailed {
		if now.Sub(at) > StaleThreshold {
			delete(t.authFailed, id)
			clearedAuthMarkers = append(clearedAuthMarkers, id)
		}
	}
	hook := t.onAuthChange
	t.mu.Unlock()

	if hook != nil {
		for _, id := range clearedAuthMarkers {
			hook(id)
		}
	}
}
