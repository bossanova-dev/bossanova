package status

import (
	"context"
	"sync"
	"time"

	"github.com/recurser/bossalib/vcs"
)

// DisplayEntry is a cached display status for a single session.
type DisplayEntry struct {
	Status              vcs.DisplayStatus
	HasFailures         bool
	HasChangesRequested bool
	IsRepairing         bool
	HeadSHA             string
	UpdatedAt           time.Time
}

// DisplayTracker is a thread-safe in-memory cache of session display statuses.
type DisplayTracker struct {
	mu         sync.RWMutex
	entries    map[string]*DisplayEntry // session ID -> entry
	onChange   func(sessionID string, oldEntry, newEntry *DisplayEntry)
	recomputer Recomputer
}

// NewDisplayTracker creates a new empty DisplayTracker.
func NewDisplayTracker() *DisplayTracker {
	return &DisplayTracker{
		entries: make(map[string]*DisplayEntry),
	}
}

// Set upserts a display status for the given session ID.
// If the status changes and an onChange callback is set, it will be called.
func (t *DisplayTracker) Set(sessionID string, info vcs.DisplayInfo) {
	t.mu.Lock()

	// Check if status changed
	oldEntry, existed := t.entries[sessionID]
	var oldStatus vcs.DisplayStatus
	if existed {
		oldStatus = oldEntry.Status
	}

	// Update entry, preserving IsRepairing from the old entry so the
	// display poller doesn't overwrite the repair plugin's flag.
	var isRepairing bool
	if existed {
		isRepairing = oldEntry.IsRepairing
	}
	newEntry := &DisplayEntry{
		Status:              info.Status,
		HasFailures:         info.HasFailures,
		HasChangesRequested: info.HasChangesRequested,
		IsRepairing:         isRepairing,
		HeadSHA:             info.HeadSHA,
		UpdatedAt:           time.Now(),
	}
	t.entries[sessionID] = newEntry

	// Capture references we need under the lock, then release before
	// firing callbacks. The Recomputer reads via Get() which takes RLock
	// itself — calling it while still holding the write lock would
	// deadlock as soon as RWMutex serialises the readers.
	statusChanged := !existed || oldStatus != info.Status
	onChange := t.onChange
	t.mu.Unlock()

	if onChange != nil && statusChanged {
		go onChange(sessionID, oldEntry, newEntry)
	}
	t.scheduleRecompute(sessionID)
}

// Get returns a copy of the cached entry for the given session ID, or nil if not found.
func (t *DisplayTracker) Get(sessionID string) *DisplayEntry {
	t.mu.RLock()
	defer t.mu.RUnlock()
	e, ok := t.entries[sessionID]
	if !ok {
		return nil
	}
	return &DisplayEntry{
		Status:              e.Status,
		HasFailures:         e.HasFailures,
		HasChangesRequested: e.HasChangesRequested,
		IsRepairing:         e.IsRepairing,
		HeadSHA:             e.HeadSHA,
		UpdatedAt:           e.UpdatedAt,
	}
}

// GetBatch returns entries for multiple session IDs.
func (t *DisplayTracker) GetBatch(sessionIDs []string) map[string]*DisplayEntry {
	t.mu.RLock()
	defer t.mu.RUnlock()
	result := make(map[string]*DisplayEntry, len(sessionIDs))
	for _, id := range sessionIDs {
		e, ok := t.entries[id]
		if !ok {
			continue
		}
		result[id] = &DisplayEntry{
			Status:              e.Status,
			HasFailures:         e.HasFailures,
			HasChangesRequested: e.HasChangesRequested,
			IsRepairing:         e.IsRepairing,
			HeadSHA:             e.HeadSHA,
			UpdatedAt:           e.UpdatedAt,
		}
	}
	return result
}

// Remove deletes the entry for the given session ID.
func (t *DisplayTracker) Remove(sessionID string) {
	t.mu.Lock()
	delete(t.entries, sessionID)
	t.mu.Unlock()
	t.scheduleRecompute(sessionID)
}

// SetRepairing sets or clears the IsRepairing flag for a session without
// touching any other fields. Creates a zero-valued entry if none exists.
func (t *DisplayTracker) SetRepairing(sessionID string, repairing bool) {
	t.mu.Lock()
	if e, ok := t.entries[sessionID]; ok {
		e.IsRepairing = repairing
		e.UpdatedAt = time.Now()
	} else {
		t.entries[sessionID] = &DisplayEntry{IsRepairing: repairing, UpdatedAt: time.Now()}
	}
	t.mu.Unlock()
	t.scheduleRecompute(sessionID)
}

// SetOnChange sets the callback function that is called when a display status changes.
// The callback receives the session ID, old entry (may be nil), and new entry.
// The callback is invoked in a goroutine to avoid blocking the Set method.
func (t *DisplayTracker) SetOnChange(fn func(sessionID string, oldEntry, newEntry *DisplayEntry)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.onChange = fn
}

// SetRecomputer wires the DisplayStatusComputer that should be invoked after
// every successful mutation. Tests construct trackers without a computer; the
// nil-safe scheduleRecompute below makes that case a no-op.
func (t *DisplayTracker) SetRecomputer(r Recomputer) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.recomputer = r
}

// scheduleRecompute calls the wired Recomputer with a background context. It
// is intentionally synchronous so the new (label, intent, spinner) is on the
// session row before the calling RPC returns — the invariant the rest of the
// system relies on. Callers MUST NOT hold t.mu when calling.
func (t *DisplayTracker) scheduleRecompute(sessionID string) {
	t.mu.RLock()
	r := t.recomputer
	t.mu.RUnlock()
	if r == nil {
		return
	}
	_ = r.Recompute(context.Background(), sessionID)
}
