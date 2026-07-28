package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	bcast "github.com/recurser/bossalib/broadcast"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/githubcallback"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// broadcastSubscriptionStore is the subset of
// db.SQLiteBroadcastSubscriptionStore the three subscription handlers use,
// narrowed here for the same reason send_broadcast.go narrows broadcastStore:
// it keeps the Server's dependency honest and lets a test drive a store failure
// without a migrated database. The firing primitives (MarkFired, ExpireOverdue,
// ListActive*) are deliberately absent — they belong to the evaluator, and the
// RPC surface must not be able to fire or retire a subscription out of band.
//
// Callers must pass a non-nil implementation or leave the field unset: a nil
// CONCRETE pointer stored in this interface is not == nil, so it would defeat
// the handlers' "not configured" guard and panic instead.
type broadcastSubscriptionStore interface {
	Create(ctx context.Context, sub *models.BroadcastSubscription) error
	Get(ctx context.Context, id string) (*models.BroadcastSubscription, error)
	List(ctx context.Context, filter db.ListBroadcastSubscriptionsFilter) ([]*models.BroadcastSubscription, error)
	Cancel(ctx context.Context, id string) (bool, error)
}

var _ broadcastSubscriptionStore = (*db.SQLiteBroadcastSubscriptionStore)(nil)

// broadcastSubscriptionToProto maps a persisted subscription onto its wire
// form.
//
// WRITE-ONLY BODY RULE — this mapper is the single choke point for it. Message
// has no counterpart on pb.BroadcastSubscription at all: unlike pb.Broadcast
// (which carries the body for the delivering owner and clears it on reads), a
// subscription is only ever built on a read path, so the body has no
// legitimate reason to exist on the wire. There is deliberately nothing to
// assign here, and nothing may be added — see the omission rule stated on
// pb.BroadcastSubscription.
func broadcastSubscriptionToProto(sub *models.BroadcastSubscription) *pb.BroadcastSubscription {
	if sub == nil {
		return nil
	}
	msg := &pb.BroadcastSubscription{
		Id:             sub.ID,
		OwnerSessionId: sub.OwnerSessionID,
		TriggerEvent:   sub.TriggerEvent.String(),
		State:          sub.State.String(),
		ExpiresAt:      timestamppb.New(sub.ExpiresAt),
		CreatedAt:      timestamppb.New(sub.CreatedAt),
		UpdatedAt:      timestamppb.New(sub.UpdatedAt),
	}
	if sub.OriginChatID != nil {
		msg.OriginChatId = *sub.OriginChatID
	}
	if sub.FiredBroadcastID != nil {
		msg.FiredBroadcastId = *sub.FiredBroadcastID
	}
	if sub.FiredAt != nil {
		msg.FiredAt = timestamppb.New(*sub.FiredAt)
	}
	// The stored selector is the canonical string a validated Selector produced,
	// so this re-parse is total in practice. A row that somehow fails to parse
	// still lists (with no selector) rather than failing the whole read — the
	// selector is metadata, not the reason the caller asked. Same policy as
	// broadcastToProto.
	if sel, err := bcast.Parse(sub.Selector); err == nil {
		msg.Selector = bcast.SelectorToProto(sel)
	}
	return msg
}

// broadcastSubscriptionError maps a subscription store error to a connect code.
// Absent rows become NotFound, everything else Internal. Store errors name the
// offending field and never the registered body, so wrapping them is safe.
func broadcastSubscriptionError(op string, err error) *connect.Error {
	if errors.Is(err, sql.ErrNoRows) {
		return connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewError(connect.CodeInternal, fmt.Errorf("%s: %w", op, err))
}

// CreateBroadcastSubscription registers a standing rule: when the owning
// session reaches an outcome matching trigger_event, the daemon broadcasts the
// registered message to the audience the selector resolves.
//
// Resolution is deferred to FIRE time — the opposite of SendBroadcast, which
// freezes its audience at send time. That is the whole point of a subscription:
// the chats that exist when the session settles are the ones told about it.
//
// Cascades are not detected: the fired broadcast wakes chats that may hold
// subscriptions of their own. See the RPC's proto comment — the fan-out cap and
// subscription expiry are what bound the blast radius in v1.
//
// The response never carries the message body — see
// broadcastSubscriptionToProto.
func (s *Server) CreateBroadcastSubscription(ctx context.Context, req *connect.Request[pb.CreateBroadcastSubscriptionRequest]) (*connect.Response[pb.CreateBroadcastSubscriptionResponse], error) {
	store := s.BroadcastSubscriptions()
	if store == nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("broadcast subscription store not configured"))
	}
	msg := req.Msg

	ownerSessionID := strings.TrimSpace(msg.GetOwnerSessionId())
	if ownerSessionID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("owner_session_id is required"))
	}
	// Decoding and validating are separate steps: SelectorFromProto is total, so
	// the boundary must make the Validate call itself. An absent or empty
	// selector is an argument error — it never means everyone.
	sel := bcast.SelectorFromProto(msg.GetSelector())
	if err := sel.Validate(); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if strings.TrimSpace(msg.GetMessage()) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("message is required"))
	}
	// An unknown trigger is refused rather than stored: the evaluator classifies
	// by this exact vocabulary, so an unrecognised value would register a rule
	// that could never match any outcome — a standing subscription that silently
	// never fires is the worst failure this feature has.
	trigger, err := models.ParseBroadcastTrigger(strings.TrimSpace(msg.GetTriggerEvent()))
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	// Expiry uses the callback convention verbatim (empty = 24h, max 30 days) by
	// sharing its parser, exactly as SendBroadcast does, so the registration
	// surfaces cannot drift apart.
	expiresIn, err := githubcallback.ParseDuration(msg.GetExpiresIn())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	// The owner must name a real session. Unlike SendBroadcast's origin chat —
	// provenance the send does not depend on — the owner id is the trigger: a
	// subscription on a session that does not exist could never fire, and would
	// sit until expiry looking healthy.
	//
	// Routed through broadcastSubscriptionError so ONLY an absent row becomes
	// NotFound. Collapsing every lookup failure to NotFound would report a
	// database error, a cancelled context or a deadline as "no such session" — a
	// permanent-looking answer to a retryable fault, which a client is right to
	// stop retrying.
	if s.sessions != nil {
		if _, err := s.sessions.Get(ctx, ownerSessionID); err != nil {
			return nil, broadcastSubscriptionError("resolve owner session", err)
		}
	}

	sub := &models.BroadcastSubscription{
		OwnerSessionID: ownerSessionID,
		TriggerEvent:   trigger,
		// Store the canonical string so the evaluator's re-parse round-trips to
		// the selector that was validated here.
		Selector:  sel.String(),
		Message:   msg.GetMessage(),
		State:     models.BroadcastSubscriptionStateActive,
		ExpiresAt: time.Now().UTC().Add(expiresIn),
	}
	if origin := strings.TrimSpace(msg.GetOriginChatId()); origin != "" {
		sub.OriginChatID = &origin
	}
	if err := store.Create(ctx, sub); err != nil {
		return nil, broadcastSubscriptionError("create broadcast subscription", err)
	}
	return connect.NewResponse(&pb.CreateBroadcastSubscriptionResponse{
		Subscription: broadcastSubscriptionToProto(sub),
	}), nil
}

// ListBroadcastSubscriptions returns subscriptions matching the optional
// request filters, in the store's deterministic created_at-then-id order. It
// never returns the registered message body (see
// broadcastSubscriptionToProto).
func (s *Server) ListBroadcastSubscriptions(ctx context.Context, req *connect.Request[pb.ListBroadcastSubscriptionsRequest]) (*connect.Response[pb.ListBroadcastSubscriptionsResponse], error) {
	store := s.BroadcastSubscriptions()
	if store == nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("broadcast subscription store not configured"))
	}
	msg := req.Msg

	filter := db.ListBroadcastSubscriptionsFilter{Limit: int(msg.GetLimit())}
	if msg.OwnerSessionId != nil {
		filter.OwnerSessionID = msg.OwnerSessionId
	}
	if msg.OriginChatId != nil {
		filter.OriginChatID = msg.OriginChatId
	}
	if msg.State != nil {
		// An unknown state is an argument error, not a silently empty result: a
		// caller filtering on a typo must be told, not shown "none match".
		state, err := models.ParseBroadcastSubscriptionState(*msg.State)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		filter.State = &state
	}
	if msg.TriggerEvent != nil {
		trigger, err := models.ParseBroadcastTrigger(*msg.TriggerEvent)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		filter.TriggerEvent = &trigger
	}

	subs, err := store.List(ctx, filter)
	if err != nil {
		return nil, broadcastSubscriptionError("list broadcast subscriptions", err)
	}
	out := make([]*pb.BroadcastSubscription, 0, len(subs))
	for _, sub := range subs {
		out = append(out, broadcastSubscriptionToProto(sub))
	}
	return connect.NewResponse(&pb.ListBroadcastSubscriptionsResponse{Subscriptions: out}), nil
}

// DeleteBroadcastSubscription retires a standing subscription by id.
//
// It cancels rather than hard-deletes: the store's Cancel is a compare-and-swap
// on the active state, which is what makes a cancel racing a fire lose cleanly
// (the broadcast already went out, and flipping the row would misreport
// history). The row therefore survives as history in the canceled state and
// still appears in an unfiltered listing — this is "stop this rule from
// firing", not "erase that it existed".
//
// Idempotent in the same two senses DeleteBroadcast is: repeating it on a row
// that is already canceled, fired or expired succeeds (the lost CAS is a benign
// no-op), and deleting an id that never existed succeeds too. A blank id is
// still an argument error.
func (s *Server) DeleteBroadcastSubscription(ctx context.Context, req *connect.Request[pb.DeleteBroadcastSubscriptionRequest]) (*connect.Response[pb.DeleteBroadcastSubscriptionResponse], error) {
	store := s.BroadcastSubscriptions()
	if store == nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("broadcast subscription store not configured"))
	}
	id := strings.TrimSpace(req.Msg.GetId())
	if id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}
	// The bool reports whether THIS caller won the CAS; a loser has nothing to
	// report but "someone else already retired it", which is success for a
	// cancel. sql.ErrNoRows is swallowed for the same reason DeleteBroadcast
	// accepts an absent id: a repeated cancel must be safe, and distinguishing
	// "already gone" from "never existed" would make a retry fail.
	if _, err := store.Cancel(ctx, id); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, broadcastSubscriptionError("delete broadcast subscription", err)
	}
	return connect.NewResponse(&pb.DeleteBroadcastSubscriptionResponse{}), nil
}
