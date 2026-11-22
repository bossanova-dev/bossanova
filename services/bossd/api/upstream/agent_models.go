package upstream

import (
	"context"
	"errors"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// AgentModelLister answers ListAgentModelsCommand. It is a separate capability
// of the command handler, not a SessionCommandHandler method, so handlers that
// predate it keep compiling and answer that the command is not wired.
type AgentModelLister interface {
	ListAgentModels(ctx context.Context, agentName string) (*pb.AgentModelsResult, error)
}

// ListAgentModels implements AgentModelLister through the daemon server.
func (a *CommandHandlerAdapter) ListAgentModels(ctx context.Context, agentName string) (*pb.AgentModelsResult, error) {
	if a.AgentModels == nil {
		return nil, errors.New("list_agent_models: model lister not wired")
	}
	return a.AgentModels.ListAgentModels(ctx, agentName)
}

// dispatchListAgentModels runs asynchronously because a plugin may start its
// agent's CLI to answer.
func (c *StreamClient) dispatchListAgentModels(ctx context.Context, cmdID string, req *pb.ListAgentModelsCommand, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	lister, ok := c.commandHandler.(AgentModelLister)
	if !ok {
		return commandErr(cmdID, "agent model listing not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		out, err := lister.ListAgentModels(ctx, req.GetAgentName())
		if err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: &pb.CommandResult{
			CommandId: cmdID,
			Ok:        true,
			Payload:   &pb.CommandResult_AgentModels{AgentModels: out},
		}}}
	})
}
