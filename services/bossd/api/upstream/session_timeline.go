package upstream

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type sessionTimelineCommands interface {
	GetSessionTimeline(context.Context, string) (*pb.GetSessionTimelineResponse, error)
}

type sessionTimelineServer interface {
	GetSessionTimeline(context.Context, *connect.Request[pb.GetSessionTimelineRequest]) (*connect.Response[pb.GetSessionTimelineResponse], error)
}

// GetSessionTimeline delegates to the daemon's dated session timeline handler.
func (a *CommandHandlerAdapter) GetSessionTimeline(ctx context.Context, sessionID string) (*pb.GetSessionTimelineResponse, error) {
	if sessionID == "" {
		return nil, errors.New("get_session_timeline: session_id required")
	}
	if a.Commands == nil {
		return nil, errors.New("get_session_timeline: command server not wired")
	}
	server, ok := a.Commands.(sessionTimelineServer)
	if !ok {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("session timeline not supported"))
	}
	resp, err := server.GetSessionTimeline(ctx, connect.NewRequest(&pb.GetSessionTimelineRequest{SessionId: sessionID}))
	if err != nil {
		return nil, fmt.Errorf("get session timeline: %w", err)
	}
	return resp.Msg, nil
}

func (c *StreamClient) dispatchGetSessionTimeline(ctx context.Context, cmdID string, req *pb.GetSessionTimelineCommand, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	return c.dispatchReadCommand(ctx, cmdID, outbound, func() (*pb.CommandResult, error) {
		handler, ok := c.commandHandler.(sessionTimelineCommands)
		if !ok {
			return nil, connect.NewError(connect.CodeUnimplemented, errors.New("session timeline not supported"))
		}
		out, err := handler.GetSessionTimeline(ctx, req.GetSessionId())
		return &pb.CommandResult{Payload: &pb.CommandResult_GetSessionTimeline{GetSessionTimeline: out}}, err
	}, classifyTimelineCommandError)
}

func classifyTimelineCommandError(err error) pb.CommandResult_ErrorCode {
	if connect.CodeOf(err) == connect.CodeUnimplemented {
		return pb.CommandResult_ERROR_CODE_UNIMPLEMENTED
	}
	return classifyCommandError(err)
}
