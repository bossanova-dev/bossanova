package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/session"
)

// ResumeSessionRun resumes the agent in place; ResumeSession controls automation.
func (s *Server) ResumeSessionRun(ctx context.Context, req *connect.Request[pb.ResumeSessionRunRequest]) (*connect.Response[pb.ResumeSessionRunResponse], error) {
	id := strings.TrimSpace(req.Msg.GetSessionId())
	if id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("session_id is required"))
	}
	sess, err := s.lifecycle.ResumeSessionRun(ctx, id)
	if err != nil {
		code := connect.CodeInternal
		if errors.Is(err, sql.ErrNoRows) {
			code = connect.CodeNotFound
		} else if errors.Is(err, session.ErrRunNotResumable) {
			code = connect.CodeFailedPrecondition
		}
		return nil, connect.NewError(code, fmt.Errorf("resume session run: %w", err))
	}
	return connect.NewResponse(&pb.ResumeSessionRunResponse{Session: s.sessionProtoWithRepo(ctx, sess)}), nil
}
