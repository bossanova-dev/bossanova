package config

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

// TestTmuxDeliveryConfig_Defaults pins the two composer-readiness budgets
// (BOS-893) and — more importantly — the fact that they are two, not one.
//
// The session-start budget has to cover tmux spawn, a full interactive
// login-shell init, exec of the agent, node boot, and TUI first paint; measured
// shell init alone ranged 0.75s to 12s on the affected host, so it ships at 45s
// and is deliberately unclamped, because nothing downstream bounds that path.
// The established-send budget runs inside the SendChatMessage RPC, which bosso
// relays under a 30s commandDeadline, so it keeps the historical 5s default and
// is clamped at 20s no matter what an operator writes.
func TestTmuxDeliveryConfig_Defaults(t *testing.T) {
	tests := []struct {
		name      string
		cfg       TmuxDeliveryConfig
		wantStart time.Duration
		wantSend  time.Duration
	}{
		{
			name:      "unset is 45s start, 5s send",
			cfg:       TmuxDeliveryConfig{},
			wantStart: 45 * time.Second,
			wantSend:  5 * time.Second,
		},
		{
			// The plan's worked example: a positive value on either key is
			// honoured verbatim, and neither key moves the other.
			name:      "explicit positives are honoured",
			cfg:       TmuxDeliveryConfig{SessionStartReadyDeadlineSeconds: 90, SendReadyDeadlineSeconds: 15},
			wantStart: 90 * time.Second,
			wantSend:  15 * time.Second,
		},
		{
			// A hand-edited zero or negative must never become a zero or
			// negative duration: that would collapse the readiness wait to "no
			// wait", which is precisely the failure the floor exists to stop.
			name:      "zero falls back on both keys",
			cfg:       TmuxDeliveryConfig{SessionStartReadyDeadlineSeconds: 0, SendReadyDeadlineSeconds: 0},
			wantStart: 45 * time.Second,
			wantSend:  5 * time.Second,
		},
		{
			name:      "negatives fall back on both keys",
			cfg:       TmuxDeliveryConfig{SessionStartReadyDeadlineSeconds: -60, SendReadyDeadlineSeconds: -1},
			wantStart: 45 * time.Second,
			wantSend:  5 * time.Second,
		},
		{
			// Only one key configured leaves the other on its own default —
			// the cross-leak the two-knob split exists to prevent.
			name:      "start only leaves send on its default",
			cfg:       TmuxDeliveryConfig{SessionStartReadyDeadlineSeconds: 120},
			wantStart: 120 * time.Second,
			wantSend:  5 * time.Second,
		},
		{
			name:      "send only leaves start on its default",
			cfg:       TmuxDeliveryConfig{SendReadyDeadlineSeconds: 12},
			wantStart: 45 * time.Second,
			wantSend:  12 * time.Second,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.SessionStartReadyDeadline(); got != tt.wantStart {
				t.Errorf("SessionStartReadyDeadline() = %v, want %v", got, tt.wantStart)
			}
			if got := tt.cfg.SendReadyDeadline(); got != tt.wantSend {
				t.Errorf("SendReadyDeadline() = %v, want %v", got, tt.wantSend)
			}
		})
	}
}

// TestTmuxDeliveryConfig_SendClampAtItsLongestValue asserts the send clamp at
// the LONGEST value the setting can produce, not at its default. Sizing
// arithmetic against a default leaves the bound unverified for anyone who edits
// the JSON — the shape of failure documented in
// docs/solutions/design-patterns/drain-a-stream-relay-on-its-own-budget-before-stopping-the-producers-behind-it.md.
//
// The bound is not decorative: bosso relays SendChatMessage under
// commandDeadline = 30s, so a send budget past 20s can outlive the relay and
// produce an ambiguous delivery that must not be retried (a retry double-types
// into the composer).
func TestTmuxDeliveryConfig_SendClampAtItsLongestValue(t *testing.T) {
	tests := []struct {
		name    string
		seconds int
		want    time.Duration
	}{
		{name: "just under the clamp is untouched", seconds: 19, want: 19 * time.Second},
		{name: "exactly at the clamp", seconds: 20, want: 20 * time.Second},
		{name: "one second over is clamped", seconds: 21, want: 20 * time.Second},
		{name: "an absurd hour is clamped", seconds: 3600, want: 20 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := TmuxDeliveryConfig{SendReadyDeadlineSeconds: tt.seconds}
			if got := cfg.SendReadyDeadline(); got != tt.want {
				t.Errorf("SendReadyDeadline() for %ds = %v, want %v", tt.seconds, got, tt.want)
			}
		})
	}
}

// TestTmuxDeliveryConfig_SessionStartIsUnclamped is the inverse assertion, and
// it is the one that keeps the two accessors from being quietly unified: the
// start path answers to no downstream relay ceiling, so an operator on a
// pathologically slow host can raise it as far as they like.
func TestTmuxDeliveryConfig_SessionStartIsUnclamped(t *testing.T) {
	cfg := TmuxDeliveryConfig{SessionStartReadyDeadlineSeconds: 3600}
	if got := cfg.SessionStartReadyDeadline(); got != time.Hour {
		t.Errorf("SessionStartReadyDeadline() for 3600s = %v, want 1h — the start path is deliberately unclamped", got)
	}
}

// TestSettings_TmuxDeliveryRoundTrip proves the block is reachable from a real
// settings.json rather than only from a hand-built struct.
func TestSettings_TmuxDeliveryRoundTrip(t *testing.T) {
	var s Settings
	raw := `{"worktree_base_dir":"/tmp/wt","tmux_delivery":{"session_start_ready_deadline_seconds":90,"send_ready_deadline_seconds":15}}`
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got := s.TmuxDelivery.SessionStartReadyDeadline(); got != 90*time.Second {
		t.Errorf("SessionStartReadyDeadline() = %v, want 90s", got)
	}
	if got := s.TmuxDelivery.SendReadyDeadline(); got != 15*time.Second {
		t.Errorf("SendReadyDeadline() = %v, want 15s", got)
	}
}

// TestSettings_TmuxDeliveryLegacyFileKeepsDefaults is the no-migration
// criterion: a settings file written before this feature existed carries no
// tmux_delivery block and must still come up on both defaults.
func TestSettings_TmuxDeliveryLegacyFileKeepsDefaults(t *testing.T) {
	var legacy Settings
	if err := json.Unmarshal([]byte(`{"worktree_base_dir":"/tmp/wt"}`), &legacy); err != nil {
		t.Fatalf("Unmarshal legacy: %v", err)
	}
	if got := legacy.TmuxDelivery.SessionStartReadyDeadline(); got != 45*time.Second {
		t.Errorf("legacy SessionStartReadyDeadline() = %v, want 45s", got)
	}
	if got := legacy.TmuxDelivery.SendReadyDeadline(); got != 5*time.Second {
		t.Errorf("legacy SendReadyDeadline() = %v, want 5s", got)
	}
}

// TestSettings_TmuxDeliveryOmittedWhenUnset keeps a fresh settings.json free of
// the block: both knobs are defaulted, so writing them out would only freeze
// today's numbers into every install's config file.
func TestSettings_TmuxDeliveryOmittedWhenUnset(t *testing.T) {
	out, err := json.Marshal(Settings{WorktreeBaseDir: "/tmp/wt"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var round map[string]any
	if err := json.Unmarshal(out, &round); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, ok := round["tmux_delivery"]; ok {
		t.Errorf("tmux_delivery present in marshalled defaults: %s", out)
	}
}

// TestTmuxDeliveryConfig_OverflowingSecondsStayInContract covers the one input
// class where guarding the configured int is not the same as guarding the
// duration it produces. time.Duration is int64 NANOSECONDS, so it tops out
// around 292 years: multiply a large enough positive seconds value by
// time.Second and the product wraps to zero or negative.
//
// That matters because both accessors publish a contract about their RESULT,
// not their input. SessionStartReadyDeadline promises it never returns a
// non-positive duration — a negative one would read downstream as "no wait",
// the exact defect BOS-893 exists to remove. SendReadyDeadline promises a
// result no larger than SendReadyDeadlineMax — and a wrapped-negative d fails
// the `d > SendReadyDeadlineMax` test, so without an explicit overflow arm the
// clamp would be skipped on the very inputs that asked for the largest budget.
//
// The smallest overflowing value is derived rather than written out, so the
// test says what it means and stays correct if time.Duration's unit ever
// changes; on a platform whose int cannot hold it there is nothing to test.
func TestTmuxDeliveryConfig_OverflowingSecondsStayInContract(t *testing.T) {
	overflowSeconds := math.MaxInt64/int64(time.Second) + 1
	if overflowSeconds > int64(math.MaxInt) {
		t.Skipf("int is too narrow on this platform to express an overflowing seconds value (%d)", overflowSeconds)
	}
	if got := time.Duration(int(overflowSeconds)) * time.Second; got > 0 {
		t.Fatalf("premise broken: %d seconds did not overflow time.Duration (got %v)", overflowSeconds, got)
	}

	for _, seconds := range []int{int(overflowSeconds), math.MaxInt} {
		cfg := TmuxDeliveryConfig{
			SessionStartReadyDeadlineSeconds: seconds,
			SendReadyDeadlineSeconds:         seconds,
		}
		// Unclamped still means unclamped for every representable value, so the
		// start knob answers an unrepresentable one with its default rather
		// than inventing a ceiling it does not otherwise have.
		if got := cfg.SessionStartReadyDeadline(); got != 45*time.Second {
			t.Errorf("SessionStartReadyDeadline() with %d seconds = %v, want the 45s default", seconds, got)
		}
		// The send knob has a ceiling, so an ask for "enormous" is answered
		// faithfully by the ceiling rather than by the 5s default.
		if got := cfg.SendReadyDeadline(); got != 20*time.Second {
			t.Errorf("SendReadyDeadline() with %d seconds = %v, want the 20s clamp", seconds, got)
		}
	}
}

// TestSwitchRespawnBudgetFor_FloorsNonPositiveReadiness pins the low edge of
// the switch budget's derivation (BOS-948).
//
// SwitchRespawnBudgetFor takes the RESOLVED readiness rather than the config
// struct, so it cannot lean on SessionStartReadyDeadline's own floor: a caller
// that hands it a zero — a client with nothing configured, a nil-client
// fallback — would otherwise get a budget made of allowances alone, roughly
// half the shipped default, and the readiness wait it bounds would be clamped
// on the very path that configured nothing. Floor it here instead, so both
// sides of the seam answer a non-positive value the same way.
func TestSwitchRespawnBudgetFor_FloorsNonPositiveReadiness(t *testing.T) {
	t.Parallel()

	want := SwitchRespawnBudgetFor(DefaultSessionStartReadyDeadline)
	tests := []struct {
		name      string
		readiness time.Duration
	}{
		{name: "zero is the unconfigured client", readiness: 0},
		{name: "a negative never becomes a shorter budget", readiness: -60 * time.Second},
		{name: "the default itself is the reference", readiness: DefaultSessionStartReadyDeadline},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := SwitchRespawnBudgetFor(tt.readiness); got != want {
				t.Errorf("SwitchRespawnBudgetFor(%v) = %v, want the default-derived %v", tt.readiness, got, want)
			}
		})
	}
}

// TestSwitchRespawnBudgetFor_SaturatesRatherThanWrapping pins the high edge,
// and the direction of the choice is the point.
//
// A readiness within one allowance of time.Duration's ceiling makes the sum
// wrap negative. Falling back to the default there — the obvious defensive move,
// and what SessionStartReadyDeadline does with an overflowing SECONDS value —
// would re-create this ticket's bug at the extreme: the largest configured
// readiness would receive the smallest budget, clamping the wait hardest
// exactly where the operator asked for the most room. Saturating keeps the
// function monotonic in its input.
//
// The companion assertion is that the accessor never actually produces such a
// value: an overflowing session_start_ready_deadline_seconds is answered with
// the default before the derivation is ever reached, so this edge is reachable
// only by an in-process caller, not by settings.json.
func TestSwitchRespawnBudgetFor_SaturatesRatherThanWrapping(t *testing.T) {
	t.Parallel()

	const ceiling = time.Duration(math.MaxInt64)
	for _, readiness := range []time.Duration{ceiling, ceiling - time.Second, ceiling - switchPolicyMargin} {
		got := SwitchRespawnBudgetFor(readiness)
		if got <= 0 {
			t.Errorf("SwitchRespawnBudgetFor(%v) = %v: the sum wrapped instead of saturating", readiness, got)
		}
		if got < readiness {
			t.Errorf("SwitchRespawnBudgetFor(%v) = %v, which is SHORTER than the readiness it must fund — "+
				"the largest configured value must not receive the smallest budget", readiness, got)
		}
	}

	// The settings.json route stops short of the edge on its own.
	overflowSeconds := math.MaxInt64/int64(time.Second) + 1
	if overflowSeconds > int64(math.MaxInt) {
		t.Skipf("int is too narrow on this platform to express an overflowing seconds value (%d)", overflowSeconds)
	}
	cfg := TmuxDeliveryConfig{SessionStartReadyDeadlineSeconds: int(overflowSeconds)}
	if got := SwitchRespawnBudgetFor(cfg.SessionStartReadyDeadline()); got != SwitchRespawnBudgetFor(DefaultSessionStartReadyDeadline) {
		t.Errorf("an overflowing configured seconds value produced %v; the accessor must answer with its "+
			"default before the derivation is reached", got)
	}
}

func TestStartChatRunBudgetFor(t *testing.T) {
	t.Parallel()

	tail := StartChatRunInRPCTail
	tests := []struct {
		name      string
		readiness time.Duration
		want      time.Duration
	}{
		{name: "1s floors the tail term", readiness: time.Second, want: time.Second + tail},
		{name: "4s exactly matches the tail floor", readiness: 4 * time.Second, want: 8 * time.Second},
		{name: "30s preserves the 2x ratio", readiness: 30 * time.Second, want: 60 * time.Second},
		{name: "45s default remains 90s", readiness: 45 * time.Second, want: 90 * time.Second},
		{name: "120s derives a 240s ceiling", readiness: 120 * time.Second, want: 240 * time.Second},
		{name: "300s derives a 600s ceiling", readiness: 300 * time.Second, want: 600 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := StartChatRunBudgetFor(tt.readiness)
			if got != tt.want {
				t.Fatalf("StartChatRunBudgetFor(%v) = %v, want %v", tt.readiness, got, tt.want)
			}
			if got <= tt.readiness+StartChatRunSubmitVerifierTail {
				t.Fatalf("StartChatRunBudgetFor(%v) = %v does not fund readiness plus submit verifier %v",
					tt.readiness, got, StartChatRunSubmitVerifierTail)
			}
			if got-tt.readiness < tt.readiness && tt.readiness >= tail {
				t.Fatalf("StartChatRunBudgetFor(%v) = %v breaks the 2x policy ratio", tt.readiness, got)
			}
		})
	}
}

func TestStartChatRunBudgetFor_DefaultCompatibilityAndFallbacks(t *testing.T) {
	t.Parallel()

	const want = 90 * time.Second
	if got := StartChatRunBudgetFor(DefaultSessionStartReadyDeadline); got != want {
		t.Fatalf("StartChatRunBudgetFor(DefaultSessionStartReadyDeadline) = %v, want %v", got, want)
	}
	for _, readiness := range []time.Duration{0, -time.Second} {
		if got := StartChatRunBudgetFor(readiness); got != want {
			t.Errorf("StartChatRunBudgetFor(%v) = %v, want default-derived %v", readiness, got, want)
		}
	}
}

func TestStartChatRunBudgetFor_SaturatesRatherThanWrapping(t *testing.T) {
	t.Parallel()

	const ceiling = time.Duration(math.MaxInt64)
	for _, readiness := range []time.Duration{ceiling, ceiling - time.Second} {
		got := StartChatRunBudgetFor(readiness)
		if got != ceiling {
			t.Errorf("StartChatRunBudgetFor(%v) = %v, want saturated %v", readiness, got, ceiling)
		}
	}
}
