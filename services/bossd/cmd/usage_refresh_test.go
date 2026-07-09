package main

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/models"
)

// TestRefreshActiveAccountUsageProbesActiveNonCooling verifies the periodic
// usage refresher (BOS-320 hardening) probes and persists a fresh snapshot for
// every active, non-cooling account and skips cooling / non-active ones — so the
// util-aware default-account selector (account.Resolver.selectDefault) always
// has fresh data to sideline capped accounts, instead of degrading to LRU on
// stale snapshots and binding a new session to a fully-capped account.
func TestRefreshActiveAccountUsageProbesActiveNonCooling(t *testing.T) {
	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Hour)
	fetched := time.Now().UTC()
	cache := &fakeDecisionUsageCache{
		probeSnap: models.UsageSnapshot{
			Util7d:    1,
			Status:    "RATE_LIMIT_PLAN_STATUS_RATE_LIMITED",
			FetchedAt: &fetched,
		},
		accounts: []*models.Account{
			{ID: "a-active", Status: models.AccountStatusActive},
			{ID: "b-active-cooldown-expired", Status: models.AccountStatusActive, CooldownUntil: &past},
			{ID: "c-cooling", Status: models.AccountStatusActive, CooldownUntil: &future},
			{ID: "d-disabled", Status: models.AccountStatusDisabled},
		},
	}

	n := refreshActiveAccountUsage(context.Background(), zerolog.Nop(), cache, cache)

	// a-active + b-active-cooldown-expired refresh; c-cooling and d-disabled skip.
	if n != 2 {
		t.Fatalf("refreshed = %d, want 2 (active non-cooling only)", n)
	}
	if cache.probeCalls != 2 || cache.recordCalls != 2 {
		t.Fatalf("calls probe=%d record=%d, want 2/2", cache.probeCalls, cache.recordCalls)
	}
}

// TestRefreshActiveAccountUsageListErrorIsSoft verifies a list failure is
// fail-soft: no probes, zero refreshed, no panic.
func TestRefreshActiveAccountUsageListErrorIsSoft(t *testing.T) {
	cache := &fakeDecisionUsageCache{listErr: context.DeadlineExceeded}
	if n := refreshActiveAccountUsage(context.Background(), zerolog.Nop(), cache, cache); n != 0 {
		t.Fatalf("refreshed = %d, want 0 on list error", n)
	}
	if cache.probeCalls != 0 {
		t.Fatalf("probeCalls = %d, want 0 on list error", cache.probeCalls)
	}
}
