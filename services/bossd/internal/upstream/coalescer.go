package upstream

import (
	"context"
	"sync"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog"
)

// StatusCoalescer buffers ChatStatusDelta messages per (session, chat)
// over a short flush window and emits only the latest entry per key when
// the window expires. Decision #11 in the design doc: the UI doesn't
// need sub-100ms status update cadence, and collapsing bursts here
// keeps egress bandwidth (and bosso CPU) proportional to "real" state
// changes rather than to boss-CLI heartbeat frequency.
//
// Keyed by (session_id, claude_id) so a session with multiple Claude
// chats — each with its own status — does not collapse to a single
// per-session entry on the wire. The downstream consumer keys its
// status map by claude_id, so per-chat fidelity must survive the
// coalescer.
//
// Backward compatibility: legacy daemons (and older publishers within
// this daemon) may emit a ChatStatusDelta with claude_id == "". Those
// collapse to a single per-session entry under coalescerKey{sessionID,
// ""}, preserving the previous session-only behavior for any caller
// that hasn't been updated yet.
type StatusCoalescer struct {
	clock  Clock
	window time.Duration
	logger zerolog.Logger

	mu      sync.Mutex
	pending map[coalescerKey]*pb.ChatStatusDelta // (session_id, claude_id) → latest
	out     chan *pb.ChatStatusDelta
}

// coalescerKey is the per-chat coalescing key. claudeID may be empty for
// legacy publishers that haven't been updated to populate it; in that
// case the key collapses to (sessionID, "") and behaves like the old
// session-only coalescer.
type coalescerKey struct {
	sessionID string
	claudeID  string
}

// NewStatusCoalescer creates a coalescer that uses the given clock to
// drive its flush ticker. Callers own the passed-in Clock — in tests
// this is usually a fakeClock so windows can be advanced deterministically.
func NewStatusCoalescer(clock Clock, window time.Duration, logger zerolog.Logger) *StatusCoalescer {
	if clock == nil {
		clock = realClock{}
	}
	if window <= 0 {
		window = 100 * time.Millisecond
	}
	return &StatusCoalescer{
		clock:   clock,
		window:  window,
		logger:  logger,
		pending: make(map[coalescerKey]*pb.ChatStatusDelta),
		// Out must be buffered enough to absorb a whole flush without
		// blocking Run's loop — the subscriber on the other side reads
		// from a bounded outbound channel, but we'd rather drop here
		// than stall there.
		out: make(chan *pb.ChatStatusDelta, 256),
	}
}

// Out returns the channel flushed statuses are emitted on. The channel
// is closed by Run when its input channel is closed, so downstream
// loops can range over it without separate lifecycle management.
func (c *StatusCoalescer) Out() <-chan *pb.ChatStatusDelta { return c.out }

// Publish inserts or replaces the pending entry for the status's
// (session_id, claude_id) key. Safe to call concurrently; flushing and
// publishing coordinate through the coalescer mutex.
func (c *StatusCoalescer) Publish(status *pb.ChatStatusDelta) {
	if status == nil {
		return
	}
	key := coalescerKey{sessionID: status.GetSessionId(), claudeID: status.GetClaudeId()}
	c.mu.Lock()
	c.pending[key] = status
	c.mu.Unlock()
}

// Drain returns and clears all currently-pending statuses. Intended
// for shutdown — the event forwarder calls Drain after closing the
// inbound channel so final state lands on the wire before reconnect.
func (c *StatusCoalescer) Drain() []*pb.ChatStatusDelta {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) == 0 {
		return nil
	}
	out := make([]*pb.ChatStatusDelta, 0, len(c.pending))
	for _, s := range c.pending {
		out = append(out, s)
	}
	c.pending = make(map[coalescerKey]*pb.ChatStatusDelta)
	return out
}

// flush emits every pending entry to Out() and clears the buffer.
// Called from Run on each window tick.
func (c *StatusCoalescer) flush() {
	c.mu.Lock()
	if len(c.pending) == 0 {
		c.mu.Unlock()
		return
	}
	batch := make([]*pb.ChatStatusDelta, 0, len(c.pending))
	for _, s := range c.pending {
		batch = append(batch, s)
	}
	c.pending = make(map[coalescerKey]*pb.ChatStatusDelta)
	c.mu.Unlock()

	for _, s := range batch {
		select {
		case c.out <- s:
		default:
			c.logger.Warn().
				Str("session_id", s.GetSessionId()).
				Str("claude_id", s.GetClaudeId()).
				Msg("coalescer out channel full, dropping status delta")
		}
	}
}

// Run drains in and writes latest-per-session snapshots to Out() on
// every window tick. Returns when ctx is cancelled or in is closed.
// Out is closed on return so downstream range loops exit cleanly.
func (c *StatusCoalescer) Run(ctx context.Context, in <-chan *pb.ChatStatusDelta) {
	defer close(c.out)

	// AfterFunc timer instead of a Ticker so tests with a fake clock
	// can step the window deterministically (Tickers in stdlib can't
	// be driven by an arbitrary Now()). Each fire reschedules itself.
	var timer Timer
	done := make(chan struct{})

	fire := make(chan struct{}, 1)
	fireFn := func() {
		// Non-blocking send so multiple quick fires during a flush
		// don't queue up — the loop below re-arms the timer after
		// each handled fire.
		select {
		case fire <- struct{}{}:
		default:
		}
	}
	timer = c.clock.AfterFunc(c.window, fireFn)
	defer func() {
		if timer != nil {
			timer.Stop()
		}
		close(done)
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case status, ok := <-in:
			if !ok {
				// Emit any pending state before exit — callers that
				// care about final delivery use Drain() directly, but
				// the common case is "flush on close".
				c.flush()
				return
			}
			c.Publish(status)
		case <-fire:
			c.flush()
			// Re-arm. Stop the old timer defensively even though
			// AfterFunc fired exactly once — this makes the goroutine
			// safe to call Stop on either exit path.
			if timer != nil {
				timer.Stop()
			}
			timer = c.clock.AfterFunc(c.window, fireFn)
		}
	}
}
