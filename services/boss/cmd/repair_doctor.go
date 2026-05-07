package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

// runRepairDoctor calls the daemon's RepairDoctor RPC and renders the
// returned checklist plus the recent-logs table. The point is to give
// the operator one command that answers "is auto-repair healthy?" without
// needing to grep daemon stderr.
func runRepairDoctor(cmd *cobra.Command) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()
	resp, err := c.RepairDoctor(ctx)
	if err != nil {
		return fmt.Errorf("RepairDoctor: %w", err)
	}

	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintln(out, "Auto-repair health check")
	_, _ = fmt.Fprintln(out, "------------------------")
	allOK := true
	for i, ck := range resp.GetChecks() {
		marker := "✓"
		if !ck.GetOk() {
			marker = "✗"
			allOK = false
		}
		_, _ = fmt.Fprintf(out, "  %d. %s %s\n", i+1, marker, ck.GetName())
		if d := ck.GetDetail(); d != "" {
			_, _ = fmt.Fprintf(out, "       %s\n", d)
		}
	}

	logs := resp.GetRecentLogs()
	if len(logs) > 0 {
		_, _ = fmt.Fprintln(out)
		_, _ = fmt.Fprintln(out, "Recent repair logs (newest first):")
		for _, l := range logs {
			head := l.GetHeadLine()
			if head == "" {
				head = "(empty file — runner crashed before opening log)"
			}
			ts := ""
			if t := l.GetModifiedAt(); t != nil {
				ts = t.AsTime().Local().Format(time.RFC3339)
			}
			_, _ = fmt.Fprintf(out, "  %s  %6d bytes  %s\n", ts, l.GetSizeBytes(), l.GetPath())
			_, _ = fmt.Fprintf(out, "       %s\n", head)
		}
	}

	_, _ = fmt.Fprintln(out)
	if allOK {
		_, _ = fmt.Fprintln(out, "All checks passed.")
		return nil
	}
	_, _ = fmt.Fprintln(out, "One or more checks failed — see details above.")
	return fmt.Errorf("repair doctor reported failures")
}
