package upstream

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// SessionLabelCommands answers session label CRUD and assignment stream commands.
type SessionLabelCommands interface {
	ListSessionLabels(ctx context.Context) (*pb.ListSessionLabelsResponse, error)
	CreateSessionLabel(ctx context.Context, cmd *pb.CreateSessionLabelCommand) (*pb.CreateSessionLabelResponse, error)
	UpdateSessionLabel(ctx context.Context, cmd *pb.UpdateSessionLabelCommand) (*pb.UpdateSessionLabelResponse, error)
	DeleteSessionLabel(ctx context.Context, cmd *pb.DeleteSessionLabelCommand) error
	SetSessionLabels(ctx context.Context, cmd *pb.SetSessionLabelsCommand) (*pb.Session, error)
}

type sessionLabelServer interface {
	ListSessionLabels(context.Context, *connect.Request[pb.ListSessionLabelsRequest]) (*connect.Response[pb.ListSessionLabelsResponse], error)
	CreateSessionLabel(context.Context, *connect.Request[pb.CreateSessionLabelRequest]) (*connect.Response[pb.CreateSessionLabelResponse], error)
	UpdateSessionLabel(context.Context, *connect.Request[pb.UpdateSessionLabelRequest]) (*connect.Response[pb.UpdateSessionLabelResponse], error)
	DeleteSessionLabel(context.Context, *connect.Request[pb.DeleteSessionLabelRequest]) (*connect.Response[pb.DeleteSessionLabelResponse], error)
	SetSessionLabels(context.Context, *connect.Request[pb.SetSessionLabelsRequest]) (*connect.Response[pb.SetSessionLabelsResponse], error)
}

func (a *CommandHandlerAdapter) ListSessionLabels(ctx context.Context) (*pb.ListSessionLabelsResponse, error) {
	srv, ok := a.Commands.(sessionLabelServer)
	if !ok {
		return nil, errors.New("list_session_labels: label commands not wired")
	}
	resp, err := srv.ListSessionLabels(ctx, connect.NewRequest(&pb.ListSessionLabelsRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (a *CommandHandlerAdapter) CreateSessionLabel(ctx context.Context, cmd *pb.CreateSessionLabelCommand) (*pb.CreateSessionLabelResponse, error) {
	srv, ok := a.Commands.(sessionLabelServer)
	if !ok {
		return nil, errors.New("create_session_label: label commands not wired")
	}
	resp, err := srv.CreateSessionLabel(ctx, connect.NewRequest(&pb.CreateSessionLabelRequest{
		Name:        cmd.GetName(),
		Color:       cmd.GetColor(),
		Description: cmd.GetDescription(),
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (a *CommandHandlerAdapter) UpdateSessionLabel(ctx context.Context, cmd *pb.UpdateSessionLabelCommand) (*pb.UpdateSessionLabelResponse, error) {
	srv, ok := a.Commands.(sessionLabelServer)
	if !ok {
		return nil, errors.New("update_session_label: label commands not wired")
	}
	resp, err := srv.UpdateSessionLabel(ctx, connect.NewRequest(&pb.UpdateSessionLabelRequest{
		Id:          cmd.GetId(),
		Name:        cmd.Name,
		Color:       cmd.Color,
		Description: cmd.Description,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (a *CommandHandlerAdapter) DeleteSessionLabel(ctx context.Context, cmd *pb.DeleteSessionLabelCommand) error {
	srv, ok := a.Commands.(sessionLabelServer)
	if !ok {
		return errors.New("delete_session_label: label commands not wired")
	}
	_, err := srv.DeleteSessionLabel(ctx, connect.NewRequest(&pb.DeleteSessionLabelRequest{Id: cmd.GetId()}))
	return err
}

func (a *CommandHandlerAdapter) SetSessionLabels(ctx context.Context, cmd *pb.SetSessionLabelsCommand) (*pb.Session, error) {
	srv, ok := a.Commands.(sessionLabelServer)
	if !ok {
		return nil, errors.New("set_session_labels: label commands not wired")
	}
	resp, err := srv.SetSessionLabels(ctx, connect.NewRequest(&pb.SetSessionLabelsRequest{
		SessionId: cmd.GetSessionId(),
		LabelIds:  cmd.GetLabelIds(),
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSession(), nil
}

func (c *StreamClient) dispatchSessionLabelCommand(ctx context.Context, cmdID string, cmd *pb.OrchestratorCommand) *pb.DaemonEvent {
	handler, ok := c.commandHandler.(SessionLabelCommands)
	if !ok {
		return commandErr(cmdID, "session label commands not wired")
	}
	switch v := cmd.GetCmd().(type) {
	case *pb.OrchestratorCommand_ListSessionLabels:
		resp, err := handler.ListSessionLabels(ctx)
		if err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: &pb.CommandResult{
			CommandId: cmdID, Ok: true,
			Payload: &pb.CommandResult_ListSessionLabels{ListSessionLabels: resp},
		}}}
	case *pb.OrchestratorCommand_CreateSessionLabel:
		resp, err := handler.CreateSessionLabel(ctx, v.CreateSessionLabel)
		if err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: &pb.CommandResult{
			CommandId: cmdID, Ok: true,
			Payload: &pb.CommandResult_CreateSessionLabel{CreateSessionLabel: resp},
		}}}
	case *pb.OrchestratorCommand_UpdateSessionLabel:
		resp, err := handler.UpdateSessionLabel(ctx, v.UpdateSessionLabel)
		if err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: &pb.CommandResult{
			CommandId: cmdID, Ok: true,
			Payload: &pb.CommandResult_UpdateSessionLabel{UpdateSessionLabel: resp},
		}}}
	case *pb.OrchestratorCommand_DeleteSessionLabel:
		if err := handler.DeleteSessionLabel(ctx, v.DeleteSessionLabel); err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: &pb.CommandResult{CommandId: cmdID, Ok: true}}}
	case *pb.OrchestratorCommand_SetSessionLabels:
		sess, err := handler.SetSessionLabels(ctx, v.SetSessionLabels)
		if err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return commandOK(cmdID, sess)
	default:
		return commandErr(cmdID, "unknown session label command")
	}
}
