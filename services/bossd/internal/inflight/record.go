// Package inflight records which chats are holding an in-flight proxied stream,
// durably enough that a hard-killed daemon (SIGKILL, OOM, panic, power loss)
// leaves behind an accurate list of the streams its death severed.
//
// The failover proxy (BOS-409) is in-process, so the proxy's lifetime IS the
// agent's connection lifetime: when bossd dies mid-response every open stream is
// cut at once, and the agent panes are left parked on a half-finished turn with
// nothing on screen that a poller can detect. The pane never re-renders, so the
// transient-API-error banner that drives the BOS-518 auto-resume lane may never
// appear at all. This package is the missing evidence: the set of chats that
// were mid-stream at the moment of death, read back on the next startup.
//
// The write policy is the whole design. Persisting lazily (on a ticker, or only
// at shutdown) loses exactly the case this exists for, because a hard kill runs
// no shutdown code — so the write has to be synchronous, and it has to happen
// while the stream is open. The record is therefore refcounted and written when
// set membership changes: the 0→1 add and the 1→0 removal.
//
// Be honest about what that costs. Refcounting amortises only across OVERLAPPING
// requests — a chat whose REPL has several calls in flight at once pays two
// writes for the whole episode however many requests it pipelines. Requests that
// do NOT overlap each open and close their own episode, so they each pay a write
// AND a rename, cheap requests included; which requests reach this path at all
// is decided by the proxy's routing, so see proxy_server.go for that, not this
// comment. The floor is two writes per non-overlapping proxied request, not two
// per burst of activity.
//
// That is accepted rather than optimised away, on this specific path only. Each
// write is a small file into the page cache plus a rename in the same directory,
// sitting in front of a call that is about to make a network round trip to
// api.anthropic.com and stream a model response back. It is noise against what
// it guards, and the alternatives all cost durability: coalescing or deferring
// the write reopens precisely the window where a SIGKILL leaves a live stream
// unrecorded, which is the entire failure this package exists to prevent.
// Skipping "unchanged" writes is not available
// either — every write here happens BECAUSE the set changed.
//
// No fsync: a SIGKILL leaves the page cache intact, and the failure modes that
// do not (power loss, kernel panic) are the ones where an over-eager resume is
// least appropriate anyway.
//
// Writes are SEQUENCED, not merely serialized. Every membership change takes a
// ticket under the state lock and the writer commits tickets in order, dropping
// any that a newer one has already overtaken. Snapshotting under the lock and
// then writing outside it would otherwise let two goroutines reach the file in
// the opposite order to the mutations that produced their snapshots — landing
// an older set last, which either erases a stream that is still live (a hard
// kill then leaves it silently unrecoverable, the exact failure this package
// exists to prevent) or resurrects one that already finished.
package inflight

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// FileName is the record's basename inside the daemon's app-data directory.
const FileName = "inflight-streams.json"

// Path returns the record's absolute path for a daemon whose app data lives in
// appDataDir (the directory holding bossd.db). Empty appDataDir ⇒ "" , which
// NewRecorder treats as "no persistence wired".
func Path(appDataDir string) string {
	if appDataDir == "" {
		return ""
	}
	return filepath.Join(appDataDir, FileName)
}

// record is the on-disk shape. WrittenAt is diagnostic only — never a decision
// input. The severance instant that matters is when the NEXT daemon notices the
// record, not when the dead one last wrote it, because a restart re-seeds every
// pane's last-output time and comparing against a pre-crash stamp would read
// every survivor as "recovered".
type record struct {
	WrittenAt       time.Time `json:"written_at"`
	AgentSessionIDs []string  `json:"agent_session_ids"`
}

// Recorder maintains the live in-flight set and mirrors it to disk. A nil
// *Recorder is a valid no-op, so a daemon built without an app-data directory
// keeps serving proxy traffic with the feature simply switched off.
type Recorder struct {
	path   string
	logger zerolog.Logger

	mu sync.Mutex
	// counts is a refcount per display id, not a set: one chat can hold several
	// concurrent proxied requests (the REPL pipelines them), and a set would let
	// the first one to finish erase a sibling that is still streaming.
	counts map[string]int
	// sealed freezes disk writes while leaving the in-memory count live. Set by
	// Seal() during a graceful drain, where the set must stop tracking the
	// handlers that the shutdown itself is about to cut.
	sealed bool
	// seq is the ticket counter. It is bumped under mu at the instant a snapshot
	// is taken, so ticket order IS mutation order.
	seq uint64

	// writeMu serializes the file writes themselves and guards written. It is
	// deliberately a SECOND lock: holding mu across the write would put a
	// synchronous rename on the critical section that every Enter/Leave in the
	// daemon contends for, turning a per-episode disk cost into a per-request
	// one.
	writeMu sync.Mutex
	// written is the highest ticket already committed to disk. A write holding
	// an older ticket is dropped rather than applied, because the newer one
	// already reflects everything the older one knew plus the mutation that
	// followed it.
	written uint64
}

// NewRecorder returns a Recorder persisting to path. An empty path yields nil —
// the no-op Recorder — so callers need no conditional.
func NewRecorder(path string, logger zerolog.Logger) *Recorder {
	if path == "" {
		return nil
	}
	return &Recorder{path: path, logger: logger, counts: map[string]int{}}
}

// Enter records that id has opened a proxied stream. Only the 0→1 transition
// touches disk.
func (r *Recorder) Enter(id string) {
	if r == nil || id == "" {
		return
	}
	r.mu.Lock()
	r.counts[id]++
	write := r.counts[id] == 1 && !r.sealed
	var ids []string
	var ticket uint64
	if write {
		r.seq++
		ticket, ids = r.seq, r.snapshotLocked()
	}
	r.mu.Unlock()
	if write {
		r.persist(ticket, ids)
	}
}

// Leave records that id's proxied stream has ended. Only the 1→0 transition
// touches disk. Unbalanced calls are ignored rather than driving the count
// negative: a handler that somehow leaves twice must not erase a live sibling.
func (r *Recorder) Leave(id string) {
	if r == nil || id == "" {
		return
	}
	r.mu.Lock()
	n, ok := r.counts[id]
	if !ok || n <= 0 {
		r.mu.Unlock()
		return
	}
	n--
	if n == 0 {
		delete(r.counts, id)
	} else {
		r.counts[id] = n
	}
	write := n == 0 && !r.sealed
	var ids []string
	var ticket uint64
	if write {
		r.seq++
		ticket, ids = r.seq, r.snapshotLocked()
	}
	r.mu.Unlock()
	if write {
		r.persist(ticket, ids)
	}
}

// Seal writes the current set and freezes all further disk writes.
//
// It exists for one ordering hazard in the graceful path. A drain that gives up
// closes the listener and cuts every surviving handler; each cut handler runs
// its deferred Leave, so by the time the process exits the record would read
// EMPTY — precisely erasing the streams the shutdown just severed. Sealing
// first pins the set as it stood when the daemon decided to stop waiting, which
// is the set the next startup must recover. A graceful drain that finished
// cleanly seals an already-empty set, so the file is removed and the steady
// state after a clean stop remains "no file".
//
// Sealing takes the LAST ticket, and that is what makes the pin hold. Setting
// the flag alone would not: a Leave already past its own sealed check, holding
// an older ticket and not yet at the file, would otherwise land after Seal and
// overwrite — or os.Remove — the very set Seal just pinned. Because Seal's
// ticket is higher than every ticket outstanding when it ran, and no ticket is
// issued after it, any such straggler is dropped by the writer.
func (r *Recorder) Seal() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.sealed {
		r.mu.Unlock()
		return
	}
	r.sealed = true
	r.seq++
	ticket, ids := r.seq, r.snapshotLocked()
	r.mu.Unlock()
	r.persist(ticket, ids)
}

// Snapshot returns the live in-flight display ids, sorted. Test/diagnostic seam.
func (r *Recorder) Snapshot() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshotLocked()
}

func (r *Recorder) snapshotLocked() []string {
	ids := make([]string, 0, len(r.counts))
	for id := range r.counts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// persist mirrors ids to disk, removing the file when the set is empty so a
// clean shutdown leaves nothing for the next startup to recover. Every failure
// is logged and swallowed: this runs on the proxy request path, and a chat that
// cannot be recorded must still be served.
//
// ticket orders this write against its concurrent siblings. Writes commit under
// writeMu, and one holding a ticket at or below the last committed one is
// dropped: a newer snapshot is already on disk, and applying an older one on
// top of it is precisely how a live stream gets erased or a finished one comes
// back. Dropping is safe because the winner's snapshot was taken later under
// the same state lock, so it already includes everything this one saw.
func (r *Recorder) persist(ticket uint64, ids []string) {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	if ticket <= r.written {
		return
	}
	r.written = ticket
	if len(ids) == 0 {
		if err := os.Remove(r.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			r.logger.Debug().Err(err).Str("path", r.path).
				Msg("in-flight stream record: failed to remove empty record")
		}
		return
	}
	if err := write(r.path, ids); err != nil {
		r.logger.Warn().Err(err).Str("path", r.path).Int("streams", len(ids)).
			Msg("in-flight stream record: failed to persist; a hard kill would not be recoverable")
	}
}

// write atomically replaces the record file: temp file in the same directory,
// 0600, then rename. Mirrors lib/bossalib/daemonstate so a torn read is
// impossible even if the daemon is killed mid-write.
func write(path string, ids []string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create record dir: %w", err)
	}
	data, err := json.Marshal(record{WrittenAt: time.Now(), AgentSessionIDs: ids})
	if err != nil {
		return fmt.Errorf("marshal record: %w", err)
	}
	tmp, err := os.CreateTemp(dir, FileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp record: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once the rename succeeds
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp record: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp record: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp record: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename record: %w", err)
	}
	return nil
}

// Restore puts ids that ReadAndClear removed but nothing ever consumed back on
// disk, so a startup that dies between the read and the resumer's trigger does
// not lose them permanently. The read is destructive by design (see
// ReadAndClear), which means the ONLY copy of those ids between those two points
// is a local variable in the startup path; any early return there is a silent,
// unrecoverable loss without this.
//
// It MERGES with whatever is on disk rather than overwriting. By the time this
// runs the new proxy may already have recorded streams of its own, and those are
// about to be severed by the very failure that is unwinding startup — clobbering
// them would trade one loss for another. A record that is present but
// UNPARSEABLE is treated as empty: it was already unusable, and the ids in hand
// are strictly better than nothing. A read that fails for any other reason
// (permissions, I/O) is different — it returns the error before writing
// anything, on the grounds that we cannot tell what we would be clobbering.
//
// Best-effort by nature, and it bypasses Recorder's write sequencing: it exists
// for a process that is on its way out, where a racing Recorder write is both
// unlikely and no worse than the loss it replaces. An empty path or an empty id
// list is a no-op.
func Restore(path string, ids []string) error {
	if path == "" || len(ids) == 0 {
		return nil
	}
	merged := map[string]struct{}{}
	if data, err := os.ReadFile(path); err == nil {
		var existing record
		if json.Unmarshal(data, &existing) == nil {
			for _, id := range existing.AgentSessionIDs {
				if id != "" {
					merged[id] = struct{}{}
				}
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read in-flight record: %w", err)
	}
	for _, id := range ids {
		if id != "" {
			merged[id] = struct{}{}
		}
	}
	if len(merged) == 0 {
		return nil
	}
	out := make([]string, 0, len(merged))
	for id := range merged {
		out = append(out, id)
	}
	sort.Strings(out)
	return write(path, out)
}

// ReadAndClear reads the previous daemon's record and deletes it, returning the
// agent session ids whose streams that daemon's death severed. A missing file
// (the normal case after a clean shutdown) returns nil with no error.
//
// The clear is not optional and its ORDERING is load-bearing: this must run
// before the new proxy starts recording, or the delete would drop entries the
// fresh daemon had already added. Clearing also makes the recovery one-shot —
// a daemon that crashes again during startup must not inherit a growing
// backlog of stale severance claims.
func ReadAndClear(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read in-flight record: %w", err)
	}
	// Remove first: a record we cannot parse is still a record we must not read
	// twice, so the delete happens whatever the contents turn out to be.
	if rerr := os.Remove(path); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
		return nil, fmt.Errorf("clear in-flight record: %w", rerr)
	}
	var rec record
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("parse in-flight record: %w", err)
	}
	ids := make([]string, 0, len(rec.AgentSessionIDs))
	for _, id := range rec.AgentSessionIDs {
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}
