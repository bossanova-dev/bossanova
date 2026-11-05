package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"

	"github.com/recurser/bossd/internal/ghusage"
)

// seedLedger records calls stamped at the given offsets before now.
func seedLedger(t *testing.T, records map[time.Duration][]ghusage.Record) *ghusage.Ledger {
	t.Helper()
	var stamp time.Time
	l := ghusage.New(t.TempDir(), zerolog.Nop(), ghusage.WithNow(func() time.Time { return stamp }))
	now := time.Now()
	for ago, recs := range records {
		stamp = now.Add(-ago)
		for _, r := range recs {
			l.Record(r.Label, r.Quota, r.Identity)
		}
	}
	return l
}

func TestGetGitHubUsageReturnsLastHourPerLabelAndQuota(t *testing.T) {
	finalize := ghusage.Record{Label: ghusage.LabelFinalize, Quota: ghusage.QuotaGraphQL}
	poller := ghusage.Record{Label: ghusage.LabelPRPoller, Quota: ghusage.QuotaCore}
	cron := ghusage.Record{Label: "cron:verify", Quota: ghusage.QuotaGraphQL}
	ledger := seedLedger(t, map[time.Duration][]ghusage.Record{
		5 * time.Minute:  {finalize, finalize, poller},
		50 * time.Minute: {finalize, poller, poller, poller},
		90 * time.Minute: {cron, cron, cron, cron, cron}, // outside the default hour
	})
	srv := &Server{gitHubUsage: ledger}

	resp, err := srv.GetGitHubUsage(context.Background(), connect.NewRequest(&bossanovav1.GetGitHubUsageRequest{}))
	if err != nil {
		t.Fatalf("GetGitHubUsage: %v", err)
	}
	type row struct {
		label, quota string
		calls        int64
	}
	var got []row
	for _, r := range resp.Msg.GetRows() {
		got = append(got, row{r.GetLabel(), r.GetQuota(), r.GetCallCount()})
	}
	want := []row{
		{ghusage.LabelPRPoller, ghusage.QuotaCore, 4},
		{ghusage.LabelFinalize, ghusage.QuotaGraphQL, 3},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("rows = %+v, want %+v", got, want)
	}
	start, end := resp.Msg.GetWindowStartedAt().AsTime(), resp.Msg.GetWindowEndedAt().AsTime()
	if d := end.Sub(start); d != time.Hour {
		t.Fatalf("window = %v, want the default hour", d)
	}

	resp, err = srv.GetGitHubUsage(context.Background(), connect.NewRequest(&bossanovav1.GetGitHubUsageRequest{WindowSeconds: 7200}))
	if err != nil {
		t.Fatalf("GetGitHubUsage(2h): %v", err)
	}
	if rows := resp.Msg.GetRows(); len(rows) != 3 || rows[0].GetLabel() != "cron:verify" || rows[0].GetCallCount() != 5 {
		t.Fatalf("2h rows = %v, want the older cron calls included first", rows)
	}
}

func TestGetGitHubUsageRejectsWindowsTheLedgerCannotAnswer(t *testing.T) {
	srv := &Server{gitHubUsage: seedLedger(t, nil)}
	for _, secs := range []int64{-1, 7201} {
		_, err := srv.GetGitHubUsage(context.Background(), connect.NewRequest(&bossanovav1.GetGitHubUsageRequest{WindowSeconds: secs}))
		var cerr *connect.Error
		if !errors.As(err, &cerr) || cerr.Code() != connect.CodeInvalidArgument {
			t.Fatalf("window %d: err = %v, want InvalidArgument", secs, err)
		}
	}
	resp, err := srv.GetGitHubUsage(context.Background(), connect.NewRequest(&bossanovav1.GetGitHubUsageRequest{WindowSeconds: 60}))
	if err != nil || len(resp.Msg.GetRows()) != 0 {
		t.Fatalf("empty ledger: resp=%v err=%v, want no rows", resp, err)
	}
}

func TestGetGitHubUsageWithoutLedgerFailsPrecondition(t *testing.T) {
	_, err := (&Server{}).GetGitHubUsage(context.Background(), connect.NewRequest(&bossanovav1.GetGitHubUsageRequest{}))
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeFailedPrecondition {
		t.Fatalf("err = %v, want FailedPrecondition", err)
	}
}

// TestGetGitHubUsageReportsBreakerAndFloorSkips pins the skip columns: each
// row carries its breaker and floor skips beside its calls, and a label the
// gates only refused still gets a row, with no calls, after every row with
// calls.
func TestGetGitHubUsageReportsBreakerAndFloorSkips(t *testing.T) {
	now := time.Now()
	l := ghusage.New(t.TempDir(), zerolog.Nop(), ghusage.WithNow(func() time.Time { return now.Add(-time.Minute) }))
	l.Record(ghusage.LabelPRPoller, ghusage.QuotaGraphQL, "")
	l.RecordSkip(ghusage.LabelPRPoller, ghusage.QuotaGraphQL, "", ghusage.OutcomeBreaker)
	l.RecordSkip(ghusage.LabelPRPoller, ghusage.QuotaGraphQL, "", ghusage.OutcomeFloor)
	l.RecordSkip(ghusage.LabelDisplayPoller, ghusage.QuotaGraphQL, "", ghusage.OutcomeFloor)
	l.RecordSkip(ghusage.LabelDisplayPoller, ghusage.QuotaGraphQL, "", ghusage.OutcomeFloor)
	l.RecordSkip(ghusage.LabelDisplayPoller, ghusage.QuotaGraphQL, "", ghusage.OutcomeBreaker)
	srv := &Server{gitHubUsage: l}

	resp, err := srv.GetGitHubUsage(context.Background(), connect.NewRequest(&bossanovav1.GetGitHubUsageRequest{}))
	if err != nil {
		t.Fatalf("GetGitHubUsage: %v", err)
	}
	type row struct {
		label          string
		calls, breaker int64
		floor          int64
	}
	var got []row
	for _, r := range resp.Msg.GetRows() {
		got = append(got, row{r.GetLabel(), r.GetCallCount(), r.GetBreakerSkipCount(), r.GetFloorSkipCount()})
	}
	want := []row{
		{ghusage.LabelPRPoller, 1, 1, 1},
		{ghusage.LabelDisplayPoller, 0, 1, 2},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("rows = %+v, want %+v", got, want)
	}
}

// TestGetGitHubUsageReturnsEachRowsIdentity_GitHubUsage_AC230601: the same
// label and quota spent on an installation token and on the user login come
// back as two rows, each naming its identity, and a call recorded with no
// identity reports the user login.
func TestGetGitHubUsageReturnsEachRowsIdentity_GitHubUsage_AC230601(t *testing.T) {
	now := time.Now()
	l := ghusage.New(t.TempDir(), zerolog.Nop(), ghusage.WithNow(func() time.Time { return now.Add(-time.Minute) }))
	install := ghusage.InstallationIdentity(7)
	l.Record(ghusage.LabelPRState, ghusage.QuotaGraphQL, install)
	l.Record(ghusage.LabelPRState, ghusage.QuotaGraphQL, install)
	l.Record(ghusage.LabelPRState, ghusage.QuotaGraphQL, "")
	srv := &Server{gitHubUsage: l}

	resp, err := srv.GetGitHubUsage(context.Background(), connect.NewRequest(&bossanovav1.GetGitHubUsageRequest{}))
	if err != nil {
		t.Fatalf("GetGitHubUsage: %v", err)
	}
	type row struct {
		label, quota, identity string
		calls                  int64
	}
	var got []row
	for _, r := range resp.Msg.GetRows() {
		got = append(got, row{r.GetLabel(), r.GetQuota(), r.GetIdentity(), r.GetCallCount()})
	}
	want := []row{
		{ghusage.LabelPRState, ghusage.QuotaGraphQL, "installation:7", 2},
		{ghusage.LabelPRState, ghusage.QuotaGraphQL, ghusage.IdentityUser, 1},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("rows = %+v, want %+v", got, want)
	}
}

func TestGetGitHubUsageReportsNotModified_GitHubUsage_AC230701(t *testing.T) {
	l := ghusage.New(t.TempDir(), zerolog.Nop())
	l.Record(ghusage.LabelPRState, ghusage.QuotaCore, ghusage.IdentityUser)
	l.RecordNotModified(ghusage.LabelPRState, ghusage.QuotaCore, ghusage.IdentityUser)
	resp, err := (&Server{gitHubUsage: l}).GetGitHubUsage(context.Background(), connect.NewRequest(&bossanovav1.GetGitHubUsageRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	rows := resp.Msg.GetRows()
	if len(rows) != 1 || rows[0].GetCallCount() != 1 || rows[0].GetNotModifiedCount() != 1 {
		t.Fatalf("rows = %v, want one billed call and one not-modified response", rows)
	}
}
