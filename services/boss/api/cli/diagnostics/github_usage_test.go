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
