package diagnostics

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/recurser/boss/api/cli/cliutil"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

const gitHubUsageTimeout = 10 * time.Second

func (deps *Group) gitHubUsageCmd() *cobra.Command {
	var window time.Duration
	cmd := &cobra.Command{
		Use:   "github-usage",
		Short: "Report the GitHub calls each cron, stage and daemon subsystem made",
		Long: "Report the GitHub calls recorded against the shared token over a recent window, " +
			"per attribution (the cron, stage, plugin or daemon subsystem that made them) and quota.\n\n" +
			"Counts are calls, not quota points: one GraphQL call can cost more than one point.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return deps.runGitHubUsage(cmd, window)
		},
	}
	cmd.Flags().DurationVar(&window, "window", time.Hour, "report window ending now, at most 2h")
	cmd.Flags().Bool(cliutil.JSONFlagName, false, "emit JSON")
	return cmd
}

type gitHubUsageClient interface {
	GetGitHubUsage(ctx context.Context, req *pb.GetGitHubUsageRequest) (*pb.GetGitHubUsageResponse, error)
}

func (deps *Group) runGitHubUsage(cmd *cobra.Command, window time.Duration) error {
	asJSON, _ := cmd.Flags().GetBool(cliutil.JSONFlagName)
	if window < time.Second {
		return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf("--window must be at least 1s, got %s", window))
	}
	c, err := deps.newClient(cmd)
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, err)
	}
	gc, ok := c.(gitHubUsageClient)
	if !ok {
		return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf("client does not support GitHub usage reports"))
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), gitHubUsageTimeout)
	defer cancel()
	resp, err := gc.GetGitHubUsage(ctx, &pb.GetGitHubUsageRequest{WindowSeconds: int64(window / time.Second)})
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, err)
	}
	if asJSON {
		return cliutil.EmitJSON(cmd, gitHubUsageToJSON(resp))
	}
	renderGitHubUsage(cmd, resp)
	return nil
}

// sortedGitHubUsageRows orders rows by calls, descending, then label and
// quota, so the biggest spender reads first and output is stable.
func sortedGitHubUsageRows(resp *pb.GetGitHubUsageResponse) []*pb.GitHubUsageRow {
	rows := append([]*pb.GitHubUsageRow(nil), resp.GetRows()...)
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.GetCallCount() != b.GetCallCount() {
			return a.GetCallCount() > b.GetCallCount()
		}
		if a.GetLabel() != b.GetLabel() {
			return a.GetLabel() < b.GetLabel()
		}
		return a.GetQuota() < b.GetQuota()
	})
	return rows
}

// gitHubQuotaTotals sums calls per quota, with the total across quotas.
func gitHubQuotaTotals(rows []*pb.GitHubUsageRow) (map[string]int64, int64) {
	totals := map[string]int64{}
	var all int64
	for _, r := range rows {
		totals[r.GetQuota()] += r.GetCallCount()
		all += r.GetCallCount()
	}
	return totals, all
}

func renderGitHubUsage(cmd *cobra.Command, resp *pb.GetGitHubUsageResponse) {
	out := cmd.OutOrStdout()
	start, end := resp.GetWindowStartedAt().AsTime().Local(), resp.GetWindowEndedAt().AsTime().Local()
	_, _ = fmt.Fprintf(out, "GitHub calls %s to %s (last %s)\n",
		start.Format(time.TimeOnly), end.Format(time.TimeOnly), end.Sub(start).Round(time.Second))
	rows := sortedGitHubUsageRows(resp)
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(out, "No GitHub calls recorded in this window.")
		return
	}
	cells := make([][]string, 0, len(rows))
	for _, r := range rows {
		cells = append(cells, []string{r.GetLabel(), r.GetQuota(), strconv.FormatInt(r.GetCallCount(), 10)})
	}
	cliutil.RenderCLITable(cmd, []string{"LABEL", "QUOTA", "CALLS"}, cells)
	totals, all := gitHubQuotaTotals(rows)
	quotas := make([]string, 0, len(totals))
	for q := range totals {
		quotas = append(quotas, q)
	}
	sort.Strings(quotas)
	line := fmt.Sprintf("Total: %d calls", all)
	for i, q := range quotas {
		sep := ", "
		if i == 0 {
			sep = " ("
		}
		line += fmt.Sprintf("%s%s %d", sep, q, totals[q])
	}
	_, _ = fmt.Fprintln(out, line+")")
}

type gitHubUsageJSON struct {
	WindowStartedAt string               `json:"window_started_at"`
	WindowEndedAt   string               `json:"window_ended_at"`
	Rows            []gitHubUsageRowJSON `json:"rows"`
	QuotaTotals     map[string]int64     `json:"quota_totals"`
	TotalCallCount  int64                `json:"total_call_count"`
}

type gitHubUsageRowJSON struct {
	Label     string `json:"label"`
	Quota     string `json:"quota"`
	CallCount int64  `json:"call_count"`
}

func gitHubUsageToJSON(resp *pb.GetGitHubUsageResponse) gitHubUsageJSON {
	rows := sortedGitHubUsageRows(resp)
	totals, all := gitHubQuotaTotals(rows)
	out := gitHubUsageJSON{
		WindowStartedAt: protoTimestampString(resp.GetWindowStartedAt()),
		WindowEndedAt:   protoTimestampString(resp.GetWindowEndedAt()),
		Rows:            make([]gitHubUsageRowJSON, 0, len(rows)),
		QuotaTotals:     totals,
		TotalCallCount:  all,
	}
	for _, r := range rows {
		out.Rows = append(out.Rows, gitHubUsageRowJSON{Label: r.GetLabel(), Quota: r.GetQuota(), CallCount: r.GetCallCount()})
	}
	return out
}
