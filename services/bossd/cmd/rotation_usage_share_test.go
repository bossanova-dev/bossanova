package main

import (
	"context"
	"testing"
	"time"

	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/rotation"
	"github.com/rs/zerolog"
)

func TestUsageRefreshSharedPolling_Accounts_AC15(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		known, poller, fresh, throttle bool
		want                           int
	}{
		{"poller", true, true, true, false, 1},
		{"fresh nonpoller", true, false, true, false, 0},
		{"stale fallback", true, false, false, false, 1},
		{"shared throttle", true, false, false, true, 0},
		{"unsynced", false, false, true, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			fetched := now.Add(-time.Hour)
			if tc.fresh {
				fetched = now.Add(-time.Minute)
			}
			cache := &fakeDecisionUsageCache{accounts: []*domain.AgentAccount{{ID: "account", Status: domain.AgentAccountStatusActive, Usage: &domain.UsageSnapshot{FetchedAt: &fetched}}}, probeSnap: domain.UsageSnapshot{FetchedAt: &now}}
			reports := 0
			share := rotation.NewUsageShare(func(context.Context, string, *domain.UsageSnapshot, time.Time) { reports++ })
			if tc.known {
				until := time.Time{}
				if tc.throttle {
					until = now.Add(time.Hour)
				}
				share.Set("account", tc.poller, until)
			}
			got := refreshActiveAccountUsage(context.Background(), zerolog.Nop(), cache, cache, nil, share)
			if got != tc.want || cache.probeCalls != tc.want {
				t.Fatalf("refreshed=%d probes=%d want %d", got, cache.probeCalls, tc.want)
			}
			if tc.poller && reports != 1 {
				t.Fatalf("reports=%d want 1", reports)
			}
		})
	}
}

func TestProbeBackoffAIMD_Accounts_AC15(t *testing.T) {
	cache := &fakeDecisionUsageCache{probeErr: throttleProbeErr(t, 0)}
	backoffs := map[string]probeBackoff{}
	for _, want := range []time.Duration{6 * time.Minute, 9 * time.Minute, 810 * time.Second, 1215 * time.Second, 30 * time.Minute, 30 * time.Minute} {
		probeUsageSnapshotForRotation(context.Background(), zerolog.Nop(), cache, cache, "account", backoffs)
		if got := backoffs["account"].interval; got != want {
			t.Fatalf("interval=%v want %v", got, want)
		}
	}
	cache.probeErr = throttleProbeErr(t, 25*time.Minute)
	backoffs["account"] = probeBackoff{interval: 6 * time.Minute}
	probeUsageSnapshotForRotation(context.Background(), zerolog.Nop(), cache, cache, "account", backoffs)
	if got := backoffs["account"].interval; got != 25*time.Minute {
		t.Fatalf("retry-after interval=%v", got)
	}
	now := time.Now()
	cache.probeErr = nil
	cache.probeSnap = domain.UsageSnapshot{FetchedAt: &now}
	for want := 22 * time.Minute; want > 0; want -= 3 * time.Minute {
		probeUsageSnapshotForRotation(context.Background(), zerolog.Nop(), cache, cache, "account", backoffs)
		if got := backoffs["account"].interval; got != want {
			t.Fatalf("decreased interval=%v want %v", got, want)
		}
	}
	probeUsageSnapshotForRotation(context.Background(), zerolog.Nop(), cache, cache, "account", backoffs)
	if _, ok := backoffs["account"]; ok {
		t.Fatal("success did not remove exhausted backoff")
	}
}

func TestCandidateUsageReusesFreshSnapshot_Accounts_AC15(t *testing.T) {
	for _, age := range []time.Duration{time.Minute, time.Hour} {
		t.Run(age.String(), func(t *testing.T) {
			now := time.Now()
			fetched := now.Add(-age)
			cache := &fakeDecisionUsageCache{accounts: []*domain.AgentAccount{{ID: "candidate", Provider: domain.AgentAccountProviderClaude, Status: domain.AgentAccountStatusActive, Health: domain.AgentAccountHealthOK, Usage: &domain.UsageSnapshot{FetchedAt: &fetched, Util5h: .2}}}, probeSnap: domain.UsageSnapshot{FetchedAt: &now, Util5h: .4}}
			usage := probeCandidateUsageForRotationSignal(context.Background(), zerolog.Nop(), cache, cache, rotation.Signal{Provider: "claude"})
			wantCalls := 1
			wantUtil := .4
			if age < probeThrottleFloor {
				wantCalls = 0
				wantUtil = .2
			}
			if cache.probeCalls != wantCalls || usage["candidate"].Util5h != wantUtil {
				t.Fatalf("probes=%d usage=%v", cache.probeCalls, usage)
			}
		})
	}
}

func TestUsageRefreshReportsThrottleAndRetainsGrowthAfterExpiry_Accounts_AC15(t *testing.T) {
	cache := &fakeDecisionUsageCache{accounts: []*domain.AgentAccount{{ID: "poller", Status: domain.AgentAccountStatusActive}}, probeErr: throttleProbeErr(t, 0)}
	var reported time.Time
	share := rotation.NewUsageShare(func(_ context.Context, id string, snap *domain.UsageSnapshot, until time.Time) {
		if id != "poller" || snap != nil {
			t.Fatalf("unexpected report id=%s snap=%v", id, snap)
		}
		reported = until
	})
	share.Set("poller", true, time.Time{})
	backoffs := map[string]probeBackoff{}
	refreshActiveAccountUsage(context.Background(), zerolog.Nop(), cache, cache, backoffs, share)
	if !reported.Equal(backoffs["poller"].until) || !reported.After(time.Now()) {
		t.Fatalf("throttle report=%v backoff=%v", reported, backoffs)
	}
	state := backoffs["poller"]
	state.until = time.Now().Add(-time.Second)
	backoffs["poller"] = state
	refreshActiveAccountUsage(context.Background(), zerolog.Nop(), cache, cache, backoffs, share)
	if backoffs["poller"].interval != 9*time.Minute || cache.probeCalls != 2 {
		t.Fatalf("backoff=%v probes=%d", backoffs, cache.probeCalls)
	}
}
