// Package upstream — stream_bus.go owns the in-process fan-out used by
// the StreamClient's EventSource. The existing plugin-facing eventbus.Bus
// is typed to EventNotification (task_ready, task_updated, external_check,
// custom) — a disjoint set from the reverse-stream payloads (session /
// chat / status). Rather than widening that proto just for an internal
// pipeline, we keep the two buses separate: publishers push StreamEvents
// straight into this bus.
//
// The bus is intentionally tiny: one subscriber (the stream client),
// bounded buffer, drop-oldest on overflow. On overflow we drop — the
// snapshot sent at every reconnect re-establishes truth, so losing a
// single delta is always recoverable.
package upstream

import (
	"context"
	"sync"

	"github.com/rs/zerolog"
)

// streamBusBufSize is the per-subscriber buffer. Sized for a full CI
// burst (N session transitions + N chat creates) without loss.
const streamBusBufSize = 256

// StreamBus is an in-memory pub/sub bus for StreamEvent. Implements
// EventSource so the StreamClient can Subscribe directly.
type StreamBus struct {
	mu          sync.RWMutex
	subscribers map[*streamSub]struct{}
	closed      bool
	logger      zerolog.Logger
}

type streamSub struct {
	ch     chan StreamEvent
	cancel context.CancelFunc
}

// NewStreamBus constructs a bus. The logger is used only for drop
// warnings; all other paths are silent by design.
func NewStreamBus(logger zerolog.Logger) *StreamBus {
	return &StreamBus{
		subscribers: make(map[*streamSub]struct{}),
		logger:      logger.With().Str("component", "stream-bus").Logger(),
	}
}

// Publish fans an event out to every current subscriber. A full channel
// drops the event for that subscriber and logs once per drop — the
// snapshot pipeline restores truth on the next reconnect so this is
// safer than blocking the publisher (which could be a lifecycle call).
func (b *StreamBus) Publish(ev StreamEvent) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.closed {
		return
	}

	for sub := range b.subscribers {
		select {
		case sub.ch <- ev:
		default:
			b.logger.Warn().Msg("stream bus subscriber buffer full, dropping event")
		}
	}
}

// Subscribe implements EventSource.Subscribe. The returned channel is
// closed when ctx is cancelled or Close() is called.
func (b *StreamBus) Subscribe(ctx context.Context) <-chan StreamEvent {
	ctx, cancel := context.WithCancel(ctx)

	sub := &streamSub{
		ch:     make(chan StreamEvent, streamBusBufSize),
		cancel: cancel,
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		cancel()
		close(sub.ch)
		return sub.ch
	}
	b.subscribers[sub] = struct{}{}
	b.mu.Unlock()

	go func() {
		<-ctx.Done()
		b.unsubscribe(sub)
	}()

	return sub.ch
}

// unsubscribe removes a subscriber and closes its channel.
func (b *StreamBus) unsubscribe(sub *streamSub) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.subscribers[sub]; !ok {
		return
	}
	delete(b.subscribers, sub)
	close(sub.ch)
}

// Close shuts down the bus. Idempotent.
func (b *StreamBus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for sub := range b.subscribers {
		sub.cancel()
		close(sub.ch)
	}
	b.subscribers = nil
}
