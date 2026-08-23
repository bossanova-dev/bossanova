package account

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// stubRegistry is a spy implementation of Registry for tests.
type stubRegistry struct {
	accounts   []AccountMeta
	getErr     error
	touchErr   error
	listErr    error
	listCalls  int
	getCalls   int
	touchCalls int
	touchedID  string
	touchedAt  time.Time
}

func (s *stubRegistry) List(_ context.Context) ([]AccountMeta, error) {
	s.listCalls++
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.accounts, nil
}

func (s *stubRegistry) Get(_ context.Context, id string) (AccountMeta, bool, error) {
	s.getCalls++
	if s.getErr != nil {
		return AccountMeta{}, false, s.getErr
	}
	for _, a := range s.accounts {
		if a.ID == id {
			return a, true, nil
		}
	}
	return AccountMeta{}, false, nil
}

func (s *stubRegistry) TouchLastUsed(_ context.Context, id string, at time.Time) error {
	s.touchCalls++
	s.touchedID = id
	s.touchedAt = at
	return s.touchErr
}

// stubMaterializer is a spy implementation of Materializer for tests.
type stubMaterializer struct {
	supports        bool
	supportsErr     error
	env             map[string]string
	materializeErr  error
	supportsCalls   int
	materializeCall int
	materializedID  string
}

func (m *stubMaterializer) SupportsRotation(_ context.Context, _ string) (bool, error) {
	m.supportsCalls++
	if m.supportsErr != nil {
		return false, m.supportsErr
	}
	return m.supports, nil
}

func (m *stubMaterializer) MaterializeAccount(_ context.Context, accountID string) (map[string]string, error) {
	m.materializeCall++
	m.materializedID = accountID
	if m.materializeErr != nil {
		return nil, m.materializeErr
	}
	return m.env, nil
}

func ptrTime(t time.Time) *time.Time { return &t }

func TestDefaultAccountID(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour) // 1h old ⇒ stale (default window is 30m)
	future := now.Add(time.Hour)
	recent := now.Add(-5 * time.Minute) // within the 30m window ⇒ fresh
	edgeFresh := now.Add(-30 * time.Minute)
	edgeStale := now.Add(-30*time.Minute - time.Second)
	soon := now.Add(2 * time.Hour)
	mid := now.Add(3 * time.Hour)
	later := now.Add(5 * time.Hour)

	tests := []struct {
		name     string
		accounts []AccountMeta
		provider string
		want     string
	}{
		{
			name:     "no accounts yields empty",
			accounts: nil,
			provider: "claude",
			want:     "",
		},
		{
			name: "lowest priority active healthy non-cooling wins",
			accounts: []AccountMeta{
				{ID: "a", Provider: "claude", Status: "active", Health: "ok", Priority: 1},
				{ID: "b", Provider: "claude", Status: "active", Health: "ok", Priority: 5},
				{ID: "c", Provider: "claude", Status: "active", Health: "ok", Priority: 3},
			},
			provider: "claude",
			want:     "a",
		},
		{
			name: "tier-1 healthy non-cooling beats a lower-priority cooling account",
			accounts: []AccountMeta{
				{ID: "cool", Provider: "claude", Status: "active", Health: "ok", Priority: 1, CoolingUntil: ptrTime(future)},
				{ID: "hot", Provider: "claude", Status: "active", Health: "ok", Priority: 9},
			},
			provider: "claude",
			want:     "hot",
		},
		{
			name: "cooling active binds via tier-2 fallback (disabled ignored)",
			accounts: []AccountMeta{
				{ID: "a", Provider: "claude", Status: "active", Health: "ok", Priority: 5, CoolingUntil: ptrTime(future)},
				{ID: "b", Provider: "claude", Status: "disabled", Health: "ok", Priority: 9},
			},
			provider: "claude",
			want:     "a",
		},
		{
			name: "past cooling is eligible again",
			accounts: []AccountMeta{
				{ID: "a", Provider: "claude", Status: "active", Health: "ok", Priority: 5, CoolingUntil: ptrTime(past)},
			},
			provider: "claude",
			want:     "a",
		},
		{
			name: "wrong provider ignored",
			accounts: []AccountMeta{
				{ID: "a", Provider: "codex", Status: "active", Health: "ok", Priority: 9},
			},
			provider: "claude",
			want:     "",
		},
		{
			name: "failed-health account is skipped even at lower priority",
			accounts: []AccountMeta{
				{ID: "a", Provider: "claude", Status: "active", Health: "failed", Priority: 1},
				{ID: "b", Provider: "claude", Status: "active", Health: "ok", Priority: 5},
			},
			provider: "claude",
			want:     "b",
		},
		{
			name: "only failed-health active account binds via tier-2 fallback",
			accounts: []AccountMeta{
				{ID: "a", Provider: "claude", Status: "active", Health: "failed", Priority: 1},
			},
			provider: "claude",
			want:     "a",
		},
		{
			name: "tie-break prefers least-recently used (LRU)",
			accounts: []AccountMeta{
				{ID: "a", Provider: "claude", Status: "active", Health: "ok", Priority: 5, LastUsedAt: ptrTime(past)},
				{ID: "b", Provider: "claude", Status: "active", Health: "ok", Priority: 5, LastUsedAt: ptrTime(now)},
			},
			provider: "claude",
			want:     "a",
		},
		{
			name: "tie-break prefers never-used over used",
			accounts: []AccountMeta{
				{ID: "a", Provider: "claude", Status: "active", Health: "ok", Priority: 5, LastUsedAt: ptrTime(past)},
				{ID: "b", Provider: "claude", Status: "active", Health: "ok", Priority: 5},
			},
			provider: "claude",
			want:     "b",
		},
		{
			name: "tie-break falls back to lexical id when last-used equal",
			accounts: []AccountMeta{
				{ID: "z", Provider: "claude", Status: "active", Health: "ok", Priority: 5},
				{ID: "a", Provider: "claude", Status: "active", Health: "ok", Priority: 5},
			},
			provider: "claude",
			want:     "a",
		},
		{
			// yuki is priority-preferred (lower Priority) but fresh-capped at 100%
			// of its 7d window; dave is fresh at 5%. Util must override priority.
			name: "fresh-capped-vs-fresh-idle→picks-idle",
			accounts: []AccountMeta{
				{ID: "yuki", Provider: "claude", Status: "active", Health: "ok", Priority: 1, UsageFetchedAt: ptrTime(recent), Util7d: 1.0},
				{ID: "dave", Provider: "claude", Status: "active", Health: "ok", Priority: 5, UsageFetchedAt: ptrTime(recent), Util5h: 0.05},
			},
			provider: "claude",
			want:     "dave",
		},
		{
			// Both snapshots are older than the 30m window: capped readings are
			// ignored and selection is byte-identical to today's moreEligible
			// (priority 1 wins over priority 5).
			name: "both-stale→identical",
			accounts: []AccountMeta{
				{ID: "a", Provider: "claude", Status: "active", Health: "ok", Priority: 1, UsageFetchedAt: ptrTime(past), Util5h: 1.0},
				{ID: "b", Provider: "claude", Status: "active", Health: "ok", Priority: 5, UsageFetchedAt: ptrTime(past), Util5h: 0.05},
			},
			provider: "claude",
			want:     "a",
		},
		{
			// A fresh low-util account outranks a higher-priority stale account.
			name: "fresh-idle-beats-stale",
			accounts: []AccountMeta{
				{ID: "stale", Provider: "claude", Status: "active", Health: "ok", Priority: 1, UsageFetchedAt: ptrTime(past)},
				{ID: "fresh", Provider: "claude", Status: "active", Health: "ok", Priority: 5, UsageFetchedAt: ptrTime(recent), Util5h: 0.1},
			},
			provider: "claude",
			want:     "fresh",
		},
		{
			// UsageFetchedAt nil on all ⇒ unknown ⇒ today's priority/LRU ordering.
			name: "unknown-usage→priority-LRU",
			accounts: []AccountMeta{
				{ID: "a", Provider: "claude", Status: "active", Health: "ok", Priority: 5},
				{ID: "b", Provider: "claude", Status: "active", Health: "ok", Priority: 1},
			},
			provider: "claude",
			want:     "b",
		},
		{
			// Every fresh candidate is capped ⇒ tier-1 empty ⇒ tier-2 returns the
			// soonest reset. capB (Reset7d=soon) beats capA (Reset5h=later) even
			// though capA has the lower priority.
			name: "all-capped→soonest-reset",
			accounts: []AccountMeta{
				{ID: "capA", Provider: "claude", Status: "active", Health: "ok", Priority: 1, UsageFetchedAt: ptrTime(recent), Util5h: 1.0, Reset5h: ptrTime(later)},
				{ID: "capB", Provider: "claude", Status: "active", Health: "ok", Priority: 5, UsageFetchedAt: ptrTime(recent), Util7d: 1.0, Reset7d: ptrTime(soon)},
			},
			provider: "claude",
			want:     "capB",
		},
		{
			// "both" is capped on BOTH windows (Util5h>=1 AND Util7d>=1) with both
			// resets set: earliestReset must pick the earlier of the two
			// (Reset7d=soon < Reset5h=later), so "both" recovers at soon and beats
			// "other2" (recovers at mid) in tier-2 — exercising the dual-capped
			// min-within-account branch even though other2 has the lower priority.
			name: "all-capped: dual-capped account ranked by its earliest reset",
			accounts: []AccountMeta{
				{ID: "both", Provider: "claude", Status: "active", Health: "ok", Priority: 5, UsageFetchedAt: ptrTime(recent), Util5h: 1.0, Util7d: 1.0, Reset5h: ptrTime(later), Reset7d: ptrTime(soon)},
				{ID: "other2", Provider: "claude", Status: "active", Health: "ok", Priority: 1, UsageFetchedAt: ptrTime(recent), Util7d: 1.0, Reset7d: ptrTime(mid)},
			},
			provider: "claude",
			want:     "both",
		},
		{
			// Both fresh candidates are capped ⇒ tier-2. capKnown has a known
			// soonest reset; capUnknown is capped with no reset time at all. A
			// known recovery instant must beat an unknown one, so capKnown wins
			// even though capUnknown has the lower (preferred) priority.
			name: "all-capped: known-reset beats unknown-reset",
			accounts: []AccountMeta{
				{ID: "capUnknown", Provider: "claude", Status: "active", Health: "ok", Priority: 1, UsageFetchedAt: ptrTime(recent), Util5h: 1.0},
				{ID: "capKnown", Provider: "claude", Status: "active", Health: "ok", Priority: 5, UsageFetchedAt: ptrTime(recent), Util7d: 1.0, Reset7d: ptrTime(soon)},
			},
			provider: "claude",
			want:     "capKnown",
		},
		{
			// A snapshot stamped in the future (clock skew / corrupt data) is not
			// trustworthy: its capped reading is ignored, so the future account
			// stays selectable and its priority 1 wins over the unknown account.
			name: "future fetched-at is not fresh (capped reading ignored)",
			accounts: []AccountMeta{
				{ID: "future", Provider: "claude", Status: "active", Health: "ok", Priority: 1, UsageFetchedAt: ptrTime(future), Util5h: 1.0},
				{ID: "other", Provider: "claude", Status: "active", Health: "ok", Priority: 5},
			},
			provider: "claude",
			want:     "future",
		},
		{
			// Snapshot exactly at the window edge counts as fresh, so the capped
			// edge account is excluded from tier-1 and the unknown-usage account
			// wins despite its lower priority. (other has unknown usage so the
			// only differentiator is edge's freshness.)
			name: "staleness boundary: edge is fresh (capped excluded)",
			accounts: []AccountMeta{
				{ID: "edge", Provider: "claude", Status: "active", Health: "ok", Priority: 1, UsageFetchedAt: ptrTime(edgeFresh), Util5h: 1.0},
				{ID: "other", Provider: "claude", Status: "active", Health: "ok", Priority: 5},
			},
			provider: "claude",
			want:     "other",
		},
		{
			// One second past the window ⇒ stale ⇒ capped reading ignored ⇒ edge
			// stays selectable and its priority 1 wins today's ordering over the
			// unknown-usage account.
			name: "staleness boundary: one second past is stale",
			accounts: []AccountMeta{
				{ID: "edge", Provider: "claude", Status: "active", Health: "ok", Priority: 1, UsageFetchedAt: ptrTime(edgeStale), Util5h: 1.0},
				{ID: "other", Provider: "claude", Status: "active", Health: "ok", Priority: 5},
			},
			provider: "claude",
			want:     "edge",
		},
		{
			// BOS-429: within an equal-priority band (usage unknown ⇒ no util
			// signal), the account whose weekly quota resets soonest in the future
			// wins even though the other account is the LRU-idler. Expiry sits above
			// LRU: b (resets in 2h) beats a (resets in 5h) despite a being idler.
			name: "weekly-expiry: soonest future reset beats LRU",
			accounts: []AccountMeta{
				{ID: "a", Provider: "claude", Status: "active", Health: "ok", Priority: 5, LastUsedAt: ptrTime(past), Reset7d: ptrTime(later)},
				{ID: "b", Provider: "claude", Status: "active", Health: "ok", Priority: 5, LastUsedAt: ptrTime(now), Reset7d: ptrTime(soon)},
			},
			provider: "claude",
			want:     "b",
		},
		{
			// A known future reset outranks both a nil (never probed) and a past
			// (already-rolled) reset, which fall together to the LRU tiebreak. "fut"
			// wins over the idler nil/past accounts.
			name: "weekly-expiry: future reset beats nil and past (both fall to LRU)",
			accounts: []AccountMeta{
				{ID: "fut", Provider: "claude", Status: "active", Health: "ok", Priority: 5, LastUsedAt: ptrTime(now), Reset7d: ptrTime(soon)},
				{ID: "pst", Provider: "claude", Status: "active", Health: "ok", Priority: 5, LastUsedAt: ptrTime(past), Reset7d: ptrTime(past)},
				{ID: "nul", Provider: "claude", Status: "active", Health: "ok", Priority: 5, LastUsedAt: ptrTime(past)},
			},
			provider: "claude",
			want:     "fut",
		},
		{
			// Explicit Priority still dominates weekly-expiry: a later-expiring
			// priority-1 account beats a sooner-expiring priority-5 one.
			name: "weekly-expiry: explicit priority dominates",
			accounts: []AccountMeta{
				{ID: "p1later", Provider: "claude", Status: "active", Health: "ok", Priority: 1, Reset7d: ptrTime(later)},
				{ID: "p5soon", Provider: "claude", Status: "active", Health: "ok", Priority: 5, Reset7d: ptrTime(soon)},
			},
			provider: "claude",
			want:     "p1later",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reg := &stubRegistry{accounts: tc.accounts}
			r := NewResolver(reg, &stubMaterializer{}, zerolog.Nop())
			got, err := r.DefaultAccountID(context.Background(), tc.provider, now)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("DefaultAccountID = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDefaultAccountID_NeverProbes proves the selection path is cache-only: it
// reads the registry once (List) and never touches the materializer (no probe,
// no MaterializeAccount) nor per-account Get.
func TestDefaultAccountID_NeverProbes(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-5 * time.Minute)
	reg := &stubRegistry{accounts: []AccountMeta{
		{ID: "a", Provider: "claude", Status: "active", Health: "ok", Priority: 1, UsageFetchedAt: ptrTime(recent), Util7d: 1.0},
		{ID: "b", Provider: "claude", Status: "active", Health: "ok", Priority: 5, UsageFetchedAt: ptrTime(recent), Util5h: 0.05},
	}}
	mat := &panicMaterializer{t: t}
	r := NewResolver(reg, mat, zerolog.Nop())
	got, err := r.DefaultAccountID(context.Background(), "claude", now)
	if err != nil {
		t.Fatalf("DefaultAccountID: %v", err)
	}
	if got != "b" {
		t.Fatalf("want b (fresh low-util), got %q", got)
	}
	if reg.listCalls != 1 {
		t.Errorf("List calls = %d, want 1", reg.listCalls)
	}
	if reg.getCalls != 0 {
		t.Errorf("Get calls = %d, want 0 (selection is List-only)", reg.getCalls)
	}
}

// panicMaterializer fails the test if any materializer method is called, proving
// DefaultAccountID performs no live/billable probe on the selection path.
type panicMaterializer struct{ t *testing.T }

func (m *panicMaterializer) SupportsRotation(context.Context, string) (bool, error) {
	m.t.Fatalf("SupportsRotation must not be called on the selection path")
	return false, nil
}

func (m *panicMaterializer) MaterializeAccount(context.Context, string) (map[string]string, error) {
	m.t.Fatalf("MaterializeAccount must not be called on the selection path")
	return nil, nil
}

func TestDefaultAccountID_FallsBackToCoolingActiveAccount(t *testing.T) {
	now := time.Now()
	cooling := now.Add(30 * time.Minute)
	reg := &stubRegistry{accounts: []AccountMeta{
		{ID: "a1", Provider: "claude", Status: "active", Health: "ok", Priority: 0, CoolingUntil: &cooling},
	}}
	r := NewResolver(reg, nil, zerolog.Nop())
	got, err := r.DefaultAccountID(context.Background(), "claude", now)
	if err != nil {
		t.Fatalf("DefaultAccountID: %v", err)
	}
	if got != "a1" {
		t.Fatalf("want a1 (cooling but active => managed, never unmanaged), got %q", got)
	}
}

func TestDefaultAccountID_PrefersHealthyOverFailedInFallback(t *testing.T) {
	now := time.Now()
	cooling := now.Add(time.Hour)
	reg := &stubRegistry{accounts: []AccountMeta{
		{ID: "bad", Provider: "claude", Status: "active", Health: "failed", Priority: 0},
		{ID: "cool", Provider: "claude", Status: "active", Health: "ok", Priority: 5, CoolingUntil: &cooling},
	}}
	r := NewResolver(reg, nil, zerolog.Nop())
	got, _ := r.DefaultAccountID(context.Background(), "claude", now)
	if got != "cool" {
		t.Fatalf("want cool (healthy-cooling beats failed), got %q", got)
	}
}

func TestDefaultAccountID_FallbackPrefersSoonestCooldown(t *testing.T) {
	now := time.Now()
	soon := now.Add(30 * time.Minute)
	late := now.Add(2 * time.Hour)
	// Same health tier (both healthy) and equal Priority so only the cooldown
	// discriminator can decide. IDs are chosen so the priority/LRU/id tie-break
	// (moreEligible) would pick "a-late": if the soonest-cooldown branch were
	// dropped, this test would bind "a-late" and fail.
	reg := &stubRegistry{accounts: []AccountMeta{
		{ID: "a-late", Provider: "claude", Status: "active", Health: "ok", Priority: 5, CoolingUntil: &late},
		{ID: "z-soon", Provider: "claude", Status: "active", Health: "ok", Priority: 5, CoolingUntil: &soon},
	}}
	r := NewResolver(reg, nil, zerolog.Nop())
	got, err := r.DefaultAccountID(context.Background(), "claude", now)
	if err != nil {
		t.Fatalf("DefaultAccountID: %v", err)
	}
	if got != "z-soon" {
		t.Fatalf("want z-soon (soonest cooldown wins in tier-2), got %q", got)
	}
}

func TestDefaultAccountID_FallbackSoonestCooldownAmongFailed(t *testing.T) {
	now := time.Now()
	soon := now.Add(15 * time.Minute)
	late := now.Add(3 * time.Hour)
	// Both failed-health (same health tier) with equal Priority; the soonest
	// cooldown must still decide even in the failed tier. Lexical id would pick
	// "a-late", so binding "z-soon" proves the cooldown branch governs.
	reg := &stubRegistry{accounts: []AccountMeta{
		{ID: "a-late", Provider: "claude", Status: "active", Health: "failed", Priority: 5, CoolingUntil: &late},
		{ID: "z-soon", Provider: "claude", Status: "active", Health: "failed", Priority: 5, CoolingUntil: &soon},
	}}
	r := NewResolver(reg, nil, zerolog.Nop())
	got, err := r.DefaultAccountID(context.Background(), "claude", now)
	if err != nil {
		t.Fatalf("DefaultAccountID: %v", err)
	}
	if got != "z-soon" {
		t.Fatalf("want z-soon (soonest cooldown wins among failed), got %q", got)
	}
}

func TestDefaultAccountID_AllDisabledFallsBackToUnmanaged(t *testing.T) {
	reg := &stubRegistry{accounts: []AccountMeta{
		{ID: "d1", Provider: "claude", Status: "disabled", Health: "ok"},
	}}
	r := NewResolver(reg, nil, zerolog.Nop())
	got, _ := r.DefaultAccountID(context.Background(), "claude", time.Now())
	if got != "" {
		t.Fatalf("want unmanaged for disabled-only provider, got %q", got)
	}
}

func TestDefaultAccountIDExcluding(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)

	t.Run("excludes-current", func(t *testing.T) {
		accounts := []AccountMeta{
			{ID: "a", Provider: "claude", Status: "active", Health: "ok", Priority: 1},
			{ID: "b", Provider: "claude", Status: "active", Health: "ok", Priority: 5},
		}
		reg := &stubRegistry{accounts: accounts}
		r := NewResolver(reg, &stubMaterializer{}, zerolog.Nop())

		winner, err := r.DefaultAccountID(context.Background(), "claude", now)
		if err != nil {
			t.Fatalf("DefaultAccountID: %v", err)
		}
		if winner != "a" {
			t.Fatalf("DefaultAccountID = %q, want a", winner)
		}

		other, err := r.DefaultAccountIDExcluding(context.Background(), "claude", winner, now)
		if err != nil {
			t.Fatalf("DefaultAccountIDExcluding: %v", err)
		}
		if other != "b" {
			t.Fatalf("DefaultAccountIDExcluding(exclude=%q) = %q, want b", winner, other)
		}
	})

	t.Run("only-account-excluded-returns-system-default", func(t *testing.T) {
		reg := &stubRegistry{accounts: []AccountMeta{
			{ID: "x", Provider: "claude", Status: "active", Health: "ok", Priority: 1},
		}}
		r := NewResolver(reg, &stubMaterializer{}, zerolog.Nop())

		got, err := r.DefaultAccountIDExcluding(context.Background(), "claude", "x", now)
		if err != nil {
			t.Fatalf("DefaultAccountIDExcluding: %v", err)
		}
		if got != SystemDefaultAccountID {
			t.Fatalf("DefaultAccountIDExcluding(only account excluded) = %q, want SystemDefaultAccountID (%q)", got, SystemDefaultAccountID)
		}
	})

	t.Run("empty-exclude-equivalent-to-DefaultAccountID", func(t *testing.T) {
		accounts := []AccountMeta{
			{ID: "a", Provider: "claude", Status: "active", Health: "ok", Priority: 5},
			{ID: "b", Provider: "claude", Status: "active", Health: "ok", Priority: 1},
			{ID: "c", Provider: "claude", Status: "active", Health: "ok", Priority: 3},
		}
		reg := &stubRegistry{accounts: accounts}
		r := NewResolver(reg, &stubMaterializer{}, zerolog.Nop())

		want, err := r.DefaultAccountID(context.Background(), "claude", now)
		if err != nil {
			t.Fatalf("DefaultAccountID: %v", err)
		}
		got, err := r.DefaultAccountIDExcluding(context.Background(), "claude", "", now)
		if err != nil {
			t.Fatalf("DefaultAccountIDExcluding: %v", err)
		}
		if got != want {
			t.Fatalf("DefaultAccountIDExcluding(exclude=\"\") = %q, want %q (== DefaultAccountID)", got, want)
		}
	})
}

func TestDefaultAccountIDNilRegistry(t *testing.T) {
	r := NewResolver(nil, nil, zerolog.Nop())
	got, err := r.DefaultAccountID(context.Background(), "claude", time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Fatalf("nil registry should yield empty, got %q", got)
	}
}

func TestResolveSpawnEnvAccountZeroShortCircuits(t *testing.T) {
	reg := &stubRegistry{accounts: []AccountMeta{{ID: "a", Provider: "claude", Status: "active"}}}
	mat := &stubMaterializer{supports: true, env: map[string]string{"K": "V"}}
	r := NewResolver(reg, mat, zerolog.Nop())

	env, err := r.ResolveSpawnEnv(context.Background(), SystemDefaultAccountID, "claude", time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env != nil {
		t.Fatalf("account 0 should return nil env, got %v", env)
	}
	if reg.getCalls != 0 || reg.touchCalls != 0 {
		t.Fatalf("account 0 must not touch registry: get=%d touch=%d", reg.getCalls, reg.touchCalls)
	}
	if mat.supportsCalls != 0 || mat.materializeCall != 0 {
		t.Fatalf("account 0 must not touch materializer: supports=%d materialize=%d", mat.supportsCalls, mat.materializeCall)
	}
}

func TestResolveSpawnEnvBoundAccountMaterializes(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	reg := &stubRegistry{accounts: []AccountMeta{{ID: "acct-1", Provider: "claude", Status: "active"}}}
	mat := &stubMaterializer{supports: true, env: map[string]string{"ANTHROPIC_API_KEY": "sk-x"}}
	r := NewResolver(reg, mat, zerolog.Nop())

	env, err := r.ResolveSpawnEnv(context.Background(), "acct-1", "claude", now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env["ANTHROPIC_API_KEY"] != "sk-x" {
		t.Fatalf("expected materialized env, got %v", env)
	}
	if mat.materializeCall != 1 || mat.materializedID != "acct-1" {
		t.Fatalf("expected one materialize of acct-1, got calls=%d id=%q", mat.materializeCall, mat.materializedID)
	}
	if reg.touchCalls != 1 || reg.touchedID != "acct-1" {
		t.Fatalf("expected one TouchLastUsed of acct-1, got calls=%d id=%q", reg.touchCalls, reg.touchedID)
	}
	if !reg.touchedAt.Equal(now) {
		t.Fatalf("expected touch at %v, got %v", now, reg.touchedAt)
	}
}

func TestResolveSpawnEnvTouchErrorStillReturnsEnv(t *testing.T) {
	reg := &stubRegistry{
		accounts: []AccountMeta{{ID: "acct-1", Provider: "claude", Status: "active"}},
		touchErr: errors.New("db down"),
	}
	mat := &stubMaterializer{supports: true, env: map[string]string{"K": "V"}}
	r := NewResolver(reg, mat, zerolog.Nop())

	env, err := r.ResolveSpawnEnv(context.Background(), "acct-1", "claude", time.Now())
	if err != nil {
		t.Fatalf("touch error must not fail resolution: %v", err)
	}
	if env["K"] != "V" {
		t.Fatalf("expected env despite touch error, got %v", env)
	}
}

func TestResolveSpawnEnvNoRotationDegrades(t *testing.T) {
	reg := &stubRegistry{accounts: []AccountMeta{{ID: "acct-1", Provider: "claude", Status: "active"}}}
	mat := &stubMaterializer{supports: false}
	r := NewResolver(reg, mat, zerolog.Nop())

	env, err := r.ResolveSpawnEnv(context.Background(), "acct-1", "claude", time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env != nil {
		t.Fatalf("no-rotation plugin should degrade to nil env, got %v", env)
	}
	if mat.materializeCall != 0 {
		t.Fatalf("no-rotation plugin must not materialize, got %d calls", mat.materializeCall)
	}
	if reg.touchCalls != 0 {
		t.Fatalf("no-rotation degrade must not touch last-used, got %d", reg.touchCalls)
	}
}

func TestResolveSpawnEnvNilMaterializerDegrades(t *testing.T) {
	reg := &stubRegistry{accounts: []AccountMeta{{ID: "acct-1", Provider: "claude", Status: "active"}}}
	r := NewResolver(reg, nil, zerolog.Nop())

	env, err := r.ResolveSpawnEnv(context.Background(), "acct-1", "claude", time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env != nil {
		t.Fatalf("nil materializer should degrade to nil env, got %v", env)
	}
}

func TestResolveSpawnEnvUnknownAccountTreatedAsZero(t *testing.T) {
	reg := &stubRegistry{accounts: nil}
	mat := &stubMaterializer{supports: true, env: map[string]string{"K": "V"}}
	r := NewResolver(reg, mat, zerolog.Nop())

	env, err := r.ResolveSpawnEnv(context.Background(), "ghost", "claude", time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env != nil {
		t.Fatalf("unknown account should degrade to nil env, got %v", env)
	}
	if mat.materializeCall != 0 {
		t.Fatalf("unknown account must not materialize, got %d", mat.materializeCall)
	}
}

func TestResolveSpawnEnvNilRegistryDoesNotPanic(t *testing.T) {
	r := NewResolver(nil, &stubMaterializer{supports: true}, zerolog.Nop())
	env, err := r.ResolveSpawnEnv(context.Background(), "acct-1", "claude", time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env != nil {
		t.Fatalf("nil registry should degrade to nil env, got %v", env)
	}
}

func TestLabel(t *testing.T) {
	reg := &stubRegistry{accounts: []AccountMeta{{ID: "acct-12345678abc", Provider: "claude", Label: "Work"}}}
	r := NewResolver(reg, nil, zerolog.Nop())

	if got, _ := r.Label(context.Background(), ""); got != UnmanagedLocalCredentialsLabel {
		t.Fatalf("empty id label = %q, want %q", got, UnmanagedLocalCredentialsLabel)
	}
	if got, _ := r.Label(context.Background(), "acct-12345678abc"); got != "Work" {
		t.Fatalf("known id label = %q, want Work", got)
	}
	// Unknown id falls back to short prefix, never empty.
	got, err := r.Label(context.Background(), "deadbeefcafebabe")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == "" {
		t.Fatalf("unknown id label must not be empty")
	}
	if got != "deadbeef" {
		t.Fatalf("unknown id label = %q, want short prefix deadbeef", got)
	}
}

func TestLabelNilRegistryFallsBackToPrefix(t *testing.T) {
	r := NewResolver(nil, nil, zerolog.Nop())
	got, err := r.Label(context.Background(), "abcdef1234")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "abcdef12" {
		t.Fatalf("nil registry label = %q, want abcdef12", got)
	}
}

// TestResolveSpawnEnvForProbeSkipsOnlyLastUsed pins the read-only contract
// BOS-867's `boss session mcp` depends on: a diagnostic probe must derive the
// SAME environment a spawn does without advancing LastUsedAt, which is the LRU
// key DefaultAccountID selects on — otherwise running a read-only command could
// change which account the NEXT session is handed.
//
// Both halves are asserted against ONE stub in ONE test on purpose: a broken
// resolver that touched nothing at all, or one that returned no env at all,
// would pass a probe-only assertion while being completely wrong. Requiring the
// spawn path to touch, the probe path not to, and both to return byte-identical
// env is what makes the assertion non-vacuous.
func TestResolveSpawnEnvForProbeSkipsOnlyLastUsed(t *testing.T) {
	now := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)
	reg := &stubRegistry{accounts: []AccountMeta{{ID: "acct-1", Provider: "claude", Status: "active"}}}
	mat := &stubMaterializer{supports: true, env: map[string]string{"ANTHROPIC_API_KEY": "sk-x"}}
	r := NewResolver(reg, mat, zerolog.Nop())

	probeEnv, err := r.ResolveSpawnEnvForProbe(context.Background(), "acct-1", "claude", now)
	if err != nil {
		t.Fatalf("ResolveSpawnEnvForProbe: %v", err)
	}
	if reg.touchCalls != 0 {
		t.Fatalf("probe path called TouchLastUsed %d times, want 0: a read-only probe must not advance the account LRU key", reg.touchCalls)
	}
	// The probe still materializes — that IS what produces the env, so skipping
	// it would return an empty map and break byte-identity with the spawn path.
	if mat.materializeCall != 1 {
		t.Fatalf("probe path materialize calls = %d, want 1: materializing is what produces the env", mat.materializeCall)
	}
	if probeEnv["ANTHROPIC_API_KEY"] != "sk-x" {
		t.Fatalf("probe env = %v, want the materialized account env", probeEnv)
	}

	spawnEnv, err := r.ResolveSpawnEnv(context.Background(), "acct-1", "claude", now)
	if err != nil {
		t.Fatalf("ResolveSpawnEnv: %v", err)
	}
	if reg.touchCalls != 1 || reg.touchedID != "acct-1" || !reg.touchedAt.Equal(now) {
		t.Fatalf("spawn path TouchLastUsed = (calls=%d id=%q at=%v), want exactly one touch of acct-1 at %v — without this half the probe assertion above proves nothing",
			reg.touchCalls, reg.touchedID, reg.touchedAt, now)
	}
	if !maps.Equal(probeEnv, spawnEnv) {
		t.Fatalf("probe env %v differs from spawn env %v: the two must come from one shared body so a probe can never describe an environment the chat never sees", probeEnv, spawnEnv)
	}
}

// --- BOS-973: durable record of a credential-injection failure --------------

// recordingRegistry is a stubRegistry that ALSO implements the optional
// injectionHealthRecorder seam, so the resolver's type assertion succeeds.
type recordingRegistry struct {
	stubRegistry
	recorded    []string
	recordErr   error
	clearCalls  int
	clearedIDs  []string
	clearErr    error
	recordedIDs []string
}

func (r *recordingRegistry) RecordInjectionFailure(_ context.Context, id, reason string) error {
	r.recordedIDs = append(r.recordedIDs, id)
	r.recorded = append(r.recorded, reason)
	return r.recordErr
}

func (r *recordingRegistry) ClearInjectionFailure(_ context.Context, id string) error {
	r.clearCalls++
	r.clearedIDs = append(r.clearedIDs, id)
	return r.clearErr
}

func newRecordingRegistry(accounts ...AccountMeta) *recordingRegistry {
	return &recordingRegistry{stubRegistry: stubRegistry{accounts: accounts}}
}

func TestResolveSpawnEnvRecordsInjectionFailure(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	reg := newRecordingRegistry(AccountMeta{ID: "a1", Provider: "codex", Status: "active", Health: "ok"})
	mat := &stubMaterializer{supports: true, materializeErr: errors.New("project codex base home: boom")}
	r := NewResolver(reg, mat, zerolog.Nop())

	env, err := r.ResolveSpawnEnv(context.Background(), "a1", "codex", now)
	// The degrade POLICY is unchanged: the error is still returned verbatim to
	// the caller, which decides to fall back to the ambient login.
	if err == nil {
		t.Fatal("ResolveSpawnEnv: want the materialize error returned unchanged")
	}
	if env != nil {
		t.Fatalf("env = %v, want nil", env)
	}
	if len(reg.recorded) != 1 {
		t.Fatalf("recorded %d injection failures, want exactly 1", len(reg.recorded))
	}
	if reg.recordedIDs[0] != "a1" {
		t.Errorf("recorded against %q, want %q", reg.recordedIDs[0], "a1")
	}
	if !strings.Contains(reg.recorded[0], "project codex base home: boom") {
		t.Errorf("recorded reason = %q, want the materialize error text", reg.recorded[0])
	}
	if reg.clearCalls != 0 {
		t.Errorf("clear called %d times on the failure path, want 0", reg.clearCalls)
	}
}

// ctxObservingRegistry models what a real store does: its write runs through
// ExecContext, so an already-cancelled context fails the UPDATE outright. The
// stub recordingRegistry ignores ctx entirely and therefore cannot catch a
// regression here.
type ctxObservingRegistry struct {
	stubRegistry
	recorded  []string
	recordErr error
}

func (r *ctxObservingRegistry) RecordInjectionFailure(ctx context.Context, _, reason string) error {
	if err := ctx.Err(); err != nil {
		r.recordErr = err
		return err
	}
	r.recorded = append(r.recorded, reason)
	return nil
}

func (r *ctxObservingRegistry) ClearInjectionFailure(ctx context.Context, _ string) error {
	return ctx.Err()
}

// A cancelled spawn is the failure class most likely to strand an operator with
// no signal: the materialize error IS a context error, so a health write that
// inherited the spawn's ctx would be guaranteed to fail and be swallowed as a
// WARN — reproducing the BOS-973 silent degrade precisely where it hurts. The
// write is detached with context.WithoutCancel so it survives.
func TestResolveSpawnEnvRecordsInjectionFailureOnCancelledSpawn(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	reg := &ctxObservingRegistry{stubRegistry: stubRegistry{
		accounts: []AccountMeta{{ID: "a1", Provider: "codex", Status: "active", Health: "ok"}},
	}}
	mat := &stubMaterializer{supports: true, materializeErr: context.Canceled}
	r := NewResolver(reg, mat, zerolog.Nop())

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the spawn is already gone by the time materialize reports it

	if _, err := r.ResolveSpawnEnv(ctx, "a1", "codex", now); err == nil {
		t.Fatal("ResolveSpawnEnv: want the materialize error returned")
	}
	if reg.recordErr != nil {
		t.Fatalf("health write saw a cancelled context (%v); it must be detached via context.WithoutCancel", reg.recordErr)
	}
	if len(reg.recorded) != 1 {
		t.Fatalf("recorded %d injection failures on a cancelled spawn, want exactly 1", len(reg.recorded))
	}
}

// The probe is a read-only diagnostic. health is a rotation-selection key
// (rotation.isSelectable), exactly like the LastUsedAt the probe already
// refuses to skew, so the probe must write no health record at all.
func TestResolveSpawnEnvForProbeRecordsNothing(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	reg := newRecordingRegistry(AccountMeta{ID: "a1", Provider: "codex", Status: "active", Health: "ok"})
	mat := &stubMaterializer{supports: true, materializeErr: errors.New("boom")}
	r := NewResolver(reg, mat, zerolog.Nop())

	if _, err := r.ResolveSpawnEnvForProbe(context.Background(), "a1", "codex", now); err == nil {
		t.Fatal("ResolveSpawnEnvForProbe: want the materialize error returned")
	}
	if len(reg.recorded) != 0 {
		t.Errorf("probe recorded %d injection failures, want 0", len(reg.recorded))
	}
	if reg.clearCalls != 0 {
		t.Errorf("probe called clear %d times, want 0", reg.clearCalls)
	}
}

func TestResolveSpawnEnvClearsInjectionFailureOnSuccess(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	reg := newRecordingRegistry(AccountMeta{ID: "a1", Provider: "codex", Status: "active", Health: "failed"})
	mat := &stubMaterializer{supports: true, env: map[string]string{"CODEX_HOME": "/tmp/a1"}}
	r := NewResolver(reg, mat, zerolog.Nop())

	env, err := r.ResolveSpawnEnv(context.Background(), "a1", "codex", now)
	if err != nil {
		t.Fatalf("ResolveSpawnEnv: %v", err)
	}
	if env["CODEX_HOME"] != "/tmp/a1" {
		t.Fatalf("env = %v, want the materialized overlay", env)
	}
	if reg.clearCalls != 1 {
		t.Fatalf("clear called %d times, want exactly 1", reg.clearCalls)
	}
	if reg.clearedIDs[0] != "a1" {
		t.Errorf("cleared %q, want %q", reg.clearedIDs[0], "a1")
	}
	if len(reg.recorded) != 0 {
		t.Errorf("recorded %d injection failures on the success path, want 0", len(reg.recorded))
	}
}

// A registry that does NOT implement the optional seam must behave exactly as
// it did before BOS-973 — the resolver's existing nil-dependency discipline.
func TestResolveSpawnEnvWithoutRecorderIsUnchanged(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	reg := &stubRegistry{accounts: []AccountMeta{{ID: "a1", Provider: "codex", Status: "active", Health: "ok"}}}
	wantErr := errors.New("boom")
	mat := &stubMaterializer{supports: true, materializeErr: wantErr}
	r := NewResolver(reg, mat, zerolog.Nop())

	env, err := r.ResolveSpawnEnv(context.Background(), "a1", "codex", now)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if env != nil {
		t.Fatalf("env = %v, want nil", env)
	}

	// …and the success path stays clean too.
	mat.materializeErr = nil
	mat.env = map[string]string{"CODEX_HOME": "/tmp/a1"}
	if _, err := r.ResolveSpawnEnv(context.Background(), "a1", "codex", now); err != nil {
		t.Fatalf("ResolveSpawnEnv without recorder: %v", err)
	}
}

// A failing health write must never turn a degradable resolve into a different
// error: the caller's degrade policy keys on the materialize error.
func TestResolveSpawnEnvRecordErrorDoesNotMaskMaterializeError(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	reg := newRecordingRegistry(AccountMeta{ID: "a1", Provider: "codex", Status: "active", Health: "ok"})
	reg.recordErr = errors.New("db is down")
	reg.clearErr = errors.New("db is down")
	wantErr := errors.New("materialize boom")
	mat := &stubMaterializer{supports: true, materializeErr: wantErr}
	r := NewResolver(reg, mat, zerolog.Nop())

	if _, err := r.ResolveSpawnEnv(context.Background(), "a1", "codex", now); !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want the materialize error %v", err, wantErr)
	}

	mat.materializeErr = nil
	mat.env = map[string]string{"CODEX_HOME": "/tmp/a1"}
	if _, err := r.ResolveSpawnEnv(context.Background(), "a1", "codex", now); err != nil {
		t.Fatalf("a failing clear must not fail the resolve: %v", err)
	}
}
