package pty

import "sync"

const defaultBufSize = 512 * 1024 // 512KB

// RingBuffer is a fixed-size circular buffer for storing recent PTY output.
type RingBuffer struct {
	mu   sync.Mutex
	buf  []byte
	pos  int  // next write position
	full bool // true once the buffer has wrapped around
}

// NewRingBuffer creates a ring buffer with the given capacity.
func NewRingBuffer(size int) *RingBuffer {
	return &RingBuffer{buf: make([]byte, size)}
}

// Write appends data to the buffer, overwriting the oldest bytes when full.
func (r *RingBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	n := len(p)
	if n >= len(r.buf) {
		// Data larger than buffer: keep only the tail.
		copy(r.buf, p[n-len(r.buf):])
		r.pos = 0
		r.full = true
		return n, nil
	}

	// How much fits before we wrap?
	space := len(r.buf) - r.pos
	if n <= space {
		copy(r.buf[r.pos:], p)
	} else {
		copy(r.buf[r.pos:], p[:space])
		copy(r.buf, p[space:])
	}
	r.pos = (r.pos + n) % len(r.buf)
	if !r.full && r.pos < n {
		r.full = true
	}
	return n, nil
}

// Bytes returns the buffered content in chronological order.
func (r *RingBuffer) Bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.full {
		out := make([]byte, r.pos)
		copy(out, r.buf[:r.pos])
		return out
	}

	out := make([]byte, len(r.buf))
	// Oldest data starts at r.pos, wraps around.
	n := copy(out, r.buf[r.pos:])
	copy(out[n:], r.buf[:r.pos])
	return out
}

// Tail returns the last n bytes of buffered content.
// If fewer than n bytes have been written, it returns all available bytes.
func (r *RingBuffer) Tail(n int) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()

	var total int
	if r.full {
		total = len(r.buf)
	} else {
		total = r.pos
	}
	if total == 0 {
		return nil
	}
	if n > total {
		n = total
	}

	out := make([]byte, n)
	// r.pos points to the next write position, so the most recent byte is at r.pos-1.
	// We need the last n bytes ending at r.pos-1 (wrapping around).
	start := (r.pos - n + len(r.buf)) % len(r.buf)
	if start+n <= len(r.buf) {
		copy(out, r.buf[start:start+n])
	} else {
		first := len(r.buf) - start
		copy(out, r.buf[start:])
		copy(out[first:], r.buf[:n-first])
	}
	return out
}

// Reset clears the buffer.
func (r *RingBuffer) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pos = 0
	r.full = false
}
