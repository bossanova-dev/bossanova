//go:build e2e

package client

import (
	"testing"
	"time"

	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

// TestParseE2ERPCDeadline pins the accept/reject table. Only a strictly positive
// duration may replace the production bound: a zero or negative value would
// expire every RPC before it left the process, and garbage must leave the bound
// alone rather than silently disarming it.
func TestParseE2ERPCDeadline(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want time.Duration
		ok   bool
	}{
		{name: "valid seconds", raw: "3s", want: 3 * time.Second, ok: true},
		{name: "valid milliseconds", raw: "750ms", want: 750 * time.Millisecond, ok: true},
		{name: "surrounding whitespace is trimmed", raw: "  2s\n", want: 2 * time.Second, ok: true},
		{name: "empty", raw: ""},
		{name: "whitespace only", raw: "   "},
		{name: "garbage", raw: "soon"},
		{name: "bare number without a unit", raw: "3"},
		{name: "zero", raw: "0s"},
		{name: "negative", raw: "-5s"},
		// Accepted by time.ParseDuration, but slowDeadlineRatio times it
		// overflows time.Duration and wraps negative, which would expire every
		// slow RPC instantly — the same disarming zero and garbage are
		// rejected for.
		{name: "large enough to overflow the slow-bound scaling", raw: "700000h"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseE2ERPCDeadline(tc.raw)
			if ok != tc.ok {
				t.Fatalf("parseE2ERPCDeadline(%q) ok = %v, want %v", tc.raw, ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Fatalf("parseE2ERPCDeadline(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// TestE2ERPCDeadlineOverrideReadsEnv proves the e2e build actually consults
// BOSS_RPC_DEADLINE_E2E — parseE2ERPCDeadline alone would pass even if the
// override were wired to the wrong variable, or to none.
func TestE2ERPCDeadlineOverrideReadsEnv(t *testing.T) {
	t.Setenv(envRPCDeadlineE2E, "1500ms")
	got, ok := e2eRPCDeadlineOverride()
	if !ok || got != 1500*time.Millisecond {
		t.Fatalf("e2eRPCDeadlineOverride() = %v, %v; want 1.5s, true", got, ok)
	}

	t.Setenv(envRPCDeadlineE2E, "")
	if got, ok := e2eRPCDeadlineOverride(); ok {
		t.Fatalf("e2eRPCDeadlineOverride() with an empty env = %v, true; want declined", got)
	}
}

// TestE2EBuildAppliesEnvDeadline proves the wiring init() performs, not just the
// parse: under the e2e tag applyE2ERPCDeadlineOverride really does move
// defaultRPCDeadline to whatever the environment names. It calls the function
// init() calls, because init() has already run by the time any test can arrange
// an environment — asserting on defaultRPCDeadline alone would assert only the
// 30s default in a CI process that sets nothing.
func TestE2EBuildAppliesEnvDeadline(t *testing.T) {
	beforeDefault, beforeSlow := defaultRPCDeadline, slowRPCDeadline
	t.Cleanup(func() { defaultRPCDeadline, slowRPCDeadline = beforeDefault, beforeSlow })

	t.Setenv(envRPCDeadlineE2E, "1234ms")
	if !applyE2ERPCDeadlineOverride() {
		t.Fatal("applyE2ERPCDeadlineOverride() declined a valid duration")
	}
	if defaultRPCDeadline != 1234*time.Millisecond {
		t.Fatalf("defaultRPCDeadline = %v, want the env-supplied 1.234s", defaultRPCDeadline)
	}

	// Both bounds must move. Scaling only the default would leave every
	// slowProcedures member — RecordChat, the call the attach view blocks on,
	// included — at the 120s production bound, which no proof step can outlast,
	// so a scenario asking for a 3s bound would silently never see one fire.
	if slowRPCDeadline != slowDeadlineRatio*1234*time.Millisecond {
		t.Fatalf("slowRPCDeadline = %v, want %v (%dx the env-supplied bound)", slowRPCDeadline, slowDeadlineRatio*1234*time.Millisecond, slowDeadlineRatio)
	}
	// The scaled bounds must still be reachable through the real lookup, not
	// just present in their vars.
	if got := deadlineFor(bossanovav1connect.DaemonServiceRecordChatProcedure); got != slowRPCDeadline {
		t.Fatalf("deadlineFor(RecordChat) = %v, want the scaled slow bound %v", got, slowRPCDeadline)
	}

	// A rejected value must leave whatever bounds are already in force alone,
	// rather than resetting them or zeroing them.
	t.Setenv(envRPCDeadlineE2E, "whenever")
	if applyE2ERPCDeadlineOverride() {
		t.Fatal("applyE2ERPCDeadlineOverride() accepted an unparseable duration")
	}
	if defaultRPCDeadline != 1234*time.Millisecond {
		t.Fatalf("defaultRPCDeadline = %v after a rejected value; want it untouched at 1.234s", defaultRPCDeadline)
	}
	if slowRPCDeadline != slowDeadlineRatio*1234*time.Millisecond {
		t.Fatalf("slowRPCDeadline = %v after a rejected value; want it untouched", slowRPCDeadline)
	}
}
