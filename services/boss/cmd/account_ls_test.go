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

func TestFmtUsageStatusMapsEnumToShortToken(t *testing.T) {
	cases := map[string]string{
		"RATE_LIMIT_PLAN_STATUS_RATE_LIMITED": "limited",
		"RATE_LIMIT_PLAN_STATUS_WARNING":      "warn",
		"RATE_LIMIT_PLAN_STATUS_ACTIVE":       "ok",
		"RATE_LIMIT_PLAN_STATUS_UNSUPPORTED":  "-",
		"RATE_LIMIT_PLAN_STATUS_UNSPECIFIED":  "-",
		"limited":                             "limited",
		"ok":                                  "ok",
	}
	for in, want := range cases {
		u := &pb.UsageSnapshot{Status: in, FetchedAt: timestamppb.Now()}
		if got := fmtUsageStatus(u); got != want {
			t.Errorf("fmtUsageStatus(%q) = %q, want %q", in, got, want)
		}
	}
	if got := fmtUsageStatus(nil); got != "-" {
		t.Errorf("fmtUsageStatus(nil) = %q, want -", got)
	}
	if got := fmtUsageStatus(&pb.UsageSnapshot{Status: "ok"}); got != "-" {
		t.Errorf("never-probed (nil FetchedAt) = %q, want -", got)
	}
}

func TestFmtDurationShortBuckets(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{5 * time.Minute, "5m"},
		{3 * time.Hour, "3h"},
		{50 * time.Hour, "2d"},
		{-time.Second, "0s"},
	}
	for _, c := range cases {
		if got := fmtDurationShort(c.d); got != c.want {
			t.Errorf("fmtDurationShort(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestFmtCooldownRelative(t *testing.T) {
	if got := fmtCooldown(nil); got != "-" {
		t.Errorf("fmtCooldown(nil) = %q, want -", got)
	}
	past := timestamppb.New(time.Now().Add(-time.Hour))
	if got := fmtCooldown(past); got != "-" {
		t.Errorf("fmtCooldown(past) = %q, want - (already recovered)", got)
	}
	future := timestamppb.New(time.Now().Add(47 * time.Minute))
	got := fmtCooldown(future)
	if !strings.HasPrefix(got, "in ") || strings.Contains(got, "T") {
		t.Errorf("fmtCooldown(future) = %q, want relative like \"in 46m\" (no RFC3339)", got)
	}
}

func TestFmtUtilMergesResetCountdown(t *testing.T) {
	reset := timestamppb.New(time.Now().Add(3 * time.Hour))
	u := &pb.UsageSnapshot{Util_5H: 0.93, Status: "ok", FetchedAt: timestamppb.Now(), Reset_5H: reset}
	got := fmtUtil(u, u.GetUtil_5H(), u.GetReset_5H())
	if got != "93% (3h)" && got != "93% (2h)" {
		t.Fatalf("fmtUtil with reset = %q, want ~\"93%% (3h)\"", got)
	}
	u2 := &pb.UsageSnapshot{Util_7D: 0.5, Status: "ok", FetchedAt: timestamppb.Now()}
	if got := fmtUtil(u2, u2.GetUtil_7D(), u2.GetReset_7D()); got != "50%" {
		t.Fatalf("fmtUtil without reset = %q, want \"50%%\"", got)
	}
	if got := fmtUtil(nil, 0, nil); got != "-" {
		t.Fatalf("fmtUtil(nil) = %q, want -", got)
	}
}

func TestAccountLSTableOmitsPriorityColumn(t *testing.T) {
	ls := findLSSubcommand(t)
	var out bytes.Buffer
	ls.SetOut(&out)
	stub := &accountLSStub{accounts: []*pb.Account{
		{Id: "acct-1", Provider: "claude", Label: "work", Priority: 0},
	}}
	if err := accountLS(ls, stub); err != nil {
		t.Fatalf("accountLS: %v", err)
	}
	if strings.Contains(out.String(), "PRIORITY") {
		t.Fatalf("PRIORITY column should be gone from the table:\n%s", out.String())
	}
}

// TestAccountLSHintWhenProviderHasNoEligibleAccount pins BOS-327: the table path
// prints an actionable hint for any provider that has accounts but none eligible,
// naming `boss account update <id> --status active`.
func TestAccountLSHintWhenProviderHasNoEligibleAccount(t *testing.T) {
	ls := findLSSubcommand(t)
	var out bytes.Buffer
	ls.SetOut(&out)
	stub := &accountLSStub{accounts: []*pb.Account{
		{Id: "acct-1", Provider: "claude", Label: "work", Status: "disabled"},
		{Id: "acct-2", Provider: "claude", Label: "home", Status: "disabled"},
	}}
	if err := accountLS(ls, stub); err != nil {
		t.Fatalf("accountLS: %v", err)
	}
	got := out.String()
	for _, want := range []string{"claude", "no eligible account", "boss account update", "--status active"} {
		if !strings.Contains(got, want) {
			t.Fatalf("hint missing %q:\n%s", want, got)
		}
	}
}

// TestAccountLSHintWhenActiveAccountUnhealthy pins BOS-327: an account that is
// active but health=="failed" is NOT eligible (rotation cannot switch onto it),
// so the hint must still fire rather than staying silent on the nominal "active"
// row. This is the exact case the neutral "no eligible account" wording exists to
// diagnose accurately.
func TestAccountLSHintWhenActiveAccountUnhealthy(t *testing.T) {
	ls := findLSSubcommand(t)
	var out bytes.Buffer
	ls.SetOut(&out)
	stub := &accountLSStub{accounts: []*pb.Account{
		{Id: "acct-1", Provider: "claude", Label: "work", Status: "active", Health: "failed"},
	}}
	if err := accountLS(ls, stub); err != nil {
		t.Fatalf("accountLS: %v", err)
	}
	if !strings.Contains(out.String(), "no eligible account") {
		t.Fatalf("hint must fire for an active-but-unhealthy account:\n%s", out.String())
	}
}

// TestAccountLSNoHintWhenProviderHasEligibleAccount pins that the hint is silent
// when a provider has at least one eligible (active AND healthy) account.
func TestAccountLSNoHintWhenProviderHasEligibleAccount(t *testing.T) {
	ls := findLSSubcommand(t)
	var out bytes.Buffer
	ls.SetOut(&out)
	stub := &accountLSStub{accounts: []*pb.Account{
		{Id: "acct-1", Provider: "claude", Label: "work", Status: "disabled"},
		{Id: "acct-2", Provider: "claude", Label: "home", Status: "active", Health: "ok"},
	}}
	if err := accountLS(ls, stub); err != nil {
		t.Fatalf("accountLS: %v", err)
	}
	if strings.Contains(out.String(), "no eligible account") {
		t.Fatalf("hint should be silent when a provider has an eligible account:\n%s", out.String())
	}
}

// TestAccountLSJSONNeverPrintsHint pins that --json output stays a pure array —
// the hint never appears there.
func TestAccountLSJSONNeverPrintsHint(t *testing.T) {
	ls := findLSSubcommand(t)
	var out bytes.Buffer
	ls.SetOut(&out)
	if err := ls.Flags().Set("json", "true"); err != nil {
		t.Fatal(err)
	}
	stub := &accountLSStub{accounts: []*pb.Account{
		{Id: "acct-1", Provider: "claude", Label: "work", Status: "disabled"},
	}}
	if err := accountLS(ls, stub); err != nil {
		t.Fatalf("accountLS: %v", err)
	}
	got := out.String()
	if strings.Contains(got, "no eligible account") || strings.Contains(got, "Hint") {
		t.Fatalf("--json output must not contain the hint:\n%s", got)
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(got), &arr); err != nil {
		t.Fatalf("--json output is not a pure array: %v\n%s", err, got)
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
