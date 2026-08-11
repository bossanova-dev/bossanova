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
	asJSON, _ := cmd.Flags().GetBool(jsonFlagName)

	c, err := newClient(cmd)
	if err != nil {
		return emitJSONFailure(cmd, asJSON, err)
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
	defer cancel()
	resolved, err := resolveSessionID(c, ctx, sessionID)
	if err != nil {
		return emitJSONFailure(cmd, asJSON, err)
	}
	resp, err := c.ListCheckSnapshots(ctx, resolved, limit)
	if err != nil {
		return emitJSONFailure(cmd, asJSON, fmt.Errorf("ListCheckSnapshots: %w", err))
	}
	if asJSON {
		rows := make([]checkSnapshotJSON, 0, len(resp.GetSnapshots()))
		for _, snap := range resp.GetSnapshots() {
			rows = append(rows, newCheckSnapshotJSON(snap))
		}
		return emitJSON(cmd, checkSnapshotListJSON{Snapshots: rows})
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

// checkSnapshotListJSON is the `boss session checks --json` envelope.
// Snapshots is always a non-nil slice so "none recorded yet" marshals as
// `[]` rather than `null`.
type checkSnapshotListJSON struct {
	Snapshots []checkSnapshotJSON `json:"snapshots"`
}

// checkSnapshotJSON is one persisted poll of a PR's checks.
type checkSnapshotJSON struct {
	PolledAt string `json:"polled_at"`
	// HeadSHA is the full SHA — the human rendering abbreviates for width,
	// a caller wants the value it can compare against git.
	HeadSHA string `json:"head_sha"`
	// ComputedStatus reuses displayStatusName so JSON and text agree.
	ComputedStatus string `json:"computed_status"`
	// Raw is the daemon's stored payload spliced in as a nested JSON value,
	// not an escaped string — a caller reads snapshots[].raw.state directly
	// instead of decoding twice. json.RawMessage is what makes that splice
	// possible; assigning raw_json to a string field double-encodes it.
	Raw json.RawMessage `json:"raw"`
	// RawInvalid carries the payload verbatim when it does not parse, so a
	// malformed row degrades to a readable string instead of either being
	// dropped or corrupting the whole envelope.
	RawInvalid string `json:"raw_invalid,omitempty"`
}

func newCheckSnapshotJSON(snap *pb.CheckSnapshot) checkSnapshotJSON {
	row := checkSnapshotJSON{
		PolledAt:       rfc3339OrEmpty(snap.GetPolledAt()),
		HeadSHA:        snap.GetHeadSha(),
		ComputedStatus: displayStatusName(snap.GetComputedStatus()),
		// Raw is never left nil: a nil json.RawMessage marshals as the
		// invalid token `` (empty), which would break the envelope.
		Raw: json.RawMessage("null"),
	}
	raw := snap.GetRawJson()
	if raw == "" {
		return row
	}
	if !json.Valid([]byte(raw)) {
		row.RawInvalid = raw
		return row
	}
	row.Raw = json.RawMessage(raw)
	return row
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
