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

// TestAccountLSSurfacesInjectionFailedAccount pins BOS-973 on the CLI surface:
// an account whose credentials could not be materialized for a spawn is
// health=failed, so `boss account ls` prints `failed` in the HEALTH column and
// — because active-but-unhealthy is not eligible — fires the existing
// no-eligible-account hint for that provider. Before BOS-973 the same account
// showed HEALTH=ok while every session silently ran on the ambient CLI login.
func TestAccountLSSurfacesInjectionFailedAccount(t *testing.T) {
	ls := findLSSubcommand(t)
	var out bytes.Buffer
	ls.SetOut(&out)
	stub := &accountLSStub{accounts: []*pb.Account{{
		Id:       "acct-codex-2",
		Provider: "codex",
		Label:    "team-codex",
		Status:   "active",
		Health:   "failed",
		LastTestError: "credential injection failed: materialize codex account: " +
			"project codex base home: existing entry is not a symlink",
	}}}
	if err := accountLS(ls, stub); err != nil {
		t.Fatalf("accountLS: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "HEALTH") || !strings.Contains(got, "failed") {
		t.Fatalf("HEALTH column must show failed:\n%s", got)
	}
	if !strings.Contains(got, "no eligible account") {
		t.Fatalf("no-eligible-account hint must fire for the injection-failed provider:\n%s", got)
	}
	if !strings.Contains(got, "codex") {
		t.Fatalf("hint must name the codex provider:\n%s", got)
	}
}

// --- credential-check columns (BOS-1142) -----------------------------------

func TestFmtAuthCheckSeparatesNeverCheckedFromCheckedAndClean(t *testing.T) {
	if got := fmtAuthCheck(nil); got != "never" {
		t.Errorf("fmtAuthCheck(nil) = %q, want never", got)
	}
	if got := fmtAuthCheck(&pb.AuthCheck{}); got != "never" {
		t.Errorf("fmtAuthCheck(zero) = %q, want never", got)
	}
	if got := fmtAuthCheck(&pb.AuthCheck{Outcome: "healthy", CheckedAt: timestamppb.Now()}); got != "ok" {
		t.Errorf("fmtAuthCheck(healthy) = %q, want ok", got)
	}
	// The whole point: the two must not render the same string.
	if fmtAuthCheck(nil) == fmtAuthCheck(&pb.AuthCheck{Outcome: "healthy"}) {
		t.Fatal("never-checked and checked-and-clean render identically")
	}
}

// TestFmtAuthCheckKeepsRefreshChainUnprovenWithinTheCheckBudget pins the third,
// previously UNPINNED mirror of the credential-check label (BOS-1174). The TUI
// list and the web pill both had tests; the `boss account ls` renderer did not,
// which is exactly why it shipped as the generic default arm rendering
// "refresh_chain_unproven:refresh_not_observed" — 43 columns into a CHECK column
// budgeted at 24 (see the rebuildTable column spec), so the truncation ate the
// identifying half and every unproven row read as a smear of "refresh_chain_un".
//
// The assertion is deliberately BOTH halves: the exact string, and that it fits.
// Pinning only the string would let a later widening of the column silently
// reintroduce a pair nobody can read at the default width.
func TestFmtAuthCheckKeepsRefreshChainUnprovenWithinTheCheckBudget(t *testing.T) {
	const checkColumnBudget = 24 // rebuildTable's CHECK column max width.

	got := fmtAuthCheck(&pb.AuthCheck{
		Outcome:      "refresh_chain_unproven",
		FailureClass: "refresh_not_observed",
	})
	if got != "refresh_chain_unproven" {
		t.Fatalf("fmtAuthCheck(refresh_chain_unproven) = %q, want the bare outcome token", got)
	}
	if len(got) > checkColumnBudget {
		t.Fatalf("fmtAuthCheck(refresh_chain_unproven) = %q (%d cols), want <= %d — it will be truncated in the CHECK column",
			got, len(got), checkColumnBudget)
	}
	// The pair the generic default arm would have produced is what must never
	// come back; assert it is genuinely over budget so the guard above is not
	// vacuous.
	if pair := "refresh_chain_unproven:refresh_not_observed"; len(pair) <= checkColumnBudget {
		t.Fatalf("the outcome:class pair is %d cols, which no longer exceeds the %d-col budget this test guards", len(pair), checkColumnBudget)
	}
}

// TestFmtAuthCheckKeepsUnrecognizedOutcomesQualified pins the other side of the
// same switch: refresh_chain_unproven drops its class BY DESIGN, so the default
// arm's outcome:class form has to stay proven for everything else. Without this
// the budget test above could be satisfied by a renderer that silently stopped
// qualifying every outcome.
func TestFmtAuthCheckKeepsUnrecognizedOutcomesQualified(t *testing.T) {
	for _, tc := range []struct {
		outcome string
		class   string
		want    string
	}{
		{"unavailable", "runner_unavailable", "unavailable:runner_unavailable"},
		{"transient", "transient_provider", "transient:transient_provider"},
		{"some_future_outcome", "", "some_future_outcome"},
	} {
		got := fmtAuthCheck(&pb.AuthCheck{Outcome: tc.outcome, FailureClass: tc.class})
		if got != tc.want {
			t.Errorf("fmtAuthCheck(%q/%q) = %q, want %q", tc.outcome, tc.class, got, tc.want)
		}
	}
}

func TestFmtAuthCheckCarriesTheFailureClass(t *testing.T) {
	got := fmtAuthCheck(&pb.AuthCheck{Outcome: "auth_invalid", FailureClass: "auth_invalidated"})
	if got != "failed:auth_invalidated" {
		t.Fatalf("fmtAuthCheck(auth_invalid) = %q, want failed:auth_invalidated", got)
	}
	if got := fmtAuthCheck(&pb.AuthCheck{Outcome: "auth_invalid"}); got != "failed" {
		t.Fatalf("fmtAuthCheck(auth_invalid, no class) = %q, want failed", got)
	}
	// Transient is not a credential failure and must not read as one.
	if got := fmtAuthCheck(&pb.AuthCheck{Outcome: "transient", FailureClass: "rate_limited"}); got != "transient:rate_limited" {
		t.Fatalf("fmtAuthCheck(transient) = %q, want transient:rate_limited", got)
	}
	if strings.HasPrefix(fmtAuthCheck(&pb.AuthCheck{Outcome: "unavailable"}), "failed") {
		t.Fatal("an unavailable check must not render as a failure")
	}
}

func TestFmtAuthCheckAgeHasNoAgeForACheckThatNeverRan(t *testing.T) {
	if got := fmtAuthCheckAge(&pb.AuthCheck{Outcome: "healthy"}); got != "-" {
		t.Fatalf("fmtAuthCheckAge without checked_at = %q, want -", got)
	}
	got := fmtAuthCheckAge(&pb.AuthCheck{CheckedAt: timestamppb.New(time.Now().Add(-2 * time.Hour))})
	if got == "-" || got == "" {
		t.Fatalf("fmtAuthCheckAge with checked_at = %q, want an age", got)
	}
}

func TestAccountLSRendersTheCredentialCheckColumns(t *testing.T) {
	ls := findLSSubcommand(t)
	var out bytes.Buffer
	ls.SetOut(&out)
	stub := &accountLSStub{accounts: []*pb.Account{
		{
			Id: "acct-codex-1", Provider: "codex", Label: "codex-one",
			Status: "active", Health: "failed",
			AuthCheck: &pb.AuthCheck{
				Outcome: "auth_invalid", FailureClass: "auth_invalidated",
				CheckedAt: timestamppb.New(time.Now().Add(-30 * time.Minute)),
			},
		},
		{Id: "acct-codex-2", Provider: "codex", Label: "codex-two", Status: "active", Health: "ok"},
	}}
	if err := accountLS(ls, stub); err != nil {
		t.Fatalf("accountLS: %v", err)
	}
	got := out.String()
	for _, want := range []string{"CHECK", "CHECKED", "failed:auth_invalidated", "never"} {
		if !strings.Contains(got, want) {
			t.Errorf("ls output missing %q:\n%s", want, got)
		}
	}
}

func TestAccountLSJSONCarriesTheCredentialCheckFields(t *testing.T) {
	ls := findLSSubcommand(t)
	var out bytes.Buffer
	ls.SetOut(&out)
	if err := ls.Flags().Set("json", "true"); err != nil {
		t.Fatal(err)
	}
	checkedAt := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	stub := &accountLSStub{accounts: []*pb.Account{
		{
			Id: "acct-codex-1", Provider: "codex",
			AuthCheck: &pb.AuthCheck{
				Outcome: "auth_invalid", FailureClass: "auth_invalidated",
				CheckedAt: timestamppb.New(checkedAt),
			},
		},
		{Id: "acct-codex-2", Provider: "codex"},
	}}
	if err := accountLS(ls, stub); err != nil {
		t.Fatalf("accountLS: %v", err)
	}
	var rows []accountJSON
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out.String())
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].AuthCheckOutcome != "auth_invalid" || rows[0].AuthCheckFailureClass != "auth_invalidated" {
		t.Fatalf("row 0 check state = %+v", rows[0])
	}
	if rows[0].AuthCheckedAt == "" {
		t.Fatal("row 0 has no auth_checked_at")
	}
	// Never checked: an empty outcome AND an empty timestamp, so a consumer can
	// tell "no check" from "check found nothing".
	if rows[1].AuthCheckOutcome != "" || rows[1].AuthCheckedAt != "" {
		t.Fatalf("row 1 should be never-checked, got %+v", rows[1])
	}
}
