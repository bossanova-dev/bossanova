package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	libtelemetry "github.com/recurser/bossalib/telemetry"
	"github.com/recurser/bossd/internal/db"
	daemontelemetry "github.com/recurser/bossd/internal/telemetry"
)

// githubCallbackError maps a GithubCallbackStore error to a connect error code.
// Validation failures become InvalidArgument, absent rows NotFound, and the
// (defensively handled) lease/trigger conflicts Aborted; everything else is
// Internal. The wrapped error text never includes the callback message body —
// the store's errors carry only field-level diagnostics.
func githubCallbackError(op string, err error) *connect.Error {
	switch {
	case errors.Is(err, db.ErrGithubCallbackInvalid):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, db.ErrGithubCallbackNotOwned):
		return connect.NewError(connect.CodePermissionDenied, err)
	case errors.Is(err, sql.ErrNoRows):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, db.ErrGithubCallbackLeaseConflict),
		errors.Is(err, db.ErrGithubCallbackTriggerConflict):
		return connect.NewError(connect.CodeAborted, err)
	default:
		return connect.NewError(connect.CodeInternal, fmt.Errorf("%s: %w", op, err))
	}
}

// CreateGithubCallback registers a durable one-shot GitHub callback. Defaults
// (24h expiry, active state, lowercased repo owner/name) and validation live in
// the store; the handler only translates the request and maps errors. The
// registered message body is passed through verbatim and never logged.
func (s *Server) CreateGithubCallback(ctx context.Context, req *connect.Request[pb.CreateGithubCallbackRequest]) (*connect.Response[pb.CreateGithubCallbackResponse], error) {
	store := s.GithubCallbacks()
	if store == nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("github callback store not configured"))
	}
	msg := req.Msg

	params := db.CreateGithubCallbackParams{
		GroupID:                 msg.GroupId,
		TargetChatID:            msg.TargetChatId,
		RepoOwner:               msg.RepoOwner,
		RepoName:                msg.RepoName,
		PRNumber:                int(msg.PrNumber),
		Trigger:                 models.GithubCallbackTrigger(msg.Trigger),
		Message:                 msg.Message,
		ShouldRequireTransition: msg.GetShouldRequireTransition(),
	}
	if msg.ExpiresAt != nil {
		t := msg.ExpiresAt.AsTime()
		params.ExpiresAt = &t
	}

	params.IndependentWatch = msg.GetIsIndependentWatch()

	cb, conflict, err := store.Create(ctx, params)
	if err != nil {
		return nil, githubCallbackError("create github callback", err)
	}
	notice := githubCallbackConflictNotice(cb, conflict)
	// Guard the dereferences on the pointer itself, not on the rendered notice
	// being non-empty — that only happens to imply conflict != nil today.
	if conflict != nil {
		s.logger.Warn().
			Str("callback_id", cb.ID).
			Str("group_id", derefOrEmpty(cb.GroupID)).
			Str("trigger", string(cb.Trigger)).
			Str("conflicting_callback_id", conflict.ID).
			Str("conflicting_group_id", derefOrEmpty(conflict.GroupID)).
			Str("conflicting_trigger", string(conflict.Trigger)).
			Msg("create github callback: mutually exclusive trigger live in another group")
	}
	s.recomputeCallbackTarget(ctx, msg.TargetChatId)
	return connect.NewResponse(&pb.CreateGithubCallbackResponse{
		GithubCallback: githubCallbackToProto(cb),
		NoticeText:     notice,
	}), nil
}

// ungroupedCallbackGroupLabel is what the notice prints where a group id would
// go for an ungrouped callback. An ungrouped row is not "no group" for the
// purpose of sibling cancellation — it is a group of one, which is exactly why
// it cannot cancel anything — so the label says so rather than printing an
// empty string the reader has to interpret.
const ungroupedCallbackGroupLabel = "<ungrouped>"

// githubCallbackConflictNotice renders the advisory for a callback that was
// just armed against a live, mutually exclusive sibling in another group.
// Returns "" when there is no conflict, so the caller can test one value for
// both "say nothing" and "say this".
//
// It names the conflicting callback's id, group, trigger and expiry — the four
// things an operator needs to find and remove it — and states plainly that both
// legs stay armed, because the whole defect is that this shape looks like a
// cancelling pair and is not one.
func githubCallbackConflictNotice(cb *models.GithubCallback, conflict *db.GithubCallbackConflict) string {
	if conflict == nil || cb == nil {
		return ""
	}
	return fmt.Sprintf(
		"warning: %s in group %q cannot be satisfied at the same time as callback %s (group %q, trigger %s, expires %s), "+
			"which is still armed for this chat and PR. They are in different groups, so neither will cancel the other: "+
			"both stay armed until they fire or expire. Put them in one --group to make them cancel each other, "+
			"or pass --independent-watch if this watch is meant to outlive its sibling.",
		cb.Trigger,
		groupLabelOrUngrouped(cb.GroupID),
		conflict.ID,
		groupLabelOrUngrouped(conflict.GroupID),
		conflict.Trigger,
		conflict.ExpiresAt.UTC().Format(time.RFC3339),
	)
}

func groupLabelOrUngrouped(group *string) string {
	if group == nil || *group == "" {
		return ungroupedCallbackGroupLabel
	}
	return *group
}

func derefOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// recomputeCallbackTarget publishes a newly armed callback's waiting state
// without waiting for the next chat heartbeat or periodic recovery sweep.
// Registration remains durable even when this best-effort projection fails.
func (s *Server) recomputeCallbackTarget(ctx context.Context, agentSessionID string) {
	if s.statusRecomputer == nil || s.agentChats == nil || agentSessionID == "" {
		return
	}
	chat, err := s.agentChats.GetByAgentSessionID(ctx, agentSessionID)
	if err != nil {
		s.logger.Warn().Err(err).Str("agent_session_id", agentSessionID).Msg("create github callback: look up target chat for status refresh")
		return
	}
	if chat == nil {
		return
	}
	if err := s.statusRecomputer.Recompute(ctx, chat.SessionID); err != nil {
		s.logger.Warn().Err(err).Str("session_id", chat.SessionID).Msg("create github callback: refresh target chat status")
	}
}

// ListGithubCallbacks returns callbacks matching the optional request filters,
// ordered by the store's deterministic created_at-then-id ordering.
func (s *Server) ListGithubCallbacks(ctx context.Context, req *connect.Request[pb.ListGithubCallbacksRequest]) (*connect.Response[pb.ListGithubCallbacksResponse], error) {
	store := s.GithubCallbacks()
	if store == nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("github callback store not configured"))
	}
	msg := req.Msg

	// Sweep overdue callbacks to the expired state before reading. Expiry is not
	// applied anywhere else on the read path, so a callback past its expires_at
	// would otherwise still surface as active (and match a state=active filter).
	// Doing it here keeps the listed state honest without a background scheduler.
	if _, expired, err := store.ExpireOverdueCallbacks(ctx, time.Now().UTC()); err != nil {
		return nil, githubCallbackError("expire overdue github callbacks", err)
	} else {
		for _, callback := range expired {
			daemontelemetry.Capture(ctx, s.telemetry, libtelemetry.EventPRCallbackDelivered, map[string]any{
				"trigger": string(callback.Trigger), "status": "abandoned", "attempt_count": callback.AttemptCount,
			})
		}
	}

	filter := db.ListGithubCallbacksFilter{
		TargetChatID: msg.TargetChatId,
		RepoOwner:    msg.RepoOwner,
		RepoName:     msg.RepoName,
	}
	if msg.PrNumber != nil {
		n := int(*msg.PrNumber)
		filter.PRNumber = &n
	}
	if msg.Trigger != nil {
		t := models.GithubCallbackTrigger(*msg.Trigger)
		filter.Trigger = &t
	}
	if msg.State != nil {
		st := models.GithubCallbackState(*msg.State)
		filter.State = &st
	}

	cbs, err := store.List(ctx, filter)
	if err != nil {
		return nil, githubCallbackError("list github callbacks", err)
	}
	out := make([]*pb.GithubCallback, 0, len(cbs))
	for _, cb := range cbs {
		out = append(out, githubCallbackToProto(cb))
	}
	return connect.NewResponse(&pb.ListGithubCallbacksResponse{GithubCallbacks: out}), nil
}

// DeleteGithubCallback removes a callback by id. When expect_target_chat_id is
// set, the store refuses to delete a row owned by a different target chat.
func (s *Server) DeleteGithubCallback(ctx context.Context, req *connect.Request[pb.DeleteGithubCallbackRequest]) (*connect.Response[pb.DeleteGithubCallbackResponse], error) {
	store := s.GithubCallbacks()
	if store == nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("github callback store not configured"))
	}
	id := strings.TrimSpace(req.Msg.Id)
	if id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}
	outcome, err := store.Delete(ctx, id, req.Msg.GetExpectTargetChatId())
	if err != nil {
		return nil, githubCallbackError("delete github callback", err)
	}
	if outcome == db.DeleteGithubCallbackOutcomeNotFound {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("github callback %s not found", id))
	}
	return connect.NewResponse(&pb.DeleteGithubCallbackResponse{Outcome: string(outcome)}), nil
}
