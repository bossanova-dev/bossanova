package main

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

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

// TestProbeUsageSnapshotSuspensionFailsHealth verifies that a probe error which
// confirms suspension (codes.PermissionDenied) proactively fails the account's
// health via MarkAccountSuspended, carrying the reason, rather than the fail-soft
// log-and-skip taken for ordinary probe errors.
func TestProbeUsageSnapshotSuspensionFailsHealth(t *testing.T) {
	const reason = "account suspended: organization disabled Claude subscription access"
	cache := &fakeDecisionUsageCache{
		probeErr: grpcstatus.Error(codes.PermissionDenied, reason),
	}

	snap, ok := probeUsageSnapshotForRotation(context.Background(), zerolog.Nop(), cache, cache, "susp-acct")
	if ok {
		t.Fatalf("ok = true, want false on suspension")
	}
	if snap.FetchedAt != nil {
		t.Fatalf("snapshot recorded on suspension: %+v", snap)
	}
	if cache.suspendCalls != 1 {
		t.Fatalf("suspendCalls = %d, want 1", cache.suspendCalls)
	}
	if cache.suspendID != "susp-acct" {
		t.Errorf("suspendID = %q, want susp-acct", cache.suspendID)
	}
	if cache.suspendReason != reason {
		t.Errorf("suspendReason = %q, want %q", cache.suspendReason, reason)
	}
	if cache.recordCalls != 0 {
		t.Errorf("recordCalls = %d, want 0 (no snapshot cached on suspension)", cache.recordCalls)
	}
}

// TestProbeUsageSnapshotTransientErrorDoesNotFailHealth verifies a generic probe
// error (not a confirmed suspension) keeps the conservative log-and-skip and
// never fails health — avoiding false positives from transient auth blips.
func TestProbeUsageSnapshotTransientErrorDoesNotFailHealth(t *testing.T) {
	cache := &fakeDecisionUsageCache{
		probeErr: grpcstatus.Error(codes.Unauthenticated, "auth_invalidated"),
	}
	if _, ok := probeUsageSnapshotForRotation(context.Background(), zerolog.Nop(), cache, cache, "acct"); ok {
		t.Fatalf("ok = true, want false on probe error")
	}
	if cache.suspendCalls != 0 {
		t.Fatalf("suspendCalls = %d, want 0 for non-suspension error", cache.suspendCalls)
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
