// Package upstream — broadcast.go holds the EGRESS half of the cross-daemon
// broadcast path's production wiring (BOS-558): the adapter that turns the send
// path's domain event into the pb.BroadcastEgress riding the daemon->bosso
// reverse stream.
//
// It sits here rather than in adapters.go because it is not an adapter in that
// file's sense — everything there hangs off CommandHandlerAdapter or is a narrow
// interface it consumes, whereas this is a StreamBus publisher with no relation
// to the inbound command surface. The INGRESS half's translator
// (CommandHandlerAdapter.DeliverBroadcast) does belong with the other command
// translators and stays there.
package upstream

import (
	"context"
	"errors"

	bcast "github.com/recurser/bossalib/broadcast"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	bcastsvc "github.com/recurser/bossd/internal/broadcast"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// BroadcastEgressPublisher adapts the daemon's StreamBus to
// broadcast.EgressPublisher: it is the EGRESS half's production wiring, turning
// the send path's domain event into the pb.BroadcastEgress that rides the
// daemon->bosso reverse stream (BOS-558).
//
// It lives here rather than in cmd/main.go because it is the only place that
// knows both vocabularies, and because it belongs next to the bus it publishes
// on. The server package depends on the broadcast.EgressPublisher interface, so
// it never learns that the transport is a stream bus.
//
// SECRET BODY: ev.Message is the broadcast prompt. It is carried onto the bus —
// bosso needs it to hand the prompt to whichever daemons resolve targets — and
// nothing here logs it.
type BroadcastEgressPublisher struct {
	bus *StreamBus
}

// NewBroadcastEgressPublisher wraps a StreamBus. A nil bus is tolerated at
// construction and reported at publish time, so a daemon with no upstream can
// be wired without a special case at the call site.
func NewBroadcastEgressPublisher(bus *StreamBus) *BroadcastEgressPublisher {
	return &BroadcastEgressPublisher{bus: bus}
}

// PublishBroadcastEgress hands one cross-daemon broadcast to the reverse stream.
//
// IT RETURNS nil FOR "HANDED TO THE BUS", NOT "DELIVERED TO BOSSO", and the gap
// between those is real rather than theoretical. StreamBus.Publish is
// drop-oldest with no return value, and pubsub.Bus.Publish iterates its
// subscribers — so with NO subscriber (the daemon is wired for upstream but the
// stream is not currently connected) the loop body never runs, no drop hook
// fires, and the event is discarded in silence. The overflow case does log, via
// NewStreamBus's OnDrop hook; the disconnected case does not, and there is no
// subscriber count to consult. Both are deliberate best-effort: the send path's
// contract is that an egress failure never fails the RPC or the local
// deliveries, and the plan's stated offline behaviour is that cross-daemon
// targets simply do not receive while bosso is unreachable — this child adds no
// egress outbox. Callers must therefore log this as "handed to the stream", not
// "published"; see Server.publishBroadcastEgress.
//
// The error case is reserved for the one thing that is genuinely a wiring bug:
// no bus at all.
func (p *BroadcastEgressPublisher) PublishBroadcastEgress(_ context.Context, ev bcastsvc.EgressEvent) error {
	if p == nil || p.bus == nil {
		return errors.New("broadcast egress: stream bus not wired")
	}
	p.bus.Publish(StreamEvent{EgressBroadcast: &BroadcastEgressEvent{Egress: &pb.BroadcastEgress{
		BroadcastId:    ev.BroadcastID,
		Selector:       bcast.SelectorToProto(ev.Selector),
		OriginDaemonId: ev.OriginDaemonID,
		OriginChatId:   ev.OriginChatID,
		Message:        ev.Message,
		ExpiresAt:      timestamppb.New(ev.ExpiresAt),
	}}})
	return nil
}
