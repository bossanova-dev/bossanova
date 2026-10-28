package upstream

import (
	"context"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// dispatchAddAccount routes an AddAgentAccountCommand to the handler and wraps the
// created account in a CommandResult{add_account} (metadata only — the inbound
// credential is never echoed). Dispatched async: registering an account touches
// the store and keyring.
func (c *StreamClient) dispatchAddAccount(ctx context.Context, cmdID string, req *pb.AddAgentAccountCommand, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		out, err := c.commandHandler.AddAgentAccount(ctx, req)
		if err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: &pb.CommandResult{
			CommandId: cmdID, Ok: true,
			Payload: &pb.CommandResult_AddAccount{AddAccount: out},
		}}}
	})
}

// dispatchRefreshAccount routes a RefreshAgentAccountCommand to the handler and wraps
// the updated account (+ optional smoke-test outcome) in a
// CommandResult{refresh_account}. Dispatched async: saving the credential and
// running the optional provider test are store/keyring/network-bound.
func (c *StreamClient) dispatchRefreshAccount(ctx context.Context, cmdID string, req *pb.RefreshAgentAccountCommand, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		out, err := c.commandHandler.RefreshAgentAccount(ctx, req)
		if err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: &pb.CommandResult{
			CommandId: cmdID, Ok: true,
			Payload: &pb.CommandResult_RefreshAccount{RefreshAccount: out},
		}}}
	})
}

// dispatchUpdateAccount routes an UpdateAgentAccountCommand to the handler and wraps
// the updated account in a CommandResult{update_account}. Only the optional
// fields the command sets are applied by the daemon handler. Dispatched async
// (store write).
func (c *StreamClient) dispatchUpdateAccount(ctx context.Context, cmdID string, req *pb.UpdateAgentAccountCommand, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		out, err := c.commandHandler.UpdateAgentAccount(ctx, req)
		if err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: &pb.CommandResult{
			CommandId: cmdID, Ok: true,
			Payload: &pb.CommandResult_UpdateAccount{UpdateAccount: out},
		}}}
	})
}

// dispatchRemoveAccount routes a RemoveAgentAccountCommand to the handler and replies
// with a success CommandResult carrying no payload (mirrors dispatchRemoveRepo).
// Dispatched async: removal touches the store and keyring.
func (c *StreamClient) dispatchRemoveAccount(ctx context.Context, cmdID string, req *pb.RemoveAgentAccountCommand, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		if err := c.commandHandler.RemoveAgentAccount(ctx, req.GetId()); err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return commandOK(cmdID, nil)
	})
}

// dispatchTestAccount routes a TestAgentAccountCommand to the handler and wraps the
// updated account (+ validation outcome) in a CommandResult{test_account}.
// Dispatched async: provider verification is network-bound.
func (c *StreamClient) dispatchTestAccount(ctx context.Context, cmdID string, req *pb.TestAgentAccountCommand, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		out, err := c.commandHandler.TestAgentAccount(ctx, req)
		if err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: &pb.CommandResult{
			CommandId: cmdID, Ok: true,
			Payload: &pb.CommandResult_TestAccount{TestAccount: out},
		}}}
	})
}
