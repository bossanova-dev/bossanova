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
	bcastsvc "github.com/recurser/bossd/internal/broadcast"
	"github.com/recurser/bossd/internal/db"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// broadcastStore is the subset of db.SQLiteBroadcastStore the three broadcast
// handlers use, declared here for the same reason the delivery worker declares
// its own `workerStore`: it keeps the Server's dependency narrow (matching every
// peer store field, which is an interface) and lets a test script a store
// failure — the post-Create cleanup path in particular — without standing up a
// migrated SQLite database.
//
// Callers must pass a non-nil implementation or leave the field unset: a nil
// CONCRETE pointer stored in this interface is not == nil, so it would defeat
// the handlers' "not configured" guard and panic instead.
type broadcastStore interface {
	Create(ctx context.Context, b *models.Broadcast) error
	CreateDeliveries(ctx context.Context, broadcastID string, targets []models.BroadcastDelivery) error
	CompleteIfSettled(ctx context.Context, broadcastID string) error
	Get(ctx context.Context, id string) (*models.Broadcast, error)
	List(ctx context.Context, filter db.ListBroadcastsFilter) ([]*models.Broadcast, error)
	Delete(ctx context.Context, id string) error
}

var _ broadcastStore = (*db.SQLiteBroadcastStore)(nil)

// broadcastToProto maps a persisted broadcast onto its wire form.
//
// SECRET-BODY RULE — this mapper is the single choke point for it. Every read
// surface that returns a Broadcast goes through here, and Message is
// DELIBERATELY left empty: the body is stored verbatim for the delivery worker
// alone and must never be echoed back on any read path, logged, or copied into
// an error or a delivery diagnostic. See the rule stated on
// pb.Broadcast.message and lib/bossalib/broadcast/proto.go; the precedent is
// the body-free githubCallbackJSON in services/boss/cmd/callback.go. Adding a
// Message assignment here would leak the secret from every handler at once,
// which is exactly why no handler builds a pb.Broadcast itself.
func broadcastToProto(b *models.Broadcast) *pb.Broadcast {
	if b == nil {
		return nil
	}
	msg := &pb.Broadcast{
		Id:        b.ID,
		State:     b.State.String(),
		CreatedAt: timestamppb.New(b.CreatedAt),
		ExpiresAt: timestamppb.New(b.ExpiresAt),
	}
	if b.OriginChatID != nil {
		msg.OriginChatId = *b.OriginChatID
	}
	// The stored selector is the canonical string a validated Selector produced,
	// so this re-parse is total in practice. A row that somehow fails to parse
	// still lists (with no selector) rather than failing the whole read — the
	// selector is metadata, not the reason the caller asked.
	if sel, err := bcast.Parse(b.Selector); err == nil {
		msg.Selector = bcast.SelectorToProto(sel)
	}
	return msg
}

// broadcastDeliveryToProto maps a delivery row onto its wire form. LastError is
// a diagnostic and never carries the message body (see the store's secret-body
// rule); it is passed through as-is.
func broadcastDeliveryToProto(d *models.BroadcastDelivery) *pb.BroadcastDelivery {
	if d == nil {
		return nil
	}
	msg := &pb.BroadcastDelivery{
		BroadcastId:  d.BroadcastID,
		TargetChatId: d.TargetChatID,
		State:        d.State.String(),
		AttemptCount: clampInt32(d.AttemptCount),
	}
	if d.LastError != nil {
		msg.LastError = *d.LastError
	}
	if d.DeliveredAt != nil {
		msg.DeliveredAt = timestamppb.New(*d.DeliveredAt)
	}
	return msg
}

// broadcastError maps a BroadcastStore error to a connect error code, mirroring
// githubCallbackError. Absent rows become NotFound, the one-shot resolve guard
// and lease conflicts Aborted, everything else Internal. Store errors carry
// field-level diagnostics only — never the message body — so wrapping them is
// safe.
func broadcastError(op string, err error) *connect.Error {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, db.ErrBroadcastAlreadyResolved),
		errors.Is(err, db.ErrBroadcastDeliveryLeaseConflict):
		return connect.NewError(connect.CodeAborted, err)
	default:
		return connect.NewError(connect.CodeInternal, fmt.Errorf("%s: %w", op, err))
	}
}

// SendBroadcast resolves a selector to an audience and records one delivery per
// target; the delivery worker then sends them asynchronously with retry.
//
// Resolution happens ONCE, here, and the audience is frozen into the delivery
// rows: a chat created after the send is never retroactively included. Over the
// fan-out cap is a hard refusal that names the count and the cap (never a
// truncation), while zero targets is a SUCCESS with no deliveries.
//
// The response never carries the message body — see broadcastToProto.
func (s *Server) SendBroadcast(ctx context.Context, req *connect.Request[pb.SendBroadcastRequest]) (*connect.Response[pb.SendBroadcastResponse], error) {
	store := s.Broadcasts()
	if store == nil || s.broadcastResolver == nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("broadcast store not configured"))
	}
	msg := req.Msg

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
	// Expiry uses the callback convention verbatim (empty = 24h, max 30 days,
	// "30m"/"24h"/"7d"/"2w" units) by sharing its parser, so the two registration
	// surfaces cannot drift apart.
	expiresIn, err := githubcallback.ParseDuration(msg.GetExpiresIn())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	// The composition below — resolve, create, freeze the audience, clean up a
	// failed materialisation, settle an empty one — lives in broadcast.Sender
	// rather than here because it has a SECOND caller: the standing-subscription
	// evaluator fires through the same type. Duplicating it would let the RPC
	// send path and the subscription send path drift.
	originChatID := strings.TrimSpace(msg.GetOriginChatId())
	expiresAt := time.Now().UTC().Add(expiresIn)
	result, err := bcastsvc.NewSender(store, s.broadcastResolver, s.logger).
		Send(ctx, bcastsvc.SendRequest{
			Selector:      sel,
			Message:       msg.GetMessage(),
			OriginChatID:  originChatID,
			IncludeOrigin: msg.GetIncludeOrigin(),
			ExpiresAt:     expiresAt,
		})
	if err != nil {
		if errors.Is(err, bcastsvc.ErrTooManyTargets) {
			// The error names the resolved count and the cap; the audience is
			// refused outright rather than truncated to the cap.
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		return nil, broadcastError("send broadcast", err)
	}

	s.publishBroadcastEgress(ctx, sel, result, msg.GetCrossDaemon(), originChatID, expiresAt)

	// Re-read so the response reports the authoritative persisted state
	// (resolved, or completed for an empty audience) rather than the pending
	// value Create left on the in-memory struct. A failed re-read falls back to
	// the state just written: cosmetically staler, but honest about the send.
	stored, err := store.Get(ctx, result.Broadcast.ID)
	if err != nil {
		s.logger.Warn().Err(err).Str("broadcast_id", result.Broadcast.ID).
			Msg("send broadcast: could not re-read the sent broadcast; reporting the state just written")
		stored = result.Broadcast
	}

	// The delivery rows come from the resolved targets rather than a read-back:
	// ListDeliveries orders by created_at/id, which does not reproduce the
	// frozen audience order the response documents (every row in a batch shares
	// one created_at).
	out := make([]*pb.BroadcastDelivery, 0, len(result.Deliveries))
	for i := range result.Deliveries {
		out = append(out, broadcastDeliveryToProto(&result.Deliveries[i]))
	}
	return connect.NewResponse(&pb.SendBroadcastResponse{
		Broadcast:  broadcastToProto(stored),
		Deliveries: out,
	}), nil
}

// publishBroadcastEgress hands a broadcast whose audience reaches beyond this
// daemon to the upstream stream, so bosso can route it to the daemons that can
// resolve targets for it (BOS-558).
//
// It runs AFTER the send has been persisted, and deliberately so: the decision
// of WHETHER to publish belongs to broadcast.ReachesBeyondDaemon (a pure
// predicate with its own tests), and the decision of WHEN is "once the local
// deliveries are committed" — the egress carries the persisted broadcast id, so
// there has to be one.
//
// A publish failure is logged and IGNORED. Past CreateDeliveries the send has
// already happened: the audience is materialised and the worker will deliver it
// whatever happens here. Failing the RPC would report a send that did occur as
// having failed, inviting a retry that resolves a SECOND audience and delivers
// the message twice under a different broadcast id — which also defeats the
// at-least-once dedupe hint carried in the prompt. A missed cross-daemon hop is
// the smaller loss, and it is visible in the log. This mirrors the same
// "everything after the commit is degraded, never returned" rule the Sender
// applies to its own settle step.
//
// SECRET-BODY RULE: neither log line may carry the message. Only the broadcast
// id, the origin daemon id and the resolved local target count are logged — and
// the publish error is deliberately NOT attached with Err(), because an
// implementation that formatted the event into its error would spill the body
// into the daemon log through it.
func (s *Server) publishBroadcastEgress(
	ctx context.Context,
	sel bcast.Selector,
	result *bcastsvc.SendResult,
	crossDaemon bool,
	originChatID string,
	expiresAt time.Time,
) {
	if s.broadcastEgress == nil || !bcastsvc.ReachesBeyondDaemon(sel, s.daemonID, crossDaemon) {
		return
	}
	event := bcastsvc.EgressEvent{
		BroadcastID:    result.Broadcast.ID,
		Selector:       sel,
		OriginDaemonID: s.daemonID,
		OriginChatID:   originChatID,
		Message:        result.Broadcast.Message,
		ExpiresAt:      expiresAt,
	}
	if err := s.broadcastEgress.PublishBroadcastEgress(ctx, event); err != nil {
		s.logger.Warn().
			Str("broadcast_id", event.BroadcastID).
			Str("origin_daemon_id", event.OriginDaemonID).
			Int("local_target_count", len(result.Deliveries)).
			Msg("send broadcast: could not publish the cross-daemon egress event; the local deliveries stand")
		return
	}
	// "handed to", NOT "published": a nil error from the publisher means the
	// event reached the reverse-stream bus, which discards it silently when no
	// stream is currently subscribed (see PublishBroadcastEgress). Claiming
	// delivery here would tell an operator debugging a missing cross-daemon
	// broadcast that the daemon end succeeded, when the truthful statement is
	// only that it handed the event over.
	s.logger.Debug().
		Str("broadcast_id", event.BroadcastID).
		Str("origin_daemon_id", event.OriginDaemonID).
		Int("local_target_count", len(result.Deliveries)).
		Msg("send broadcast: handed the cross-daemon egress event to the reverse stream")
}

// ListBroadcasts returns broadcasts matching the optional request filters, in
// the store's deterministic created_at-then-id order. It never returns the
// message body (see broadcastToProto).
func (s *Server) ListBroadcasts(ctx context.Context, req *connect.Request[pb.ListBroadcastsRequest]) (*connect.Response[pb.ListBroadcastsResponse], error) {
	store := s.Broadcasts()
	if store == nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("broadcast store not configured"))
	}
	msg := req.Msg

	filter := db.ListBroadcastsFilter{Limit: int(msg.GetLimit())}
	if msg.State != nil {
		// An unknown state is an argument error, not a silently empty result: a
		// caller filtering on a typo must be told, not shown "none match".
		state, err := models.ParseBroadcastState(*msg.State)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		filter.State = &state
	}
	if msg.OriginChatId != nil {
		filter.OriginChatID = msg.OriginChatId
	}
	if msg.TargetChatId != nil {
		filter.TargetChatID = msg.TargetChatId
	}

	broadcasts, err := store.List(ctx, filter)
	if err != nil {
		return nil, broadcastError("list broadcasts", err)
	}
	out := make([]*pb.Broadcast, 0, len(broadcasts))
	for _, b := range broadcasts {
		out = append(out, broadcastToProto(b))
	}
	return connect.NewResponse(&pb.ListBroadcastsResponse{Broadcasts: out}), nil
}

// DeleteBroadcast removes a broadcast and its deliveries (via ON DELETE
// CASCADE) by id.
//
// Idempotent: deleting an absent id succeeds, exactly as the RPC's proto
// contract documents and as DeleteGithubCallback behaves, so a repeated cancel
// is safe. (The store's Delete cannot distinguish "absent" from "removed", so
// reporting NotFound would require a racy pre-read that a concurrent delete
// could still defeat.)
//
// LIMITATION — delete is not cancellation. It does not respect leases (see
// SQLiteBroadcastStore.Delete): a delivery a worker has ALREADY leased is
// cascaded away underneath its owner, which is still holding the rendered prompt
// and goes on to send it; only the owner's MarkDelivered then fails. So deleting
// a broadcast mid-flight can still deliver to the target the worker had already
// claimed — a window bounded by one attempt deadline. Closing it properly means
// a state transition to the `canceled` state the model already defines, which
// needs a store primitive that does not exist yet; a hard delete is what this
// surface has. Callers should treat DeleteBroadcast as "stop scheduling this",
// not "recall it".
func (s *Server) DeleteBroadcast(ctx context.Context, req *connect.Request[pb.DeleteBroadcastRequest]) (*connect.Response[pb.DeleteBroadcastResponse], error) {
	store := s.Broadcasts()
	if store == nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("broadcast store not configured"))
	}
	id := strings.TrimSpace(req.Msg.GetId())
	if id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}
	if err := store.Delete(ctx, id); err != nil {
		return nil, broadcastError("delete broadcast", err)
	}
	return connect.NewResponse(&pb.DeleteBroadcastResponse{}), nil
}
