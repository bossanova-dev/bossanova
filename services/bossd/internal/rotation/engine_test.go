package rotation

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/models"
	libtelemetry "github.com/recurser/bossalib/telemetry"
)

type rotationTelemetryRecorder struct{ properties []map[string]any }

func (r *rotationTelemetryRecorder) Capture(_ context.Context, _ libtelemetry.Event, _ string, props map[string]any) {
	r.properties = append(r.properties, props)
}
func (*rotationTelemetryRecorder) Identify(context.Context, string, map[string]any) {}
func (*rotationTelemetryRecorder) Alias(context.Context, string, string)            {}
func (*rotationTelemetryRecorder) Close()                                           {}

func enableRotationTelemetry(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("BOSS_SETTINGS_PATH", filepath.Join(t.TempDir(), "settings.json"))
	s := config.DefaultSettings()
	s.EventTracingEnabled = true
	if err := config.Save(s); err != nil {
		t.Fatal(err)
	}
}

var base = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func fixedClock() func() time.Time { return func() time.Time { return base } }

func tp(d time.Duration) *time.Time { t := base.Add(d); return &t }

// mkAcct builds a claude-provider account for selection tests.
func mkAcct(id string, priority int, health models.AccountHealth, status models.AccountStatus, cooldown, lastUsed *time.Time) *models.Account {
	return &models.Account{
		ID:            id,
		Provider:      models.AccountProviderClaude,
		Priority:      priority,
		Health:        health,
		Status:        status,
		CooldownUntil: cooldown,
		LastUsedAt:    lastUsed,
	}
}

// withReset7d attaches a weekly-quota reset instant to an account's usage
// snapshot, for the BOS-429 weekly-expiry ordering cases.
func withReset7d(a *models.Account, reset *time.Time) *models.Account {
	a.Usage = &models.UsageSnapshot{Reset7d: reset}
	return a
}

const claude = "claude"

func newEngineForTest(store *fakeStore, opts ...Option) *Engine {
	return NewEngine(store, append([]Option{WithClock(fixedClock())}, opts...)...)
}

func TestDecideTelemetrySwapNoopAndNil(t *testing.T) {
	enableRotationTelemetry(t)
	ok, active := models.AccountHealthOK, models.AccountStatusActive
	store := newFakeStore(mkAcct("capped", 0, ok, active, nil, tp(0)), mkAcct("next", 1, ok, active, nil, tp(-time.Hour)))
	recorder := &rotationTelemetryRecorder{}
	eng := newEngineForTest(store, WithTelemetry(recorder))
	out, err := eng.Decide(context.Background(), Signal{Provider: claude, CappedAccountID: "capped", Kind: UsageLimited, ResetAt: tp(time.Hour), RotationCapable: true})
	if err != nil || out.Kind != OutcomeRotate {
		t.Fatalf("Decide = %#v, %v", out, err)
	}
	if len(recorder.properties) != 0 {
		t.Fatalf("Decide must not capture before a switch: %#v", recorder.properties)
	}
	eng.CaptureReactiveRotation(context.Background(), claude, "usage_limit")
	if len(recorder.properties) != 1 || recorder.properties[0]["rotation_reason"] != "usage_limit" || recorder.properties[0]["provider"] != "claude" {
		t.Fatalf("captures = %#v", recorder.properties)
	}
	if _, err := eng.Decide(context.Background(), Signal{Provider: claude, CappedAccountID: "capped", RotationCapable: false}); err != nil {
		t.Fatal(err)
	}
	if len(recorder.properties) != 1 {
		t.Fatalf("no-op added capture: %#v", recorder.properties)
	}
	nilEngine := newEngineForTest(newFakeStore(mkAcct("a", 0, ok, active, nil, tp(0)), mkAcct("b", 1, ok, active, nil, tp(-time.Hour))))
	if _, err := nilEngine.Decide(context.Background(), Signal{Provider: claude, CappedAccountID: "a", Kind: UsageLimited, ResetAt: tp(time.Hour), RotationCapable: true}); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureProactiveRotationTelemetry(t *testing.T) {
	enableRotationTelemetry(t)
	recorder := &rotationTelemetryRecorder{}
	eng := newEngineForTest(newFakeStore(), WithTelemetry(recorder))
	eng.CaptureProactiveRotation(context.Background(), claude)
	if len(recorder.properties) != 1 {
		t.Fatalf("captures = %#v, want one", recorder.properties)
	}
	props := recorder.properties[0]
	if props["rotation_reason"] != "proactive" || props["provider"] != claude || props["status"] != "rotated" {
		t.Errorf("properties = %#v", props)
	}
}

func TestTelemetryProviderIsBounded(t *testing.T) {
	for input, want := range map[string]string{"claude": "claude", "codex": "codex", "opencode": "opencode", "plugin-x": "other", "": "other"} {
		if got := telemetryProvider(input); got != want {
			t.Errorf("telemetryProvider(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestDecideOrdering(t *testing.T) {
	ok := models.AccountHealthOK
	failed := models.AccountHealthFailed
	active := models.AccountStatusActive
	disabled := models.AccountStatusDisabled

	tests := []struct {
		name     string
		accts    []*models.Account
		capped   string
		wantNext string
	}{
		{
			name: "priority tie broken by LRU (nil last-used first)",
			accts: []*models.Account{
				mkAcct("capped", 0, ok, active, nil, tp(0)),
				mkAcct("A", 1, ok, active, nil, tp(-1*time.Hour)),
				mkAcct("B", 1, ok, active, nil, nil), // nil last-used => most stale
			},
			capped:   "capped",
			wantNext: "B",
		},
		{
			name: "lower-priority healthy chosen over higher-priority cooling",
			accts: []*models.Account{
				mkAcct("capped", 2, ok, active, nil, tp(0)),
				mkAcct("high", 0, ok, active, tp(30*time.Minute), tp(-1*time.Hour)),
				mkAcct("low", 1, ok, active, nil, tp(-1*time.Hour)),
			},
			capped:   "capped",
			wantNext: "low",
		},
		{
			name: "failed account skipped",
			accts: []*models.Account{
				mkAcct("capped", 2, ok, active, nil, tp(0)),
				mkAcct("F", 0, failed, active, nil, tp(-2*time.Hour)),
				mkAcct("G", 1, ok, active, nil, tp(-1*time.Hour)),
			},
			capped:   "capped",
			wantNext: "G",
		},
		{
			name: "disabled account skipped",
			accts: []*models.Account{
				mkAcct("capped", 2, ok, active, nil, tp(0)),
				mkAcct("D", 0, ok, disabled, nil, tp(-2*time.Hour)),
				mkAcct("E", 1, ok, active, nil, tp(-1*time.Hour)),
			},
			capped:   "capped",
			wantNext: "E",
		},
		{
			name: "capped account itself skipped",
			accts: []*models.Account{
				mkAcct("capped", 0, ok, active, nil, tp(-2*time.Hour)),
				mkAcct("O", 1, ok, active, nil, tp(-1*time.Hour)),
			},
			capped:   "capped",
			wantNext: "O",
		},
		{
			// BOS-429: within one priority band the soonest FUTURE weekly reset
			// wins even though A is the idler account (LRU would pick A). Expiry
			// sits above LRU, so B (resets in 2h) beats A (resets in 5h).
			name: "weekly-expiry: soonest future reset beats LRU",
			accts: []*models.Account{
				mkAcct("capped", 0, ok, active, nil, tp(0)),
				withReset7d(mkAcct("A", 1, ok, active, nil, tp(-2*time.Hour)), tp(5*time.Hour)),
				withReset7d(mkAcct("B", 1, ok, active, nil, tp(-1*time.Hour)), tp(2*time.Hour)),
			},
			capped:   "capped",
			wantNext: "B",
		},
		{
			// A known future reset outranks a nil (never-probed) reset within the
			// band, even though the nil-reset account is the idler.
			name: "weekly-expiry: known future reset beats nil reset",
			accts: []*models.Account{
				mkAcct("capped", 0, ok, active, nil, tp(0)),
				mkAcct("nilreset", 1, ok, active, nil, nil),
				withReset7d(mkAcct("future", 1, ok, active, nil, tp(-1*time.Hour)), tp(3*time.Hour)),
			},
			capped:   "capped",
			wantNext: "future",
		},
		{
			// Explicit Priority still dominates: a later-expiring priority-1 account
			// beats a sooner-expiring priority-2 one.
			name: "weekly-expiry: explicit priority dominates",
			accts: []*models.Account{
				mkAcct("capped", 0, ok, active, nil, tp(0)),
				withReset7d(mkAcct("p2soon", 2, ok, active, nil, nil), tp(1*time.Hour)),
				withReset7d(mkAcct("p1later", 1, ok, active, nil, nil), tp(9*time.Hour)),
			},
			capped:   "capped",
			wantNext: "p1later",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eng := newEngineForTest(newFakeStore(tt.accts...))
			out, err := eng.Decide(context.Background(), Signal{
				Provider:        claude,
				CappedAccountID: tt.capped,
				Kind:            UsageLimited,
				RotationCapable: true,
			})
			if err != nil {
				t.Fatalf("Decide returned error: %v", err)
			}
			if out.Kind != OutcomeRotate {
				t.Fatalf("Kind = %v, want OutcomeRotate", out.Kind)
			}
			if out.NextAccount == nil || out.NextAccount.ID != tt.wantNext {
				got := "<nil>"
				if out.NextAccount != nil {
					got = out.NextAccount.ID
				}
				t.Fatalf("NextAccount = %s, want %s", got, tt.wantNext)
			}
			if out.CappedAccountID != tt.capped {
				t.Fatalf("CappedAccountID = %s, want %s", out.CappedAccountID, tt.capped)
			}
		})
	}
}

// TestDecide_EmptyCappedAccount_RotatesWithoutCooling pins the unbound-session
// contract (BOS-320): a UsageLimited signal with no CappedAccountID (an unbound
// session has no bound account to cool) still selects an eligible rotation
// target but writes no cooldown.
func TestDecide_EmptyCappedAccount_RotatesWithoutCooling(t *testing.T) {
	ok := models.AccountHealthOK
	active := models.AccountStatusActive
	// Two eligible claude accounts, no capped account (unbound session).
	a := mkAcct("a", 0, ok, active, nil, tp(-1*time.Hour))
	b := mkAcct("b", 1, ok, active, nil, tp(-2*time.Hour))
	store := newFakeStore(a, b)
	eng := newEngineForTest(store)

	out, err := eng.Decide(context.Background(), Signal{
		Provider:        claude,
		CappedAccountID: "", // unbound
		Kind:            UsageLimited,
		RotationCapable: true,
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if out.Kind != OutcomeRotate || out.NextAccount == nil {
		t.Fatalf("Kind=%v NextAccount=%v, want OutcomeRotate with a target", out.Kind, out.NextAccount)
	}
	if out.CooldownApplied {
		t.Errorf("CooldownApplied = true, want false for empty capped account")
	}
	if store.cooldownCalls != 0 {
		t.Errorf("SetCooldownIfNotCooling called %d times, want 0 for empty capped account", store.cooldownCalls)
	}
	if store.writes != 0 {
		t.Errorf("store.writes = %d, want 0 (nothing to cool)", store.writes)
	}
}

func TestSelectProactiveCandidate_WritesNoCooldown(t *testing.T) {
	ok := models.AccountHealthOK
	active := models.AccountStatusActive
	// Bound account A is nearing its cap (0.85) but NOT exhausted; candidates
	// B (0.2) and C (0.9). B is the idlest selectable candidate.
	a := mkAcct("A", 0, ok, active, nil, tp(0))
	b := mkAcct("B", 1, ok, active, nil, tp(-1*time.Hour))
	c := mkAcct("C", 2, ok, active, nil, tp(-2*time.Hour))
	store := newFakeStore(a, b, c)
	eng := newEngineForTest(store)

	chosen, err := eng.SelectProactiveCandidate(context.Background(), claude, "A",
		map[string]float64{"A": 0.85, "B": 0.2, "C": 0.9})
	if err != nil {
		t.Fatalf("SelectProactiveCandidate error: %v", err)
	}
	if chosen == nil || chosen.ID != "B" {
		got := "<nil>"
		if chosen != nil {
			got = chosen.ID
		}
		t.Fatalf("chosen = %s, want B (lowest util)", got)
	}
	// The bound account must NOT be cooled by the read-only proactive path.
	if a.CooldownUntil != nil {
		t.Errorf("bound account A.CooldownUntil = %v, want nil (proactive path writes no cooldown)", *a.CooldownUntil)
	}
	if store.writes != 0 {
		t.Errorf("store.writes = %d, want 0 (proactive selection is read-only)", store.writes)
	}
}

func TestSelectProactiveCandidate_EmptyUtilizationYieldsNil(t *testing.T) {
	ok := models.AccountHealthOK
	active := models.AccountStatusActive
	a := mkAcct("A", 0, ok, active, nil, tp(0))
	b := mkAcct("B", 1, ok, active, nil, tp(-1*time.Hour))
	store := newFakeStore(a, b)
	eng := newEngineForTest(store)

	chosen, err := eng.SelectProactiveCandidate(context.Background(), claude, "A", nil)
	if err != nil {
		t.Fatalf("SelectProactiveCandidate error: %v", err)
	}
	if chosen != nil {
		t.Errorf("chosen = %s, want nil (a proactive switch must never target an unprobed account)", chosen.ID)
	}
}

func TestSelectProactiveCandidate_OnlyCoolingOrFailedYieldsNil(t *testing.T) {
	ok := models.AccountHealthOK
	failed := models.AccountHealthFailed
	active := models.AccountStatusActive
	a := mkAcct("A", 0, ok, active, nil, tp(0))
	cooling := mkAcct("cooling", 1, ok, active, tp(2*time.Hour), tp(-1*time.Hour))
	broken := mkAcct("broken", 2, failed, active, nil, tp(-2*time.Hour))
	store := newFakeStore(a, cooling, broken)
	eng := newEngineForTest(store)

	chosen, err := eng.SelectProactiveCandidate(context.Background(), claude, "A",
		map[string]float64{"A": 0.85, "cooling": 0.1, "broken": 0.1})
	if err != nil {
		t.Fatalf("SelectProactiveCandidate error: %v", err)
	}
	if chosen != nil {
		t.Errorf("chosen = %s, want nil (only cooling/failed candidates exist)", chosen.ID)
	}
}

func TestDecideUtilizationAwareSelection(t *testing.T) {
	ok := models.AccountHealthOK
	active := models.AccountStatusActive
	devops := mkAcct("devops", 0, ok, active, nil, tp(0))
	yuki := mkAcct("yuki", 1, ok, active, nil, tp(-1*time.Hour))
	near := mkAcct("near", 2, ok, active, nil, tp(-2*time.Hour))
	exhausted := mkAcct("exhausted", 3, ok, active, nil, tp(-3*time.Hour))
	store := newFakeStore(devops, near, exhausted, yuki)
	eng := newEngineForTest(store)

	out, err := eng.Decide(context.Background(), Signal{
		Provider:        claude,
		CappedAccountID: "devops",
		Kind:            UsageLimited,
		ResetAt:         tp(90 * time.Minute),
		RotationCapable: true,
		Utilization: map[string]float64{
			"near":      0.72,
			"exhausted": 1,
			"yuki":      0.08,
		},
	})
	if err != nil {
		t.Fatalf("Decide error: %v", err)
	}
	if out.Kind != OutcomeRotate {
		t.Fatalf("Kind = %v, want OutcomeRotate", out.Kind)
	}
	if out.NextAccount == nil || out.NextAccount.ID != "yuki" {
		t.Fatalf("NextAccount = %v, want yuki (lowest healthy utilization)", out.NextAccount)
	}
}

func TestDecideUtilizationSkipsUnprobedCandidates(t *testing.T) {
	ok := models.AccountHealthOK
	active := models.AccountStatusActive
	devops := mkAcct("devops", 0, ok, active, nil, tp(0))
	unknown := mkAcct("unknown", 1, ok, active, nil, tp(-1*time.Hour))
	exhausted := mkAcct("exhausted", 2, ok, active, nil, tp(-2*time.Hour))
	store := newFakeStore(devops, unknown, exhausted)
	eng := newEngineForTest(store)

	out, err := eng.Decide(context.Background(), Signal{
		Provider:        claude,
		CappedAccountID: "devops",
		Kind:            UsageLimited,
		ResetAt:         tp(90 * time.Minute),
		RotationCapable: true,
		Utilization: map[string]float64{
			"exhausted": 1,
		},
	})
	if err != nil {
		t.Fatalf("Decide error: %v", err)
	}
	if out.Kind != OutcomeAllExhausted {
		t.Fatalf("Kind = %v, want OutcomeAllExhausted", out.Kind)
	}
	if out.NextAccount != nil {
		t.Fatalf("NextAccount = %v, want nil because unknown candidate was not probed", out.NextAccount)
	}
}

func TestDecideEmptyUtilizationPreservesLegacyUnlessProbeRequired(t *testing.T) {
	ok := models.AccountHealthOK
	active := models.AccountStatusActive

	t.Run("legacy empty map", func(t *testing.T) {
		devops := mkAcct("devops", 0, ok, active, nil, tp(0))
		yuki := mkAcct("yuki", 1, ok, active, nil, tp(-1*time.Hour))
		store := newFakeStore(devops, yuki)
		eng := newEngineForTest(store)

		out, err := eng.Decide(context.Background(), Signal{
			Provider:        claude,
			CappedAccountID: "devops",
			Kind:            UsageLimited,
			ResetAt:         tp(90 * time.Minute),
			RotationCapable: true,
			Utilization:     map[string]float64{},
		})
		if err != nil {
			t.Fatalf("Decide error: %v", err)
		}
		if out.Kind != OutcomeRotate || out.NextAccount == nil || out.NextAccount.ID != "yuki" {
			t.Fatalf("out = %#v, want legacy rotate to yuki", out)
		}
	})

	t.Run("candidate probe required", func(t *testing.T) {
		devops := mkAcct("devops", 0, ok, active, nil, tp(0))
		yuki := mkAcct("yuki", 1, ok, active, nil, tp(-1*time.Hour))
		store := newFakeStore(devops, yuki)
		eng := newEngineForTest(store)

		out, err := eng.Decide(context.Background(), Signal{
			Provider:               claude,
			CappedAccountID:        "devops",
			Kind:                   UsageLimited,
			ResetAt:                tp(90 * time.Minute),
			RotationCapable:        true,
			Utilization:            map[string]float64{},
			CandidateProbeRequired: true,
		})
		if err != nil {
			t.Fatalf("Decide error: %v", err)
		}
		if out.Kind != OutcomeAllExhausted {
			t.Fatalf("Kind = %v, want OutcomeAllExhausted", out.Kind)
		}
		if out.NextAccount != nil {
			t.Fatalf("NextAccount = %v, want nil because candidate probe was required", out.NextAccount)
		}
	})
}

func TestUsageSnapshotConfirmsLimitedExactStatus(t *testing.T) {
	fetched := base
	if !UsageSnapshotConfirmsLimited(models.UsageSnapshot{
		Status:    "RATE_LIMIT_PLAN_STATUS_RATE_LIMITED",
		FetchedAt: &fetched,
	}) {
		t.Fatal("RATE_LIMIT_PLAN_STATUS_RATE_LIMITED should confirm limited")
	}
	if UsageSnapshotConfirmsLimited(models.UsageSnapshot{
		Status:    "NOT_RATE_LIMITED",
		FetchedAt: &fetched,
	}) {
		t.Fatal("NOT_RATE_LIMITED must not confirm limited by substring")
	}
}

func TestDecideCooldownDuration(t *testing.T) {
	ok := models.AccountHealthOK
	active := models.AccountStatusActive

	tests := []struct {
		name      string
		resetAt   *time.Time
		opts      []Option
		wantUntil time.Time
	}{
		{name: "ResetAt honored", resetAt: tp(90 * time.Minute), wantUntil: base.Add(90 * time.Minute)},
		{name: "nil ResetAt uses 60m default", resetAt: nil, wantUntil: base.Add(60 * time.Minute)},
		{name: "WithDefaultCooldown override honored", resetAt: nil, opts: []Option{WithDefaultCooldown(5 * time.Minute)}, wantUntil: base.Add(5 * time.Minute)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capped := mkAcct("capped", 0, ok, active, nil, tp(0))
			other := mkAcct("B", 1, ok, active, nil, tp(-1*time.Hour))
			store := newFakeStore(capped, other)
			eng := newEngineForTest(store, tt.opts...)

			out, err := eng.Decide(context.Background(), Signal{
				Provider:        claude,
				CappedAccountID: "capped",
				Kind:            UsageLimited,
				ResetAt:         tt.resetAt,
				RotationCapable: true,
			})
			if err != nil {
				t.Fatalf("Decide error: %v", err)
			}
			if !out.CooldownApplied {
				t.Fatalf("CooldownApplied = false, want true")
			}
			if capped.CooldownUntil == nil || !capped.CooldownUntil.Equal(tt.wantUntil) {
				t.Fatalf("CooldownUntil = %v, want %v", capped.CooldownUntil, tt.wantUntil)
			}
		})
	}
}

// TestDecideSuppressCooldownBenchesNothingButStillRotates proves the BOS-584
// escape hatch: a UsageLimited signal whose dispatcher could NOT confirm the cap
// (a transient upstream 429) still picks a rotation target, but writes no
// cooldown at all — the capped account stays selectable the instant the signal
// is handled. SuppressCooldown must gate ONLY the cooldown write; candidate
// selection, health, and the outcome kind are untouched.
func TestDecideSuppressCooldownBenchesNothingButStillRotates(t *testing.T) {
	ok := models.AccountHealthOK
	active := models.AccountStatusActive

	tests := []struct {
		name             string
		suppressCooldown bool
		wantCooling      bool
		wantCooldownCall int
	}{
		{name: "suppressed cap benches nothing", suppressCooldown: true, wantCooling: false, wantCooldownCall: 0},
		{name: "unsuppressed cap benches as before", suppressCooldown: false, wantCooling: true, wantCooldownCall: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capped := mkAcct("capped", 0, ok, active, nil, tp(0))
			other := mkAcct("B", 1, ok, active, nil, tp(-1*time.Hour))
			store := newFakeStore(capped, other)
			eng := newEngineForTest(store)

			out, err := eng.Decide(context.Background(), Signal{
				Provider:         claude,
				CappedAccountID:  "capped",
				Kind:             UsageLimited,
				RotationCapable:  true,
				SuppressCooldown: tt.suppressCooldown,
			})
			if err != nil {
				t.Fatalf("Decide error: %v", err)
			}
			if out.Kind != OutcomeRotate {
				t.Fatalf("Kind = %v, want OutcomeRotate (suppression must not stop rotation)", out.Kind)
			}
			if out.NextAccount == nil || out.NextAccount.ID != "B" {
				t.Fatalf("NextAccount = %v, want B", out.NextAccount)
			}
			if got := capped.CooldownUntil != nil; got != tt.wantCooling {
				t.Fatalf("capped cooling = %v (until %v), want %v", got, capped.CooldownUntil, tt.wantCooling)
			}
			if store.cooldownCalls != tt.wantCooldownCall {
				t.Fatalf("cooldownCalls = %d, want %d", store.cooldownCalls, tt.wantCooldownCall)
			}
			if out.CooldownApplied != tt.wantCooling {
				t.Fatalf("CooldownApplied = %v, want %v", out.CooldownApplied, tt.wantCooling)
			}
			if capped.Health != models.AccountHealthOK {
				t.Fatalf("capped.Health = %v, want ok (suppression never fails health)", capped.Health)
			}
		})
	}
}

// TestDecideSuppressCooldownOnLastAccountShiftsKind pins the one way suppression
// DOES change the outcome kind. Kind is resolved after the write from the
// account list, and minFutureCooldown only sees future cooldowns — so when the
// capped account is the last selectable one, the cooldown this signal declined
// to write is also the cooldown that would have produced OutcomeAllExhausted.
// The single-account pool is the common deployment and was the incident's own
// shape, so pin it rather than leave it to a reader's inference.
func TestDecideSuppressCooldownOnLastAccountShiftsKind(t *testing.T) {
	ok := models.AccountHealthOK
	active := models.AccountStatusActive

	tests := []struct {
		name             string
		suppressCooldown bool
		wantKind         OutcomeKind
	}{
		{name: "confirmed cap on the last account is exhausted", suppressCooldown: false, wantKind: OutcomeAllExhausted},
		{name: "suppressed cap on the last account is no-eligible", suppressCooldown: true, wantKind: OutcomeNoEligibleAccount},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			only := mkAcct("only", 0, ok, active, nil, tp(0))
			eng := newEngineForTest(newFakeStore(only))

			out, err := eng.Decide(context.Background(), Signal{
				Provider:         claude,
				CappedAccountID:  "only",
				Kind:             UsageLimited,
				RotationCapable:  true,
				SuppressCooldown: tt.suppressCooldown,
			})
			if err != nil {
				t.Fatalf("Decide error: %v", err)
			}
			if out.NextAccount != nil {
				t.Fatalf("NextAccount = %v, want nil (no alternate exists)", out.NextAccount)
			}
			if out.Kind != tt.wantKind {
				t.Fatalf("Kind = %v, want %v", out.Kind, tt.wantKind)
			}
			// Either way the account must not be left benched by a suppressed cap.
			if got := only.CooldownUntil != nil; got == tt.suppressCooldown {
				t.Fatalf("cooling = %v, want %v", got, !tt.suppressCooldown)
			}
		})
	}
}

// gatedStore parks every DecideTx caller until the test releases it, so two
// signals can be held in flight SIMULTANEOUSLY. Entering DecideTx at all is the
// proof a caller was not coalesced away by single-flight.
type gatedStore struct {
	*fakeStore
	entered chan struct{}
	release chan struct{}
}

func (g *gatedStore) DecideTx(ctx context.Context, provider string, fn func(tx TxAccountView) error) error {
	g.entered <- struct{}{}
	<-g.release
	return g.fakeStore.DecideTx(ctx, provider, fn)
}

// TestDecideConcurrentSuppressionDoesNotCoalesce pins the BOS-584 half of the
// single-flight key. SuppressCooldown selects a DIFFERENT write, not a duplicate
// of the same one, so two concurrent signals that disagree about whether the
// account may be benched must each execute:
//
//   - if the suppressing signal won, the non-suppressing caller would get
//     CooldownApplied=false, which the headless initial-cap path reads as "a
//     duplicate the engine already handled" and answers by skipping its restart —
//     stalling the session instead of rotating it;
//   - if the non-suppressing signal won, an account whose probe said it was
//     healthy would be benched anyway, i.e. the exact bug suppression prevents.
//
// Both callers are held inside DecideTx at once, so a coalesced second caller
// never arrives and the test fails deterministically rather than flakily.
func TestDecideConcurrentSuppressionDoesNotCoalesce(t *testing.T) {
	ok := models.AccountHealthOK
	active := models.AccountStatusActive
	capped := mkAcct("capped", 0, ok, active, nil, tp(0))
	other := mkAcct("B", 1, ok, active, nil, tp(-1*time.Hour))
	gated := &gatedStore{
		fakeStore: newFakeStore(capped, other),
		entered:   make(chan struct{}, 2),
		release:   make(chan struct{}),
	}
	eng := NewEngine(gated, WithClock(fixedClock()))

	sig := func(suppress bool) Signal {
		return Signal{
			Provider: claude, CappedAccountID: "capped", Kind: UsageLimited,
			RotationCapable: true, SuppressCooldown: suppress,
		}
	}
	outs := make([]Outcome, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, suppress := range []bool{true, false} {
		wg.Add(1)
		go func(idx int, s bool) {
			defer wg.Done()
			outs[idx], errs[idx] = eng.Decide(context.Background(), sig(s))
		}(i, suppress)
		// Wait for THIS caller to reach the store before launching the next, so
		// the second call provably overlaps the first's in-flight execution.
		select {
		case <-gated.entered:
		case <-time.After(5 * time.Second):
			close(gated.release)
			wg.Wait()
			t.Fatalf("caller %d never reached the store: the signals coalesced", i)
		}
	}
	close(gated.release)
	wg.Wait()

	for i := range outs {
		if errs[i] != nil {
			t.Fatalf("caller %d error: %v", i, errs[i])
		}
	}
	if outs[0].CooldownApplied {
		t.Fatal("suppressing caller CooldownApplied = true, want false")
	}
	if !outs[1].CooldownApplied {
		t.Fatal("non-suppressing caller CooldownApplied = false, want true (it must not inherit the suppressing verdict)")
	}
	if capped.CooldownUntil == nil {
		t.Fatal("capped.CooldownUntil = nil, want set by the confirmed cap")
	}
	if gated.cooldownCalls != 1 {
		t.Fatalf("cooldownCalls = %d, want 1 (only the non-suppressing signal writes)", gated.cooldownCalls)
	}
}

func TestDecideAuthInvalidatedVsQuota(t *testing.T) {
	ok := models.AccountHealthOK
	active := models.AccountStatusActive

	t.Run("auth invalidation fails health, no cooldown, never re-selected", func(t *testing.T) {
		a := mkAcct("A", 0, ok, active, nil, tp(-3*time.Hour))
		b := mkAcct("B", 1, ok, active, nil, tp(-2*time.Hour))
		c := mkAcct("C", 2, ok, active, nil, tp(-1*time.Hour))
		store := newFakeStore(a, b, c)
		eng := newEngineForTest(store)

		out, err := eng.Decide(context.Background(), Signal{
			Provider: claude, CappedAccountID: "A", Kind: AuthInvalidated, RotationCapable: true,
		})
		if err != nil {
			t.Fatalf("Decide error: %v", err)
		}
		if a.Health != models.AccountHealthFailed {
			t.Fatalf("A.Health = %v, want failed", a.Health)
		}
		if a.CooldownUntil != nil {
			t.Fatalf("A.CooldownUntil = %v, want nil (auth invalidation sets no cooldown)", a.CooldownUntil)
		}
		if out.CooldownApplied {
			t.Fatalf("CooldownApplied = true, want false for auth invalidation")
		}
		if out.NextAccount == nil || out.NextAccount.ID != "B" {
			t.Fatalf("NextAccount = %v, want B", out.NextAccount)
		}

		// Subsequent decision (cap B) must never re-select the failed A.
		out2, err := eng.Decide(context.Background(), Signal{
			Provider: claude, CappedAccountID: "B", Kind: UsageLimited, RotationCapable: true,
		})
		if err != nil {
			t.Fatalf("second Decide error: %v", err)
		}
		if out2.NextAccount == nil || out2.NextAccount.ID != "C" {
			t.Fatalf("second NextAccount = %v, want C (A stays failed)", out2.NextAccount)
		}
	})

	t.Run("usage limit sets cooldown, leaves health untouched", func(t *testing.T) {
		a := mkAcct("A", 0, ok, active, nil, tp(-1*time.Hour))
		b := mkAcct("B", 1, ok, active, nil, tp(-2*time.Hour))
		store := newFakeStore(a, b)
		eng := newEngineForTest(store)

		if _, err := eng.Decide(context.Background(), Signal{
			Provider: claude, CappedAccountID: "A", Kind: UsageLimited, RotationCapable: true,
		}); err != nil {
			t.Fatalf("Decide error: %v", err)
		}
		if a.Health != models.AccountHealthOK {
			t.Fatalf("A.Health = %v, want ok (usage limit must not fail health)", a.Health)
		}
		if a.CooldownUntil == nil {
			t.Fatalf("A.CooldownUntil = nil, want set")
		}
	})
}

func TestDecideTerminal(t *testing.T) {
	ok := models.AccountHealthOK
	failed := models.AccountHealthFailed
	active := models.AccountStatusActive

	t.Run("all cooling => AllExhausted with min future cooldown", func(t *testing.T) {
		a := mkAcct("A", 0, ok, active, nil, tp(-1*time.Hour))
		b := mkAcct("B", 1, ok, active, tp(30*time.Minute), tp(-2*time.Hour))
		c := mkAcct("C", 2, ok, active, tp(90*time.Minute), tp(-3*time.Hour))
		store := newFakeStore(a, b, c)
		eng := newEngineForTest(store)

		out, err := eng.Decide(context.Background(), Signal{
			Provider: claude, CappedAccountID: "A", Kind: UsageLimited,
			ResetAt: tp(120 * time.Minute), RotationCapable: true,
		})
		if err != nil {
			t.Fatalf("Decide error: %v", err)
		}
		if out.Kind != OutcomeAllExhausted {
			t.Fatalf("Kind = %v, want OutcomeAllExhausted", out.Kind)
		}
		if out.NextAccount != nil {
			t.Fatalf("NextAccount = %v, want nil", out.NextAccount)
		}
		wantResume := base.Add(30 * time.Minute)
		if !out.ResumeAt.Equal(wantResume) {
			t.Fatalf("ResumeAt = %v, want %v (min future cooldown)", out.ResumeAt, wantResume)
		}
	})

	t.Run("auth-invalidated with a cooling healthy sibling => AllExhausted", func(t *testing.T) {
		// Plan step 4: AuthInvalidated on the capped account, no healthy
		// not-cooling candidate, but a healthy+active sibling is merely cooling =>
		// OutcomeAllExhausted with ResumeAt = that sibling's future cooldown.
		a := mkAcct("A", 0, ok, active, nil, tp(-1*time.Hour))                // capped, will be failed
		b := mkAcct("B", 1, ok, active, tp(45*time.Minute), tp(-2*time.Hour)) // healthy but cooling
		store := newFakeStore(a, b)
		eng := newEngineForTest(store)

		out, err := eng.Decide(context.Background(), Signal{
			Provider: claude, CappedAccountID: "A", Kind: AuthInvalidated, RotationCapable: true,
		})
		if err != nil {
			t.Fatalf("Decide error: %v", err)
		}
		if a.Health != models.AccountHealthFailed {
			t.Fatalf("A.Health = %v, want failed", a.Health)
		}
		if out.Kind != OutcomeAllExhausted {
			t.Fatalf("Kind = %v, want OutcomeAllExhausted", out.Kind)
		}
		wantResume := base.Add(45 * time.Minute)
		if !out.ResumeAt.Equal(wantResume) {
			t.Fatalf("ResumeAt = %v, want %v (cooling sibling B)", out.ResumeAt, wantResume)
		}
	})

	t.Run("all permanently failed => NoEligibleAccount", func(t *testing.T) {
		a := mkAcct("A", 0, ok, active, nil, tp(-1*time.Hour))
		b := mkAcct("B", 1, failed, active, nil, tp(-2*time.Hour))
		c := mkAcct("C", 2, failed, active, nil, tp(-3*time.Hour))
		store := newFakeStore(a, b, c)
		eng := newEngineForTest(store)

		out, err := eng.Decide(context.Background(), Signal{
			Provider: claude, CappedAccountID: "A", Kind: AuthInvalidated, RotationCapable: true,
		})
		if err != nil {
			t.Fatalf("Decide error: %v", err)
		}
		// No candidate and none recovering: the fall-through must report the
		// no-eligible-account terminal state, distinct from the capability
		// short-circuit's OutcomeStatusOnly (BOS-327).
		if out.Kind != OutcomeNoEligibleAccount {
			t.Fatalf("Kind = %v, want OutcomeNoEligibleAccount", out.Kind)
		}
		if !out.ResumeAt.IsZero() {
			t.Fatalf("ResumeAt = %v, want zero", out.ResumeAt)
		}
	})

	t.Run("all disabled => NoEligibleAccount", func(t *testing.T) {
		// Every account (including the capped one) is disabled: no candidate, and
		// none is a recovery candidate (a disabled account is not selectable even
		// after a cooldown expires), so the usage-limited fall-through reports
		// no-eligible rather than exhausted (BOS-327).
		disabled := models.AccountStatusDisabled
		a := mkAcct("A", 0, ok, disabled, nil, tp(-1*time.Hour))
		b := mkAcct("B", 1, ok, disabled, nil, tp(-2*time.Hour))
		c := mkAcct("C", 2, ok, disabled, nil, tp(-3*time.Hour))
		store := newFakeStore(a, b, c)
		eng := newEngineForTest(store)

		out, err := eng.Decide(context.Background(), Signal{
			Provider: claude, CappedAccountID: "A", Kind: UsageLimited, RotationCapable: true,
		})
		if err != nil {
			t.Fatalf("Decide error: %v", err)
		}
		if out.Kind != OutcomeNoEligibleAccount {
			t.Fatalf("Kind = %v, want OutcomeNoEligibleAccount", out.Kind)
		}
		if !out.ResumeAt.IsZero() {
			t.Fatalf("ResumeAt = %v, want zero", out.ResumeAt)
		}
	})
}

func TestDecideCapabilityAbsent(t *testing.T) {
	ok := models.AccountHealthOK
	active := models.AccountStatusActive
	store := newFakeStore(
		mkAcct("A", 0, ok, active, nil, tp(-1*time.Hour)),
		mkAcct("B", 1, ok, active, nil, tp(-2*time.Hour)),
	)
	eng := newEngineForTest(store)

	out, err := eng.Decide(context.Background(), Signal{
		Provider: claude, CappedAccountID: "A", Kind: UsageLimited, RotationCapable: false,
	})
	if err != nil {
		t.Fatalf("Decide error: %v", err)
	}
	if out.Kind != OutcomeStatusOnly {
		t.Fatalf("Kind = %v, want OutcomeStatusOnly", out.Kind)
	}
	if out.CappedAccountID != "A" {
		t.Fatalf("CappedAccountID = %s, want A", out.CappedAccountID)
	}
	if store.txCalls != 0 {
		t.Fatalf("txCalls = %d, want 0 (capability short-circuit must not touch the store)", store.txCalls)
	}
	if store.writes != 0 {
		t.Fatalf("writes = %d, want 0", store.writes)
	}
}

func TestDecideIdempotentDuplicateSignal(t *testing.T) {
	ok := models.AccountHealthOK
	active := models.AccountStatusActive
	a := mkAcct("A", 0, ok, active, nil, tp(-1*time.Hour))
	b := mkAcct("B", 1, ok, active, nil, tp(-2*time.Hour))
	c := mkAcct("C", 2, ok, active, nil, tp(-3*time.Hour))
	store := newFakeStore(a, b, c)
	eng := newEngineForTest(store)

	sig := Signal{Provider: claude, CappedAccountID: "A", Kind: UsageLimited, ResetAt: tp(60 * time.Minute), RotationCapable: true}

	out1, err := eng.Decide(context.Background(), sig)
	if err != nil {
		t.Fatalf("first Decide error: %v", err)
	}
	if !out1.CooldownApplied {
		t.Fatalf("first CooldownApplied = false, want true")
	}

	out2, err := eng.Decide(context.Background(), sig)
	if err != nil {
		t.Fatalf("second Decide error: %v", err)
	}
	if out2.CooldownApplied {
		t.Fatalf("second CooldownApplied = true, want false (duplicate signal)")
	}
	if out1.NextAccount.ID != out2.NextAccount.ID {
		t.Fatalf("NextAccount changed between calls: %s vs %s", out1.NextAccount.ID, out2.NextAccount.ID)
	}
	if store.writes != 1 {
		t.Fatalf("writes = %d, want exactly 1", store.writes)
	}
}

// TestDecideDuplicateReflectsLiveStateNotReplay pins the deliberate contract for
// a late duplicate whose provider state moved on since the original signal: the
// engine is stateless, so the duplicate's NextAccount is recomputed from the
// CURRENT accounts rather than replayed. Scenario: A is capped (cools A, rotates
// to B); then a DISTINCT cap cools B (rotates to C); then A's original signal is
// redelivered. The duplicate must (1) write nothing more for A — the single
// cooldown-write idempotency invariant holds — and (2) report CooldownApplied ==
// false, but its NextAccount is C (the only healthy account now), never the
// now-cooling B. Callers gate real rotation on CooldownApplied; see the
// Outcome.NextAccount contract.
func TestDecideDuplicateReflectsLiveStateNotReplay(t *testing.T) {
	ok := models.AccountHealthOK
	active := models.AccountStatusActive
	a := mkAcct("A", 0, ok, active, nil, tp(-1*time.Hour))
	b := mkAcct("B", 1, ok, active, nil, tp(-2*time.Hour))
	c := mkAcct("C", 2, ok, active, nil, tp(-3*time.Hour))
	store := newFakeStore(a, b, c)
	eng := newEngineForTest(store)

	capA := Signal{Provider: claude, CappedAccountID: "A", Kind: UsageLimited, ResetAt: tp(60 * time.Minute), RotationCapable: true}
	capB := Signal{Provider: claude, CappedAccountID: "B", Kind: UsageLimited, ResetAt: tp(60 * time.Minute), RotationCapable: true}

	out1, err := eng.Decide(context.Background(), capA)
	if err != nil {
		t.Fatalf("cap A error: %v", err)
	}
	if !out1.CooldownApplied || out1.NextAccount == nil || out1.NextAccount.ID != "B" {
		t.Fatalf("cap A: applied=%v next=%v, want applied=true next=B", out1.CooldownApplied, out1.NextAccount)
	}

	out2, err := eng.Decide(context.Background(), capB)
	if err != nil {
		t.Fatalf("cap B error: %v", err)
	}
	if !out2.CooldownApplied || out2.NextAccount == nil || out2.NextAccount.ID != "C" {
		t.Fatalf("cap B: applied=%v next=%v, want applied=true next=C", out2.CooldownApplied, out2.NextAccount)
	}
	if store.writes != 2 {
		t.Fatalf("writes after two distinct caps = %d, want 2", store.writes)
	}

	// A's original signal is redelivered after B was independently cooled.
	dup, err := eng.Decide(context.Background(), capA)
	if err != nil {
		t.Fatalf("duplicate A error: %v", err)
	}
	if dup.CooldownApplied {
		t.Fatalf("duplicate A CooldownApplied = true, want false (already cooling)")
	}
	if store.writes != 2 {
		t.Fatalf("writes after duplicate A = %d, want 2 (no extra cooldown write)", store.writes)
	}
	if dup.NextAccount == nil || dup.NextAccount.ID != "C" {
		t.Fatalf("duplicate A NextAccount = %v, want C (live state; B is now cooling)", dup.NextAccount)
	}
}

func TestDecideConcurrent(t *testing.T) {
	ok := models.AccountHealthOK
	active := models.AccountStatusActive

	for _, n := range []int{8, 32} {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			a := mkAcct("A", 0, ok, active, nil, tp(-1*time.Hour))
			b := mkAcct("B", 1, ok, active, nil, tp(-2*time.Hour))
			c := mkAcct("C", 2, ok, active, nil, tp(-3*time.Hour))
			store := newFakeStore(a, b, c)
			eng := newEngineForTest(store)
			sig := Signal{Provider: claude, CappedAccountID: "A", Kind: UsageLimited, ResetAt: tp(60 * time.Minute), RotationCapable: true}

			outcomes := make([]Outcome, n)
			errs := make([]error, n)
			var wg sync.WaitGroup
			start := make(chan struct{})
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func(idx int) {
					defer wg.Done()
					<-start
					outcomes[idx], errs[idx] = eng.Decide(context.Background(), sig)
				}(i)
			}
			close(start)
			wg.Wait()

			appliedCount := 0
			for i := 0; i < n; i++ {
				if errs[i] != nil {
					t.Fatalf("goroutine %d error: %v", i, errs[i])
				}
				if outcomes[i].Kind != OutcomeRotate {
					t.Fatalf("goroutine %d Kind = %v, want OutcomeRotate", i, outcomes[i].Kind)
				}
				if outcomes[i].NextAccount == nil || outcomes[i].NextAccount.ID != "B" {
					t.Fatalf("goroutine %d NextAccount = %v, want B", i, outcomes[i].NextAccount)
				}
				if outcomes[i].CooldownApplied {
					appliedCount++
				}
			}
			// The persistent guard is the true idempotency proof: exactly one
			// cooldown write hits the store regardless of coalescing.
			if store.writes != 1 {
				t.Fatalf("store.writes = %d, want exactly 1", store.writes)
			}
			// At least one caller observed the fresh apply. (Under singleflight
			// result-sharing more than one caller may see the shared true; the
			// authoritative single-apply invariant is store.writes==1 above.)
			if appliedCount < 1 {
				t.Fatalf("appliedCount = %d, want >= 1", appliedCount)
			}
		})
	}
}

// TestDecideConcurrentDistinctAccounts proves the single-flight key is scoped to
// the (provider, capped account, kind) triple: concurrent caps for DIFFERENT
// accounts on the SAME provider must NOT coalesce — each must get its own
// cooldown write and its own correct CappedAccountID. (Provider-only keying
// would drop the second account's cooldown and hand it the first's decision.)
func TestDecideConcurrentDistinctAccounts(t *testing.T) {
	ok := models.AccountHealthOK
	active := models.AccountStatusActive
	a := mkAcct("A", 0, ok, active, nil, tp(-1*time.Hour))
	b := mkAcct("B", 1, ok, active, nil, tp(-2*time.Hour))
	c := mkAcct("C", 2, ok, active, nil, tp(-3*time.Hour))
	d := mkAcct("D", 3, ok, active, nil, tp(-4*time.Hour))
	store := newFakeStore(a, b, c, d)
	eng := newEngineForTest(store)

	capped := []string{"A", "B"}
	outs := make([]Outcome, len(capped))
	errs := make([]error, len(capped))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, id := range capped {
		wg.Add(1)
		go func(idx int, cappedID string) {
			defer wg.Done()
			<-start
			outs[idx], errs[idx] = eng.Decide(context.Background(), Signal{
				Provider: claude, CappedAccountID: cappedID, Kind: UsageLimited,
				ResetAt: tp(60 * time.Minute), RotationCapable: true,
			})
		}(i, id)
	}
	close(start)
	wg.Wait()

	for i := range capped {
		if errs[i] != nil {
			t.Fatalf("caller %d error: %v", i, errs[i])
		}
		if outs[i].CappedAccountID != capped[i] {
			t.Fatalf("caller %d CappedAccountID = %s, want %s (signals coalesced!)", i, outs[i].CappedAccountID, capped[i])
		}
	}
	if a.CooldownUntil == nil {
		t.Fatalf("A.CooldownUntil = nil, want set (A's cap was dropped)")
	}
	if b.CooldownUntil == nil {
		t.Fatalf("B.CooldownUntil = nil, want set (B's cap was dropped)")
	}
	if store.writes != 2 {
		t.Fatalf("store.writes = %d, want 2 (one per distinct capped account)", store.writes)
	}
}

// TestDecideResumeAtExcludesUnrecoverable proves ResumeAt is the earliest
// cooldown among accounts that will actually be selectable when it expires: a
// cooling-but-failed/disabled account is skipped, so ResumeAt reflects a genuine
// recovery rather than a slot that stays unusable.
func TestDecideResumeAtExcludesUnrecoverable(t *testing.T) {
	ok := models.AccountHealthOK
	failed := models.AccountHealthFailed
	active := models.AccountStatusActive
	disabled := models.AccountStatusDisabled

	// A is capped (cooled to 100m). B is failed but cooling to the EARLIEST time
	// (20m) — must be ignored. C is disabled and cooling to 40m — must be ignored.
	// D is healthy+active cooling to 50m — the true earliest recovery.
	a := mkAcct("A", 0, ok, active, nil, tp(-1*time.Hour))
	b := mkAcct("B", 1, failed, active, tp(20*time.Minute), tp(-2*time.Hour))
	c := mkAcct("C", 2, ok, disabled, tp(40*time.Minute), tp(-3*time.Hour))
	d := mkAcct("D", 3, ok, active, tp(50*time.Minute), tp(-4*time.Hour))
	store := newFakeStore(a, b, c, d)
	eng := newEngineForTest(store)

	out, err := eng.Decide(context.Background(), Signal{
		Provider: claude, CappedAccountID: "A", Kind: UsageLimited,
		ResetAt: tp(100 * time.Minute), RotationCapable: true,
	})
	if err != nil {
		t.Fatalf("Decide error: %v", err)
	}
	if out.Kind != OutcomeAllExhausted {
		t.Fatalf("Kind = %v, want OutcomeAllExhausted", out.Kind)
	}
	wantResume := base.Add(50 * time.Minute)
	if !out.ResumeAt.Equal(wantResume) {
		t.Fatalf("ResumeAt = %v, want %v (earliest recoverable, not the failed/disabled 20m/40m)", out.ResumeAt, wantResume)
	}
}

func TestDecideRestartRecovery(t *testing.T) {
	ok := models.AccountHealthOK
	active := models.AccountStatusActive
	a := mkAcct("A", 0, ok, active, nil, tp(-1*time.Hour))
	b := mkAcct("B", 1, ok, active, nil, tp(-2*time.Hour))
	c := mkAcct("C", 2, ok, active, nil, tp(-3*time.Hour))
	store := newFakeStore(a, b, c)

	// Engine 1 cools A.
	out1, err := newEngineForTest(store).Decide(context.Background(), Signal{
		Provider: claude, CappedAccountID: "A", Kind: UsageLimited, ResetAt: tp(30 * time.Minute), RotationCapable: true,
	})
	if err != nil {
		t.Fatalf("engine1 error: %v", err)
	}
	if out1.NextAccount.ID != "B" {
		t.Fatalf("engine1 NextAccount = %s, want B", out1.NextAccount.ID)
	}

	// A FRESH engine over the SAME store must honor the persisted cooldown.
	out2, err := newEngineForTest(store).Decide(context.Background(), Signal{
		Provider: claude, CappedAccountID: "B", Kind: UsageLimited, ResetAt: tp(90 * time.Minute), RotationCapable: true,
	})
	if err != nil {
		t.Fatalf("engine2 error: %v", err)
	}
	if out2.NextAccount == nil || out2.NextAccount.ID != "C" {
		t.Fatalf("engine2 NextAccount = %v, want C (A still cooling)", out2.NextAccount)
	}

	// A third fresh engine: every account now cooling => terminal from store state only.
	out3, err := newEngineForTest(store).Decide(context.Background(), Signal{
		Provider: claude, CappedAccountID: "C", Kind: UsageLimited, ResetAt: tp(120 * time.Minute), RotationCapable: true,
	})
	if err != nil {
		t.Fatalf("engine3 error: %v", err)
	}
	if out3.Kind != OutcomeAllExhausted {
		t.Fatalf("engine3 Kind = %v, want OutcomeAllExhausted", out3.Kind)
	}
	wantResume := base.Add(30 * time.Minute)
	if !out3.ResumeAt.Equal(wantResume) {
		t.Fatalf("engine3 ResumeAt = %v, want %v", out3.ResumeAt, wantResume)
	}
}

func TestDecideMidTransactionCrashRollsBack(t *testing.T) {
	ok := models.AccountHealthOK
	active := models.AccountStatusActive
	a := mkAcct("A", 0, ok, active, nil, tp(-1*time.Hour))
	b := mkAcct("B", 1, ok, active, nil, tp(-2*time.Hour))
	store := newFakeStore(a, b)
	store.crash = true
	eng := newEngineForTest(store)

	sig := Signal{Provider: claude, CappedAccountID: "A", Kind: UsageLimited, ResetAt: tp(60 * time.Minute), RotationCapable: true}

	if _, err := eng.Decide(context.Background(), sig); err == nil {
		t.Fatalf("Decide error = nil, want crash error")
	}
	if a.CooldownUntil != nil {
		t.Fatalf("A.CooldownUntil = %v, want nil (rolled back)", a.CooldownUntil)
	}
	if store.writes != 0 {
		t.Fatalf("store.writes = %d, want 0 (rolled back)", store.writes)
	}

	// Re-issue after recovery: behaves as a first-time signal.
	store.crash = false
	out, err := eng.Decide(context.Background(), sig)
	if err != nil {
		t.Fatalf("recovered Decide error: %v", err)
	}
	if !out.CooldownApplied {
		t.Fatalf("recovered CooldownApplied = false, want true (first-time signal)")
	}
	if out.NextAccount == nil || out.NextAccount.ID != "B" {
		t.Fatalf("recovered NextAccount = %v, want B", out.NextAccount)
	}
	if store.writes != 1 {
		t.Fatalf("store.writes = %d, want 1", store.writes)
	}
}
