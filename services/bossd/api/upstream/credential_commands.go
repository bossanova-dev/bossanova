package upstream

import (
	"context"
	"fmt"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// CredentialSyncHandler handles encrypted credential stream notifications.
type CredentialSyncHandler interface {
	Receive(context.Context, string) error
	DevicesChanged(context.Context, string) error
}

// Receive delegates an available credential to the daemon-owned engine.
func (a *CommandHandlerAdapter) Receive(ctx context.Context, id string) error {
	if a.CredentialSync == nil {
		return fmt.Errorf("credential sync not wired")
	}
	return a.CredentialSync.Receive(ctx, id)
}

// DevicesChanged applies approved devices and revocations to the credential key.
func (a *CommandHandlerAdapter) DevicesChanged(ctx context.Context, id string) error {
	if a.CredentialSync == nil {
		return fmt.Errorf("credential sync not wired")
	}
	return a.CredentialSync.DevicesChanged(ctx, id)
}
func (c *StreamClient) dispatchCredentialSync(ctx context.Context, id, syncedID, revokedID string, devices bool, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	h, ok := c.commandHandler.(CredentialSyncHandler)
	if !ok {
		return commandErrCode(id, "credential sync not wired", pb.CommandResult_ERROR_CODE_FAILED_PRECONDITION)
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		var err error
		if devices {
			err = h.DevicesChanged(ctx, revokedID)
		} else {
			err = h.Receive(ctx, syncedID)
		}
		if err != nil {
			return commandErrCode(id, err.Error(), classifyCommandError(err))
		}
		return commandOK(id, nil)
	})
}
