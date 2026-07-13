package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/rotation"
)

type fakeDecisionUsageCache struct {
	probeCalls    int
	recordCalls   int
	probeSnap     models.UsageSnapshot
	probeErr      error
	recordErr     error
	accounts      []*models.Account
	listErr       error
	suspendCalls  int
	suspendID     string
	suspendReason string
	suspendErr    error
}

func (f *fakeDecisionUsageCache) ProbeUsageSnapshot(_ context.Context, _ string) (models.UsageSnapshot, error) {
	f.probeCalls++
	if f.probeErr != nil {
		return models.UsageSnapshot{}, f.probeErr
	}
	return f.probeSnap, nil
}

func (f *fakeDecisionUsageCache) RecordUsageProbe(_ context.Context, _ string, _ models.UsageSnapshot) error {
	f.recordCalls++
	if f.recordErr != nil {
		return f.recordErr
	}
	return nil
}

func (f *fakeDecisionUsageCache) List(context.Context) ([]*models.Account, error) {
	return f.accounts, f.listErr
}

func (f *fakeDecisionUsageCache) MarkAccountSuspended(_ context.Context, id string, reason string) error {
	f.suspendCalls++
	f.suspendID = id
	f.suspendReason = reason
	return f.suspendErr
}

func probedResetForTest(cache *fakeDecisionUsageCache) *time.Time {
	snap, ok := probeUsageSnapshotForRotation(context.Background(), zerolog.Nop(), cache, cache, "acct-1")
	if !ok || !rotation.UsageSnapshotConfirmsLimited(snap) {
		return nil
	}
	return rotation.UsageSnapshotResetAt(snap)
}

func TestCacheUsageProbeForRotationDecisionWritesBoundAccount(t *testing.T) {
	reset := time.Now().Add(5 * time.Hour).UTC().Truncate(time.Millisecond)
	fetched := time.Now().UTC().Truncate(time.Millisecond)
	cache := &fakeDecisionUsageCache{
		probeSnap: models.UsageSnapshot{
			Util5h:    1,
			Reset5h:   &reset,
			Status:    "RATE_LIMIT_PLAN_STATUS_RATE_LIMITED",
			FetchedAt: &fetched,
		},
	}

	got := probedResetForTest(cache)

	if cache.probeCalls != 1 || cache.recordCalls != 1 {
		t.Fatalf("calls probe=%d record=%d, want 1/1", cache.probeCalls, cache.recordCalls)
	}
	if got == nil || !got.Equal(reset) {
		t.Fatalf("reset = %v, want probed reset %v", got, reset)
	}
}

func TestCacheUsageProbeForRotationDecisionIgnoresBannerReset(t *testing.T) {
	bannerReset := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Millisecond)
	probeReset := time.Now().Add(5 * time.Hour).UTC().Truncate(time.Millisecond)
	fetched := time.Now().UTC().Truncate(time.Millisecond)
	cache := &fakeDecisionUsageCache{
		probeSnap: models.UsageSnapshot{
			Util5h:    1,
			Reset5h:   &probeReset,
			Status:    "RATE_LIMIT_PLAN_STATUS_RATE_LIMITED",
			FetchedAt: &fetched,
		},
	}

	_ = bannerReset
	got := probedResetForTest(cache)

	if cache.probeCalls != 1 || cache.recordCalls != 1 {
		t.Fatalf("calls probe=%d record=%d, want 1/1", cache.probeCalls, cache.recordCalls)
	}
	if got == nil || !got.Equal(probeReset) {
		t.Fatalf("reset = %v, want probe reset %v (banner reset must not be authoritative)", got, probeReset)
	}
}

func TestCacheUsageProbeForRotationDecisionFallsBackToReset7d(t *testing.T) {
	reset := time.Now().Add(7 * 24 * time.Hour).UTC().Truncate(time.Millisecond)
	fetched := time.Now().UTC().Truncate(time.Millisecond)
	cache := &fakeDecisionUsageCache{
		probeSnap: models.UsageSnapshot{
			Util7d:    1,
			Reset7d:   &reset,
			Status:    "RATE_LIMIT_PLAN_STATUS_RATE_LIMITED",
			FetchedAt: &fetched,
		},
	}

	got := probedResetForTest(cache)

	if cache.probeCalls != 1 || cache.recordCalls != 1 {
		t.Fatalf("calls probe=%d record=%d, want 1/1", cache.probeCalls, cache.recordCalls)
	}
	if got == nil || !got.Equal(reset) {
		t.Fatalf("reset = %v, want 7d reset %v", got, reset)
	}
}

func TestCacheUsageProbeForRotationDecisionUsesExhaustedWindowReset(t *testing.T) {
	reset5h := time.Now().Add(5 * time.Hour).UTC().Truncate(time.Millisecond)
	reset7d := time.Now().Add(7 * 24 * time.Hour).UTC().Truncate(time.Millisecond)
	fetched := time.Now().UTC().Truncate(time.Millisecond)
	tests := []struct {
		name string
		snap models.UsageSnapshot
		want time.Time
	}{
		{
			name: "weekly exhausted chooses weekly reset",
			snap: models.UsageSnapshot{
				Util5h:    0.4,
				Util7d:    1,
				Reset5h:   &reset5h,
				Reset7d:   &reset7d,
				FetchedAt: &fetched,
			},
			want: reset7d,
		},
		{
			name: "both exhausted chooses later reset",
			snap: models.UsageSnapshot{
				Util5h:    1,
				Util7d:    1,
				Reset5h:   &reset5h,
				Reset7d:   &reset7d,
				FetchedAt: &fetched,
			},
			want: reset7d,
		},
		{
			name: "rate limited with ambiguous utilization chooses later reset",
			snap: models.UsageSnapshot{
				Util5h:    0.99,
				Util7d:    0.99,
				Reset5h:   &reset5h,
				Reset7d:   &reset7d,
				Status:    "RATE_LIMIT_PLAN_STATUS_RATE_LIMITED",
				FetchedAt: &fetched,
			},
			want: reset7d,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache := &fakeDecisionUsageCache{probeSnap: tt.snap}

			got := probedResetForTest(cache)

			if cache.probeCalls != 1 || cache.recordCalls != 1 {
				t.Fatalf("calls probe=%d record=%d, want 1/1", cache.probeCalls, cache.recordCalls)
			}
			if got == nil || !got.Equal(tt.want) {
				t.Fatalf("reset = %v, want exhausted window reset %v", got, tt.want)
			}
		})
	}
}

func TestCacheUsageProbeForRotationDecisionExtendsBannerWithProbedReset(t *testing.T) {
	bannerReset := time.Now().Add(5 * time.Hour).UTC().Truncate(time.Millisecond)
	probedReset := time.Now().Add(7 * 24 * time.Hour).UTC().Truncate(time.Millisecond)
	fetched := time.Now().UTC().Truncate(time.Millisecond)
	cache := &fakeDecisionUsageCache{
		probeSnap: models.UsageSnapshot{
			Util7d:    1,
			Reset7d:   &probedReset,
			FetchedAt: &fetched,
		},
	}

	_ = bannerReset
	got := probedResetForTest(cache)

	if cache.probeCalls != 1 || cache.recordCalls != 1 {
		t.Fatalf("calls probe=%d record=%d, want 1/1", cache.probeCalls, cache.recordCalls)
	}
	if got == nil || !got.Equal(probedReset) {
		t.Fatalf("reset = %v, want later probed reset %v", got, probedReset)
	}
}

func TestCacheUsageProbeForRotationSignalHydratesHeadlessUsageLimit(t *testing.T) {
	fetched := time.Now().UTC().Truncate(time.Millisecond)
	cache := &fakeDecisionUsageCache{
		probeSnap: models.UsageSnapshot{
			Util5h:    0.08,
			FetchedAt: &fetched,
		},
		accounts: []*models.Account{
			{
				ID:       "acct-next",
				Provider: models.AccountProviderCodex,
				Status:   models.AccountStatusActive,
				Health:   models.AccountHealthOK,
			},
		},
	}

	got := cacheUsageProbeForRotationSignal(context.Background(), zerolog.Nop(), cache, cache, rotation.Signal{
		Provider:        "codex",
		CappedAccountID: "acct-1",
		Kind:            rotation.UsageLimited,
		RotationCapable: true,
	})

	if cache.probeCalls != 1 || cache.recordCalls != 1 {
		t.Fatalf("calls probe=%d record=%d, want 1/1", cache.probeCalls, cache.recordCalls)
	}
	if got.ResetAt != nil {
		t.Fatalf("ResetAt = %v, want unchanged nil (capped reset comes from confirm gate)", got.ResetAt)
	}
	if !got.CandidateProbeRequired {
		t.Fatal("CandidateProbeRequired = false, want true for usage-limited rotation")
	}
	if got.Utilization["acct-next"] != 0.08 {
		t.Fatalf("Utilization = %v, want acct-next=0.08", got.Utilization)
	}
}

func TestCacheUsageProbeForRotationSignalMarksUnavailableCandidateProbes(t *testing.T) {
	cache := &fakeDecisionUsageCache{
		probeErr: errors.New("probe unsupported"),
		accounts: []*models.Account{
			{
				ID:       "acct-next",
				Provider: models.AccountProviderCodex,
				Status:   models.AccountStatusActive,
				Health:   models.AccountHealthOK,
			},
		},
	}

	got := cacheUsageProbeForRotationSignal(context.Background(), zerolog.Nop(), cache, cache, rotation.Signal{
		Provider:        "codex",
		CappedAccountID: "acct-1",
		Kind:            rotation.UsageLimited,
		RotationCapable: true,
	})

	if cache.probeCalls != 1 || cache.recordCalls != 0 {
		t.Fatalf("calls probe=%d record=%d, want 1/0", cache.probeCalls, cache.recordCalls)
	}
	if !got.CandidateProbeRequired {
		t.Fatal("CandidateProbeRequired = false, want true when candidate probing is required")
	}
	if got.Utilization == nil {
		t.Fatal("Utilization = nil, want empty non-nil map to record that candidate probing ran")
	}
	if len(got.Utilization) != 0 {
		t.Fatalf("Utilization = %v, want empty map", got.Utilization)
	}
}

func TestCacheUsageProbeForRotationSignalSkipsUnavailableCandidateSnapshots(t *testing.T) {
	fetched := time.Now().UTC().Truncate(time.Millisecond)
	for _, status := range []string{
		"RATE_LIMIT_PLAN_STATUS_UNSUPPORTED",
		"RATE_LIMIT_PLAN_STATUS_UNSPECIFIED",
	} {
		t.Run(status, func(t *testing.T) {
			cache := &fakeDecisionUsageCache{
				probeSnap: models.UsageSnapshot{
					Status:    status,
					FetchedAt: &fetched,
				},
				accounts: []*models.Account{
					{
						ID:       "acct-next",
						Provider: models.AccountProviderCodex,
						Status:   models.AccountStatusActive,
						Health:   models.AccountHealthOK,
					},
				},
			}

			got := cacheUsageProbeForRotationSignal(context.Background(), zerolog.Nop(), cache, cache, rotation.Signal{
				Provider:        "codex",
				CappedAccountID: "acct-1",
				Kind:            rotation.UsageLimited,
				RotationCapable: true,
			})

			if cache.probeCalls != 1 || cache.recordCalls != 1 {
				t.Fatalf("calls probe=%d record=%d, want 1/1", cache.probeCalls, cache.recordCalls)
			}
			if !got.CandidateProbeRequired {
				t.Fatal("CandidateProbeRequired = false, want true when candidate probing is required")
			}
			if got.Utilization == nil {
				t.Fatal("Utilization = nil, want empty non-nil map to record that candidate probing ran")
			}
			if len(got.Utilization) != 0 {
				t.Fatalf("Utilization = %v, want empty map for %s", got.Utilization, status)
			}
		})
	}
}

func TestCacheUsageProbeForRotationSignalRequiresProbeWhenCandidateListFails(t *testing.T) {
	cache := &fakeDecisionUsageCache{listErr: errors.New("sqlite busy")}

	got := cacheUsageProbeForRotationSignal(context.Background(), zerolog.Nop(), cache, cache, rotation.Signal{
		Provider:        "codex",
		CappedAccountID: "acct-1",
		Kind:            rotation.UsageLimited,
		RotationCapable: true,
	})

	if !got.CandidateProbeRequired {
		t.Fatal("CandidateProbeRequired = false, want true even when candidate list fails")
	}
	if got.Utilization != nil {
		t.Fatalf("Utilization = %v, want nil after list failure", got.Utilization)
	}
}

func TestCacheUsageProbeForRotationDecisionFailSafe(t *testing.T) {
	cache := &fakeDecisionUsageCache{probeErr: errors.New("probe down")}

	got := probedResetForTest(cache)

	if cache.probeCalls != 1 || cache.recordCalls != 0 {
		t.Fatalf("calls probe=%d record=%d, want 1/0", cache.probeCalls, cache.recordCalls)
	}
	if got != nil {
		t.Fatalf("reset = %v, want nil after probe failure", got)
	}
}

func TestCacheUsageProbeForRotationDecisionUsesResetWhenCacheWriteFails(t *testing.T) {
	reset := time.Now().Add(7 * 24 * time.Hour).UTC().Truncate(time.Millisecond)
	fetched := time.Now().UTC().Truncate(time.Millisecond)
	cache := &fakeDecisionUsageCache{
		probeSnap: models.UsageSnapshot{
			Util7d:    1,
			Reset7d:   &reset,
			FetchedAt: &fetched,
		},
		recordErr: errors.New("sqlite busy"),
	}

	got := probedResetForTest(cache)

	if cache.probeCalls != 1 || cache.recordCalls != 1 {
		t.Fatalf("calls probe=%d record=%d, want 1/1", cache.probeCalls, cache.recordCalls)
	}
	if got == nil || !got.Equal(reset) {
		t.Fatalf("reset = %v, want probed reset despite cache write failure %v", got, reset)
	}
}

func TestCacheUsageProbeForRotationDecisionSkipsUnavailableProbe(t *testing.T) {
	cache := &fakeDecisionUsageCache{}

	got := probedResetForTest(cache)

	if cache.probeCalls != 1 || cache.recordCalls != 0 {
		t.Fatalf("calls probe=%d record=%d, want 1/0", cache.probeCalls, cache.recordCalls)
	}
	if got != nil {
		t.Fatalf("reset = %v, want nil when probe produces no snapshot", got)
	}
}
