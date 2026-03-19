package status

import (
	"sync"
	"time"

	"github.com/recurser/bossalib/vcs"
)

// PRDisplayEntry is a cached display status for a single session's PR.
type PRDisplayEntry struct {
	Status      vcs.PRDisplayStatus
	HasFailures bool
	UpdatedAt   time.Time
}

// PRTracker is a thread-safe in-memory cache of PR display statuses.
type PRTracker struct {
	mu      sync.RWMutex
	entries map[string]*PRDisplayEntry // session ID -> entry
}

// NewPRTracker creates a new empty PRTracker.
func NewPRTracker() *PRTracker {
	return &PRTracker{
		entries: make(map[string]*PRDisplayEntry),
	}
}

// Set upserts a display status for the given session ID.
func (t *PRTracker) Set(sessionID string, info vcs.PRDisplayInfo) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries[sessionID] = &PRDisplayEntry{
		Status:      info.Status,
		HasFailures: info.HasFailures,
		UpdatedAt:   time.Now(),
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
		Status:      e.Status,
		HasFailures: e.HasFailures,
		UpdatedAt:   e.UpdatedAt,
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
			Status:      e.Status,
			HasFailures: e.HasFailures,
			UpdatedAt:   e.UpdatedAt,
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
