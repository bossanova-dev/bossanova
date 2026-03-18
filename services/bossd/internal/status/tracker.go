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
}

// Tracker is a thread-safe in-memory cache of chat process statuses.
type Tracker struct {
	mu      sync.RWMutex
	entries map[string]*Entry // claude_id -> entry
}

// NewTracker creates a new empty Tracker.
func NewTracker() *Tracker {
	return &Tracker{
		entries: make(map[string]*Entry),
	}
}

// Update upserts a heartbeat for the given claude ID.
func (t *Tracker) Update(claudeID string, status pb.ChatStatus, lastOutputAt time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries[claudeID] = &Entry{
		Status:       status,
		LastOutputAt: lastOutputAt,
		ReceivedAt:   time.Now(),
	}
}

// Get returns the cached entry for the given claude ID, or nil if not found
// or stale (older than StaleThreshold).
func (t *Tracker) Get(claudeID string) *Entry {
	t.mu.RLock()
	defer t.mu.RUnlock()
	e, ok := t.entries[claudeID]
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
func (t *Tracker) GetBatch(claudeIDs []string) map[string]*Entry {
	t.mu.RLock()
	defer t.mu.RUnlock()
	result := make(map[string]*Entry, len(claudeIDs))
	now := time.Now()
	for _, id := range claudeIDs {
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
			}
		}
	}
	return result
}

// Remove deletes the entry for the given claude ID.
func (t *Tracker) Remove(claudeID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, claudeID)
}

// Cleanup removes all stale entries (older than StaleThreshold).
func (t *Tracker) Cleanup() {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	for id, e := range t.entries {
		if now.Sub(e.ReceivedAt) > StaleThreshold {
			delete(t.entries, id)
		}
	}
}
