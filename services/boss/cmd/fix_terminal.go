package main

import (
	"github.com/spf13/cobra"

	"github.com/recurser/boss/internal/termreset"
)

// fixTerminalCmd builds the `boss fix-terminal` escape hatch. It writes
// termreset.AbnormalExitReset — every input-reporting mode an abnormal exit can
// strand (mouse, focus, bracketed paste, modifyOtherKeys/Kitty) — to stdout and
// nothing else, no banner and no newline, so `ssh <host> boss fix-terminal`
// repairs a stranded local terminal without printing noise into it. The user-
// facing Short/Long stay in plain language. See BOS-650.
func fixTerminalCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "fix-terminal",
		Short: "Clear stranded terminal mouse- and focus-reporting modes",
		Long: `Clear stranded terminal mouse- and focus-reporting modes.

If a boss session died abnormally — an SSH connection dropped, a terminal tab
was closed, the process was killed — the terminal can be left in xterm
mouse-reporting mode. Every mouse movement over the window is then echoed as
garbage text such as "35;51;14M35;48;14M" or "^[[<35;31;13M", and native
click-drag selection stops working. Focus reporting can be stranded the same
way, printing a stray "I" or "O" every time the window gains or loses focus.

This command writes the disable sequences for both and exits. It prints
nothing else, so it is safe to run through SSH against the stuck terminal:

  boss fix-terminal
  ssh <host> boss fix-terminal

Boss also writes the same reset every time the TUI starts, so simply launching
boss again on the affected terminal usually clears it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			termreset.WriteAbnormalExitReset(cmd.OutOrStdout())
			return nil
		},
		SilenceUsage: true,
	}
}
