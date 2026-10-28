package upstream

import (
	"context"
	"fmt"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/domain"
)

// AgentAccountSyncHandler applies domain metadata without exposing protobufs internally.
type AgentAccountSyncHandler interface {
	Apply(context.Context, domain.ApplyAgentAccountSync) error
}

// Apply delegates a metadata sync to the daemon-owned applier.
func (a *CommandHandlerAdapter) Apply(ctx context.Context, cmd domain.ApplyAgentAccountSync) error {
	if a.AccountSync == nil {
		return fmt.Errorf("account sync applier not wired")
	}
	return a.AccountSync.Apply(ctx, cmd)
}

func (c *StreamClient) dispatchApplyAgentAccountSync(ctx context.Context, id string, cmd *pb.ApplyAgentAccountSyncCommand, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	handler, ok := c.commandHandler.(AgentAccountSyncHandler)
	if !ok {
		return commandErrCode(id, "account sync applier not wired", pb.CommandResult_ERROR_CODE_FAILED_PRECONDITION)
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		err := handler.Apply(ctx, domain.ApplyAgentAccountSync{LocalAgentAccountID: cmd.GetLocalAgentAccountId(), SyncedAgentAccountID: cmd.GetSyncedAgentAccountId(), Version: cmd.GetVersion(), DesiredStatus: cmd.GetDesiredStatus(), Priority: int(cmd.GetPriority()), IsQuarantined: cmd.GetIsQuarantined(), ShouldVerifyAuth: cmd.GetShouldVerifyAuth()})
		if err != nil {
			return commandErrCode(id, err.Error(), classifyCommandError(err))
		}
		return commandOK(id, nil)
	})
}
