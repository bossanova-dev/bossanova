package upstream

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// SessionServeCommands answers StartSessionServeCommand and StopSessionServeCommand.
// Kept as an optional capability on the command handler (matching ChatAgentCommands)
// so handlers that predate it keep compiling.
type SessionServeCommands interface {
	StartSessionServe(ctx context.Context, req *connect.Request[pb.StartSessionServeRequest]) (*connect.Response[pb.StartSessionServeResponse], error)
	StopSessionServe(ctx context.Context, req *connect.Request[pb.StopSessionServeRequest]) (*connect.Response[pb.StopSessionServeResponse], error)
}

// StartSessionServe starts a session's serve command via the daemon server.
func (a *CommandHandlerAdapter) StartSessionServe(ctx context.Context, sessionID string) (*pb.SessionServe, error) {
	srv, ok := a.Commands.(SessionServeCommands)
	if !ok {
		return nil, errors.New("start_session_serve: serve commands not wired")
	}
	resp, err := srv.StartSessionServe(ctx, connect.NewRequest(&pb.StartSessionServeRequest{SessionId: sessionID}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetServe(), nil
}

// StopSessionServe stops a session's serve command via the daemon server.
func (a *CommandHandlerAdapter) StopSessionServe(ctx context.Context, sessionID string) (*pb.SessionServe, error) {
	srv, ok := a.Commands.(SessionServeCommands)
	if !ok {
		return nil, errors.New("stop_session_serve: serve commands not wired")
	}
	resp, err := srv.StopSessionServe(ctx, connect.NewRequest(&pb.StopSessionServeRequest{SessionId: sessionID}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetServe(), nil
}

type sessionServeHandler interface {
	StartSessionServe(ctx context.Context, sessionID string) (*pb.SessionServe, error)
	StopSessionServe(ctx context.Context, sessionID string) (*pb.SessionServe, error)
}

func (c *StreamClient) dispatchSessionServe(ctx context.Context, cmdID string, cmd *pb.OrchestratorCommand, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	handler, ok := c.commandHandler.(sessionServeHandler)
	if !ok {
		return commandErr(cmdID, "session serve commands are not wired")
	}
	if start := cmd.GetStartSessionServe(); start != nil {
		return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
			serve, err := handler.StartSessionServe(ctx, start.GetSessionId())
			if err != nil {
				return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
			}
			return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: &pb.CommandResult{
				CommandId: cmdID,
				Ok:        true,
				Payload:   &pb.CommandResult_SessionServe{SessionServe: serve},
			}}}
		})
	}
	stop := cmd.GetStopSessionServe()
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		serve, err := handler.StopSessionServe(ctx, stop.GetSessionId())
		if err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: &pb.CommandResult{
			CommandId: cmdID,
			Ok:        true,
			Payload:   &pb.CommandResult_SessionServe{SessionServe: serve},
		}}}
	})
}
