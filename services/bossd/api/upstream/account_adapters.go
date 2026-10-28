package upstream

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// AddAgentAccount implements SessionCommandHandler.AddAgentAccount, translating the stream
// command into the daemon's AddAgentAccountRequest. The credential blob is inbound
// only — forwarded verbatim into the keyring by the daemon handler and never
// logged or echoed here.
func (a *CommandHandlerAdapter) AddAgentAccount(ctx context.Context, cmd *pb.AddAgentAccountCommand) (*pb.AddAgentAccountResponse, error) {
	if a.Commands == nil {
		return nil, errors.New("add_account: command server not wired")
	}
	resp, err := a.Commands.AddAgentAccount(ctx, connect.NewRequest(&pb.AddAgentAccountRequest{
		Provider:   cmd.GetProvider(),
		Label:      cmd.GetLabel(),
		Priority:   cmd.GetPriority(),
		Credential: cmd.GetCredential(),
	}))
	if err != nil {
		return nil, fmt.Errorf("add account: %w", err)
	}
	return resp.Msg, nil
}

// RefreshAgentAccount implements SessionCommandHandler.RefreshAgentAccount, translating the
// stream command into the daemon's RefreshAgentAccountRequest. The credential is
// inbound only; test_after_save is copied through so the daemon runs the smoke
// check when requested.
func (a *CommandHandlerAdapter) RefreshAgentAccount(ctx context.Context, cmd *pb.RefreshAgentAccountCommand) (*pb.RefreshAgentAccountResponse, error) {
	if a.Commands == nil {
		return nil, errors.New("refresh_account: command server not wired")
	}
	resp, err := a.Commands.RefreshAgentAccount(ctx, connect.NewRequest(&pb.RefreshAgentAccountRequest{
		Id:            cmd.GetId(),
		Credential:    cmd.GetCredential(),
		TestAfterSave: cmd.GetTestAfterSave(),
	}))
	if err != nil {
		return nil, fmt.Errorf("refresh account: %w", err)
	}
	return resp.Msg, nil
}

// UpdateAgentAccount implements SessionCommandHandler.UpdateAgentAccount, translating the
// stream command into the daemon's UpdateAgentAccountRequest. The optional label /
// priority / status pointers are forwarded 1:1 so the daemon applies present-only
// semantics; allowed_models (repeated, no presence) is passed straight through.
func (a *CommandHandlerAdapter) UpdateAgentAccount(ctx context.Context, cmd *pb.UpdateAgentAccountCommand) (*pb.UpdateAgentAccountResponse, error) {
	if a.Commands == nil {
		return nil, errors.New("update_account: command server not wired")
	}
	resp, err := a.Commands.UpdateAgentAccount(ctx, connect.NewRequest(&pb.UpdateAgentAccountRequest{
		Id:            cmd.GetId(),
		Label:         cmd.Label,
		Priority:      cmd.Priority,
		Status:        cmd.Status,
		AllowedModels: cmd.GetAllowedModels(),
	}))
	if err != nil {
		return nil, fmt.Errorf("update account: %w", err)
	}
	return resp.Msg, nil
}

// RemoveAgentAccount implements SessionCommandHandler.RemoveAgentAccount by delegating to
// the daemon's RemoveAgentAccount connect handler. The RemoveAgentAccountResponse carries
// no payload, so the response is discarded.
func (a *CommandHandlerAdapter) RemoveAgentAccount(ctx context.Context, id string) error {
	if a.Commands == nil {
		return errors.New("remove_account: command server not wired")
	}
	if _, err := a.Commands.RemoveAgentAccount(ctx, connect.NewRequest(&pb.RemoveAgentAccountRequest{Id: id})); err != nil {
		return fmt.Errorf("remove account: %w", err)
	}
	return nil
}

// TestAgentAccount implements SessionCommandHandler.TestAgentAccount by delegating to the
// daemon's TestAgentAccount connect handler and unwrapping the response message.
func (a *CommandHandlerAdapter) TestAgentAccount(ctx context.Context, cmd *pb.TestAgentAccountCommand) (*pb.TestAgentAccountResponse, error) {
	if a.Commands == nil {
		return nil, errors.New("test_account: command server not wired")
	}
	resp, err := a.Commands.TestAgentAccount(ctx, connect.NewRequest(&pb.TestAgentAccountRequest{Id: cmd.GetId()}))
	if err != nil {
		return nil, fmt.Errorf("test account: %w", err)
	}
	return resp.Msg, nil
}
