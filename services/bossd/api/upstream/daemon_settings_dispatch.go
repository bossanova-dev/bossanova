package upstream

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// DaemonSettingsHandler reads and changes the web-editable part of the
// daemon's settings.json.
type DaemonSettingsHandler interface {
	GetDaemonSettings(ctx context.Context) (*pb.DaemonSettingsResult, error)
	UpdateDaemonSettings(ctx context.Context, update *pb.DaemonSettingsUpdate) (*pb.DaemonSettingsResult, error)
}

var errDaemonSettingsNotWired = errors.New("daemon settings handler not wired")

// GetDaemonSettings implements DaemonSettingsHandler through the server wired
// into Settings after construction, as Commands is.
func (a *CommandHandlerAdapter) GetDaemonSettings(ctx context.Context) (*pb.DaemonSettingsResult, error) {
	if a.Settings == nil {
		return nil, errDaemonSettingsNotWired
	}
	return a.Settings.GetDaemonSettings(ctx)
}

// UpdateDaemonSettings implements DaemonSettingsHandler like GetDaemonSettings.
func (a *CommandHandlerAdapter) UpdateDaemonSettings(ctx context.Context, update *pb.DaemonSettingsUpdate) (*pb.DaemonSettingsResult, error) {
	if a.Settings == nil {
		return nil, errDaemonSettingsNotWired
	}
	return a.Settings.UpdateDaemonSettings(ctx, update)
}

func (c *StreamClient) dispatchDaemonSettings(ctx context.Context, cmdID string, outbound chan<- *pb.DaemonEvent, run func(DaemonSettingsHandler) (*pb.DaemonSettingsResult, error)) *pb.DaemonEvent {
	if c.daemonSettings == nil {
		return commandErr(cmdID, errDaemonSettingsNotWired.Error())
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		result, err := run(c.daemonSettings)
		if err != nil {
			return commandErrCode(cmdID, err.Error(), classifyDaemonSettingsError(err))
		}
		return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: &pb.CommandResult{
			CommandId: cmdID, Ok: true,
			Payload: &pb.CommandResult_DaemonSettings{DaemonSettings: result},
		}}}
	})
}

// classifyDaemonSettingsError adds two arms the shared classifier withholds
// from older commands. These commands are new, so no caller relies on either
// rendering as CodeAborted. A permission bypass refused for want of full cloud
// control is a cloud control refusal.
func classifyDaemonSettingsError(err error) pb.CommandResult_ErrorCode {
	switch connect.CodeOf(err) {
	case connect.CodeInvalidArgument:
		return pb.CommandResult_ERROR_CODE_INVALID_ARGUMENT
	case connect.CodePermissionDenied:
		return pb.CommandResult_ERROR_CODE_CLOUD_CONTROL_DISABLED
	default:
		return classifyCommandError(err)
	}
}
