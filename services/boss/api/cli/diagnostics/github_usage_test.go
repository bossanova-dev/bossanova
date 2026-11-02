package diagnostics

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/recurser/boss/api/cli/cliutil"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type gitHubUsageFake struct {
	repairFake
	resp *pb.GetGitHubUsageResponse
	reqs []*pb.GetGitHubUsageRequest
}

func (f *gitHubUsageFake) GetGitHubUsage(_ context.Context, req *pb.GetGitHubUsageRequest) (*pb.GetGitHubUsageResponse, error) {
	f.reqs = append(f.reqs, req)
	return f.resp, nil
}

func newGitHubUsageFake() *gitHubUsageFake {
	end := time.Date(2026, 11, 1, 11, 30, 0, 0, time.UTC)
	return &gitHubUsageFake{resp: &pb.GetGitHubUsageResponse{
		// Deliberately unsorted: the command orders by calls, descending.
		Rows: []*pb.GitHubUsageRow{
			{Label: "daemon:unattributed", Quota: "core", CallCount: 2},
			{Label: "daemon:poller", Quota: "graphql", CallCount: 40},
			{Label: "stage:finalize", Quota: "graphql", CallCount: 7},
			{Label: "daemon:poller", Quota: "core", CallCount: 11},
			// Rows the gh wrapper records for cron gates and agent sessions.
			{Label: "session:interactive", Quota: "core", CallCount: 3},
			{Label: "cron:bs-verify", Quota: "graphql", CallCount: 9},
		},
		WindowStartedAt: timestamppb.New(end.Add(-time.Hour)),
		WindowEndedAt:   timestamppb.New(end),
	}}
}

func runGitHubUsageCmd(t *testing.T, fake *gitHubUsageFake, args ...string) (string, error) {
	t.Helper()
	group := New(func(*cobra.Command) (Client, error) { return fake, nil }, Options{})
	cmd := group.gitHubUsageCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestGitHubUsagePrintsTableSortedByCalls_GitHubUsage_AC220201(t *testing.T) {
	fake := newGitHubUsageFake()
	text, err := runGitHubUsageCmd(t, fake)
	if err != nil {
		t.Fatalf("github-usage: %v", err)
	}
	if len(fake.reqs) != 1 || fake.reqs[0].GetWindowSeconds() != 3600 {
		t.Fatalf("requests = %v, want one with the default 3600s window", fake.reqs)
	}
	order := []string{"daemon:poller", "cron:bs-verify", "stage:finalize", "session:interactive", "daemon:unattributed"}
	last := -1
	for _, label := range order {
		i := strings.Index(text, label)
		if i < 0 || i < last {
			t.Fatalf("output = %q, want rows ordered by calls: %v", text, order)
		}
		last = i
	}
	for _, want := range []string{"LABEL", "QUOTA", "CALLS", "40", "(last 1h0m0s)", "Total: 72 calls (core 16, graphql 56)"} {
		if !strings.Contains(text, want) {
			t.Errorf("output = %q, want it to contain %q", text, want)
		}
	}
}

func TestGitHubUsageJSONCarriesRowsAndTotals_GitHubUsage_AC220201(t *testing.T) {
	fake := newGitHubUsageFake()
	text, err := runGitHubUsageCmd(t, fake, "--window", "30m", "--"+cliutil.JSONFlagName)
	if err != nil {
		t.Fatalf("github-usage --json: %v", err)
	}
	if fake.reqs[0].GetWindowSeconds() != 1800 {
		t.Fatalf("window_seconds = %d, want 1800", fake.reqs[0].GetWindowSeconds())
	}
	var got struct {
		WindowStartedAt string `json:"window_started_at"`
		WindowEndedAt   string `json:"window_ended_at"`
		Rows            []struct {
			Label     string `json:"label"`
			Quota     string `json:"quota"`
			CallCount int64  `json:"call_count"`
		} `json:"rows"`
		QuotaTotals    map[string]int64 `json:"quota_totals"`
		TotalCallCount int64            `json:"total_call_count"`
	}
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, text)
	}
	if got.WindowEndedAt != "2026-11-01T11:30:00Z" || got.WindowStartedAt != "2026-11-01T10:30:00Z" {
		t.Fatalf("window = [%s, %s]", got.WindowStartedAt, got.WindowEndedAt)
	}
	if len(got.Rows) != 6 || got.Rows[0].Label != "daemon:poller" || got.Rows[0].Quota != "graphql" || got.Rows[0].CallCount != 40 {
		t.Fatalf("rows = %+v, want the poller's 40 GraphQL calls first", got.Rows)
	}
	if got.QuotaTotals["core"] != 16 || got.QuotaTotals["graphql"] != 56 || got.TotalCallCount != 72 {
		t.Fatalf("totals = %v / %d, want core 16, graphql 56, 72 in all", got.QuotaTotals, got.TotalCallCount)
	}
	labels := map[string]bool{}
	for _, row := range got.Rows {
		labels[row.Label] = true
	}
	if !labels["cron:bs-verify"] || !labels["session:interactive"] {
		t.Fatalf("rows = %+v, want the cron: and session: rows the gh wrapper records", got.Rows)
	}
}

func TestGitHubUsageReportsAnEmptyWindow(t *testing.T) {
	fake := newGitHubUsageFake()
	fake.resp.Rows = nil
	text, err := runGitHubUsageCmd(t, fake)
	if err != nil {
		t.Fatalf("github-usage: %v", err)
	}
	if !strings.Contains(text, "No GitHub calls recorded in this window.") {
		t.Fatalf("output = %q, want the empty-window message", text)
	}
	text, err = runGitHubUsageCmd(t, fake, "--json")
	if err != nil {
		t.Fatalf("github-usage --json: %v", err)
	}
	if !strings.Contains(text, `"rows": []`) {
		t.Fatalf("output = %q, want an empty rows array, not null", text)
	}
}

func TestGitHubUsageRejectsSubSecondWindow(t *testing.T) {
	fake := newGitHubUsageFake()
	if _, err := runGitHubUsageCmd(t, fake, "--window", "500ms"); err == nil || !strings.Contains(err.Error(), "--window") {
		t.Fatalf("err = %v, want a --window error", err)
	}
	if len(fake.reqs) != 0 {
		t.Fatal("a rejected window must not reach the daemon")
	}
}

// newGitHubUsageSkipsFake is a quota-exhaustion window: the pollers were
// refused by the breaker and the floor, and one loop was only ever refused.
func newGitHubUsageSkipsFake() *gitHubUsageFake {
	fake := newGitHubUsageFake()
	fake.resp.Rows = []*pb.GitHubUsageRow{
		{Label: "daemon:display-poller", Quota: "graphql", CallCount: 0, FloorSkipCount: 6},
		{Label: "daemon:pr-poller", Quota: "graphql", CallCount: 12, BreakerSkipCount: 30, FloorSkipCount: 4},
		{Label: "daemon:callbacks", Quota: "graphql", CallCount: 12, BreakerSkipCount: 2},
		{Label: "plugin:repair", Quota: "core", CallCount: 5, FloorSkipCount: 1},
		{Label: "daemon:reconcile-pr-lookup", Quota: "core", CallCount: 0, BreakerSkipCount: 1},
	}
	return fake
}

func TestGitHubUsageTableShowsBreakerAndFloorSkips_GitHubUsage_AC222301(t *testing.T) {
	text, err := runGitHubUsageCmd(t, newGitHubUsageSkipsFake())
	if err != nil {
		t.Fatalf("github-usage: %v", err)
	}
	t.Logf("rendered table:\n%s", text)
	// Calls first, then skips: the pr-poller's 34 skips outrank the evaluator's
	// 2 at equal calls, and the refused-only loops still get rows.
	order := []string{"daemon:pr-poller", "daemon:callbacks", "plugin:repair", "daemon:display-poller", "daemon:reconcile-pr-lookup"}
	last := -1
	for _, label := range order {
		i := strings.Index(text, label)
		if i < 0 || i < last {
			t.Fatalf("output = %q, want rows ordered by calls, then skips: %v", text, order)
		}
		last = i
	}
	for _, want := range []string{
		"BREAKER SKIPS", "FLOOR SKIPS",
		"Total: 29 calls (core 5, graphql 24)",
		"Skipped: 33 by the breaker (core 1, graphql 32), 11 by the floor (core 1, graphql 10)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output = %q, want it to contain %q", text, want)
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "daemon:pr-poller") && !strings.Contains(strings.Join(strings.Fields(line), " "), "graphql 12 30 4") {
			t.Errorf("pr-poller row = %q, want calls 12, breaker skips 30, floor skips 4", line)
		}
	}
}

func TestGitHubUsageTableOmitsTheSkipsLineWhenNothingWasSkipped(t *testing.T) {
	text, err := runGitHubUsageCmd(t, newGitHubUsageFake())
	if err != nil {
		t.Fatalf("github-usage: %v", err)
	}
	if strings.Contains(text, "Skipped:") {
		t.Fatalf("output = %q, want no skips line for a window without skips", text)
	}
}

func TestGitHubUsageJSONCarriesSkipCounts_GitHubUsage_AC222301(t *testing.T) {
	text, err := runGitHubUsageCmd(t, newGitHubUsageSkipsFake(), "--"+cliutil.JSONFlagName)
	if err != nil {
		t.Fatalf("github-usage --json: %v", err)
	}
	var got struct {
		Rows []struct {
			Label            string `json:"label"`
			CallCount        int64  `json:"call_count"`
			BreakerSkipCount *int64 `json:"breaker_skip_count"`
			FloorSkipCount   *int64 `json:"floor_skip_count"`
		} `json:"rows"`
		BreakerSkipTotals     map[string]int64 `json:"quota_breaker_skip_totals"`
		FloorSkipTotals       map[string]int64 `json:"quota_floor_skip_totals"`
		TotalBreakerSkipCount int64            `json:"total_breaker_skip_count"`
		TotalFloorSkipCount   int64            `json:"total_floor_skip_count"`
	}
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, text)
	}
	if len(got.Rows) != 5 {
		t.Fatalf("rows = %+v, want 5", got.Rows)
	}
	for _, r := range got.Rows {
		if r.BreakerSkipCount == nil || r.FloorSkipCount == nil {
			t.Fatalf("row %q lacks a skip field: %s", r.Label, text)
		}
	}
	first := got.Rows[0]
	if first.Label != "daemon:pr-poller" || first.CallCount != 12 || *first.BreakerSkipCount != 30 || *first.FloorSkipCount != 4 {
		t.Fatalf("first row = %+v, want the pr-poller's 12 calls, 30 breaker and 4 floor skips", first)
	}
	if got.TotalBreakerSkipCount != 33 || got.TotalFloorSkipCount != 11 ||
		got.BreakerSkipTotals["graphql"] != 32 || got.FloorSkipTotals["core"] != 1 {
		t.Fatalf("skip totals = %+v / %+v, %d / %d", got.BreakerSkipTotals, got.FloorSkipTotals,
			got.TotalBreakerSkipCount, got.TotalFloorSkipCount)
	}
}
