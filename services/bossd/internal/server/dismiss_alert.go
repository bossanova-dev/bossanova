package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/domain"
)

// DismissSessionAlert records the operator's dismissal of one session alert
// (BOS-1537). It writes the key onto the session — one key per session, the
// latest replacing any earlier one — then recomputes the display status and
// republishes the session. Nothing hides the alert yet: the persisted key is
// read by attention.Compute (Input.DismissedAlertKey), and no bossd producer
// calls attention.Compute until the follow-up that surfaces fix rows, so the
// republished row is otherwise unchanged. The session's event history is not
// touched.
func (s *Server) DismissSessionAlert(ctx context.Context, req *connect.Request[pb.DismissSessionAlertRequest]) (*connect.Response[pb.DismissSessionAlertResponse], error) {
	id := strings.TrimSpace(req.Msg.GetSessionId())
	if id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("session_id is required"))
	}
	key := strings.TrimSpace(req.Msg.GetAlertKey())
	if key == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("alert_key is required"))
	}

	if err := s.sessions.SetDismissedAlert(ctx, id, key, time.Now().UTC()); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session %s not found", id))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("dismiss session alert: %w", err))
	}

	// The dismissal is committed; a failed recompute must not fail the RPC,
	// the next heartbeat or sweep recomputes the row anyway.
	if s.statusRecomputer != nil {
		if err := s.statusRecomputer.Recompute(ctx, id); err != nil {
			s.logger.Warn().Err(err).Str("session", id).Msg("dismiss session alert: recompute display status")
		}
	}

	sess, err := s.sessions.Get(ctx, id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("reload session: %w", err))
	}
	p := s.sessionProtoWithRepo(ctx, sess)
	if s.onSessionUpdated != nil {
		s.onSessionUpdated(ctx, p)
	}
	return connect.NewResponse(&pb.DismissSessionAlertResponse{Session: p}), nil
}
