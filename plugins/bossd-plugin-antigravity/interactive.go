package main

import (
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/recurser/bossalib/contract/plugin/loginshell"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// agyReadyMarker is the glyph that opens agy's composer line. Earlier user
// turns in the scrollback render with the same "> ", so the host must read the
// bottom-most marker.
const agyReadyMarker = ">"

// buildInteractiveCommand renders the tmux argv:
//
//	agy [--conversation <id>] [--model m] [--effort e] [--dangerously-skip-permissions] [--mode m] --settings <trust> [--prompt-interactive <input>]
//
// --prompt-interactive submits its input as the first turn, so the launch
// consumes the initial input. agy has no system-prompt flag, so bossd's session
// context travels inside that first turn and is dropped when there is none.
// Delivery stays unconfirmed: agy's transcript stamps turns to the second, which
// cannot prove a turn came after a launch in the same second.
func (r *Runner) buildInteractiveCommand(req *bossanovav1.BuildInteractiveCommandRequest) (*bossanovav1.BuildInteractiveCommandResponse, error) {
	args := []string{"agy"}
	if req.GetResume() && req.GetSessionId() != "" {
		args = append(args, "--conversation", req.GetSessionId())
	}
	args = append(args, r.sessionFlags(req.GetModel(), req.GetEffort(), req.GetWorktreePath())...)

	input := interactiveInitialInput(req)
	support := bossanovav1.AppendSystemPromptSupport_APPEND_SYSTEM_PROMPT_SUPPORT_NONE
	if input != "" {
		args = append(args, "--prompt-interactive")
		if req.GetAppendSystemPrompt() != "" {
			support = bossanovav1.AppendSystemPromptSupport_APPEND_SYSTEM_PROMPT_SUPPORT_IN_ARGV
		}
	}
	argv, err := loginshell.WrapInitialInput(r.loginShell, loginshell.Flags(r.loginShell), args, input, req.GetInitialInputPath())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "write initial input: %v", err)
	}
	return &bossanovav1.BuildInteractiveCommandResponse{
		Argv:                      argv,
		ReadyMarker:               agyReadyMarker,
		CommandPrefix:             "/",
		ConsumesInitialInput:      input != "",
		AppendSystemPromptSupport: support,
	}, nil
}

// interactiveInitialInput renders the first turn. A boss command stays first
// so agy expands it as a slash command; the session context follows it.
// Otherwise the context leads, as a preamble to the user's prompt.
func interactiveInitialInput(req *bossanovav1.BuildInteractiveCommandRequest) string {
	input := req.GetInitialPrompt()
	command := req.GetInitialCommand()
	if command != "" {
		input = "/" + strings.TrimLeft(command, "/$")
	}
	if input == "" {
		return ""
	}
	context := req.GetAppendSystemPrompt()
	switch {
	case context == "":
		return input
	case command != "":
		return input + "\n\n" + context
	default:
		return context + "\n\n" + input
	}
}
