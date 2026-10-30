package upstream

import (
	"context"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// StreamEvent is the union of session/chat/status events the daemon
// publishes internally for the reverse stream. It intentionally does not
// reuse the plugin-facing EventNotification (which has a disjoint oneof
// set) — keeping stream events in-package avoids enlarging the plugin
// proto for a purely internal pipeline.
type StreamEvent struct {
	// Exactly one of the following is non-nil.
	Session                 *SessionEvent
	Chat                    *ChatEvent
	Status                  *StatusEvent
	Interests               *InterestsEvent
	EgressBroadcast         *BroadcastEgressEvent
	AgentAccounts           *AgentAccountsEvent
	AgentAccountUsageReport *pb.AgentAccountUsageReport
}

// BroadcastEgressEvent carries an outbound cross-daemon broadcast up the
// reverse stream: "this daemon originated a broadcast whose audience reaches
// beyond itself; route it". It wraps the already-built proto rather than
// restating its fields so the envelope never holds a second, drifting copy.
//
// SECRET BODY: Egress.Message is the broadcast prompt. It rides the bus to
// bosso for delivery and MUST NEVER be logged, echoed on a read surface, or
// put in an error detail — the same rule daemon.proto states on
// Broadcast.message. Nothing in this package logs the contents of this event;
// log broadcast id, origin daemon id, and counts only.
type BroadcastEgressEvent struct {
	Egress *pb.BroadcastEgress
}

// InterestsEvent carries the daemon's complete current GitHub callback-interest
// set as a steady-state refresh. It has snapshot semantics: the full set every
// time, and an empty slice is a valid message meaning "withdraw all" (the daemon
// holds no live callbacks). The connect/reconnect DaemonSnapshot carries the
// guaranteed-first full set; this event carries subsequent changes.
type InterestsEvent struct {
	Interests []*pb.CallbackInterest
}

// SessionEvent describes a session lifecycle change. Kind mirrors the
// SessionDelta proto one-to-one; Session is omitted on delete.
type SessionEvent struct {
	Kind    pb.SessionDelta_Kind
	Session *pb.Session
}

// ChatEvent describes a chat lifecycle change.
type ChatEvent struct {
	Kind pb.ChatDelta_Kind
	Chat *pb.AgentChatMetadata
}

// StatusEvent is a raw chat-status heartbeat, pre-coalescing. The
// coalescer (T3.4) dedupes bursts before they reach the wire.
type StatusEvent struct {
	Status *pb.ChatStatusDelta
}

// EventSource is the subscribe-side of the daemon's internal stream
// event bus. Implementations close the returned channel when ctx is
// cancelled or the source shuts down. A concrete eventbus adapter lands
// in T3.7 when the publishers (lifecycle, chat store, display computer)
// get wired to it.
type EventSource interface {
	Subscribe(ctx context.Context) <-chan StreamEvent
}
