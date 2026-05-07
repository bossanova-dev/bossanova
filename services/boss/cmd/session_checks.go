package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// runSessionChecks prints the daemon's persisted view of a session's CI
// checks alongside the DisplayStatus it computed for each snapshot. The
// rationale lives in the corresponding RPC handler — answering "why did
// the TUI think this PR was passing when GitHub says failing?" without
// the operator having to re-run gh by hand. Limit defaults to 5; older
// snapshots remain in SQLite if a wider sweep is needed.
func runSessionChecks(cmd *cobra.Command, sessionID string, limit int32) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
	defer cancel()
	resolved, err := resolveSessionID(c, ctx, sessionID)
	if err != nil {
		return err
	}
	resp, err := c.ListCheckSnapshots(ctx, resolved, limit)
	if err != nil {
		return fmt.Errorf("ListCheckSnapshots: %w", err)
	}
	out := cmd.OutOrStdout()
	if len(resp.GetSnapshots()) == 0 {
		_, _ = fmt.Fprintln(out, "No check snapshots recorded yet — the daemon will start writing them on the next display poll cycle.")
		return nil
	}
	for i, snap := range resp.GetSnapshots() {
		when := ""
		if t := snap.GetPolledAt(); t != nil {
			when = t.AsTime().Local().Format(time.RFC3339)
		}
		_, _ = fmt.Fprintf(out, "── snapshot %d  %s  head=%s  computed=%s\n",
			i+1, when, shortSHA(snap.GetHeadSha()), displayStatusName(snap.GetComputedStatus()))
		_, _ = fmt.Fprintln(out, indentJSON(snap.GetRawJson()))
		_, _ = fmt.Fprintln(out)
	}
	return nil
}

// indentJSON pretty-prints raw_json with two-space indentation; falls
// back to the original string when the JSON is malformed (which would
// itself be a useful signal for the operator).
func indentJSON(raw string) string {
	if raw == "" {
		return ""
	}
	var generic any
	if err := json.Unmarshal([]byte(raw), &generic); err != nil {
		return "  " + raw
	}
	pretty, err := json.MarshalIndent(generic, "  ", "  ")
	if err != nil {
		return "  " + raw
	}
	return "  " + string(pretty)
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	if sha == "" {
		return "(none)"
	}
	return sha
}

// displayStatusName turns the proto enum into the kebab label the rest
// of the TUI uses (idle, checking, failing, conflict, …).
func displayStatusName(s pb.DisplayStatus) string {
	name := strings.TrimPrefix(s.String(), "DISPLAY_STATUS_")
	return strings.ToLower(name)
}
