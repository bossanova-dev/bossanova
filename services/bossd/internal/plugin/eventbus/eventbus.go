package eventbus

import (
	"context"
	"sync"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// subscriberBufSize is the channel buffer size per subscriber.
const subscriberBufSize = 64

// Bus is an in-memory pub/sub event bus for plugin-to-core communication.
// Events are published as proto EventNotification messages. Subscribers
// receive events on buffered channels. If a subscriber's buffer is full,
// the event is dropped and a warning is logged.
type Bus struct {
	mu          sync.RWMutex
	subscribers map[*subscriber]struct{}
	closed      bool
	logger      zerolog.Logger
}

type subscriber struct {
	ch     chan *bossanovav1.EventNotification
	cancel context.CancelFunc
}

// New creates a new event bus.
func New(logger zerolog.Logger) *Bus {
	return &Bus{
		subscribers: make(map[*subscriber]struct{}),
		logger:      logger,
	}
}

// Publish sends an event to all current subscribers. If a subscriber's
// channel buffer is full, the event is dropped for that subscriber and
// a warning is logged. Publish is a no-op after Close.
func (b *Bus) Publish(event *bossanovav1.EventNotification) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.closed {
		return
	}

	for sub := range b.subscribers {
		select {
		case sub.ch <- event:
		default:
			b.logger.Warn().
				Str("source", event.GetSource()).
				Msg("eventbus: subscriber buffer full, dropping event")
		}
	}
}

// Subscribe returns a channel that receives published events. The
// subscription is automatically removed when the provided context is
// cancelled. The caller should consume from the returned channel until
// it is closed.
func (b *Bus) Subscribe(ctx context.Context) <-chan *bossanovav1.EventNotification {
	ctx, cancel := context.WithCancel(ctx)

	sub := &subscriber{
		ch:     make(chan *bossanovav1.EventNotification, subscriberBufSize),
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
func (b *Bus) unsubscribe(sub *subscriber) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if _, ok := b.subscribers[sub]; !ok {
		return
	}
	delete(b.subscribers, sub)
	close(sub.ch)
}

// Close shuts down the bus: all subscriber channels are closed and
// future Publish calls become no-ops. Close is idempotent.
func (b *Bus) Close() {
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
