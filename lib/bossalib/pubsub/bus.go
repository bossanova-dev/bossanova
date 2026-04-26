// Package pubsub provides a generic broadcast bus used by bossd's StreamBus
// and bosso's DaemonState change-event broadcaster.
//
// One publisher → many subscribers. Each subscriber gets a bounded channel.
// On overflow, the oldest event is dropped (the subscriber is expected to
// either re-snapshot on reconnect or tolerate gaps).
//
// Subscriber lifecycle:
//
//	sub, cancel := bus.Subscribe()
//	defer cancel()
//	for ev := range sub { ... }   // closed on cancel() or bus.Close()
package pubsub

import (
	"sync"
)

const defaultBuffer = 256

// Bus is a generic one-publisher / many-subscriber broadcast bus. The zero
// value is not usable — call New to construct a bus.
type Bus[T any] struct {
	mu     sync.Mutex
	subs   map[*subscriber[T]]struct{}
	closed bool
	onDrop func()
}

type subscriber[T any] struct {
	ch chan T
}

// Option configures a Bus[T] at construction time. Options are applied in
// order by New.
type Option[T any] func(*Bus[T])

// WithOnDrop registers a callback invoked once for every event dropped due
// to a full subscriber buffer. The callback is invoked while the bus mutex
// is held, so it must be cheap and non-blocking — typically a counter
// increment or a single structured-log line. A nil fn is a no-op.
func WithOnDrop[T any](fn func()) Option[T] {
	return func(b *Bus[T]) {
		b.onDrop = fn
	}
}

// New constructs a Bus[T] ready to accept Subscribe calls.
func New[T any](opts ...Option[T]) *Bus[T] {
	b := &Bus[T]{subs: make(map[*subscriber[T]]struct{})}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// Subscribe returns a receive channel and a cancel function. The channel is
// closed when cancel() is called or the bus is closed. Calling cancel more
// than once is safe; subsequent calls are no-ops.
//
// Calling Subscribe on a closed bus returns an already-closed channel and a
// no-op cancel — callers can range over it without special-casing the
// closed-bus case.
func (b *Bus[T]) Subscribe() (<-chan T, func()) {
	s := &subscriber[T]{ch: make(chan T, defaultBuffer)}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		close(s.ch)
		return s.ch, func() {}
	}
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	return s.ch, func() { b.unsubscribe(s) }
}

// Publish fans the event out to every subscriber. On a full subscriber
// channel, the oldest event is dropped (non-blocking publish): the
// publisher is never blocked by a slow subscriber. If an OnDrop hook is
// registered, it is invoked once per drop (per subscriber).
//
// The drop-oldest semantics matches bossd's reverse-stream model: every
// reconnect re-establishes truth via a snapshot, so losing a single delta
// is always recoverable.
func (b *Bus[T]) Publish(ev T) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	for s := range b.subs {
		select {
		case s.ch <- ev:
		default:
			// Subscriber buffer is full. Notify the drop hook (if any)
			// before evicting, so the hook reflects the per-subscriber
			// drop count.
			if b.onDrop != nil {
				b.onDrop()
			}
			// Drop oldest, push new.
			select {
			case <-s.ch:
			default:
			}
			select {
			case s.ch <- ev:
			default:
			}
		}
	}
}

// Close closes the bus. All subscriber channels are closed. Subsequent
// Publish calls are no-ops; subsequent Subscribe calls return an
// already-closed channel. Close is idempotent.
func (b *Bus[T]) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for s := range b.subs {
		close(s.ch)
	}
	b.subs = nil
}

func (b *Bus[T]) unsubscribe(s *subscriber[T]) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.subs[s]; !ok {
		return
	}
	delete(b.subs, s)
	close(s.ch)
}
