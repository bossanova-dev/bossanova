package server

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/session"
)

// ResumeSessionRun resumes the agent in place; ResumeSession controls automation.
//
// No dated API bump for refusing a revival onto a PR or branch another live
// session holds: the RPC is local-only (the remote client refuses it) and the
// CLI is co-deployed, and the refusal writes nothing.
// See docs/api-versioning.md#skipping-a-bump-justify-it-in-the-diff.
func (s *Server) ResumeSessionRun(ctx context.Context, req *connect.Request[pb.ResumeSessionRunRequest]) (*connect.Response[pb.ResumeSessionRunResponse], error) {
	id := strings.TrimSpace(req.Msg.GetSessionId())
	if id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("session_id is required"))
	}
	sess, err := s.lifecycle.ResumeSessionRun(ctx, id)
	if err != nil {
		code := connect.CodeInternal
		if errors.Is(err, domain.ErrNotFound) {
			code = connect.CodeNotFound
		} else if errors.Is(err, session.ErrRunNotResumable) || session.IsDuplicateActiveSessionError(err) {
			code = connect.CodeFailedPrecondition
		} else if errors.Is(err, context.Canceled) {
			// A lock wait times out with DeadlineExceeded, never Canceled, so
			// Canceled is the caller giving up.
			code = connect.CodeCanceled
		} else if errors.Is(err, session.ErrStartPathBusy) || errors.Is(err, session.ErrRunResumeBusy) {
			code = connect.CodeUnavailable
		}
		return nil, connect.NewError(code, fmt.Errorf("resume session run: %w", err))
	}
	return connect.NewResponse(&pb.ResumeSessionRunResponse{Session: s.sessionProtoWithRepo(ctx, sess)}), nil
}
