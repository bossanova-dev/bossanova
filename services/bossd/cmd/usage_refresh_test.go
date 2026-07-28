package main

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/recurser/bossalib/models"
)

// TestRefreshActiveAccountUsageProbesEveryActiveAccount verifies the periodic
// usage refresher (BOS-320 hardening) probes and persists a fresh snapshot for
// every ACTIVE account — including cooling ones (BOS-584) — and skips only
// non-active ones. Fresh snapshots keep the util-aware default-account selector
// (account.Resolver.selectDefault) able to sideline capped accounts instead of
// degrading to LRU; probing cooling accounts too is what lets a wrong cooldown
// self-heal. A cooling account the probe still confirms limited keeps its bench.
func TestRefreshActiveAccountUsageProbesEveryActiveAccount(t *testing.T) {
	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Hour)
	fetched := time.Now().UTC()
	coolingAcct := &models.Account{ID: "c-cooling", Status: models.AccountStatusActive, CooldownUntil: &future}
	cache := &fakeDecisionUsageCache{
		probeSnap: models.UsageSnapshot{
			Util7d:    1,
			Status:    "RATE_LIMIT_PLAN_STATUS_RATE_LIMITED",
			FetchedAt: &fetched,
		},
		accounts: []*models.Account{
			{ID: "a-active", Status: models.AccountStatusActive},
			{ID: "b-active-cooldown-expired", Status: models.AccountStatusActive, CooldownUntil: &past},
			coolingAcct,
			{ID: "d-disabled", Status: models.AccountStatusDisabled},
		},
	}

	n := refreshActiveAccountUsage(context.Background(), zerolog.Nop(), cache, cache)

	// The three active accounts refresh; only d-disabled is skipped.
	if n != 3 {
		t.Fatalf("refreshed = %d, want 3 (every active account, cooling included)", n)
	}
	if cache.probeCalls != 3 || cache.recordCalls != 3 {
		t.Fatalf("calls probe=%d record=%d, want 3/3", cache.probeCalls, cache.recordCalls)
	}
	if len(cache.updates) != 0 {
		t.Fatalf("account updates = %+v, want none (probe still confirms the cap)", cache.updates)
	}
	if coolingAcct.CooldownUntil == nil {
		t.Fatal("c-cooling lost its cooldown even though the probe confirmed the cap")
	}
}

// TestRefreshActiveAccountUsageReconcilesContradictedCooldown is the BOS-584
// safety net: a cooling account whose fresh, available probe says it is NOT
// limited has cooldown_until cleared, so a wrong bench costs one refresh
// interval rather than a full hour. Nothing else in production ever cleared
// cooldown_until — expiry was purely wall-clock.
func TestRefreshActiveAccountUsageReconcilesContradictedCooldown(t *testing.T) {
	future := time.Now().Add(time.Hour)
	fetched := time.Now().UTC()
	cooling := &models.Account{ID: "yuki", Status: models.AccountStatusActive, CooldownUntil: &future}
	cache := &fakeDecisionUsageCache{
		// The exact snapshot observed during the incident.
		probeSnap: models.UsageSnapshot{
			Status:    "RATE_LIMIT_PLAN_STATUS_ACTIVE",
			Util5h:    0.11,
			Util7d:    0.02,
			FetchedAt: &fetched,
		},
		accounts: []*models.Account{cooling},
	}

	if n := refreshActiveAccountUsage(context.Background(), zerolog.Nop(), cache, cache); n != 1 {
		t.Fatalf("refreshed = %d, want 1", n)
	}
	if len(cache.updates) != 1 {
		t.Fatalf("account updates = %d, want exactly 1 cooldown clear", len(cache.updates))
	}
	up := cache.updates[0]
	if up.id != "yuki" {
		t.Fatalf("updated account = %q, want yuki", up.id)
	}
	if up.params.CooldownUntil == nil || *up.params.CooldownUntil != nil {
		t.Fatalf("CooldownUntil param = %v, want a set-to-NULL request", up.params.CooldownUntil)
	}
	if cooling.CooldownUntil != nil {
		t.Fatalf("cooldown_until = %v, want cleared", cooling.CooldownUntil)
	}
}

// TestRefreshActiveAccountUsageKeepsACooldownWrittenDuringTheProbe pins the
// reconciler's re-read guard. The account row is listed BEFORE a network probe,
// so a genuine, probe-confirmed 429 can bench it while that probe is in flight.
// Clearing off the stale listed row would make this sweep re-create the very bug
// it exists to undo, so a cooldown that changed under it is left alone.
func TestRefreshActiveAccountUsageKeepsACooldownWrittenDuringTheProbe(t *testing.T) {
	staleBench := time.Now().Add(10 * time.Minute)
	freshBench := time.Now().Add(3 * time.Hour)
	fetched := time.Now().UTC()

	// listed is what the sweep saw; the store now holds a NEWER bench, as though a
	// confirmed cap committed while the probe was in flight.
	listed := &models.Account{ID: "yuki", Status: models.AccountStatusActive, CooldownUntil: &staleBench}
	rebenched := &models.Account{ID: "yuki", Status: models.AccountStatusActive, CooldownUntil: &freshBench}
	cache := &fakeDecisionUsageCache{
		probeSnap: models.UsageSnapshot{
			Status:    "RATE_LIMIT_PLAN_STATUS_ACTIVE",
			Util5h:    0.11,
			FetchedAt: &fetched,
		},
		accounts: []*models.Account{listed},
		getByID:  map[string]*models.Account{"yuki": rebenched},
	}

	refreshActiveAccountUsage(context.Background(), zerolog.Nop(), cache, cache)

	if cache.getCalls != 1 {
		t.Fatalf("Get calls = %d, want 1 (the clear must re-read before writing)", cache.getCalls)
	}
	if len(cache.updates) != 0 {
		t.Fatalf("account updates = %+v, want none (the newer bench must survive)", cache.updates)
	}
	if rebenched.CooldownUntil == nil || !rebenched.CooldownUntil.Equal(freshBench) {
		t.Fatalf("cooldown_until = %v, want the newer bench %v", rebenched.CooldownUntil, freshBench)
	}
}

// TestRefreshActiveAccountUsageKeepsCooldownWhenReReadFails completes the
// reconciler's fail-closed contract: if the guard cannot see the row it is about
// to overwrite, it must not overwrite it. Clearing blind on a read failure would
// reintroduce exactly the clobber the re-read exists to prevent.
func TestRefreshActiveAccountUsageKeepsCooldownWhenReReadFails(t *testing.T) {
	future := time.Now().Add(time.Hour)
	fetched := time.Now().UTC()
	cooling := &models.Account{ID: "yuki", Status: models.AccountStatusActive, CooldownUntil: &future}
	cache := &fakeDecisionUsageCache{
		probeSnap: models.UsageSnapshot{
			Status:    "RATE_LIMIT_PLAN_STATUS_ACTIVE",
			Util5h:    0.11,
			FetchedAt: &fetched,
		},
		accounts: []*models.Account{cooling},
		getErr:   context.DeadlineExceeded,
	}

	if n := refreshActiveAccountUsage(context.Background(), zerolog.Nop(), cache, cache); n != 1 {
		t.Fatalf("refreshed = %d, want 1 (the probe itself still succeeded)", n)
	}
	if len(cache.updates) != 0 {
		t.Fatalf("account updates = %+v, want none (an unreadable row is never cleared)", cache.updates)
	}
	if cooling.CooldownUntil == nil || !cooling.CooldownUntil.Equal(future) {
		t.Fatalf("cooldown_until = %v, want unchanged %v", cooling.CooldownUntil, future)
	}
}

// nonClearingRecorder implements usageSnapshotRecorder + accountListStore but
// NOT accountCooldownClearer, so the reconciler's capability downcast fails. A
// compile-time pin guards the production store, but the runtime branch is what
// actually runs, and every other test injects a fake that satisfies everything.
type nonClearingRecorder struct{ accounts []*models.Account }

//nolint:unparam // the error return is required by usageSnapshotProber; this fake always succeeds so the reconciler reaches its downcast.
func (r *nonClearingRecorder) ProbeUsageSnapshot(_ context.Context, _ string) (models.UsageSnapshot, error) {
	fetched := time.Now().UTC()
	return models.UsageSnapshot{Status: "RATE_LIMIT_PLAN_STATUS_ACTIVE", Util5h: 0.11, FetchedAt: &fetched}, nil
}
func (r *nonClearingRecorder) RecordUsageProbe(context.Context, string, models.UsageSnapshot) error {
	return nil
}
func (r *nonClearingRecorder) MarkAccountSuspended(context.Context, string, string) error { return nil }

//nolint:unparam // the error return is required by accountListStore; this fake always succeeds.
func (r *nonClearingRecorder) List(context.Context) ([]*models.Account, error) {
	return r.accounts, nil
}

// TestRefreshActiveAccountUsageBailOutsKeepTheBench covers the reconciler's
// remaining refusals to clear. It is the only production path that ever clears
// cooldown_until, and its guard rails ARE the change — a bail-out that silently
// stopped bailing would hand back the bug. Each case must leave the bench alone.
func TestRefreshActiveAccountUsageBailOutsKeepTheBench(t *testing.T) {
	fetched := time.Now().UTC()
	contradicting := models.UsageSnapshot{
		Status: "RATE_LIMIT_PLAN_STATUS_ACTIVE", Util5h: 0.11, FetchedAt: &fetched,
	}

	t.Run("recorder that cannot clear leaves the bench in place", func(t *testing.T) {
		future := time.Now().Add(time.Hour)
		cooling := &models.Account{ID: "yuki", Status: models.AccountStatusActive, CooldownUntil: &future}
		rec := &nonClearingRecorder{accounts: []*models.Account{cooling}}

		if n := refreshActiveAccountUsage(context.Background(), zerolog.Nop(), rec, rec); n != 1 {
			t.Fatalf("refreshed = %d, want 1", n)
		}
		if cooling.CooldownUntil == nil || !cooling.CooldownUntil.Equal(future) {
			t.Fatalf("cooldown_until = %v, want unchanged %v", cooling.CooldownUntil, future)
		}
	})

	t.Run("deleted account is not a store fault and clears nothing", func(t *testing.T) {
		future := time.Now().Add(time.Hour)
		cooling := &models.Account{ID: "yuki", Status: models.AccountStatusActive, CooldownUntil: &future}
		cache := &fakeDecisionUsageCache{
			probeSnap: contradicting,
			accounts:  []*models.Account{cooling},
			getErr:    sql.ErrNoRows,
		}

		refreshActiveAccountUsage(context.Background(), zerolog.Nop(), cache, cache)

		if len(cache.updates) != 0 {
			t.Fatalf("account updates = %+v, want none (the row is gone)", cache.updates)
		}
	})

	t.Run("account that stopped cooling mid-probe clears nothing", func(t *testing.T) {
		future := time.Now().Add(time.Hour)
		cooling := &models.Account{ID: "yuki", Status: models.AccountStatusActive, CooldownUntil: &future}
		// The re-read finds the bench already gone — expired or cleared by another
		// writer. There is nothing left to undo, so no redundant write.
		uncooled := &models.Account{ID: "yuki", Status: models.AccountStatusActive}
		cache := &fakeDecisionUsageCache{
			probeSnap: contradicting,
			accounts:  []*models.Account{cooling},
			getByID:   map[string]*models.Account{"yuki": uncooled},
		}

		refreshActiveAccountUsage(context.Background(), zerolog.Nop(), cache, cache)

		if len(cache.updates) != 0 {
			t.Fatalf("account updates = %+v, want none (already not cooling)", cache.updates)
		}
	})

	t.Run("account missing from the store clears nothing", func(t *testing.T) {
		future := time.Now().Add(time.Hour)
		cooling := &models.Account{ID: "yuki", Status: models.AccountStatusActive, CooldownUntil: &future}
		cache := &fakeDecisionUsageCache{
			probeSnap: contradicting,
			accounts:  []*models.Account{cooling},
			getNil:    map[string]bool{"yuki": true},
		}

		refreshActiveAccountUsage(context.Background(), zerolog.Nop(), cache, cache)

		if len(cache.updates) != 0 {
			t.Fatalf("account updates = %+v, want none (no row to reconcile)", cache.updates)
		}
	})
}

// TestRefreshActiveAccountUsageKeepsCooldownWhenProbeIsNotAuthoritative pins the
// guard rails on the reconciliation sweep: a cooldown is cleared ONLY on a
// fresh, available probe that contradicts it. A probe error or an
// unsupported/unspecified probe leaves the bench exactly as it is — clearing on
// an unverifiable probe would be the mirror image of the bug this ticket fixes.
func TestRefreshActiveAccountUsageKeepsCooldownWhenProbeIsNotAuthoritative(t *testing.T) {
	fetched := time.Now().UTC()

	tests := []struct {
		name     string
		snap     models.UsageSnapshot
		probeErr error
	}{
		{name: "probe error keeps the cooldown", probeErr: context.DeadlineExceeded},
		{
			name: "unsupported probe keeps the cooldown",
			snap: models.UsageSnapshot{Status: "RATE_LIMIT_PLAN_STATUS_UNSUPPORTED", FetchedAt: &fetched},
		},
		{
			name: "unspecified probe keeps the cooldown",
			snap: models.UsageSnapshot{Status: "RATE_LIMIT_PLAN_STATUS_UNSPECIFIED", FetchedAt: &fetched},
		},
		{
			name: "still-limited probe keeps the cooldown",
			snap: models.UsageSnapshot{Status: "RATE_LIMIT_PLAN_STATUS_RATE_LIMITED", Util5h: 1, FetchedAt: &fetched},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			future := time.Now().Add(time.Hour)
			cooling := &models.Account{ID: "cooling", Status: models.AccountStatusActive, CooldownUntil: &future}
			cache := &fakeDecisionUsageCache{
				probeSnap: tt.snap,
				probeErr:  tt.probeErr,
				accounts:  []*models.Account{cooling},
			}

			refreshActiveAccountUsage(context.Background(), zerolog.Nop(), cache, cache)

			if len(cache.updates) != 0 {
				t.Fatalf("account updates = %+v, want none", cache.updates)
			}
			if cooling.CooldownUntil == nil || !cooling.CooldownUntil.Equal(future) {
				t.Fatalf("cooldown_until = %v, want unchanged %v", cooling.CooldownUntil, future)
			}
		})
	}
}

// TestRefreshActiveAccountUsageSurvivesCooldownClearFailure keeps the sweep
// fail-soft: an Update failure is logged and skipped, never fatal, and the
// remaining accounts still refresh.
func TestRefreshActiveAccountUsageSurvivesCooldownClearFailure(t *testing.T) {
	future := time.Now().Add(time.Hour)
	fetched := time.Now().UTC()
	cooling := &models.Account{ID: "cooling", Status: models.AccountStatusActive, CooldownUntil: &future}
	cache := &fakeDecisionUsageCache{
		probeSnap: models.UsageSnapshot{Status: "RATE_LIMIT_PLAN_STATUS_ACTIVE", FetchedAt: &fetched},
		accounts: []*models.Account{
			cooling,
			{ID: "healthy", Status: models.AccountStatusActive},
		},
		updateErr: context.DeadlineExceeded,
	}

	if n := refreshActiveAccountUsage(context.Background(), zerolog.Nop(), cache, cache); n != 2 {
		t.Fatalf("refreshed = %d, want 2 despite the failed cooldown clear", n)
	}
	if cooling.CooldownUntil == nil {
		t.Fatal("cooldown cleared locally despite the store rejecting the write")
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
