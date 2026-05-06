// Package claude manages Claude CLI subprocess lifecycle for coding sessions.
package agent

import (
	"context"
	"sync"
	"time"
)

// DefaultRingBufferSize is the number of output lines kept in memory per session.
const DefaultRingBufferSize = 1000

// OutputLine is a single line of output from a Claude process.
type OutputLine struct {
	Text      string
	Timestamp time.Time
}

// AgentRunner manages Claude CLI subprocesses.
type AgentRunner interface {
	// Start spawns a Claude CLI process in workDir with the given plan.
	// If resume is non-nil, it resumes an existing Claude session.
	// If sessionID is non-empty, it is passed via --session-id and used as the tracking key.
	// When sessionID is empty, a generated claude-<timestamp> ID is used instead.
	// Returns the session ID assigned to this process.
	Start(ctx context.Context, workDir, plan string, resume *string, sessionID string) (string, error)

	// Stop terminates the Claude process for the given session.
	Stop(sessionID string) error

	// IsRunning reports whether a Claude process is active for the session.
	IsRunning(sessionID string) bool

	// ExitError returns the exit error for a completed session.
	// Returns nil if the session is still running, exited successfully,
	// or is unknown.
	ExitError(sessionID string) error

	// Subscribe returns a channel that receives output lines for the session.
	// The channel is closed when the process exits or the caller cancels ctx.
	Subscribe(ctx context.Context, sessionID string) (<-chan OutputLine, error)

	// History returns the buffered output lines for a session.
	History(sessionID string) []OutputLine
}

// --- Ring Buffer ---

// ringBuffer is a fixed-size circular buffer of OutputLine entries.
type ringBuffer struct {
	mu    sync.RWMutex
	buf   []OutputLine
	size  int
	head  int // next write position
	count int // total items written (for overflow detection)
}

func newRingBuffer(size int) *ringBuffer {
	return &ringBuffer{
		buf:  make([]OutputLine, size),
		size: size,
	}
}

// add appends a line to the ring buffer.
func (rb *ringBuffer) add(line OutputLine) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	rb.buf[rb.head] = line
	rb.head = (rb.head + 1) % rb.size
	rb.count++
}

// lines returns all stored lines in chronological order.
func (rb *ringBuffer) lines() []OutputLine {
	rb.mu.RLock()
	defer rb.mu.RUnlock()

	if rb.count == 0 {
		return nil
	}

	n := rb.count
	if n > rb.size {
		n = rb.size
	}

	result := make([]OutputLine, n)
	if rb.count <= rb.size {
		// Buffer hasn't wrapped yet.
		copy(result, rb.buf[:n])
	} else {
		// Buffer has wrapped: read from head (oldest) to end, then start to head.
		start := rb.head // oldest entry
		copied := copy(result, rb.buf[start:])
		copy(result[copied:], rb.buf[:start])
	}

	return result
}

// --- Subscribers ---

// subscribers manages broadcast channels for output streaming.
type subscribers struct {
	mu     sync.RWMutex
	chans  []chan OutputLine
	closed bool
}

func newSubscribers() *subscribers {
	return &subscribers{}
}

// add creates a new subscription channel. The channel is removed when ctx is cancelled.
func (s *subscribers) add(ctx context.Context) <-chan OutputLine {
	ch := make(chan OutputLine, 64)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		close(ch)
		return ch
	}
	s.chans = append(s.chans, ch)
	s.mu.Unlock()

	// Remove the channel when the context is cancelled.
	go func() {
		<-ctx.Done()
		s.remove(ch)
	}() // no safego: trivial cleanup, cannot panic

	return ch
}

// broadcast sends a line to all subscriber channels.
func (s *subscribers) broadcast(line OutputLine) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, ch := range s.chans {
		select {
		case ch <- line:
		default:
			// Slow consumer — drop the line rather than blocking.
		}
	}
}

// close closes all subscriber channels.
func (s *subscribers) close() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closed = true
	for _, ch := range s.chans {
		close(ch)
	}
	s.chans = nil
}

// remove removes a specific channel from subscribers.
func (s *subscribers) remove(ch chan OutputLine) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, c := range s.chans {
		if c == ch {
			s.chans = append(s.chans[:i], s.chans[i+1:]...)
			close(ch)
			return
		}
	}
}
