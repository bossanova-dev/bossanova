package broadcast

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"

	bcast "github.com/recurser/bossalib/broadcast"
	"github.com/recurser/bossalib/models"
)

// This file is the INGRESS half of the cross-daemon broadcast path: it turns a
// broadcast bosso routed here into LOCAL delivery rows.
//
// It adds NO second delivery path. Receive reuses the same Resolver (through the
// package's send seam) and the same Sender the SendBroadcast RPC uses, so the
// fan-out cap, the StartError filter and the terminal-session guard apply
// identically to a routed broadcast and a locally-issued one, and the existing
// delivery worker drains the rows on its next tick with no knowledge that they
// arrived from elsewhere.
//
// THE INGRESS HOLDS NO EgressPublisher, AND MUST NOT. That structural absence is
// the anti-storm guarantee: if receiving a broadcast could publish one, two
// daemons that each resolve targets for the other's selector would trade the
// same message forever. Egress is a decision the SEND path makes about a
// broadcast this daemon ORIGINATED (see egress.go and the deliberately separate
// pb.BroadcastEgress / pb.BroadcastCommand message types); it is not something
// a receipt can trigger. TestIngressReceiveNeverPublishesEgress asserts the
// absence by reflection so it survives a future field being added here.
//
// The message body is a SECRET on this path exactly as it is on the send path:
// it reaches the store (the worker's only source) and nothing else. No log line
// and no error string returned from here may contain it.

// IngressStore is the subset of db.SQLiteBroadcastStore the ingress reads
// through: one point lookup for the idempotency probe, plus the state-guarded
// delete that clears a stranded row (see Receive step 3). It is narrowed for the
// same reason SendStore is — it keeps the dependency honest and lets a test
// script the probe (including its failure) without a migrated SQLite database.
//
// Get reports sql.ErrNoRows when the broadcast is absent; see Receive for why
// only that error may be read as "absent".
//
// DeleteUnmaterialised is deliberately the state-guarded delete rather than the
// plain one: the ingress only ever wants to remove a row it believes is a
// never-materialised send, and doing that as read-then-delete is a check-then-act
// race whose loser cascades away a fully materialised broadcast. See the store
// method's own comment.
type IngressStore interface {
	Get(ctx context.Context, id string) (*models.Broadcast, error)
	DeleteUnmaterialised(ctx context.Context, id string) (bool, error)
}

// ErrInvalidInbound marks an inbound broadcast that is PERMANENTLY unacceptable
// — a missing id, a blank body, a missing expiry, an unparseable selector.
//
// It exists so the transport boundary can tell a permanent failure from a
// transient one. The reverse stream is at-least-once: an error the sender reads
// as "try again" WILL come back, so a malformed command that reports the same
// undifferentiated failure as "database is locked" is a poison pill that
// redelivers forever. The dispatcher maps this sentinel (and ErrTooManyTargets,
// which is equally deterministic — the same selector resolves over the cap every
// time) to an invalid-argument command result, so the router can drop it.
//
// Errors wrapping it never carry the message body; see the file comment.
var ErrInvalidInbound = errors.New("invalid inbound broadcast")

// ErrIngressInFlight marks a redelivery that arrived while a concurrent sibling
// is still materialising the same broadcast — the deliberate opposite of
// ErrInvalidInbound.
//
// It is TRANSIENT, and it is an error rather than a nil deferral for one
// reason: at the moment we observe the sibling it holds a PENDING row, so no
// audience is committed yet and the send may still fail and clean up. Reporting
// ok would let this attempt retire a command whose only other attempt then
// vanishes, leaving no broadcast and no deliveries with nothing left to retry.
// Deferring without acknowledging keeps the redelivery coming until an attempt
// reaches an outcome that is actually settled — see the probe in Receive.
//
// It carries NO special typing at the transport boundary, and must not acquire
// any: the adapter maps only the deterministic sentinels to invalid-argument and
// leaves everything else retryable, which is precisely the handling this needs.
// Errors wrapping it never carry the message body; see the file comment.
var ErrIngressInFlight = errors.New("inbound broadcast is already being materialised")

// ingressSender is the one method Ingress needs from *Sender, declared as an
// interface for the same reason audienceResolver is: a test can script the send
// (including a failure) without standing up a store behind it, and the
// dependency stays a seam rather than a concrete type.
type ingressSender interface {
	Send(ctx context.Context, req SendRequest) (*SendResult, error)
}

// InboundBroadcast is one broadcast this daemon has been told it is a target of.
//
// It is the ingress path's own vocabulary rather than the wire proto, mirroring
// EgressEvent: the decision of what a receipt DOES stays free of the transport
// that delivered it, and the adapter that turns a pb.BroadcastCommand into this
// lives with the upstream command dispatcher.
type InboundBroadcast struct {
	// ID is the ORIGIN's broadcast id, persisted verbatim. Every daemon that
	// delivers this broadcast records it under this id, which is the only thing
	// that makes a redelivery recognisable — see the idempotency probe in Receive.
	ID string
	// Selector is the origin's validated audience, travelling unresolved. It is
	// re-resolved HERE against local chats only; the origin's resolution says
	// nothing about which of our chats match.
	Selector bcast.Selector
	// OriginDaemonID is the daemon that issued the broadcast. Receive's loop guard
	// compares it against this daemon's persisted id.
	OriginDaemonID string
	// OriginChatID is the chat the broadcast was issued from, on the ORIGIN
	// daemon, or "" for an operator-issued send. It is provenance here, never a
	// local chat id — see IncludeOrigin in Receive.
	OriginChatID string
	// Message is the SECRET body (see the file comment).
	Message string
	// ExpiresAt is the origin's absolute expiry, honoured verbatim so a slow route
	// cannot extend a broadcast's lifetime past what the sender asked for.
	ExpiresAt time.Time
}

// Ingress materialises inbound cross-daemon broadcasts as local delivery rows.
//
// Its fields are exactly a store (for the idempotency probe), the send seam, the
// local daemon id (for the loop guard) and a logger. Notably NOT among them: an
// EgressPublisher (see the file comment).
type Ingress struct {
	store    IngressStore
	sender   ingressSender
	daemonID string
	logger   zerolog.Logger
	// now reads the current time for the stranded-row age test. Injectable
	// because that test is the ONLY thing separating "a sibling is materialising
	// this right now" from "nobody is coming back for this", and a test that had
	// to sleep StrandedAfter to cover it would not be written. Supplied through
	// the constructor, matching SubscriptionEvaluator rather than inventing a
	// third clock shape for this package.
	now func() time.Time
}

// StrandedAfter is how long a broadcast may sit pending before the ingress
// treats it as abandoned rather than in-flight.
//
// A healthy send is pending only between Create and CreateDeliveries — two
// statements, milliseconds apart. A row still pending a minute later means the
// send died between them AND the Sender's best-effort cleanup Delete also
// failed. The value is deliberately far from both edges it separates: long
// enough that no concurrent sibling is ever mistaken for a corpse (the
// consequence there is mutual destruction, since Delete cascades), short enough
// to recover well inside any broadcast's expiry window (the consequence there is
// only that recovery waits for the next redelivery).
//
// Recovery is therefore conditional on a redelivery arriving LATE — more than
// StrandedAfter after the strand. Clearing on the FIRST redelivery instead is
// what would let two concurrent receives destroy each other's rows, so the wait
// is not negotiable. What keeps the wait from being a dead end is that an early
// redelivery is refused rather than acknowledged (ErrIngressInFlight): the
// refusal is transient, so the stream comes back, and recovery no longer depends
// on the first redelivery happening to arrive late.
const StrandedAfter = time.Minute

// NewIngress constructs an Ingress over the broadcast store and the send path.
// now may be nil (defaults to time.Now), matching NewSubscriptionEvaluator.
//
// daemonID must be this daemon's PERSISTED id (upstream.ResolveDaemonID), the
// same value the egress path stamps as origin_daemon_id — the loop guard
// compares the two, and a per-process value would defeat it.
func NewIngress(store IngressStore, sender ingressSender, daemonID string, now func() time.Time, logger zerolog.Logger) *Ingress {
	if now == nil {
		now = time.Now
	}
	return &Ingress{store: store, sender: sender, daemonID: daemonID, now: now, logger: logger}
}

// Receive turns one inbound broadcast into local delivery rows. Its ORDER is the
// specification:
//
//  1. LOOP GUARD, before anything else. A command whose origin daemon is this
//     daemon is our own broadcast echoed back, and is dropped without touching
//     the store or the resolver. A drop returns nil: it is a success, not a
//     retryable failure, and bosso must not redeliver it.
//  2. VALIDATE. A missing id, a blank body or an invalid selector is an argument
//     error naming the FIELD.
//  3. IDEMPOTENCY, on the ORIGIN's id. A broadcast we already hold RESOLVED
//     creates no additional rows. A row stranded at PENDING is unmaterialised,
//     so it is cleared and re-resolved rather than mistaken for a completed
//     handling. A row pending only briefly is a concurrent sibling: it is left
//     untouched, but the redelivery is REFUSED (ErrIngressInFlight) rather than
//     acknowledged, because that sibling has committed no audience yet — see the
//     comment at the probe.
//  4. MATERIALISE through the Sender, which gives local-only resolution, the
//     fan-out cap, the StartError filter and persistence under the origin's id.
//  5. LOG the receipt with the broadcast id, the origin daemon id and the
//     resolved target count. Never the body, and never the selector — the rule
//     is "ids and counts only", which the selector satisfies but which is worth
//     keeping narrow so nobody has to re-derive that a selector holds no prose.
//
// ErrTooManyTargets propagates (wrapped, so errors.Is still finds it): a runaway
// inbound selector is loud, never silently truncated.
func (i *Ingress) Receive(ctx context.Context, in InboundBroadcast) error {
	// 1. Loop guard. Trimmed on both sides, matching how ReachesBeyondDaemon and
	// the store normalise ids.
	//
	// A BLANK LOCAL ID (daemonID == "", e.g. no upstream configured so
	// ResolveDaemonID never ran) is deliberately NOT treated as matching. The
	// tempting reading — "we cannot prove this is not us, so drop it" — would make
	// an operator-issued command with no origin daemon, or any command at all once
	// the local id is unknown, vanish silently: the guard would swallow the fleet's
	// broadcasts and report success. Fail towards DELIVERING instead, which is the
	// mirror image of ReachesBeyondDaemon's decision 3 (a blank local id publishes
	// nothing): a daemon that cannot identify itself never ORIGINATES a routed
	// broadcast, so it has no echo of its own to receive, and step 3's idempotency
	// probe is the second line of defence if one somehow arrives.
	local := strings.TrimSpace(i.daemonID)
	origin := strings.TrimSpace(in.OriginDaemonID)
	if local != "" && origin == local {
		i.logger.Debug().
			Str("broadcast_id", in.ID).
			Str("origin_daemon_id", origin).
			Msg("broadcast ingress: dropped a command this daemon originated")
		return nil
	}

	// 2. Validate. Errors name the field and never the body, and every one of them
	// wraps ErrInvalidInbound: a malformed command is PERMANENT, and the boundary
	// has to be able to say so or an at-least-once stream will redeliver it
	// forever (see ErrInvalidInbound).
	//
	// ExpiresAt is checked HERE, not only in the transport adapter that decodes
	// the wire timestamp. The adapter rejects an absent/zero expires_at because
	// timestamppb's nil AsTime() is the 1970 epoch; but NewIngress and
	// BroadcastReceiver are exported, so a second caller would bypass that and
	// persist a broadcast every delivery is instantly overdue for, with no error
	// anywhere. One invariant, enforced at the domain boundary that owns it.
	if strings.TrimSpace(in.ID) == "" {
		return fmt.Errorf("%w: broadcast_id is required", ErrInvalidInbound)
	}
	if strings.TrimSpace(in.Message) == "" {
		return fmt.Errorf("%w: broadcast %s: message is required", ErrInvalidInbound, in.ID)
	}
	if in.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: broadcast %s: expires_at is required", ErrInvalidInbound, in.ID)
	}
	if err := in.Selector.Validate(); err != nil {
		return fmt.Errorf("%w: broadcast %s: invalid selector: %v", ErrInvalidInbound, in.ID, err)
	}

	// 3. Idempotency, keyed on the ORIGIN's id. ONLY sql.ErrNoRows means
	// "absent": any other error is a read we could not make, and treating it as
	// absent would re-resolve the audience and deliver the same message a second
	// time under the same id.
	//
	// EXISTENCE ALONE IS NOT THE TEST — the row's STATE is. Create and
	// CreateDeliveries are separate transactions: Create leaves the row PENDING,
	// and CreateDeliveries is what atomically inserts the audience and moves it
	// pending -> resolved. A row can therefore be stranded at pending with no
	// deliveries, when CreateDeliveries fails AND Sender's best-effort cleanup
	// Delete fails too (both plausible together under one "database is locked"
	// burst). Nothing drives such a row forward: the worker only lists resolved
	// broadcasts. So reading bare existence as "already handled" would make the
	// at-least-once REDELIVERY — the very mechanism that exists to recover this —
	// return success forever while the broadcast is never delivered. Silent
	// non-delivery, on the one path built to prevent it.
	//
	// A pending row holds no delivery rows (the audience insert and the state move
	// commit together or not at all), so clearing one cannot orphan or duplicate a
	// delivery. But "pending" is ALSO the transient state of a send that is
	// in-flight right now, between Create and CreateDeliveries — and dispatch is
	// async over an at-least-once stream, so two Receive calls for one id really
	// can overlap. Deleting a sibling's live row would make its CreateDeliveries
	// fail, and Delete cascades, so the two calls would destroy each other's work
	// and leave nothing delivered.
	//
	// StrandedAfter is what separates the two: it is orders of magnitude longer
	// than the millisecond window a healthy send spends pending, and orders of
	// magnitude shorter than a broadcast's expiry, so a row still pending past it
	// is one nobody is coming back for. A younger pending row is treated as a
	// concurrent sibling and left strictly alone.
	//
	// LEFT ALONE, BUT NOT ACKNOWLEDGED. Deferring to the sibling means we create
	// no rows; it must NOT mean we report success. At the moment we observe it,
	// the sibling holds a PENDING row and has therefore committed no audience at
	// all — and it may still fail its CreateDeliveries and clean the row away, at
	// which point this id has no broadcast and no deliveries anywhere. A success
	// here would be the receipt for that outcome: the two overlapping attempts are
	// redeliveries of ONE command, so an ok from either can retire it, and the
	// sibling's own failure result arrives too late to undo that. Nothing is
	// delivered and nothing ever retries — the same silent non-delivery the
	// state-over-existence rule above exists to prevent, reached by the other
	// branch.
	//
	// So this returns ErrIngressInFlight, which is TRANSIENT by construction: the
	// adapter types only the deterministic sentinels as InvalidArgument and leaves
	// everything else retryable, so this comes back. Every redelivery converges —
	// a sibling that resolved is the already-recorded no-op below, a sibling that
	// failed and cleaned up leaves nothing to find and we materialise, and a
	// sibling that failed and stranded its row is cleared once the retry lands
	// past StrandedAfter. Only an outcome nobody is waiting on is acknowledged.
	//
	// The age test is a HEURISTIC and is not load-bearing on its own; the delete
	// is what has to be safe. DeleteUnmaterialised folds "and it is still pending"
	// into the DELETE's own WHERE clause, so a sibling that commits its audience
	// between our Get and our delete simply is not matched — the read-then-delete
	// race has no losing branch. It reports whether it removed anything, and a
	// false there means the row moved on under us: somebody else materialised it,
	// which is the redelivery no-op by another name. (Sender likewise declines to
	// clean up after ErrBroadcastAlreadyResolved or sql.ErrNoRows.)
	switch existing, err := i.store.Get(ctx, in.ID); {
	case err == nil && existing != nil && existing.State == models.BroadcastStatePending:
		if age := i.now().Sub(existing.UpdatedAt); age < StrandedAfter {
			i.logger.Debug().
				Str("broadcast_id", in.ID).
				Str("origin_daemon_id", origin).
				Dur("age", age).
				Msg("broadcast ingress: a sibling is materialising this broadcast right now; no additional rows created, asking for a redelivery")
			return fmt.Errorf("%w: broadcast %s: a sibling is materialising it", ErrIngressInFlight, in.ID)
		}
		cleared, err := i.store.DeleteUnmaterialised(ctx, in.ID)
		if err != nil {
			return fmt.Errorf("clear stranded inbound broadcast %s: %w", in.ID, err)
		}
		if !cleared {
			i.logger.Debug().
				Str("broadcast_id", in.ID).
				Str("origin_daemon_id", origin).
				Msg("broadcast ingress: the stranded broadcast was materialised under us; no additional rows created")
			return nil
		}
		i.logger.Warn().
			Str("broadcast_id", in.ID).
			Str("origin_daemon_id", origin).
			Msg("broadcast ingress: cleared a stranded unmaterialised broadcast; re-resolving")
	case err == nil && existing != nil:
		i.logger.Debug().
			Str("broadcast_id", in.ID).
			Str("origin_daemon_id", origin).
			Msg("broadcast ingress: broadcast already recorded here; no additional rows created")
		return nil
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("look up inbound broadcast %s: %w", in.ID, err)
	}

	// 4. Materialise through the existing send path.
	//
	// IncludeOrigin is TRUE. Self-exclusion exists so a chat broadcasting "I
	// finished" does not wake itself into a loop, and it can only apply where the
	// origin chat is one of the local candidates. Here origin_chat_id names a chat
	// on ANOTHER daemon, so excluding it is a no-op at best — and if a local chat
	// happened to share that id, it would silently drop a target the selector
	// genuinely matched, with no error anywhere. The selector alone decides this
	// daemon's audience; origin_chat_id is provenance.
	res, err := i.sender.Send(ctx, SendRequest{
		ID:            in.ID,
		Selector:      in.Selector,
		Message:       in.Message,
		OriginChatID:  in.OriginChatID,
		IncludeOrigin: true,
		ExpiresAt:     in.ExpiresAt,
	})
	if err != nil {
		// Wrapped, not replaced: ErrTooManyTargets must stay findable by errors.Is
		// so the dispatcher can tell "the selector was too wide" from "the write
		// broke". Sender errors never carry the body (see its own tests).
		return fmt.Errorf("materialise inbound broadcast %s: %w", in.ID, err)
	}

	// 5. Receipt: ids and counts only.
	i.logger.Info().
		Str("broadcast_id", in.ID).
		Str("origin_daemon_id", origin).
		Int("target_count", len(res.Deliveries)).
		Msg("broadcast ingress: materialised an inbound broadcast for local delivery")
	return nil
}
