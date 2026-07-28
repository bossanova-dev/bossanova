package broadcast

import (
	"context"
	"strings"
	"time"

	bcast "github.com/recurser/bossalib/broadcast"
)

// EgressEvent is one broadcast this daemon originated whose audience reaches
// beyond itself, on its way up the daemon->bosso reverse stream for routing.
//
// It is the send path's own vocabulary rather than the wire proto so the
// decision to publish (this file) stays free of the transport that carries it:
// the adapter that turns this into a pb.BroadcastEgress lives with the upstream
// stream client, and the RPC handler never touches the stream types.
//
// Message is the SECRET body. It is carried ONLY because bosso must hand the
// prompt to whichever daemons resolve targets for the selector — a selector is
// re-resolved per daemon, so the body cannot be left behind and fetched later.
// It must never be logged, echoed on a read surface, or put in an error string;
// see the rule stated on pb.Broadcast.message and on broadcastToProto in
// services/bossd/internal/server/send_broadcast.go. Publish failures are logged
// with the broadcast id, the origin daemon id and counts ONLY.
type EgressEvent struct {
	// BroadcastID is the ORIGIN's broadcast id. Every daemon that delivers this
	// broadcast persists it under this id, so a redelivery is recognisable.
	BroadcastID string
	// Selector is the validated audience as issued here. It travels unresolved:
	// each receiving daemon re-resolves it against its own local chats.
	Selector bcast.Selector
	// OriginDaemonID is this daemon's PERSISTED id (upstream.ResolveDaemonID),
	// never a per-process value — the receiving side's loop guard drops any
	// command whose origin daemon id equals its own, and a value that changed
	// per process would defeat it.
	OriginDaemonID string
	// OriginChatID is the chat the broadcast was issued from, or "" for an
	// operator-issued send with no originating chat.
	OriginChatID string
	// Message is the SECRET body (see the type comment).
	Message string
	// ExpiresAt is the origin's absolute expiry, carried so a slow route cannot
	// extend a broadcast's lifetime past what the sender asked for.
	ExpiresAt time.Time
}

// EgressPublisher hands a cross-daemon broadcast to the transport that carries
// it upstream.
//
// It is an interface, and the send path depends on it rather than on the
// upstream stream bus, for two reasons: the RPC handler must be testable with a
// scripted publisher (including one that fails, since a publish failure must
// never fail the RPC), and the bus lives in a package the server would
// otherwise have to import. Production wiring adapts this onto the daemon's
// upstream.StreamBus.
//
// An error means "not published" — nothing more. Implementations must not log
// the event's Message, and callers must not put a returned error anywhere the
// body could ride along with it.
type EgressPublisher interface {
	PublishBroadcastEgress(ctx context.Context, ev EgressEvent) error
}

// ReachesBeyondDaemon reports whether a broadcast issued here needs routing to
// other daemons — i.e. whether the send path should publish an egress event.
//
// True when the caller explicitly asked (crossDaemon), or when the selector
// names a `daemon:` value that is not this daemon's id.
//
// Values are compared after trimming, and NOT because the matcher does the same
// — it does not. bcast.Selector.Matches is a bare slices.Contains against the
// chat's DaemonID (lib/bossalib/broadcast/match.go), so a padded value matches
// nothing anywhere. Trimming here is one-directional and defensive: it exists so
// a padded copy of OUR OWN id is recognised as us and does not publish. Being
// stricter than the matcher is the safe direction — the failure it prevents is
// fanning a broadcast out to the fleet, while the failure it cannot cause is at
// worst a padded foreign id that routes and then matches zero chats, which is
// what a padded value does on every daemon regardless.
//
// Three boundary decisions, each deliberate:
//
//  1. A selector naming ONLY this daemon's id does NOT reach beyond. It names no
//     daemon but us, so routing it would hand bosso a broadcast every other
//     daemon must resolve to zero targets, and would send our own id back to us
//     for the ingress loop guard to discard.
//
//     Note what this does NOT claim: that such a selector matches local chats.
//     bcast.Selector.Matches compares a `daemon:` value against the chat's
//     DaemonID, and no write path populates agent_chats.daemon_id today (the
//     column is NOT NULL and defaults to the empty string), so a `daemon:` clause
//     resolves to zero targets on EVERY daemon, local included. Until that column
//     carries a value, the
//     workable cross-daemon shape is the explicit cross_daemon flag over a
//     repo:/agent:/chat: selector; wiring the daemon dimension end to end is a
//     follow-up on the broadcast epic. This predicate is correct either way —
//     the case simply cannot arise yet.
//
//  2. A selector naming NO daemon dimension at all does NOT reach beyond unless
//     crossDaemon. "repo:foo" is ambiguous about reach, and resolving that
//     ambiguity towards the fleet would make every existing single-daemon
//     broadcast silently fan out — a behaviour change nobody asked for, and the
//     one with a message-storm failure mode. cross_daemon is how a caller says
//     "yes, other daemons too" (see the field's proto comment).
//
//  3. An UNKNOWN local daemon id (daemonID == "", e.g. no upstream configured so
//     ResolveDaemonID never ran) does NOT infer egress from the selector; only
//     the explicit crossDaemon flag publishes. The tempting alternative — treat
//     every named daemon value as foreign, since none can be proven to be us —
//     is the wrong way to fail here: the ingress loop guard recognises an echo
//     by comparing origin_daemon_id against the local id, and with an empty
//     local id it can match nothing, so exactly when we cannot identify
//     ourselves is when a self-echo would NOT be dropped. Fail closed and stay
//     local. In practice this costs nothing: a daemon with no resolved id has no
//     upstream stream to publish on either.
func ReachesBeyondDaemon(sel bcast.Selector, daemonID string, crossDaemon bool) bool {
	if crossDaemon {
		return true
	}
	local := strings.TrimSpace(daemonID)
	if local == "" {
		// Decision 3: cannot tell foreign from self, so infer nothing.
		return false
	}
	for i := range sel.Clauses {
		for _, id := range sel.Clauses[i].DaemonIDs {
			// A blank value is not addressable (see bcast.Target) and must not be
			// read as "some other daemon": a wire-decoded selector can carry one,
			// and treating it as foreign would fan a junk selector out fleet-wide.
			if trimmed := strings.TrimSpace(id); trimmed != "" && trimmed != local {
				return true
			}
		}
	}
	return false
}
