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
			"Each row names the identity its calls spent: user for the operator's gh login, or " +
			"installation:<id> for a Boss GitHub App installation pool.\n\n" +
			"CALLS counts billed calls; NOT MODIFIED counts free conditional REST responses (304). " +
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

// gitHubUsageIdentityUser is the identity of a call on the operator's gh login.
// An older daemon reports no identity, and every call it counted was on it.
const gitHubUsageIdentityUser = "user"

// gitHubUsageIdentity is the identity r's calls spent, with an empty one from an
// older daemon read as the user login.
func gitHubUsageIdentity(r *pb.GitHubUsageRow) string {
	if id := r.GetIdentity(); id != "" {
		return id
	}
	return gitHubUsageIdentityUser
}

// sortedGitHubUsageRows orders rows by calls, descending, then by skips,
// descending, then label, quota and identity, so the biggest spender reads
// first, a loop that was only refused is still listed, and output is stable.
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
		if a.GetQuota() != b.GetQuota() {
			return a.GetQuota() < b.GetQuota()
		}
		return gitHubUsageIdentity(a) < gitHubUsageIdentity(b)
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

// gitHubUsageTotals sums billed calls, free responses and skips per quota,
// and billed calls per identity.
type gitHubUsageTotals struct {
	calls, breakerSkips, floorSkips gitHubQuotaCount
	notModified                     int64
	identityCalls                   map[string]*gitHubQuotaCount
}

// identities lists every identity with a row, the user login first and then
// the installations in key order.
func (t gitHubUsageTotals) identities() []string {
	ids := make([]string, 0, len(t.identityCalls))
	for id := range t.identityCalls {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if (ids[i] == gitHubUsageIdentityUser) != (ids[j] == gitHubUsageIdentityUser) {
			return ids[i] == gitHubUsageIdentityUser
		}
		return ids[i] < ids[j]
	})
	return ids
}

func gitHubUsageTotalsOf(rows []*pb.GitHubUsageRow) gitHubUsageTotals {
	t := gitHubUsageTotals{
		calls:         gitHubQuotaCount{perQuota: map[string]int64{}},
		breakerSkips:  gitHubQuotaCount{perQuota: map[string]int64{}},
		floorSkips:    gitHubQuotaCount{perQuota: map[string]int64{}},
		identityCalls: map[string]*gitHubQuotaCount{},
	}
	for _, r := range rows {
		id := gitHubUsageIdentity(r)
		if t.identityCalls[id] == nil {
			t.identityCalls[id] = &gitHubQuotaCount{perQuota: map[string]int64{}}
		}
		t.identityCalls[id].add(r.GetQuota(), r.GetCallCount())
		t.calls.add(r.GetQuota(), r.GetCallCount())
		t.notModified += r.GetNotModifiedCount()
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
			r.GetLabel(), gitHubUsageIdentity(r), r.GetQuota(),
			strconv.FormatInt(r.GetCallCount(), 10),
			strconv.FormatInt(r.GetNotModifiedCount(), 10),
			strconv.FormatInt(r.GetBreakerSkipCount(), 10),
			strconv.FormatInt(r.GetFloorSkipCount(), 10),
		})
	}
	cliutil.RenderCLITable(cmd, []string{"LABEL", "IDENTITY", "QUOTA", "CALLS", "NOT MODIFIED", "BREAKER SKIPS", "FLOOR SKIPS"}, cells)
	totals := gitHubUsageTotalsOf(rows)
	_, _ = fmt.Fprintf(out, "Total: %d calls%s", totals.calls.all, totals.calls)
	if totals.notModified > 0 {
		_, _ = fmt.Fprintf(out, "; %d not modified (free)", totals.notModified)
	}
	_, _ = fmt.Fprintln(out)
	for _, id := range totals.identities() {
		c := totals.identityCalls[id]
		_, _ = fmt.Fprintf(out, "By token: %s: %d calls%s\n", id, c.all, c)
	}
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
	TotalNotModifiedCount  int64                `json:"total_not_modified_count"`
	QuotaBreakerSkipTotals map[string]int64     `json:"quota_breaker_skip_totals"`
	TotalBreakerSkipCount  int64                `json:"total_breaker_skip_count"`
	QuotaFloorSkipTotals   map[string]int64     `json:"quota_floor_skip_totals"`
	TotalFloorSkipCount    int64                `json:"total_floor_skip_count"`
	// IdentityTotals maps each identity to its calls per quota, plus "total".
	IdentityTotals map[string]map[string]int64 `json:"identity_totals"`
}

type gitHubUsageRowJSON struct {
	Label            string `json:"label"`
	Identity         string `json:"identity"`
	Quota            string `json:"quota"`
	CallCount        int64  `json:"call_count"`
	NotModifiedCount int64  `json:"not_modified_count"`
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
		TotalNotModifiedCount:  totals.notModified,
		QuotaBreakerSkipTotals: totals.breakerSkips.perQuota,
		TotalBreakerSkipCount:  totals.breakerSkips.all,
		QuotaFloorSkipTotals:   totals.floorSkips.perQuota,
		TotalFloorSkipCount:    totals.floorSkips.all,
		IdentityTotals:         make(map[string]map[string]int64, len(totals.identityCalls)),
	}
	for id, c := range totals.identityCalls {
		perQuota := make(map[string]int64, len(c.perQuota)+1)
		for q, n := range c.perQuota {
			perQuota[q] = n
		}
		perQuota["total"] = c.all
		out.IdentityTotals[id] = perQuota
	}
	for _, r := range rows {
		out.Rows = append(out.Rows, gitHubUsageRowJSON{
			Label:            r.GetLabel(),
			Identity:         gitHubUsageIdentity(r),
			Quota:            r.GetQuota(),
			CallCount:        r.GetCallCount(),
			NotModifiedCount: r.GetNotModifiedCount(),
			BreakerSkipCount: r.GetBreakerSkipCount(),
			FloorSkipCount:   r.GetFloorSkipCount(),
		})
	}
	return out
}
