package session

import (
	"bytes"
	"strings"
	"sync"
)

// setupBusMaxLineBytes caps the in-progress line the bus accumulates between
// newlines. Bytes past the cap are DISCARDED until the next '\n'.
//
// Without it a setup script that emits a large newline-free blob — a curl
// progress meter on a non-TTY, a minified bundle, a binary cat'd by mistake —
// would grow the buffer without limit inside the daemon. The io.Pipe path this
// replaces was bounded (a bufio reader sized to this same constant, which
// failed the bootstrap on overflow); truncating is the friendlier form of the
// same bound, and matches the truncation the RPC applies on the way out.
const setupBusMaxLineBytes = 256 * 1024

// setupBusSubscriberBuffer is the per-subscriber queue depth. A subscriber
// that falls this far behind is DROPPED (its line is discarded), never waited
// on: the writer is the bootstrap goroutine, and making it wait on a remote
// RPC client is the io.Pipe deadlock BOS-720 removes. Losing a line of setup
// output to a stalled client is strictly better than wedging the bootstrap
// that produced it.
const setupBusSubscriberBuffer = 256

// SetupBus fans setup-script output out to zero or more subscribers without
// ever blocking the writer.
//
// It replaces the io.Pipe that StreamCreateSession used to hand to
// StartSession as opts.SetupOutput. That pipe did not merely OBSERVE the
// bootstrap, it DROVE it: io.Pipe is synchronous and unbuffered, so pw.Write()
// returns only once a reader consumes — which made streamSetupOutput reading
// the pipe the only thing letting StartSession proceed, and a client that
// stopped draining blocked the bootstrap forever.
//
// SetupBus inverts that: the bootstrap writes, subscribers subscribe, and a
// subscriber's liveness is its own problem.
type SetupBus struct {
	mu          sync.Mutex
	closed      bool
	partial     []byte
	replayLimit int
	replay      []string
	subscribers map[int]chan string
	nextID      int
}

// NewSetupBus returns a bus that retains up to replayLimit of the most recent
// lines for subscribers that arrive after writing has begun. A zero limit
// retains none.
func NewSetupBus(replayLimit int) *SetupBus {
	return &SetupBus{replayLimit: replayLimit, subscribers: map[int]chan string{}}
}

// Write implements io.Writer. It splits p into whole lines (carrying an
// unterminated tail over to the next call) and fans each completed line out.
// It NEVER blocks and NEVER returns an error — an error would surface inside
// the setup-script plumbing as a bootstrap failure, which a slow RPC client
// must not be able to cause.
func (b *SetupBus) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return len(p), nil
	}

	n := len(p)
	for len(p) > 0 {
		idx := bytes.IndexByte(p, '\n')
		if idx < 0 {
			b.appendPartialLocked(p)
			break
		}
		b.appendPartialLocked(p[:idx])
		b.flushPartialLocked()
		p = p[idx+1:]
	}
	return n, nil
}

// appendPartialLocked accumulates bytes of the line still being assembled,
// keeping at most setupBusMaxLineBytes of it. Once the cap is reached the rest
// of the line is dropped on the floor until its terminating newline arrives.
func (b *SetupBus) appendPartialLocked(chunk []byte) {
	room := setupBusMaxLineBytes - len(b.partial)
	if room <= 0 {
		return
	}
	if len(chunk) > room {
		chunk = chunk[:room]
	}
	b.partial = append(b.partial, chunk...)
}

// flushPartialLocked publishes the assembled line and starts a fresh one. The
// trailing '\r' of a CRLF is trimmed here rather than in Write so that a CRLF
// split across two Write calls is still handled.
func (b *SetupBus) flushPartialLocked() {
	line := strings.TrimSuffix(string(b.partial), "\r")
	b.partial = nil
	b.publishLocked(line)
}

// publishLocked records the line for replay and offers it to every live
// subscriber, skipping any whose buffer is full.
func (b *SetupBus) publishLocked(line string) {
	if b.replayLimit > 0 {
		b.replay = append(b.replay, line)
		if len(b.replay) > b.replayLimit {
			b.replay = b.replay[len(b.replay)-b.replayLimit:]
		}
	}
	for _, ch := range b.subscribers {
		select {
		case ch <- line:
		default:
			// Dropped: this subscriber is not draining. See the constant's doc.
		}
	}
}

// Subscribe returns a channel of setup-output lines plus a cancel func the
// caller MUST invoke (defer it) to unregister. Retained lines are replayed
// first. Subscribing to a closed bus still replays, then returns a closed
// channel and a no-op cancel — so the RPC's subscribe-after-handoff race costs
// the client nothing, whether or not the bootstrap beat it to settling.
func (b *SetupBus) Subscribe() (<-chan string, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()

	ch := make(chan string, setupBusSubscriberBuffer)

	// Replay the NEWEST lines that fit, not the oldest. The replay window is
	// allowed to be larger than one subscriber's buffer, and a plain
	// oldest-first walk would fill the buffer with the stalest lines and then
	// drop every fresher one — inverting the drop policy exactly where it
	// matters most, since a late subscriber wants the tail.
	//
	// This runs BEFORE the closed check, not after: a bootstrap that settles
	// between the RPC's Start and its Subscribe has already closed the bus, and
	// returning an empty channel there would eat the whole setup script of
	// exactly the runs that need it most — a fast failure whose one diagnostic
	// line is the only thing the client ever had to go on. A closed buffered
	// channel still drains what it holds before reporting closed.
	replay := b.replay
	if len(replay) > cap(ch) {
		replay = replay[len(replay)-cap(ch):]
	}
	for _, line := range replay {
		select {
		case ch <- line:
		default:
		}
	}

	if b.closed {
		close(ch)
		return ch, func() {}
	}
	id := b.nextID
	b.nextID++
	b.subscribers[id] = ch

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if sub, ok := b.subscribers[id]; ok {
				delete(b.subscribers, id)
				close(sub)
			}
		})
	}
}

// Close flushes any unterminated tail, closes every live subscriber channel,
// and makes further Writes silent no-ops. Idempotent.
func (b *SetupBus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	if len(b.partial) > 0 {
		b.flushPartialLocked()
	}
	b.closed = true
	for id, ch := range b.subscribers {
		delete(b.subscribers, id)
		close(ch)
	}
}
