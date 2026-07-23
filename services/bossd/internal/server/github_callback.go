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
	"github.com/recurser/bossd/internal/db"
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
		GroupID:      msg.GroupId,
		TargetChatID: msg.TargetChatId,
		RepoOwner:    msg.RepoOwner,
		RepoName:     msg.RepoName,
		PRNumber:     int(msg.PrNumber),
		Trigger:      models.GithubCallbackTrigger(msg.Trigger),
		Message:      msg.Message,
	}
	if msg.ExpiresAt != nil {
		t := msg.ExpiresAt.AsTime()
		params.ExpiresAt = &t
	}

	cb, err := store.Create(ctx, params)
	if err != nil {
		return nil, githubCallbackError("create github callback", err)
	}
	return connect.NewResponse(&pb.CreateGithubCallbackResponse{GithubCallback: githubCallbackToProto(cb)}), nil
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
	if _, err := store.ExpireOverdue(ctx, time.Now().UTC()); err != nil {
		return nil, githubCallbackError("expire overdue github callbacks", err)
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

// DeleteGithubCallback removes a callback by id. The store's delete is
// idempotent, so deleting an already-absent id succeeds — repeated cancel is
// safe.
func (s *Server) DeleteGithubCallback(ctx context.Context, req *connect.Request[pb.DeleteGithubCallbackRequest]) (*connect.Response[pb.DeleteGithubCallbackResponse], error) {
	store := s.GithubCallbacks()
	if store == nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("github callback store not configured"))
	}
	id := strings.TrimSpace(req.Msg.Id)
	if id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}
	if err := store.Delete(ctx, id); err != nil {
		return nil, githubCallbackError("delete github callback", err)
	}
	return connect.NewResponse(&pb.DeleteGithubCallbackResponse{}), nil
}
