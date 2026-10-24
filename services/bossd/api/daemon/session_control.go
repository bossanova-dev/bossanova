package server

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossd/internal/domain"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (s *Server) AttachSession(ctx context.Context, req *connect.Request[pb.AttachSessionRequest], stream *connect.ServerStream[pb.AttachSessionResponse]) error {
	if req.Msg.Id == "" {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}

	// Verify the session exists.
	sess, err := s.sessions.Get(ctx, req.Msg.Id)
	if err != nil {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("session not found: %w", err))
	}

	// Send initial state to let the client know the session is valid.
	if err := stream.Send(&pb.AttachSessionResponse{
		Event: &pb.AttachSessionResponse_StateChange{
			StateChange: &pb.StateChange{
				PreviousState: pb.SessionState(clampInt32(int(sess.State))),
				NewState:      pb.SessionState(clampInt32(int(sess.State))),
			},
		},
	}); err != nil {
		return err
	}

	// If no Claude process is running, send ended and return.
	if sess.AgentSessionID == nil || !s.agent.IsRunning(*sess.AgentSessionID) {
		return stream.Send(&pb.AttachSessionResponse{
			Event: &pb.AttachSessionResponse_SessionEnded{
				SessionEnded: &pb.SessionEnded{
					FinalState: pb.SessionState(clampInt32(int(sess.State))),
				},
			},
		})
	}

	claudeSessionID := *sess.AgentSessionID

	// Send existing ring buffer contents as initial burst.
	history := s.agent.History(claudeSessionID)
	for _, line := range history {
		if err := stream.Send(&pb.AttachSessionResponse{
			Event: &pb.AttachSessionResponse_OutputLine{
				OutputLine: &pb.OutputLine{
					Text:      line.Text,
					Timestamp: timestamppb.New(line.Timestamp),
				},
			},
		}); err != nil {
			return err
		}
	}

	// Subscribe to new output lines. Use a subscription-scoped context so the
	// subscriber is always deregistered from the broadcaster when this handler
	// returns — including on stream.Send errors (client disconnect), panics, or
	// normal process exit. Without this, a cleanup goroutine inside the
	// broadcaster relies on the handler's ctx being cancelled by the transport
	// layer, which is a leak risk under unusual client-disconnect semantics.
	subCtx, cancelSub := context.WithCancel(ctx)
	defer cancelSub()

	ch, err := s.agent.Subscribe(subCtx, claudeSessionID)
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("subscribe: %w", err))
	}

	// Stream new lines until process exits or client disconnects. On Send
	// error, return immediately; the deferred cancelSub triggers subscriber
	// removal so the broadcaster stops referencing this channel.
	for line := range ch {
		if err := stream.Send(&pb.AttachSessionResponse{
			Event: &pb.AttachSessionResponse_OutputLine{
				OutputLine: &pb.OutputLine{
					Text:      line.Text,
					Timestamp: timestamppb.New(line.Timestamp),
				},
			},
		}); err != nil {
			return err
		}
	}

	// Process exited — send session ended.
	// Re-fetch session to get latest state.
	sess, err = s.sessions.Get(ctx, req.Msg.Id)
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("get session: %w", err))
	}

	return stream.Send(&pb.AttachSessionResponse{
		Event: &pb.AttachSessionResponse_SessionEnded{
			SessionEnded: &pb.SessionEnded{
				FinalState: pb.SessionState(clampInt32(int(sess.State))),
			},
		},
	})
}

func (s *Server) StopSession(ctx context.Context, req *connect.Request[pb.StopSessionRequest]) (*connect.Response[pb.StopSessionResponse], error) {
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}

	if err := s.lifecycle.StopSession(ctx, req.Msg.Id); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("stop session: %w", err))
	}

	// Notify the task orchestrator so it can unblock the repo's task queue.
	if s.completionNotifier != nil {
		s.completionNotifier.HandleSessionCompleted(ctx, req.Msg.Id, domain.TaskMappingStatusFailed)
	}

	sess, err := s.sessions.Get(ctx, req.Msg.Id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get session: %w", err))
	}

	return connect.NewResponse(&pb.StopSessionResponse{Session: SessionToProto(sess)}), nil
}

// setSessionAutomation validates the id, toggles the session's automation flag,
// and returns the refreshed session proto. The action label is woven into the
// update error so callers surface a context-specific message (e.g. "pause
// session: ...", "resume session: ..."). State machine integration in Leg 6.
func (s *Server) setSessionAutomation(ctx context.Context, id string, enabled bool, action string) (*pb.Session, error) {
	if id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}

	if _, err := s.sessions.Update(ctx, id, domain.UpdateSessionParams{
		IsAutomationEnabled: &enabled,
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("%s session: %w", action, err))
	}

	session, err := s.sessions.Get(ctx, id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get session: %w", err))
	}

	return SessionToProto(session), nil
}

func (s *Server) PauseSession(ctx context.Context, req *connect.Request[pb.PauseSessionRequest]) (*connect.Response[pb.PauseSessionResponse], error) {
	session, err := s.setSessionAutomation(ctx, req.Msg.Id, false, "pause")
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&pb.PauseSessionResponse{Session: session}), nil
}

func (s *Server) ResumeSession(ctx context.Context, req *connect.Request[pb.ResumeSessionRequest]) (*connect.Response[pb.ResumeSessionResponse], error) {
	session, err := s.setSessionAutomation(ctx, req.Msg.Id, true, "resume")
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&pb.ResumeSessionResponse{Session: session}), nil
}

func (s *Server) RetrySession(ctx context.Context, req *connect.Request[pb.RetrySessionRequest]) (*connect.Response[pb.RetrySessionResponse], error) {
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}

	// Retry clears blocked reason, re-enables automation and gives repair a
	// fresh failure budget. Full state machine in Leg 6.
	var nilStr *string
	blockedReason := &nilStr // double pointer: set to NULL
	automationEnabled := true
	freshRepairBudget := 0
	if _, err := s.sessions.Update(ctx, req.Msg.Id, domain.UpdateSessionParams{
		BlockedReason:          blockedReason,
		IsAutomationEnabled:    &automationEnabled,
		LastRepairAttemptCount: &freshRepairBudget,
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("retry session: %w", err))
	}

	session, err := s.sessions.Get(ctx, req.Msg.Id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get session: %w", err))
	}

	return connect.NewResponse(&pb.RetrySessionResponse{Session: SessionToProto(session)}), nil
}

func (s *Server) CloseSession(ctx context.Context, req *connect.Request[pb.CloseSessionRequest]) (*connect.Response[pb.CloseSessionResponse], error) {
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}

	closedState := int(machine.Closed)
	if _, err := s.sessions.Update(ctx, req.Msg.Id, domain.UpdateSessionParams{
		State: &closedState,
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("close session: %w", err))
	}

	// Notify the task orchestrator so it can unblock the repo's task queue.
	if s.completionNotifier != nil {
		s.completionNotifier.HandleSessionCompleted(ctx, req.Msg.Id, domain.TaskMappingStatusFailed)
	}

	session, err := s.sessions.Get(ctx, req.Msg.Id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get session: %w", err))
	}

	return connect.NewResponse(&pb.CloseSessionResponse{Session: SessionToProto(session)}), nil
}
