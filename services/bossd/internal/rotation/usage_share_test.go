package rotation

import (
	"context"
	"testing"
	"time"

	"github.com/recurser/bossd/internal/domain"
)

func TestUsageShareProbeRules_Accounts_AC15(t *testing.T) {
	now := time.Now()
	fresh := now.Add(-time.Minute)
	stale := now.Add(-time.Hour)
	share := NewUsageShare(nil)
	for _, tc := range []struct {
		name          string
		known, poller bool
		fetched       *time.Time
		throttle      time.Time
		want          bool
	}{
		{name: "unknown retains local polling", fetched: &fresh, want: true},
		{name: "poller refreshes fresh snapshot", known: true, poller: true, fetched: &fresh, want: true},
		{name: "nonpoller reuses fresh snapshot", known: true, fetched: &fresh},
		{name: "nonpoller falls back for stale snapshot", known: true, fetched: &stale, want: true},
		{name: "nonpoller respects shared throttle", known: true, fetched: &stale, throttle: now.Add(time.Minute)},
		{name: "nonpoller falls back without snapshot", known: true, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.known {
				share.Set(tc.name, tc.poller, tc.throttle)
			}
			if got := share.ShouldProbe(tc.name, &domain.UsageSnapshot{FetchedAt: tc.fetched}, now, 30*time.Minute); got != tc.want {
				t.Fatalf("ShouldProbe=%v want %v", got, tc.want)
			}
		})
	}
}

func TestUsageShareReportsOnlyPoller_Accounts_AC15(t *testing.T) {
	calls := 0
	share := NewUsageShare(func(context.Context, string, *domain.UsageSnapshot, time.Time) { calls++ })
	share.Report(context.Background(), "unknown", nil, time.Time{})
	share.Set("member", false, time.Time{})
	share.Report(context.Background(), "member", nil, time.Time{})
	share.Set("member", true, time.Time{})
	share.Report(context.Background(), "member", nil, time.Now())
	if calls != 1 {
		t.Fatalf("reports=%d want 1", calls)
	}
}

func TestUsageShareConfiguredStaleness_Accounts_AC15(t *testing.T) {
	now := time.Now()
	fetched := now.Add(-10 * time.Minute)
	share := NewUsageShare(nil, 5*time.Minute)
	share.Set("member", false, time.Time{})
	if !share.ShouldProbe("member", &domain.UsageSnapshot{FetchedAt: &fetched}, now, share.StalenessWindow()) {
		t.Fatal("custom staleness did not allow fallback")
	}
}
