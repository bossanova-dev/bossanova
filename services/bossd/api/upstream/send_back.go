package upstream

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type operatorReviewCommands interface {
	OperatorMergeSession(context.Context, *pb.OperatorMergeSessionRequest) (*pb.OperatorMergeSessionResponse, error)
	SendBackToRepair(context.Context, *pb.SendBackToRepairRequest) (*pb.SendBackToRepairResponse, error)
}

type operatorReviewServer interface {
	OperatorMergeSession(context.Context, *connect.Request[pb.OperatorMergeSessionRequest]) (*connect.Response[pb.OperatorMergeSessionResponse], error)
	SendBackToRepair(context.Context, *connect.Request[pb.SendBackToRepairRequest]) (*connect.Response[pb.SendBackToRepairResponse], error)
}

// SendBackToRepair runs the daemon's pinned send-back with its local credentials.
func (a *CommandHandlerAdapter) SendBackToRepair(ctx context.Context, req *pb.SendBackToRepairRequest) (*pb.SendBackToRepairResponse, error) {
	if req.GetId() == "" {
		return nil, errors.New("send_back_to_repair: session_id required")
	}
	if a.Commands == nil {
		return nil, errors.New("send_back_to_repair: command server not wired")
	}
	resp, err := a.Commands.SendBackToRepair(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, fmt.Errorf("send back to repair: %w", err)
	}
	return resp.Msg, nil
}

func (c *StreamClient) dispatchSendBackToRepair(ctx context.Context, cmdID string, req *pb.SendBackToRepairCommand, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	return c.dispatchOperatorReview(ctx, cmdID, outbound, func(ctx context.Context) (*pb.CommandResult, error) {
		resp, err := c.commandHandler.SendBackToRepair(ctx, &pb.SendBackToRepairRequest{Id: req.GetSessionId(), ExpectedHeadSha: req.GetExpectedHeadSha(), ExpectedProofDigest: req.GetExpectedProofDigest(), Note: req.GetNote()})
		return &pb.CommandResult{Payload: &pb.CommandResult_SendBackToRepair{SendBackToRepair: resp}}, err
	})
}

func (c *StreamClient) dispatchOperatorReview(ctx context.Context, cmdID string, outbound chan<- *pb.DaemonEvent, run func(context.Context) (*pb.CommandResult, error)) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		result, err := run(ctx)
		if err != nil {
			return commandErrCode(cmdID, refusalText(err), classifyAttentionCommandError(err))
		}
		result.CommandId, result.Ok = cmdID, true
		return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: result}}
	})
}
