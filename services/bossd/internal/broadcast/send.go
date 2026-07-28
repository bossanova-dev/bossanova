package broadcast

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog"

	bcast "github.com/recurser/bossalib/broadcast"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
)

// cleanupTimeout bounds the detached delete that removes a broadcast whose
// audience failed to materialise.
const cleanupTimeout = 5 * time.Second

// SendStore is the subset of db.SQLiteBroadcastStore the send path writes
// through. Narrowed for the same reason workerStore is: it keeps the dependency
// honest and lets a test script a store failure — the post-Create cleanup path
// in particular — without standing up a migrated SQLite database.
type SendStore interface {
	Create(ctx context.Context, b *models.Broadcast) error
	CreateDeliveries(ctx context.Context, broadcastID string, targets []models.BroadcastDelivery) error
	CompleteIfSettled(ctx context.Context, broadcastID string) error
	Delete(ctx context.Context, id string) error
}

// audienceResolver is the one method Sender needs from *Resolver, declared as an
// interface so a send test can script an audience (including the over-cap
// refusal) without a chat and session store behind it.
type audienceResolver interface {
	Resolve(ctx context.Context, sel bcast.Selector, originChatID string, includeOrigin bool) ([]bcast.Target, error)
}

// SendRequest is one broadcast to resolve and record.
//
// Message is a SECRET body: it is here because a send needs it, and must never
// reach a log line, an error string, or a persisted diagnostic.
type SendRequest struct {
	// ID is an optional pre-minted broadcast id. Empty lets the store mint one.
	//
	// It exists for standing subscriptions: the evaluator has to record the
	// broadcast id on the subscription row as part of the CAS that precedes the
	// send, so the id cannot come back out of the send — it is minted up front
	// and the send must honour it, or fired_broadcast_id points at nothing.
	ID string
	// Selector is the audience. Validation belongs to the caller: an invalid
	// selector is an argument error to reject, not an audience to silently narrow.
	Selector bcast.Selector
	// Message is the SECRET body to deliver.
	Message string
	// OriginChatID is the chat this send is attributed to, or "" for an
	// operator-issued one.
	OriginChatID string
	// IncludeOrigin keeps the origin chat in its own audience. It only disables
	// the resolver's self-exclusion — it never ADDS a target, so the selector
	// still decides the audience either way.
	//
	// False is the default and what an explicit send passes: there the origin is
	// the SENDER, which must not wake itself. A fired subscription passes TRUE,
	// because there the origin is the REQUESTER — see Sender.SendBroadcast for
	// why that inversion is load-bearing.
	IncludeOrigin bool
	// ExpiresAt bounds the delivery window.
	ExpiresAt time.Time
}

// SendResult reports what a completed send recorded.
type SendResult struct {
	// Broadcast is the persisted row with its post-send state applied
	// (resolved, or completed when the audience was empty and settled
	// synchronously). It is the in-memory value, not a re-read.
	Broadcast *models.Broadcast
	// Deliveries is the frozen audience, in resolution order. Reading them back
	// would not reproduce that order (every row in a batch shares one created_at).
	Deliveries []models.BroadcastDelivery
}

// Sender is THE broadcast send path: resolve an audience, record the broadcast
// and freeze the audience into delivery rows for the worker to drain.
//
// It exists as a type rather than as handler code because there are two callers
// — the SendBroadcast RPC and the standing-subscription evaluator — and a second
// copy of this composition is exactly the drift the subscription feature must
// not introduce. "Subscriptions are a thin layer over the existing delivery
// path" is structural here: the evaluator fires into the same Sender the RPC
// sends through.
// Sender deliberately holds NO clock. Its siblings (SubscriptionEvaluator,
// DeliveryWorker) take an injectable `now` because they make time-dependent
// decisions; the Sender makes none — ExpiresAt arrives on the request, already
// derived from its caller's clock. A `now` field here would look like a seam
// and control nothing.
type Sender struct {
	store    SendStore
	resolver audienceResolver
	logger   zerolog.Logger
}

// NewSender constructs a Sender over the broadcast store and audience resolver.
func NewSender(store SendStore, resolver audienceResolver, logger zerolog.Logger) *Sender {
	return &Sender{store: store, resolver: resolver, logger: logger}
}

// Send resolves the request's audience and records the broadcast plus one
// delivery per target; the delivery worker then sends them asynchronously with
// retry.
//
// Resolution happens ONCE, here, and the audience is frozen into the delivery
// rows: a chat created afterwards is never retroactively included. Over the
// fan-out cap is a hard refusal that names the count and the cap (never a
// truncation), and it happens before anything is written. Zero targets is a
// SUCCESS with no deliveries, settled synchronously so it reaches its terminal
// state rather than lingering as resolved until the worker's next tick.
//
// Errors are wrapped with the operation that failed and never carry the message
// body. ErrTooManyTargets is preserved through errors.Is so the RPC boundary can
// map it to an argument error.
func (s *Sender) Send(ctx context.Context, req SendRequest) (*SendResult, error) {
	targets, err := s.resolver.Resolve(ctx, req.Selector, req.OriginChatID, req.IncludeOrigin)
	if err != nil {
		// Deliberately unwrapped for the cap: the boundary distinguishes "you asked
		// for too many" (an argument error) from "resolution broke" by errors.Is.
		if errors.Is(err, ErrTooManyTargets) {
			return nil, err
		}
		return nil, fmt.Errorf("resolve broadcast audience: %w", err)
	}

	broadcast := &models.Broadcast{
		ID:        req.ID,
		Selector:  req.Selector.String(),
		Message:   req.Message,
		State:     models.BroadcastStatePending,
		ExpiresAt: req.ExpiresAt,
	}
	if req.OriginChatID != "" {
		origin := req.OriginChatID
		broadcast.OriginChatID = &origin
	}
	if err := s.store.Create(ctx, broadcast); err != nil {
		return nil, fmt.Errorf("create broadcast: %w", err)
	}

	deliveries := make([]models.BroadcastDelivery, 0, len(targets))
	for _, t := range targets {
		deliveries = append(deliveries, models.BroadcastDelivery{
			BroadcastID:    broadcast.ID,
			TargetChatID:   t.ChatID,
			TargetDaemonID: t.DaemonID,
			State:          models.BroadcastDeliveryStatePending,
		})
	}
	// One transaction stamps target_count and moves the broadcast pending ->
	// resolved alongside the delivery inserts, so the audience is never
	// observable as half-materialised.
	if err := s.store.CreateDeliveries(ctx, broadcast.ID, deliveries); err != nil {
		// TWO failures must NOT be cleaned up after, because in both of them the
		// row under this id is not the pending one we created:
		//
		//   - ErrBroadcastAlreadyResolved — somebody else's RESOLVED broadcast,
		//     complete with its materialised audience.
		//   - sql.ErrNoRows — no row at all, so there is definitionally nothing
		//     of ours to remove.
		//
		// Delete is a hard delete that cascades to broadcast_deliveries, so
		// "cleaning up" in either case reaches for an id we no longer own: it
		// would destroy a send that SUCCEEDED and report only our own error,
		// leaving the caller who was told ok with no rows and no trace. Clean up
		// only after a failure that leaves OUR unmaterialised row behind.
		if !errors.Is(err, db.ErrBroadcastAlreadyResolved) && !errors.Is(err, sql.ErrNoRows) {
			s.cleanUpUnmaterialised(ctx, broadcast.ID)
		}
		return nil, fmt.Errorf("create broadcast deliveries: %w", err)
	}

	// PAST THIS POINT THE SEND HAS HAPPENED. CreateDeliveries committed, so the
	// audience is materialised and the worker will deliver it whatever happens
	// next. Everything below only decides how accurately the result DESCRIBES
	// that fact, so a failure here is logged and degraded, never returned:
	// reporting failure would invite a retry that resolves a second audience and
	// delivers the message twice — under two different broadcast ids, which
	// defeats the at-least-once dedupe hint carried in the prompt.
	broadcast.State = models.BroadcastStateResolved
	if len(deliveries) == 0 {
		// Zero targets is a successful send, and an empty batch still resolves the
		// broadcast and stamps target_count = 0. But a resolved broadcast with no
		// delivery rows has nothing to drive it forward except the worker's
		// per-tick CompleteIfSettled, so it would linger as "resolved" until the
		// next poll. Settle it here; if that fails, the worker still completes it.
		if err := s.store.CompleteIfSettled(ctx, broadcast.ID); err != nil {
			s.logger.Warn().Err(err).Str("broadcast_id", broadcast.ID).
				Msg("send broadcast: could not settle the empty broadcast; the worker will complete it")
		} else {
			broadcast.State = models.BroadcastStateCompleted
		}
	}
	return &SendResult{Broadcast: broadcast, Deliveries: deliveries}, nil
}

// SendBroadcast fires the broadcast a standing subscription produced, satisfying
// the evaluator's sender contract.
//
// Two properties are load-bearing and neither is optional.
//
// The broadcast is persisted under the id the EVALUATOR minted (see
// SendRequest.ID), so the subscription's fired_broadcast_id resolves.
//
// And IncludeOrigin is TRUE, which inverts the default an explicit SendBroadcast
// uses. Self-exclusion exists so a chat BROADCASTING "I finished" does not wake
// itself into a loop — there the origin is the sender. On a fired subscription
// the origin is the REQUESTER: it registered "tell me when this child finishes"
// and is the whole reason the message exists. Excluding it would silently drop
// the one chat that asked, leaving the subscription marked fired with an empty
// audience and no error anywhere — the exact silent no-fire this feature must
// not have. The selector alone decides the audience; origin_chat_id is
// provenance.
//
// NO CROSS-DAEMON EGRESS (BOS-558), deliberately. A fired subscription resolves
// and delivers LOCALLY only: it never calls ReachesBeyondDaemon and never
// publishes a BroadcastEgress, so a subscription whose selector names a foreign
// daemon fires with a local-only audience. That is an asymmetry with the
// SendBroadcast RPC, and it is recorded here rather than left to be discovered,
// because "subscriptions are a thin layer over the existing delivery path" makes
// every divergence between the two paths worth naming. It is scoped, not
// accidental: the cross-daemon child wires the RPC's explicit cross_daemon flag,
// which a standing subscription has no way to express yet, and inferring fleet
// reach for a background evaluator is exactly the silent fan-out that
// ReachesBeyondDaemon's decision 2 refuses. Giving subscriptions a cross-daemon
// reach is a follow-up on the broadcast epic, not a gap to paper over here.
func (s *Sender) SendBroadcast(ctx context.Context, b SubscriptionBroadcast) error {
	_, err := s.Send(ctx, SendRequest{
		ID:            b.ID,
		Selector:      b.Selector,
		Message:       b.Message,
		OriginChatID:  b.OriginChatID,
		IncludeOrigin: true,
		ExpiresAt:     b.ExpiresAt,
	})
	return err
}

// cleanUpUnmaterialised removes a broadcast whose audience failed to
// materialise.
//
// Create committed in its own transaction, so without this a failed send would
// strand a pending broadcast with no delivery rows: the worker only lists
// resolved ones, so nothing drives it, and the lazy sweep would later retire a
// message that was never sent as `expired` — while holding its secret body at
// rest for the whole expiry window. A failed send must leave nothing behind,
// which is what the over-the-cap path already guarantees by refusing before
// Create. Cleanup is best-effort: the original failure is what the caller is
// told about either way.
//
// WithoutCancel because a cancelled caller context is one of the likelier ways
// CreateDeliveries fails at all, and the cleanup must still run when it does.
// Bounded so a wedged write cannot outlive the send.
func (s *Sender) cleanUpUnmaterialised(ctx context.Context, broadcastID string) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := s.store.Delete(cleanupCtx, broadcastID); err != nil {
		s.logger.Warn().Err(err).Str("broadcast_id", broadcastID).
			Msg("send broadcast: could not clean up the broadcast whose audience failed to materialise")
	}
}
