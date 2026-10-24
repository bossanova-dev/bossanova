package attachprocess

import (
	"context"
	"os/exec"
)

// Command and ExitError keep process implementation types outside TUI screens.
type Command = exec.Cmd
type ExitError = exec.ExitError

// OS constructs operating system processes.
type OS struct{}

func (OS) Command(name string, args ...string) *Command { return exec.Command(name, args...) }
func (OS) CommandContext(ctx context.Context, name string, args ...string) *Command {
	return exec.CommandContext(ctx, name, args...)
}
