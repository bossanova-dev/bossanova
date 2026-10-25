package status

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/recurser/bossalib/kit/vcs"
)

// DisplayEntry is a cached display status for a single session.
type DisplayEntry struct {
	Status vcs.DisplayStatus
	// HasBuildReceipt is refreshed on each poll; it is never latched across heads.
	HasBuildReceipt bool
	// ReceiptHeadSHA is deliberately latched: the last head a Set saw carrying
	// a build receipt. Unlike HasBuildReceipt it survives a Set for a new,
	// unreceipted head, so whichever tracker writer sees a push first cannot
	// erase the receipted ancestor the display poller carries the receipt from
	// (BOS-1452). Daemon-internal and in-memory only.
	ReceiptHeadSHA      string
	HasFailures         bool
	HasChangesRequested bool
	ChangesRequestedBy  []string
	IsRepairing         bool
	SettingUp           bool
	// Merging is true while any merge holds the session (HoldMerging). It is
	// derived from the tracker's mergeHolds, so neither a poll nor Remove can
	// clobber it mid-merge.
	Merging bool
	// Archiving is true while an archive (Lifecycle.ArchiveSession) is in flight
	// for this session. Transient, like Merging: set via SetArchiving around the
	// archive chokepoint and preserved across polls in Set so a mid-archive poll
	// can't clobber it. Surfaced on Session.archive_pending so the TUI renders
	// "Archiving…" only when the daemon actually has an archive running (BOS-425).
	Archiving bool
	HeadSHA   string
	// Mergeable is the PR's last-polled mergeability (nil unknown / true
	// mergeable / false conflicting), surfaced on Session.pr_mergeable so a
	// conflict-after-green is readable without a merge attempt. Refreshed by
	// every poll via Set — unlike IsRepairing, it is not preserved across a
	// Set that omits it, because each poll carries the authoritative value.
	Mergeable *bool
	// VerifyReason is the verify stage's park reason, set only when Status is
	// DisplayStatusNeedsHuman (and possibly empty there). Like Mergeable it is
	// refreshed by every poll via Set, not preserved.
	VerifyReason string
	// VerifyLabel is the operator-written label the park carries after its
	// codes (empty when it names none). Refreshed by every poll, like
	// VerifyReason. Cached for the session read path only: no client
	// projection reads it yet (HydrateVerifyAttention names codes alone).
	VerifyLabel string
	// BlockedRunWorkingAt is the block (the session's state_entered_at) during
	// which the run's own chat was last seen working on a turn the block did not
	// see, or parked on a callback it armed in that block; zero when it was not.
	// Set by the blocked-run sweep, kept across polls, and read by
	// session.BlockedAlertFacts.
	BlockedRunWorkingAt time.Time
	UpdatedAt           time.Time
	// Restored marks an entry Restore seeded from the last persisted poll and
	// no live poll has replaced: the first live Set reports its status as new,
	// and nothing may act on it as the PR's current state.
	Restored bool
}

// ParkCodes returns the verify park's codes for displaystatus.Input.ParkCodes:
// the comma-joined VerifyReason of a NeedsHuman entry, or nil for a nil entry
// or any other status.
func (e *DisplayEntry) ParkCodes() []string {
	if e == nil || e.Status != vcs.DisplayStatusNeedsHuman || e.VerifyReason == "" {
		return nil
	}
	return strings.Split(e.VerifyReason, ",")
}

// DisplayTracker is a thread-safe in-memory cache of session display statuses.
type DisplayTracker struct {
	mu         sync.RWMutex
	entries    map[string]*DisplayEntry // session ID -> entry
	onChange   func(sessionID string, oldEntry, newEntry *DisplayEntry)
	recomputer Recomputer
	// mergeHolds counts the merges holding each session: the operator merge
	// wraps MergeSession, and a second MergeSession can start under the first,
	// so Merging drops only when the last hold is released. It lives beside
	// entries, not in them, so Remove and recreation keep the count.
	mergeHolds map[string]int
}

// NewDisplayTracker creates a new empty DisplayTracker.
func NewDisplayTracker() *DisplayTracker {
	return &DisplayTracker{
		entries:    make(map[string]*DisplayEntry),
		mergeHolds: make(map[string]int),
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
	var archiving bool
	if existed {
		archiving = oldEntry.Archiving
	}
	var receiptHeadSHA string
	if existed {
		receiptHeadSHA = oldEntry.ReceiptHeadSHA
	}
	var blockedRunWorkingAt time.Time
	if existed {
		blockedRunWorkingAt = oldEntry.BlockedRunWorkingAt
	}
	if info.HasBuildReceipt && info.HeadSHA != "" {
		receiptHeadSHA = info.HeadSHA
	} else if info.BuildReceiptSeen && info.HeadSHA != "" && info.HeadSHA != receiptHeadSHA {
		// A pending or failed boss/build on a newer head supersedes the
		// latched success: carrying from it would skip that head's verdict.
		receiptHeadSHA = ""
	}
	newEntry := &DisplayEntry{
		Status:              info.Status,
		HasBuildReceipt:     info.HasBuildReceipt,
		ReceiptHeadSHA:      receiptHeadSHA,
		HasFailures:         info.HasFailures,
		HasChangesRequested: info.HasChangesRequested,
		ChangesRequestedBy:  info.ChangesRequestedBy,
		IsRepairing:         isRepairing,
		SettingUp:           settingUp,
		Merging:             t.mergeHolds[sessionID] > 0,
		Archiving:           archiving,
		HeadSHA:             info.HeadSHA,
		Mergeable:           info.Mergeable,
		VerifyReason:        info.VerifyReason,
		VerifyLabel:         info.VerifyLabel,
		BlockedRunWorkingAt: blockedRunWorkingAt,
		UpdatedAt:           time.Now(),
	}
	t.entries[sessionID] = newEntry

	// Capture references we need under the lock, then release before
	// firing callbacks. The Recomputer reads via Get() which takes RLock
	// itself — calling it while still holding the write lock would
	// deadlock as soon as RWMutex serialises the readers.
	statusChanged := !existed || oldEntry.Restored || oldStatus != info.Status
	onChange := t.onChange
	t.mu.Unlock()

	if onChange != nil && statusChanged {
		go onChange(sessionID, oldEntry, newEntry)
	}
	t.scheduleRecompute(sessionID)
}

// Restore seeds a session's entry with the status the daemon last persisted,
// dated when it was polled, so a restarted daemon serves what it last saw
// rather than nothing until the PR is read again. It never replaces an entry a
// live writer already set, and it neither reports a change nor recomputes:
// nothing changed. The first live Set reports the status as new, as it would
// on an empty tracker.
func (t *DisplayTracker) Restore(sessionID string, info vcs.DisplayInfo, polledAt time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.entries[sessionID]; ok {
		return
	}
	t.entries[sessionID] = &DisplayEntry{
		Status:              info.Status,
		HasBuildReceipt:     info.HasBuildReceipt,
		HasFailures:         info.HasFailures,
		HasChangesRequested: info.HasChangesRequested,
		Merging:             t.mergeHolds[sessionID] > 0,
		HeadSHA:             info.HeadSHA,
		Mergeable:           info.Mergeable,
		VerifyReason:        info.VerifyReason,
		VerifyLabel:         info.VerifyLabel,
		UpdatedAt:           polledAt,
		Restored:            true,
	}
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
		HasBuildReceipt:     e.HasBuildReceipt,
		ReceiptHeadSHA:      e.ReceiptHeadSHA,
		HasFailures:         e.HasFailures,
		HasChangesRequested: e.HasChangesRequested,
		ChangesRequestedBy:  e.ChangesRequestedBy,
		IsRepairing:         e.IsRepairing,
		SettingUp:           e.SettingUp,
		Merging:             e.Merging,
		Archiving:           e.Archiving,
		HeadSHA:             e.HeadSHA,
		Mergeable:           e.Mergeable,
		VerifyReason:        e.VerifyReason,
		VerifyLabel:         e.VerifyLabel,
		BlockedRunWorkingAt: e.BlockedRunWorkingAt,
		UpdatedAt:           e.UpdatedAt,
		Restored:            e.Restored,
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
			HasBuildReceipt:     e.HasBuildReceipt,
			ReceiptHeadSHA:      e.ReceiptHeadSHA,
			HasFailures:         e.HasFailures,
			HasChangesRequested: e.HasChangesRequested,
			ChangesRequestedBy:  e.ChangesRequestedBy,
			IsRepairing:         e.IsRepairing,
			SettingUp:           e.SettingUp,
			Merging:             e.Merging,
			Archiving:           e.Archiving,
			HeadSHA:             e.HeadSHA,
			Mergeable:           e.Mergeable,
			VerifyReason:        e.VerifyReason,
			VerifyLabel:         e.VerifyLabel,
			BlockedRunWorkingAt: e.BlockedRunWorkingAt,
			UpdatedAt:           e.UpdatedAt,
			Restored:            e.Restored,
		}
	}
	return result
}

// Remove deletes the entry for the given session ID. A session a merge still
// holds keeps a merging placeholder.
func (t *DisplayTracker) Remove(sessionID string) {
	t.mu.Lock()
	delete(t.entries, sessionID)
	if t.mergeHolds[sessionID] > 0 {
		t.entries[sessionID] = &DisplayEntry{Merging: true, UpdatedAt: time.Now()}
	}
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

// HoldMerging marks a session as merging until the returned release runs.
// Holds count, so a nested or concurrent merge on the same session keeps
// "merging" until the last one releases; release is idempotent. The first hold
// creates a zero-valued entry if none exists, and the last release drops an
// entry that exists only because of the hold, so a zero-Status placeholder
// does not linger and mis-read a passing PR as "not passing". Each change
// recomputes synchronously, so clients see "merging" before the caller
// continues.
func (t *DisplayTracker) HoldMerging(sessionID string) (release func()) {
	t.addMergeHold(sessionID, 1)
	var once sync.Once
	return func() {
		once.Do(func() { t.addMergeHold(sessionID, -1) })
	}
}

func (t *DisplayTracker) addMergeHold(sessionID string, delta int) {
	t.mu.Lock()
	holds := max(t.mergeHolds[sessionID]+delta, 0)
	if holds > 0 {
		t.mergeHolds[sessionID] = holds
	} else {
		delete(t.mergeHolds, sessionID)
	}
	e, ok := t.entries[sessionID]
	switch {
	case ok:
		e.Merging = holds > 0
		e.UpdatedAt = time.Now()
		if e.isEmpty() {
			delete(t.entries, sessionID)
		}
	case holds > 0:
		t.entries[sessionID] = &DisplayEntry{Merging: true, UpdatedAt: time.Now()}
	}
	t.mu.Unlock()
	t.scheduleRecompute(sessionID)
}

// SetArchiving sets or clears the transient "archiving" flag for a session
// while an archive (Lifecycle.ArchiveSession) is in flight. Setting creates a
// zero-valued entry if none exists; clearing removes an entry that exists ONLY
// because of this flag (no polled PR status), so a zero-Status placeholder does
// not linger and mis-read a passing PR as "not passing". An entry carrying real
// PR status (or another flag) is preserved with only Archiving toggled off.
// Unlike SettingUp/Merging — which the display computer turns into a
// daemon-computed label/intent — Archiving is surfaced
// only as Session.archive_pending (convert.go / display_computer.go); the
// "Archiving…" spinner is rendered client-side by the TUI. The synchronous
// scheduleRecompute publishes the refreshed session (carrying archive_pending)
// to clients before the caller continues — so an archive in flight streams the
// signal for its full duration (BOS-425).
func (t *DisplayTracker) SetArchiving(sessionID string, archiving bool) {
	t.mu.Lock()
	if e, ok := t.entries[sessionID]; ok {
		e.Archiving = archiving
		e.UpdatedAt = time.Now()
		if !archiving && e.isEmpty() {
			delete(t.entries, sessionID)
		}
	} else if archiving {
		t.entries[sessionID] = &DisplayEntry{Archiving: archiving, UpdatedAt: time.Now()}
	}
	t.mu.Unlock()
	t.scheduleRecompute(sessionID)
}

// SetBlockedRunWorking records the block during which the session's run is
// working or parked on a callback it armed in it; a zero blockedAt clears it.
// It reports whether the record changed, and recomputes only then. Setting
// creates an entry when none exists; clearing drops an entry that held nothing
// else.
func (t *DisplayTracker) SetBlockedRunWorking(sessionID string, blockedAt time.Time) bool {
	t.mu.Lock()
	e, ok := t.entries[sessionID]
	switch {
	case ok && e.BlockedRunWorkingAt.Equal(blockedAt):
		t.mu.Unlock()
		return false
	case ok:
		e.BlockedRunWorkingAt = blockedAt
		e.UpdatedAt = time.Now()
		if e.isEmpty() {
			delete(t.entries, sessionID)
		}
	case blockedAt.IsZero():
		t.mu.Unlock()
		return false
	default:
		t.entries[sessionID] = &DisplayEntry{BlockedRunWorkingAt: blockedAt, UpdatedAt: time.Now()}
	}
	t.mu.Unlock()
	t.scheduleRecompute(sessionID)
	return true
}

// isEmpty reports whether the entry carries no state at all — no polled PR
// status and no transient flag set — so a placeholder left behind after a
// transient flag (SettingUp/Merging/Archiving) is cleared can be dropped rather
// than lingering as a zero-Status entry that would mis-read a passing PR as "not
// passing". SetSettingUp, SetArchiving, and the last merge release use it after
// clearing their flag, at which point the just-cleared flag is already false.
func (e *DisplayEntry) isEmpty() bool {
	return e.Status == vcs.DisplayStatusUnspecified &&
		!e.HasFailures && !e.HasChangesRequested &&
		!e.IsRepairing && !e.SettingUp && !e.Merging && !e.Archiving && e.HeadSHA == "" &&
		e.BlockedRunWorkingAt.IsZero()
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
