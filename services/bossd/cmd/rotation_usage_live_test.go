package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/dbtest"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/rotation"
	"github.com/rs/zerolog"
)

type failingRotationUsageStore struct {
	*db.SQLiteAgentAccountStore
	failedWrites int
}

func (s *failingRotationUsageStore) RecordUsageProbe(context.Context, string, domain.UsageSnapshot) error {
	s.failedWrites++
	return errors.New("usage cache unavailable")
}

func TestRotationSelectionUsesLiveUsageAfterCacheWriteFailure_Accounts_AC10(t *testing.T) {
	for _, proactive := range []bool{false, true} {
		name := "reactive"
		if proactive {
			name = "proactive"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := db.NewAgentAccountStore(dbtest.New(t))
			accounts := make([]*domain.AgentAccount, 0, 3)
			for _, label := range []string{"capped", "A", "B"} {
				a, err := store.Create(ctx, db.CreateAgentAccountParams{Provider: domain.AgentAccountProviderClaude, Label: label, Priority: 0})
				if err != nil {
					t.Fatal(err)
				}
				accounts = append(accounts, a)
			}
			now := time.Now().UTC()
			reset := now.Add(time.Hour)
			weeklyA, weeklyB := now.Add(120*time.Hour), now.Add(121*time.Hour)
			staleAt := now.Add(-probeThrottleFloor - time.Minute)
			if err := store.RecordUsageProbe(ctx, accounts[1].ID, domain.UsageSnapshot{Util5h: .95, Util7d: .9, Reset7d: &weeklyB, Reset5h: &reset, FetchedAt: &staleAt}); err != nil {
				t.Fatal(err)
			}
			failing := &failingRotationUsageStore{SQLiteAgentAccountStore: store}
			prober := &fakeDecisionUsageCache{probeSnapByID: map[string]domain.UsageSnapshot{accounts[1].ID: {Util5h: .60, Util7d: .30, Reset5h: &reset, Reset7d: &weeklyA, FetchedAt: &now}, accounts[2].ID: {Util5h: .10, Util7d: .40, Reset5h: &reset, Reset7d: &weeklyB, FetchedAt: &now}}}
			sig := cacheUsageProbeForRotationSignal(ctx, zerolog.Nop(), prober, failing, rotation.Signal{Provider: "claude", CappedAccountID: accounts[0].ID, Kind: rotation.UsageLimited, RotationCapable: true})
			eng := rotation.NewEngine(failing)
			var chosen *domain.AgentAccount
			var err error
			if proactive {
				chosen, err = eng.SelectProactiveCandidate(ctx, "claude", "", accounts[0].ID, sig.CandidateUsage, 1)
			} else {
				var out rotation.Outcome
				out, err = eng.Decide(ctx, sig)
				chosen = out.NextAccount
			}
			if err != nil {
				t.Fatal(err)
			}
			if chosen == nil || chosen.ID != accounts[1].ID {
				t.Errorf("candidate = %v, want A (%s)", chosen, accounts[1].ID)
			}
			if failing.failedWrites != 2 {
				t.Errorf("failed cache writes = %d, want 2", failing.failedWrites)
			}
			stored, err := store.Get(ctx, accounts[1].ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Usage == nil || stored.Usage.Util5h != .95 {
				t.Fatalf("stored usage = %+v, want stale 0.95", stored.Usage)
			}
		})
	}
}

func TestCacheUsageProbeForRotationSignalWithoutCandidates(t *testing.T) {
	cache := &fakeDecisionUsageCache{}
	sig := cacheUsageProbeForRotationSignal(context.Background(), zerolog.Nop(), cache, cache, rotation.Signal{
		Provider: "claude", Kind: rotation.UsageLimited, RotationCapable: true,
	})
	if sig.CandidateUsage != nil {
		t.Fatalf("candidate usage = %v, want nil without candidates", sig.CandidateUsage)
	}
	if cache.probeCalls != 0 || cache.recordCalls != 0 {
		t.Fatalf("probe/record calls = %d/%d, want 0/0", cache.probeCalls, cache.recordCalls)
	}
}
