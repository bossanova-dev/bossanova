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
	ChangesRequestedBy  []string
	IsRepairing         bool
	SettingUp           bool
	// Merging is true while a PR merge (MergeSession) is in flight. Transient,
	// like SettingUp: set via SetMerging around the blocking merge and preserved
	// across polls in Set so a mid-merge poll can't clobber it.
	Merging bool
	HeadSHA string
	// Mergeable is the PR's last-polled mergeability (nil unknown / true
	// mergeable / false conflicting), surfaced on Session.pr_mergeable so a
	// conflict-after-green is readable without a merge attempt. Refreshed by
	// every poll via Set — unlike IsRepairing, it is not preserved across a
	// Set that omits it, because each poll carries the authoritative value.
	Mergeable *bool
	UpdatedAt time.Time
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
	var settingUp bool
	if existed {
		settingUp = oldEntry.SettingUp
	}
	var merging bool
	if existed {
		merging = oldEntry.Merging
	}
	newEntry := &DisplayEntry{
		Status:              info.Status,
		HasFailures:         info.HasFailures,
		HasChangesRequested: info.HasChangesRequested,
		ChangesRequestedBy:  info.ChangesRequestedBy,
		IsRepairing:         isRepairing,
		SettingUp:           settingUp,
		Merging:             merging,
		HeadSHA:             info.HeadSHA,
		Mergeable:           info.Mergeable,
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
		ChangesRequestedBy:  e.ChangesRequestedBy,
		IsRepairing:         e.IsRepairing,
		SettingUp:           e.SettingUp,
		Merging:             e.Merging,
		HeadSHA:             e.HeadSHA,
		Mergeable:           e.Mergeable,
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
			ChangesRequestedBy:  e.ChangesRequestedBy,
			IsRepairing:         e.IsRepairing,
			SettingUp:           e.SettingUp,
			Merging:             e.Merging,
			HeadSHA:             e.HeadSHA,
			Mergeable:           e.Mergeable,
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

// SetSettingUp sets or clears the transient "initializing" SettingUp flag for
// a session without touching any other fields. Setting the flag creates a
// zero-valued entry if none exists; clearing it removes an entry that exists
// ONLY because of this flag (no polled PR status), so a zero-Status
// placeholder does not linger. That matters because other paths treat entry
// presence as authoritative — e.g. MergeSession allows the merge only when
// Get returns nil — and a lingering empty entry would mis-read a passing PR as
// "not passing". An entry that already carries real PR status (or IsRepairing)
// is preserved with only SettingUp toggled off. The synchronous
// scheduleRecompute publishes the new (label, intent, spinner) to clients
// before the caller continues.
func (t *DisplayTracker) SetSettingUp(sessionID string, settingUp bool) {
	t.mu.Lock()
	if e, ok := t.entries[sessionID]; ok {
		e.SettingUp = settingUp
		e.UpdatedAt = time.Now()
		if !settingUp && e.isEmpty() {
			delete(t.entries, sessionID)
		}
	} else if settingUp {
		t.entries[sessionID] = &DisplayEntry{SettingUp: settingUp, UpdatedAt: time.Now()}
	}
	t.mu.Unlock()
	t.scheduleRecompute(sessionID)
}

// SetMerging sets or clears the transient "merging" flag for a session while a
// PR merge (MergeSession) is in flight, mirroring SetSettingUp exactly. Setting
// creates a zero-valued entry if none exists; clearing removes an entry that
// exists ONLY because of this flag (no polled PR status), so a zero-Status
// placeholder does not linger and mis-read a passing PR as "not passing". An
// entry carrying real PR status (or another flag) is preserved with only
// Merging toggled off. The synchronous scheduleRecompute publishes the new
// (label, intent, spinner) to clients before the caller continues — so a merge
// in flight streams "merging" for its full duration.
func (t *DisplayTracker) SetMerging(sessionID string, merging bool) {
	t.mu.Lock()
	if e, ok := t.entries[sessionID]; ok {
		e.Merging = merging
		e.UpdatedAt = time.Now()
		if !merging && e.isEmpty() {
			delete(t.entries, sessionID)
		}
	} else if merging {
		t.entries[sessionID] = &DisplayEntry{Merging: merging, UpdatedAt: time.Now()}
	}
	t.mu.Unlock()
	t.scheduleRecompute(sessionID)
}

// isEmpty reports whether the entry carries no state at all — no polled PR
// status and no transient flag set — so a placeholder left behind after a
// transient flag (SettingUp/Merging) is cleared can be dropped rather than
// lingering as a zero-Status entry that would mis-read a passing PR as "not
// passing". Both SetSettingUp and SetMerging use it after toggling their flag
// off, at which point the just-cleared flag is already false.
func (e *DisplayEntry) isEmpty() bool {
	return e.Status == vcs.DisplayStatusUnspecified &&
		!e.HasFailures && !e.HasChangesRequested &&
		!e.IsRepairing && !e.SettingUp && !e.Merging && e.HeadSHA == ""
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
