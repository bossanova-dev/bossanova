package status

import (
	"sync"
	"time"

	"github.com/recurser/bossalib/vcs"
)

// PRDisplayEntry is a cached display status for a single session's PR.
type PRDisplayEntry struct {
	Status              vcs.PRDisplayStatus
	HasFailures         bool
	HasChangesRequested bool
	UpdatedAt           time.Time
}

// PRTracker is a thread-safe in-memory cache of PR display statuses.
type PRTracker struct {
	mu       sync.RWMutex
	entries  map[string]*PRDisplayEntry // session ID -> entry
	onChange func(sessionID string, oldEntry, newEntry *PRDisplayEntry)
}

// NewPRTracker creates a new empty PRTracker.
func NewPRTracker() *PRTracker {
	return &PRTracker{
		entries: make(map[string]*PRDisplayEntry),
	}
}

// Set upserts a display status for the given session ID.
// If the status changes and an onChange callback is set, it will be called.
func (t *PRTracker) Set(sessionID string, info vcs.PRDisplayInfo) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Check if status changed
	oldEntry, existed := t.entries[sessionID]
	var oldStatus vcs.PRDisplayStatus
	if existed {
		oldStatus = oldEntry.Status
	}

	// Update entry
	newEntry := &PRDisplayEntry{
		Status:              info.Status,
		HasFailures:         info.HasFailures,
		HasChangesRequested: info.HasChangesRequested,
		UpdatedAt:           time.Now(),
	}
	t.entries[sessionID] = newEntry

	// Trigger callback if status changed
	if t.onChange != nil && (!existed || oldStatus != info.Status) {
		go t.onChange(sessionID, oldEntry, newEntry)
	}
}

// Get returns a copy of the cached entry for the given session ID, or nil if not found.
func (t *PRTracker) Get(sessionID string) *PRDisplayEntry {
	t.mu.RLock()
	defer t.mu.RUnlock()
	e, ok := t.entries[sessionID]
	if !ok {
		return nil
	}
	return &PRDisplayEntry{
		Status:              e.Status,
		HasFailures:         e.HasFailures,
		HasChangesRequested: e.HasChangesRequested,
		UpdatedAt:           e.UpdatedAt,
	}
}

// GetBatch returns entries for multiple session IDs.
func (t *PRTracker) GetBatch(sessionIDs []string) map[string]*PRDisplayEntry {
	t.mu.RLock()
	defer t.mu.RUnlock()
	result := make(map[string]*PRDisplayEntry, len(sessionIDs))
	for _, id := range sessionIDs {
		e, ok := t.entries[id]
		if !ok {
			continue
		}
		result[id] = &PRDisplayEntry{
			Status:              e.Status,
			HasFailures:         e.HasFailures,
			HasChangesRequested: e.HasChangesRequested,
			UpdatedAt:           e.UpdatedAt,
		}
	}
	return result
}

// Remove deletes the entry for the given session ID.
func (t *PRTracker) Remove(sessionID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, sessionID)
}

// SetOnChange sets the callback function that is called when a PR status changes.
// The callback receives the session ID, old entry (may be nil), and new entry.
// The callback is invoked in a goroutine to avoid blocking the Set method.
func (t *PRTracker) SetOnChange(fn func(sessionID string, oldEntry, newEntry *PRDisplayEntry)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.onChange = fn
}
