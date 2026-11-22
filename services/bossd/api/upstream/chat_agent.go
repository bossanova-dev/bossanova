package upstream

import (
	"context"
	"errors"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// ChatAgentCommands answers AnswerChatQuestionCommand and StopChatCommand. It
// is a separate capability of the command handler, not a SessionCommandHandler
// method, so handlers that predate it keep compiling and answer that the
// command is not wired.
type ChatAgentCommands interface {
	AnswerChatQuestion(ctx context.Context, cmd *pb.AnswerChatQuestionCommand) (*pb.AnswerChatQuestionResult, error)
	StopChat(ctx context.Context, cmd *pb.StopChatCommand) error
}

// AnswerChatQuestion implements ChatAgentCommands through the daemon server,
// which is the adapter's Commands.
func (a *CommandHandlerAdapter) AnswerChatQuestion(ctx context.Context, cmd *pb.AnswerChatQuestionCommand) (*pb.AnswerChatQuestionResult, error) {
	agents, ok := a.Commands.(ChatAgentCommands)
	if !ok {
		return nil, errors.New("answer_chat_question: chat agent commands not wired")
	}
	return agents.AnswerChatQuestion(ctx, cmd)
}

// StopChat implements ChatAgentCommands through the daemon server.
func (a *CommandHandlerAdapter) StopChat(ctx context.Context, cmd *pb.StopChatCommand) error {
	agents, ok := a.Commands.(ChatAgentCommands)
	if !ok {
		return errors.New("stop_chat: chat agent commands not wired")
	}
	return agents.StopChat(ctx, cmd)
}

// dispatchChatAgent routes the commands that act on one chat's agent.
func (c *StreamClient) dispatchChatAgent(ctx context.Context, cmdID string, cmd *pb.OrchestratorCommand, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	if stop := cmd.GetStopChat(); stop != nil {
		return c.dispatchStopChat(ctx, cmdID, stop, outbound)
	}
	return c.dispatchAnswerChatQuestion(ctx, cmdID, cmd.GetAnswerChatQuestion(), outbound)
}

// dispatchAnswerChatQuestion runs asynchronously: the daemon watches the pane
// for a few seconds to confirm the agent took the answer.
func (c *StreamClient) dispatchAnswerChatQuestion(ctx context.Context, cmdID string, req *pb.AnswerChatQuestionCommand, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	handler, ok := c.commandHandler.(ChatAgentCommands)
	if !ok {
		return commandErr(cmdID, "answering a chat question is not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		out, err := handler.AnswerChatQuestion(ctx, req)
		if err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: &pb.CommandResult{
			CommandId: cmdID,
			Ok:        true,
			Payload:   &pb.CommandResult_AnswerChatQuestion{AnswerChatQuestion: out},
		}}}
	})
}

// dispatchStopChat runs asynchronously because ending a pane waits on tmux.
func (c *StreamClient) dispatchStopChat(ctx context.Context, cmdID string, req *pb.StopChatCommand, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	handler, ok := c.commandHandler.(ChatAgentCommands)
	if !ok {
		return commandErr(cmdID, "stopping a chat is not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		if err := handler.StopChat(ctx, req); err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: &pb.CommandResult{CommandId: cmdID, Ok: true}}}
	})
}

// setOrNil is value for an optional field, unset when value is empty.
func setOrNil(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
