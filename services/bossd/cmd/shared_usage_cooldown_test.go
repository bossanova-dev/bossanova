package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossd/internal/domain"
)

// A daemon that is not the usage poller only ever sees relayed readings, so a
// relayed reading that contradicts its cooldown must clear it: after a weekly
// limit reset the account was otherwise unselectable until the old reset time.
func TestSharedUsageReadingReconcilesCooldown(t *testing.T) {
	now := time.Now()
	fresh := now.Add(-time.Minute)
	stale := now.Add(-time.Hour)
	available := domain.UsageSnapshot{Status: "RATE_LIMIT_PLAN_STATUS_ACTIVE", FetchedAt: &fresh}
	capped := domain.UsageSnapshot{Status: "RATE_LIMIT_PLAN_STATUS_RATE_LIMITED", Util7d: 1, FetchedAt: &fresh}
	staleAvailable := domain.UsageSnapshot{Status: "RATE_LIMIT_PLAN_STATUS_ACTIVE", FetchedAt: &stale}

	for _, tc := range []struct {
		name        string
		snap        domain.UsageSnapshot
		wantCleared bool
	}{
		{"fresh available reading clears the bench", available, true},
		{"capped reading keeps the bench", capped, false},
		{"stale relayed reading keeps the bench", staleAvailable, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bench := now.Add(4 * 24 * time.Hour)
			acct := &domain.AgentAccount{ID: "madverts", Status: domain.AgentAccountStatusActive, CooldownUntil: &bench}
			store := &fakeDecisionUsageCache{accounts: []*domain.AgentAccount{acct}}
			recorder := sharedUsageCooldownRecorder{store: store, logger: zerolog.Nop(), staleness: 30 * time.Minute, now: func() time.Time { return now }}

			if err := recorder.RecordUsageProbe(context.Background(), "madverts", tc.snap); err != nil {
				t.Fatalf("RecordUsageProbe: %v", err)
			}
			if store.recordCalls != 1 {
				t.Fatalf("recorded %d readings, want 1", store.recordCalls)
			}
			if cleared := acct.CooldownUntil == nil; cleared != tc.wantCleared {
				t.Fatalf("cooldown cleared = %v, want %v (cooldown_until=%v)", cleared, tc.wantCleared, acct.CooldownUntil)
			}
		})
	}
}

func TestSharedUsageReadingRecordFailureLeavesCooldown(t *testing.T) {
	now := time.Now()
	bench := now.Add(time.Hour)
	acct := &domain.AgentAccount{ID: "madverts", Status: domain.AgentAccountStatusActive, CooldownUntil: &bench}
	store := &fakeDecisionUsageCache{accounts: []*domain.AgentAccount{acct}, recordErr: errors.New("disk full")}
	recorder := sharedUsageCooldownRecorder{store: store, logger: zerolog.Nop(), staleness: 30 * time.Minute, now: func() time.Time { return now }}

	err := recorder.RecordUsageProbe(context.Background(), "madverts", domain.UsageSnapshot{Status: "RATE_LIMIT_PLAN_STATUS_ACTIVE", FetchedAt: &now})
	if err == nil {
		t.Fatal("RecordUsageProbe error = nil, want the store's write error")
	}
	if acct.CooldownUntil == nil {
		t.Fatal("cooldown cleared although the reading was never recorded")
	}
}
