package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type accountLSStub struct {
	provider string
	refresh  bool
	accounts []*pb.Account
	err      error
}

func (s *accountLSStub) ListAccounts(_ context.Context, provider string, refresh bool) ([]*pb.Account, error) {
	s.provider = provider
	s.refresh = refresh
	return s.accounts, s.err
}

func findLSSubcommand(t *testing.T) *cobra.Command {
	t.Helper()
	account := accountCmd()
	for _, c := range account.Commands() {
		if c.Name() == "ls" {
			return c
		}
	}
	t.Fatalf("account command has no `ls` subcommand")
	return nil
}

func TestAccountLSCommandShape(t *testing.T) {
	ls := findLSSubcommand(t)
	for _, name := range []string{"provider", "json", "refresh"} {
		if ls.Flags().Lookup(name) == nil {
			t.Errorf("expected --%s flag on `account ls`", name)
		}
	}
}

func TestAccountLSJSONIncludesUsageAndForwardsRefresh(t *testing.T) {
	ls := findLSSubcommand(t)
	var out bytes.Buffer
	ls.SetOut(&out)
	if err := ls.Flags().Set("provider", "claude"); err != nil {
		t.Fatal(err)
	}
	if err := ls.Flags().Set("json", "true"); err != nil {
		t.Fatal(err)
	}
	if err := ls.Flags().Set("refresh", "true"); err != nil {
		t.Fatal(err)
	}
	fetched := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	stub := &accountLSStub{accounts: []*pb.Account{{
		Id: "acct-1", Provider: "claude", Label: "work",
		Usage: &pb.UsageSnapshot{
			Util_5H: 0.75, Util_7D: 0.25,
			Status: "limited", PlanTier: "pro",
			FetchedAt: timestamppb.New(fetched),
		},
	}}}

	if err := accountLS(ls, stub); err != nil {
		t.Fatalf("accountLS: %v", err)
	}
	if stub.provider != "claude" || !stub.refresh {
		t.Fatalf("forwarded provider/refresh = %q/%v, want claude/true", stub.provider, stub.refresh)
	}
	var got []map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal output %q: %v", out.String(), err)
	}
	row := got[0]
	if row["util_5h"] != 0.75 || row["util_7d"] != 0.25 || row["usage_status"] != "limited" || row["plan_tier"] != "pro" {
		t.Fatalf("usage fields missing from json: %#v", row)
	}
	if row["usage_fetched_at"] != fetched.Format(time.RFC3339) {
		t.Fatalf("usage_fetched_at = %#v, want %s", row["usage_fetched_at"], fetched.Format(time.RFC3339))
	}
}

func TestAccountLSTableShowsUsageAndNeverProbedDash(t *testing.T) {
	ls := findLSSubcommand(t)
	var out bytes.Buffer
	ls.SetOut(&out)
	stub := &accountLSStub{accounts: []*pb.Account{
		{
			Id: "acct-1", Provider: "claude", Label: "work",
			Usage: &pb.UsageSnapshot{Util_5H: 0.73, Util_7D: 0.10, Status: "ok", FetchedAt: timestamppb.Now()},
		},
		{Id: "acct-2", Provider: "codex", Label: "empty"},
		{
			Id: "acct-3", Provider: "claude", Label: "unsupported",
			Usage: &pb.UsageSnapshot{
				Status:    "RATE_LIMIT_PLAN_STATUS_UNSUPPORTED",
				FetchedAt: timestamppb.Now(),
			},
		},
		{
			Id: "acct-4", Provider: "claude", Label: "unspecified",
			Usage: &pb.UsageSnapshot{
				Status:    "RATE_LIMIT_PLAN_STATUS_UNSPECIFIED",
				FetchedAt: timestamppb.Now(),
			},
		},
	}}

	if err := accountLS(ls, stub); err != nil {
		t.Fatalf("accountLS: %v", err)
	}
	got := out.String()
	for _, want := range []string{"UTIL5H", "UTIL7D", "USAGE", "AGE", "73%", "10%", "ok"} {
		if !strings.Contains(got, want) {
			t.Fatalf("table output missing %q: %s", want, got)
		}
	}
	if !strings.Contains(got, "acct-2") || !strings.Contains(got, "-") {
		t.Fatalf("never-probed row should render dashes: %s", got)
	}
	var unsupportedRow string
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "acct-3") {
			unsupportedRow = line
			break
		}
	}
	if unsupportedRow == "" {
		t.Fatalf("unsupported account row missing: %s", got)
	}
	if strings.Contains(unsupportedRow, "0%") {
		t.Fatalf("unsupported usage should render util dashes, not 0%%: %s", got)
	}
	var unspecifiedRow string
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "acct-4") {
			unspecifiedRow = line
			break
		}
	}
	if unspecifiedRow == "" {
		t.Fatalf("unspecified account row missing: %s", got)
	}
	if strings.Contains(unspecifiedRow, "0%") {
		t.Fatalf("unspecified usage should render util dashes, not 0%%: %s", got)
	}
}
