// Package upstream — stream_bus.go owns the in-process fan-out used by
// the StreamClient's EventSource. The existing plugin-facing eventbus.Bus
// is typed to EventNotification (task_ready, task_updated, external_check,
// custom) — a disjoint set from the reverse-stream payloads (session /
// chat / status). Rather than widening that proto just for an internal
// pipeline, we keep the two buses separate: publishers push StreamEvents
// straight into this bus.
//
// The bus is intentionally tiny: bounded buffer, drop-oldest on overflow.
// On overflow we drop AND log a warning — the snapshot sent at every
// reconnect re-establishes truth, so losing a single delta is always
// recoverable, but the warning is the only signal that a slow subscriber
// is causing UI gaps.
//
// Implementation note: this type is now a thin wrapper over the generic
// pubsub.Bus[T] in bossalib so bosso can share the broadcast primitive.
// The wrapper preserves the existing EventSource shape (Subscribe(ctx)
// returns a channel; ctx cancellation closes it) which differs from
// pubsub's (channel, cancelFunc) signature.
package upstream

import (
	"context"
	"sync"

	"github.com/recurser/bossalib/pubsub"
	"github.com/recurser/bossalib/safego"
	"github.com/rs/zerolog"
)

// StreamBus is an in-memory pub/sub bus for StreamEvent. Implements
// EventSource so the StreamClient can Subscribe directly.
type StreamBus struct {
	bus    *pubsub.Bus[StreamEvent]
	logger zerolog.Logger
	// stop is closed by Close() to signal Subscribe-spawned watcher
	// goroutines to exit even when their ctx never cancels. Without this,
	// callers that pass a context.Background()-derived ctx would leak the
	// watcher goroutine for the rest of the process lifetime once Close
	// has been called.
	stop      chan struct{}
	closeOnce sync.Once
}

// NewStreamBus constructs a bus. The logger is tagged with
// component="stream-bus" and used by the OnDrop hook to surface a warning
// every time a subscriber's buffer overflows — that warning is the only
// signal that a slow consumer is causing UI gaps, so silent drops are
// considered an observability regression.
func NewStreamBus(logger zerolog.Logger) *StreamBus {
	log := logger.With().Str("component", "stream-bus").Logger()
	return &StreamBus{
		bus: pubsub.New[StreamEvent](pubsub.WithOnDrop[StreamEvent](func() {
			log.Warn().Msg("stream bus subscriber buffer full, dropping event")
		})),
		logger: log,
		stop:   make(chan struct{}),
	}
}

// Publish fans an event out to every current subscriber. A full
// subscriber channel drops the oldest buffered event so the publisher is
// never blocked — the snapshot pipeline restores truth on the next
// reconnect, which makes drop-oldest safer than blocking a lifecycle hook.
func (b *StreamBus) Publish(ev StreamEvent) {
	b.bus.Publish(ev)
}

// Subscribe implements EventSource.Subscribe. The returned channel is
// closed when ctx is cancelled or Close() is called.
//
// The pubsub.Bus subscriber API is (channel, cancelFunc); here we bridge
// that to the ctx-driven lifecycle by spawning a watcher goroutine that
// invokes cancel when ctx is done OR when the bus itself is closed. The
// stop-channel branch prevents a watcher leak when callers pass a
// long-lived ctx (e.g. context.Background) and rely on Close() for
// shutdown.
func (b *StreamBus) Subscribe(ctx context.Context) <-chan StreamEvent {
	ch, cancel := b.bus.Subscribe()

	// Bridge ctx → pubsub cancel. safego.Go gives us panic recovery and a
	// done channel; we don't need to wait on done because the goroutine's
	// lifetime is bounded by ctx-or-stop. Cancel is idempotent on the
	// pubsub side, so racing against bus.Close() is safe.
	_ = safego.Go(b.logger, func() {
		select {
		case <-ctx.Done():
		case <-b.stop:
		}
		cancel()
	})

	return ch
}

// Close shuts down the bus. Idempotent. Closes b.stop first so any
// in-flight Subscribe watcher goroutines unblock and call their pubsub
// cancel — that prevents the watcher leak that would otherwise occur for
// callers with long-lived contexts.
func (b *StreamBus) Close() {
	b.closeOnce.Do(func() {
		close(b.stop)
	})
	b.bus.Close()
}
