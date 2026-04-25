package upstream

import (
	"context"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/safego"
)

// subscribeDeltas drains the internal StreamEvent bus and forwards each
// event to the outbound DaemonEvent channel, mapping SessionEvent →
// DaemonEvent_Session and ChatEvent → DaemonEvent_Chat directly, while
// ChatStatus events route through a StatusCoalescer so a burst of
// per-frame heartbeats collapses to one on-wire message per ~100ms
// window (decision #11). The function returns when ctx is cancelled or
// the EventSource closes its channel — no-op when events is nil so the
// snapshot-only path in tests works without a bus.
func (c *StreamClient) subscribeDeltas(ctx context.Context, outbound chan<- *pb.DaemonEvent) {
	if c.events == nil {
		<-ctx.Done()
		return
	}

	// Coalescer flushes on its own ticker and writes straight to
	// outbound. It runs as a sibling goroutine so a slow consumer on
	// outbound doesn't block the delta subscriber below (which, if
	// blocked, would stall every other delta).
	statusCh := make(chan *pb.ChatStatusDelta, 64)
	coalescer := NewStatusCoalescer(c.clock, c.coalesceWindow, c.logger)
	coalescerDone := safego.Go(c.logger, func() {
		coalescer.Run(ctx, statusCh)
	})
	defer func() {
		close(statusCh)
		<-coalescerDone
		// Drain any residual statuses the coalescer held when it
		// shutdown so the stream sees the final state before reconnect.
		for _, s := range coalescer.Drain() {
			select {
			case outbound <- &pb.DaemonEvent{Event: &pb.DaemonEvent_Status{Status: s}}:
			case <-ctx.Done():
				return
			}
		}
	}()

	// Coalescer → outbound forwarder. Runs on this goroutine is
	// tempting but would block receive-from-bus during a full outbound
	// buffer. Split off a small fan-in goroutine so each side has
	// independent backpressure.
	forwarderDone := safego.Go(c.logger, func() {
		for s := range coalescer.Out() {
			select {
			case outbound <- &pb.DaemonEvent{Event: &pb.DaemonEvent_Status{Status: s}}:
			case <-ctx.Done():
				return
			}
		}
	})
	defer func() { <-forwarderDone }()

	ch := c.events.Subscribe(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			c.forwardEvent(ctx, ev, outbound, statusCh)
		}
	}
}

// forwardEvent maps a single StreamEvent to its DaemonEvent wrapper. It
// deliberately does not block on a full outbound buffer for longer than
// ctx allows — losing a delta during reconnect is preferable to stalling
// the whole forwarder, which would wedge the coalescer too.
func (c *StreamClient) forwardEvent(
	ctx context.Context,
	ev StreamEvent,
	outbound chan<- *pb.DaemonEvent,
	statusCh chan<- *pb.ChatStatusDelta,
) {
	switch {
	case ev.Session != nil:
		out := &pb.DaemonEvent{Event: &pb.DaemonEvent_Session{
			Session: &pb.SessionDelta{Kind: ev.Session.Kind, Session: ev.Session.Session},
		}}
		select {
		case outbound <- out:
		case <-ctx.Done():
		}
	case ev.Chat != nil:
		out := &pb.DaemonEvent{Event: &pb.DaemonEvent_Chat{
			Chat: &pb.ChatDelta{Kind: ev.Chat.Kind, Chat: ev.Chat.Chat},
		}}
		select {
		case outbound <- out:
		case <-ctx.Done():
		}
	case ev.Status != nil:
		// Route through the coalescer. statusCh has a buffered 64 slots
		// so a burst fits; if full we drop (the coalescer holds the
		// latest per-session anyway, so this mostly affects first
		// delivery latency for a cold session).
		select {
		case statusCh <- ev.Status.Status:
		case <-ctx.Done():
		default:
			c.logger.Debug().Msg("coalescer inbound full, dropping status event")
		}
	}
}
