package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

// runRepairStart calls the daemon's StartRepairWorkflow RPC to (re-)arm the
// auto-repair workflow, then prints the daemon's one-line detail. This is the
// operator's manual recovery surface when the repair plugin was stopped or
// restarted and the workflow is sitting CANCELLED — no bossd restart needed.
func runRepairStart(cmd *cobra.Command) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()
	resp, err := c.StartRepairWorkflow(ctx)
	if err != nil {
		return fmt.Errorf("StartRepairWorkflow: %w", err)
	}

	out := cmd.OutOrStdout()
	if resp.GetAlreadyRunning() {
		_, _ = fmt.Fprintf(out, "Auto-repair already armed: %s\n", resp.GetDetail())
		return nil
	}
	_, _ = fmt.Fprintf(out, "Auto-repair armed: %s\n", resp.GetDetail())
	return nil
}
