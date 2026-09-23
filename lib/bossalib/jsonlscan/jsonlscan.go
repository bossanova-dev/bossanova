// Package jsonlscan reads newline-delimited JSON under a bounded parse budget.
//
// It replaces the `bufio.Scanner` + `Buffer(initial, ceiling)` shape that every
// agent-JSONL reader in this repo used to carry. That shape turns line length
// into a correctness cliff: a record one byte over the ceiling fails the whole
// read with a bare `bufio.Scanner: token too long`, so the only remedy is to
// raise the ceiling — and the ceiling has already been raised once, from
// 256 KiB to 8 MiB against a measured 742 KiB record, only to be exceeded six
// days later by a 12.83 MiB one.
//
// The mechanism here removes the cliff instead of moving it. A record longer
// than MaxLineBytes is drained to the next newline WITHOUT ever being held
// whole, counted, and skipped; every record that fits the budget is returned;
// the read reaches EOF. Callers that need to know ask Skipped().
//
// Skipping is safe for this file family because every over-long record
// observed has been a `function_call_output` / `custom_tool_call_output`
// envelope, and every consumer in this repo discards those: the codex
// transcript reader keeps only `event_msg`, and telemetry reads only the type
// discriminator and token counts, both of which sit in the first few hundred
// bytes. Buffering 13 MiB in order to `continue` past it buys nothing.
//
// Allocation is bounded for the lifetime of a Reader, not per line: one read
// chunk (ChunkBytes) plus at most one accumulator, allocated once and reused.
// The accumulator does not grow — it is allocated straight at the budget the
// first time a record spans more than one chunk. So a file whose records all
// fit a single chunk never allocates it at all, and a file carrying any record
// over ChunkBytes pays the whole budget once.
//
// That eagerness is deliberate, not an oversight: growing geometrically from
// the chunk size would cost more CUMULATIVE allocation on the way to the
// budget than one allocation at the budget, and cumulative allocation is what
// the peak-allocation bound in this package's tests measures.
package jsonlscan

import (
	"bufio"
	"io"
)

const (
	// ChunkBytes is the size of the single buffered read window. It is also
	// the largest record that costs no accumulator allocation at all.
	ChunkBytes = 64 * 1024

	// MaxLineBytes is the parse budget: a record longer than this is skipped
	// rather than parsed.
	//
	// It is deliberately the value this path already accepted — 8 MiB — and
	// not a fresh round number. 256 KiB is the value this path already
	// rejected as too small. Picking either of the two values the path has
	// already judged keeps the bound honest; picking a third would restate
	// the guess that produced the defect.
	//
	// The obligation attached to this number is now far weaker than it was:
	// it is the point at which a record is skipped rather than parsed, not
	// the point at which the reader fails. Exceeding it costs a counted skip,
	// not a failed read, so it can no longer become the next incident.
	MaxLineBytes = 8 * 1024 * 1024
)

// Reader yields the records of a JSONL stream that fit the parse budget and
// counts the ones that do not.
//
// Its shape is deliberately the subset of *bufio.Scanner that every call site
// in this repo actually used — Scan/Bytes/Err — so conversion is mechanical
// and no site is tempted to keep a private ceiling. Bytes() is valid only
// until the next call to Scan, exactly as with bufio.Scanner.
type Reader struct {
	br      *bufio.Reader
	max     int
	acc     []byte // lazily allocated once, at cap == max; reused across lines
	line    []byte
	lineNo  int
	skipped int
	err     error
	done    bool
}

// New returns a Reader over r using the house budget.
func New(r io.Reader) *Reader {
	return NewWithLimits(r, ChunkBytes, MaxLineBytes)
}

// NewWithLimits returns a Reader with an explicit read-chunk size and parse
// budget. Tests use it to drive the skip path without writing megabyte
// fixtures; production callers should use New so that no call site carries a
// private ceiling.
func NewWithLimits(r io.Reader, chunkBytes, maxLineBytes int) *Reader {
	if chunkBytes < 1 {
		chunkBytes = ChunkBytes
	}
	if maxLineBytes < 1 {
		maxLineBytes = MaxLineBytes
	}
	return &Reader{br: bufio.NewReaderSize(r, chunkBytes), max: maxLineBytes}
}

// Scan advances to the next record that fits the budget, skipping and counting
// any that do not. It reports whether a record is available in Bytes.
func (r *Reader) Scan() bool {
	if r.done {
		return false
	}
	for {
		res, err := r.readLine()
		if err != nil {
			r.finish(err)
			return false
		}
		if res.over {
			r.lineNo++
			r.skipped++
			if res.atEOF {
				r.finish(nil)
				return false
			}
			continue
		}
		if !res.ok {
			r.finish(nil)
			return false
		}
		r.lineNo++
		r.line = res.line
		r.done = res.atEOF
		return true
	}
}

// Bytes returns the record read by the most recent Scan. The slice is only
// valid until the next call to Scan.
func (r *Reader) Bytes() []byte { return r.line }

// Err returns the first non-EOF read error, or nil. It never returns
// bufio.ErrTooLong: an over-long record is a counted skip here, not a failure,
// which is the whole point of the package. Every other error passes through
// untouched so a caller's own classification still sees the real cause.
func (r *Reader) Err() error { return r.err }

// Skipped returns how many records exceeded the parse budget and were drained.
// Zero means the stream parsed cleanly; any other value is the number a caller
// should surface, because a silent skip is a quieter version of the same
// problem the package exists to fix.
func (r *Reader) Skipped() int { return r.skipped }

// Line returns the 1-based index of the last physical line the Reader
// consumed, whether it was returned or skipped. Skipped records still advance
// it, so a reported line number keeps naming the same line of the file a
// reader would count by hand.
func (r *Reader) Line() int { return r.lineNo }

func (r *Reader) finish(err error) {
	r.done = true
	r.line = nil
	if err != nil && r.err == nil {
		r.err = err
	}
}

// lineResult is the outcome of consuming one physical line.
type lineResult struct {
	line  []byte // the record, terminator stripped; valid when ok
	ok    bool   // a record within budget was read
	over  bool   // the line exceeded the budget and was drained, not held
	atEOF bool   // the stream ended on or immediately after this line
}

// readLine consumes exactly one physical line. An over-budget line is drained
// chunk by chunk so it is never materialised: that is what keeps peak
// allocation independent of the longest line in the file.
func (r *Reader) readLine() (lineResult, error) {
	var (
		n    int    // budget-relevant bytes seen so far, terminators excluded
		acc  []byte // nil until the line spans more than one read chunk
		over bool
	)
	for {
		chunk, err := r.br.ReadSlice('\n')
		switch err {
		case bufio.ErrBufferFull:
			// No newline in this window, so every byte counts toward the
			// budget and none of them is a terminator.
			n += len(chunk)
			if over || n > r.max {
				over = true
				acc = nil // release the partial record; we will not return it
				continue
			}
			acc = r.appendLine(acc, chunk)
		case nil, io.EOF:
			if err == io.EOF && len(chunk) == 0 {
				// The stream ended cleanly on a previous terminator.
				switch {
				case over:
					return lineResult{over: true, atEOF: true}, nil
				case len(acc) > 0:
					return lineResult{line: dropCR(acc), ok: true, atEOF: true}, nil
				default:
					return lineResult{atEOF: true}, nil
				}
			}
			body := chunk
			if err == nil {
				body = chunk[:len(chunk)-1] // drop the '\n'
			}
			n += len(body)
			if over || n > r.max {
				return lineResult{over: true, atEOF: err == io.EOF}, nil
			}
			if acc == nil {
				return lineResult{line: dropCR(body), ok: true, atEOF: err == io.EOF}, nil
			}
			return lineResult{line: dropCR(r.appendLine(acc, body)), ok: true, atEOF: err == io.EOF}, nil
		default:
			return lineResult{}, err
		}
	}
}

// appendLine grows the accumulator at most once, straight to the budget, and
// reuses it for every subsequent line. Doubling from the chunk size would cost
// roughly twice the budget in cumulative allocation for a single long line,
// which defeats the bound this package exists to hold.
func (r *Reader) appendLine(acc, chunk []byte) []byte {
	if r.acc == nil {
		r.acc = make([]byte, 0, r.max)
	}
	if acc == nil {
		acc = r.acc[:0]
	}
	return append(acc, chunk...)
}

// dropCR strips the '\r' of a CRLF terminator. bufio.ScanLines does this and a
// hand-rolled splitter that forgets to is invisible under LF-only fixtures —
// which is every fixture this repo had before this package.
func dropCR(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == '\r' {
		return b[:len(b)-1]
	}
	return b
}
