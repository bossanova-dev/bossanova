package diagnostics

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
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
			"per attribution (the cron, stage, plugin or daemon subsystem that made them) and quota, " +
			"with the attempts the rate-limit breaker and the background quota floor skipped " +
			"without contacting GitHub.\n\n" +
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

// sortedGitHubUsageRows orders rows by calls, descending, then by skips,
// descending, then label and quota, so the biggest spender reads first, a loop
// that was only refused is still listed, and output is stable.
func sortedGitHubUsageRows(resp *pb.GetGitHubUsageResponse) []*pb.GitHubUsageRow {
	rows := append([]*pb.GitHubUsageRow(nil), resp.GetRows()...)
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.GetCallCount() != b.GetCallCount() {
			return a.GetCallCount() > b.GetCallCount()
		}
		if gitHubUsageSkips(a) != gitHubUsageSkips(b) {
			return gitHubUsageSkips(a) > gitHubUsageSkips(b)
		}
		if a.GetLabel() != b.GetLabel() {
			return a.GetLabel() < b.GetLabel()
		}
		return a.GetQuota() < b.GetQuota()
	})
	return rows
}

func gitHubUsageSkips(r *pb.GitHubUsageRow) int64 {
	return r.GetBreakerSkipCount() + r.GetFloorSkipCount()
}

// gitHubQuotaCount is one count summed per quota and across quotas.
type gitHubQuotaCount struct {
	perQuota map[string]int64
	all      int64
}

func (c *gitHubQuotaCount) add(quota string, n int64) {
	c.perQuota[quota] += n
	c.all += n
}

// String renders the per-quota split, " (core 16, graphql 56)", in quota order.
func (c gitHubQuotaCount) String() string {
	quotas := make([]string, 0, len(c.perQuota))
	for q := range c.perQuota {
		quotas = append(quotas, q)
	}
	sort.Strings(quotas)
	parts := make([]string, 0, len(quotas))
	for _, q := range quotas {
		parts = append(parts, fmt.Sprintf("%s %d", q, c.perQuota[q]))
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// gitHubUsageTotals sums calls, breaker skips and floor skips per quota.
type gitHubUsageTotals struct {
	calls, breakerSkips, floorSkips gitHubQuotaCount
}

func gitHubUsageTotalsOf(rows []*pb.GitHubUsageRow) gitHubUsageTotals {
	t := gitHubUsageTotals{
		calls:        gitHubQuotaCount{perQuota: map[string]int64{}},
		breakerSkips: gitHubQuotaCount{perQuota: map[string]int64{}},
		floorSkips:   gitHubQuotaCount{perQuota: map[string]int64{}},
	}
	for _, r := range rows {
		t.calls.add(r.GetQuota(), r.GetCallCount())
		t.breakerSkips.add(r.GetQuota(), r.GetBreakerSkipCount())
		t.floorSkips.add(r.GetQuota(), r.GetFloorSkipCount())
	}
	return t
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
		cells = append(cells, []string{
			r.GetLabel(), r.GetQuota(),
			strconv.FormatInt(r.GetCallCount(), 10),
			strconv.FormatInt(r.GetBreakerSkipCount(), 10),
			strconv.FormatInt(r.GetFloorSkipCount(), 10),
		})
	}
	cliutil.RenderCLITable(cmd, []string{"LABEL", "QUOTA", "CALLS", "BREAKER SKIPS", "FLOOR SKIPS"}, cells)
	totals := gitHubUsageTotalsOf(rows)
	_, _ = fmt.Fprintf(out, "Total: %d calls%s\n", totals.calls.all, totals.calls)
	if totals.breakerSkips.all+totals.floorSkips.all > 0 {
		_, _ = fmt.Fprintf(out, "Skipped: %d by the breaker%s, %d by the floor%s\n",
			totals.breakerSkips.all, totals.breakerSkips, totals.floorSkips.all, totals.floorSkips)
	}
}

type gitHubUsageJSON struct {
	WindowStartedAt        string               `json:"window_started_at"`
	WindowEndedAt          string               `json:"window_ended_at"`
	Rows                   []gitHubUsageRowJSON `json:"rows"`
	QuotaTotals            map[string]int64     `json:"quota_totals"`
	TotalCallCount         int64                `json:"total_call_count"`
	QuotaBreakerSkipTotals map[string]int64     `json:"quota_breaker_skip_totals"`
	TotalBreakerSkipCount  int64                `json:"total_breaker_skip_count"`
	QuotaFloorSkipTotals   map[string]int64     `json:"quota_floor_skip_totals"`
	TotalFloorSkipCount    int64                `json:"total_floor_skip_count"`
}

type gitHubUsageRowJSON struct {
	Label            string `json:"label"`
	Quota            string `json:"quota"`
	CallCount        int64  `json:"call_count"`
	BreakerSkipCount int64  `json:"breaker_skip_count"`
	FloorSkipCount   int64  `json:"floor_skip_count"`
}

func gitHubUsageToJSON(resp *pb.GetGitHubUsageResponse) gitHubUsageJSON {
	rows := sortedGitHubUsageRows(resp)
	totals := gitHubUsageTotalsOf(rows)
	out := gitHubUsageJSON{
		WindowStartedAt:        protoTimestampString(resp.GetWindowStartedAt()),
		WindowEndedAt:          protoTimestampString(resp.GetWindowEndedAt()),
		Rows:                   make([]gitHubUsageRowJSON, 0, len(rows)),
		QuotaTotals:            totals.calls.perQuota,
		TotalCallCount:         totals.calls.all,
		QuotaBreakerSkipTotals: totals.breakerSkips.perQuota,
		TotalBreakerSkipCount:  totals.breakerSkips.all,
		QuotaFloorSkipTotals:   totals.floorSkips.perQuota,
		TotalFloorSkipCount:    totals.floorSkips.all,
	}
	for _, r := range rows {
		out.Rows = append(out.Rows, gitHubUsageRowJSON{
			Label:            r.GetLabel(),
			Quota:            r.GetQuota(),
			CallCount:        r.GetCallCount(),
			BreakerSkipCount: r.GetBreakerSkipCount(),
			FloorSkipCount:   r.GetFloorSkipCount(),
		})
	}
	return out
}
