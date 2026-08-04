package main

// Progress-liveness probe (BOS-667).
//
// Pane content is not a progress signal: an animated spinner keeps the pane
// changing while the turn behind it is dead, so the daemon's content-change
// comparison reads a hung run as "working". The agent's own transcript is the
// only record of SEMANTIC progress, so this file answers "when did the last
// transcript record land, and what does that record leave the agent doing?".
//
// Two rules govern everything below:
//
//   - Fail open. Missing, unreadable, empty, or unclassifiable ⇒ known=false
//     and NO timestamp. The daemon raises nothing on known=false, and a false
//     "your session is dead" banner on a healthy long build is worse than the
//     silent stall this detects. Never fabricate a timestamp or a phase.
//   - Stat first. The daemon calls this on its 3 s tick for every live chat, so
//     an unchanged (size, mtime) returns the memoized snapshot without opening
//     the file, and a changed one reads only the trailing window.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// progressTailSize is the trailing byte window read when the transcript has
// moved. Claude writes one JSON object per line and a tool_result line can be
// large, so this is wider than transcriptTailSize's scan window; if no complete
// record fits inside it we fail open rather than widening further.
const progressTailSize = 64 * 1024

// progressCacheLimit bounds the memo map. One entry per live chat is the real
// working set; the cap only stops a long-lived daemon from accumulating
// entries for chats that have gone away. On overflow ONE arbitrary entry is
// evicted (random replacement) rather than the map being dropped wholesale: a
// wholesale drop at the cap would make a daemon sitting just above it re-read
// and re-parse every transcript on every tick, where single eviction keeps the
// hit rate at ~(limit-1)/limit. Correctness does not depend on either choice —
// a miss is only ever a re-read.
const progressCacheLimit = 512

// errTranscriptUnstable reports that the transcript changed shape between the
// caller's stat and the read — the file it describes is not the file the caller
// is about to memoize a snapshot for.
var errTranscriptUnstable = errors.New("transcript changed between stat and read")

// progressSnapshot is the probe's answer for one transcript. The zero value is
// the fail-open answer: no timestamp, UNKNOWN phase, known=false.
type progressSnapshot struct {
	lastProgressAt time.Time
	phase          bossanovav1.AgentProgressPhase
	known          bool
}

// progressCacheEntry memoizes a snapshot against the (size, mtime) it was read
// from.
type progressCacheEntry struct {
	size    int64
	modTime time.Time
	snap    progressSnapshot
}

// progressProbe memoizes per-transcript snapshots. The zero value is ready to
// use, so a Server built literally in tests needs no constructor.
type progressProbe struct {
	mu    sync.Mutex
	cache map[string]progressCacheEntry
}

// snapshot returns the progress snapshot for the transcript at path, reading
// the file only when its size or mtime has moved since the last call.
func (p *progressProbe) snapshot(path string) progressSnapshot {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Size() == 0 {
		// Missing, unreadable, a directory, or not yet written: fail open, and
		// drop any memo so a recreated transcript is not answered from a stale
		// snapshot.
		p.forget(path)
		return progressSnapshot{phase: bossanovav1.AgentProgressPhase_AGENT_PROGRESS_PHASE_UNKNOWN}
	}

	size, modTime := info.Size(), info.ModTime()

	p.mu.Lock()
	if entry, ok := p.cache[path]; ok && entry.size == size && entry.modTime.Equal(modTime) {
		p.mu.Unlock()
		return entry.snap
	}
	p.mu.Unlock()

	snap, err := readProgressTail(path)
	if err != nil {
		// The read failed rather than the CONTENT being unclassifiable, so this
		// snapshot says nothing about the bytes at this (size, mtime). Memoizing
		// it would pin known=false until the file next moves — and in the stall
		// this probe exists to detect the file never moves again, so a single
		// transient EACCES/EMFILE would suppress the detection permanently. Fail
		// open for this tick only and re-read on the next one.
		return snap
	}

	p.mu.Lock()
	if p.cache == nil {
		p.cache = make(map[string]progressCacheEntry, 16)
	}
	for len(p.cache) >= progressCacheLimit {
		for k := range p.cache { // map iteration order is unspecified: random victim
			delete(p.cache, k)
			break
		}
	}
	p.cache[path] = progressCacheEntry{size: size, modTime: modTime, snap: snap}
	p.mu.Unlock()

	return snap
}

func (p *progressProbe) forget(path string) {
	p.mu.Lock()
	delete(p.cache, path)
	p.mu.Unlock()
}

// readProgressTail reads the trailing window of the JSONL transcript at path
// and classifies the last COMPLETE record that says something about what the
// agent is doing. Two kinds of line are walked past on the way back: a partial
// JSON object (the poller can read the file mid-write, so the final line is
// often truncated) and a known bookkeeping record (see
// isInertProgressRecord) — real transcripts nearly always end in a run of
// those, so stopping at the first one would answer known=false for most live
// chats. An unrecognised record type still stops the walk and fails open.
//
// Both outcomes fail open, but they are NOT interchangeable to the caller: a
// non-nil error means the file could not be read (or changed under us), so the
// snapshot describes no particular file state and must not be memoized against
// one. A nil error means the bytes were read and classified — including a
// confident "unclassifiable", which is a property of the content and is safe to
// cache.
func readProgressTail(path string) (progressSnapshot, error) {
	unknown := progressSnapshot{phase: bossanovav1.AgentProgressPhase_AGENT_PROGRESS_PHASE_UNKNOWN}

	// filepath.Clean as the last transform before the read normalizes the
	// transcript path (built by transcriptPath, which rejects traversal in the
	// caller-supplied session id) and satisfies gosec G304.
	cleaned := filepath.Clean(path)
	f, err := os.Open(cleaned)
	if err != nil {
		return unknown, err
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return unknown, err
	}
	if info.IsDir() || info.Size() == 0 {
		// The caller's own Stat saw a non-empty regular file, so this contradicts
		// it: the transcript was replaced or truncated between the two calls and
		// the (size, mtime) the caller holds is already stale.
		return unknown, errTranscriptUnstable
	}

	var offset int64
	if info.Size() > progressTailSize {
		offset = info.Size() - progressTailSize
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return unknown, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return unknown, err
	}

	// When we seeked into the middle of the file the first line is almost
	// certainly partial — drop up to the first newline.
	if offset > 0 {
		nl := bytes.IndexByte(data, '\n')
		if nl < 0 {
			return unknown, nil
		}
		data = data[nl+1:]
	}

	// newestWrite is the timestamp of the most recent BOOKKEEPING record walked
	// past on the way back to the last semantic one. It is a liveness signal
	// even though it is not a phase signal: something in the process is still
	// appending, so the turn is not silently dead. Keeping it is what stops a
	// long auto-compaction — which emits `system` records for minutes while the
	// last semantic record ages — from reading as a stall.
	var newestWrite time.Time

	lines := bytes.Split(data, []byte{'\n'})
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		var rec transcriptLineWithTS
		if err := json.Unmarshal(line, &rec); err != nil {
			// Truncated / partially-written line — keep walking backwards to
			// the last complete record.
			continue
		}
		ts, tsErr := time.Parse(time.RFC3339Nano, rec.Timestamp)

		phase, ok := classifyProgressPhase(rec)
		if !ok {
			if !isInertProgressRecord(rec.Type) {
				// A shape a future Claude version adds, and we have no idea
				// what it leaves the agent doing. Fail open rather than guess.
				return unknown, nil
			}
			// Known bookkeeping. Real transcripts almost always end in a run of
			// these (`…assistant, system, system, last-prompt` is the single
			// most common tail), so bailing here would make the probe answer
			// known=false for most live chats. Walk past it to the last record
			// that actually says what the agent is doing.
			//
			// Walking backwards means the first timestamped one we see is the
			// newest.
			if tsErr == nil && newestWrite.IsZero() {
				newestWrite = ts
			}
			continue
		}
		if tsErr != nil {
			// Classified, but there is no trustworthy "when". A liveness
			// answer without a timestamp is not an answer.
			return unknown, nil
		}
		last := ts
		if newestWrite.After(last) {
			last = newestWrite
		}
		return progressSnapshot{lastProgressAt: last, phase: phase, known: true}, nil
	}
	return unknown, nil
}

// inertProgressRecordTypes are the transcript record types that carry no
// information about what the agent is doing. They are bookkeeping the CLI
// appends around the semantic conversation — the prompt echo, the mode banner,
// attachment manifests, queue operations, title/PR metadata, file-history
// entries — and every one of them was observed trailing real transcripts.
//
// This is an ALLOWLIST on purpose: an unrecognised type still fails open. The
// cost of guessing wrong about a type we have never seen is a false "your
// session is dead" banner, which the whole design is built to avoid.
var inertProgressRecordTypes = map[string]struct{}{
	"agent-name":            {},
	"ai-title":              {},
	"attachment":            {},
	"custom-title":          {},
	"file-history-delta":    {},
	"file-history-snapshot": {},
	"last-prompt":           {},
	"mode":                  {},
	"permission-mode":       {},
	"pr-link":               {},
	"queue-operation":       {},
	"summary":               {},
	"system":                {},
}

// isInertProgressRecord reports whether recType is known bookkeeping that the
// backwards walk may skip on its way to the last semantic record.
func isInertProgressRecord(recType string) bool {
	_, ok := inertProgressRecordTypes[recType]
	return ok
}

// classifyProgressPhase maps a transcript record to the phase it leaves the
// agent in. ok=false means "this record maps to no phase" — the caller then
// walks past it if it is known bookkeeping, and fails open if it is not.
func classifyProgressPhase(rec transcriptLineWithTS) (bossanovav1.AgentProgressPhase, bool) {
	switch rec.Type {
	case "user":
		// Claude logs tool results as type:"user" entries carrying
		// tool_result blocks. Both those and a genuine user turn leave the
		// agent owing an assistant message, i.e. a model request is in
		// flight — the shape a silently-dead turn leaves behind.
		return bossanovav1.AgentProgressPhase_AGENT_PROGRESS_PHASE_AWAITING_MODEL, true
	case "assistant":
		if contentHasBlockType(rec.Message.Content, "tool_use") {
			// A tool is running. A 15-minute build writes nothing to the
			// transcript until it returns, so this is legitimately quiet.
			return bossanovav1.AgentProgressPhase_AGENT_PROGRESS_PHASE_EXECUTING_TOOL, true
		}
		if contentHasBlockType(rec.Message.Content, "text") {
			return bossanovav1.AgentProgressPhase_AGENT_PROGRESS_PHASE_IDLE, true
		}
		// An assistant record with neither text nor a tool call (e.g. a
		// thinking-only block) says nothing about liveness.
		return bossanovav1.AgentProgressPhase_AGENT_PROGRESS_PHASE_UNKNOWN, false
	}
	return bossanovav1.AgentProgressPhase_AGENT_PROGRESS_PHASE_UNKNOWN, false
}

// contentHasBlockType reports whether a message's content carries a block of
// the given type. String content is plain text by definition.
func contentHasBlockType(raw json.RawMessage, want string) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return false
	}
	switch raw[0] {
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err == nil && s != "" {
			return want == "text"
		}
	case '[':
		var blocks []struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &blocks); err == nil {
			for _, b := range blocks {
				if b.Type == want {
					return true
				}
			}
		}
	}
	return false
}
