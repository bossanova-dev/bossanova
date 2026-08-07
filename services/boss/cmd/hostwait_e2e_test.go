//go:build e2e

package main

import (
	"strings"
	"testing"
)

// TestResolveE2EHostReconnectDestination covers the BOS-713 fixture seed: only
// a genuinely non-empty, non-falsey BOSS_HOST_E2E_RECONNECT stages the
// reconnecting screen, and the value is the destination shown on it.
func TestResolveE2EHostReconnectDestination(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "unset means no seed"},
		{name: "a destination is used verbatim", value: "user@build-box", want: "user@build-box"},
		{name: "surrounding space is trimmed", value: "  user@build-box  ", want: "user@build-box"},
		// An operator who exports the var as "0" plainly means off; staging a
		// full-screen blocking wait instead would hijack their run.
		{name: "zero means off", value: "0"},
		{name: "false with case and space means off", value: "  FALSE "},
		{name: "off means off", value: "off"},
		{name: "no means off", value: "no"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("BOSS_HOST_E2E_RECONNECT", tt.value)
			if got := resolveE2EHostReconnectDestination(); got != tt.want {
				t.Fatalf("resolveE2EHostReconnectDestination() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveE2EHostReconnectPolls(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  int
	}{
		{name: "unset uses the default", want: e2eHostReconnectDefaultPolls},
		{name: "an explicit count is honoured", value: "7", want: 7},
		{name: "zero recovers on the first probe", value: "0", want: 0},
		// A bad knob must not abort a run that is otherwise fine.
		{name: "garbage falls back to the default", value: "soon", want: e2eHostReconnectDefaultPolls},
		{name: "negative falls back to the default", value: "-2", want: e2eHostReconnectDefaultPolls},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("BOSS_HOST_E2E_RECONNECT_POLLS", tt.value)
			if got := resolveE2EHostReconnectPolls(); got != tt.want {
				t.Fatalf("resolveE2EHostReconnectPolls() = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestRunE2EHostReconnectSeedSkipsWithoutDestination pins the no-seed path: an
// unset var must return immediately rather than block a headless test run on a
// full-screen wait that nothing will ever dismiss.
func TestRunE2EHostReconnectSeedSkipsWithoutDestination(t *testing.T) {
	t.Setenv("BOSS_HOST_E2E_RECONNECT", "")
	if err := runE2EHostReconnectSeed(); err != nil {
		t.Fatalf("runE2EHostReconnectSeed() = %v, want nil", err)
	}
}

// TestResolveE2EHostReconnectReason covers the BOS-724 seed: the reconnecting
// screen reports the supervisor's classified failure, and the harness can never
// produce one for real because no test in this repo runs an ssh child.
func TestResolveE2EHostReconnectReason(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "unset means no reason was recorded"},
		{
			name:  "a classified failure is used verbatim",
			value: "remotehost: ssh tunnel to user@build-box exited: signal: terminated",
			want:  "remotehost: ssh tunnel to user@build-box exited: signal: terminated",
		},
		{name: "surrounding space is trimmed", value: "  ssh exited  ", want: "ssh exited"},
		// Printing "Last tunnel failure: 0" would be nonsense on screen.
		{name: "zero means off", value: "0"},
		{name: "false with case and space means off", value: "  FALSE "},
		{name: "off means off", value: "off"},
		{name: "no means off", value: "no"},
		// The value is rendered into a captured frame, so anything that could
		// move the cursor or recolour the terminal is refused outright.
		{name: "an ansi escape is refused", value: "ssh exited \x1b[31mred\x1b[0m"},
		{name: "an embedded newline is refused", value: "ssh exited\nboss daemon status"},
		{name: "a carriage return is refused", value: "ssh exited\rrewritten"},
		// An overlong value would push the action bar off the frame, which is
		// the affordance the scenario exists to prove is still there.
		{name: "an overlong reason is refused", value: strings.Repeat("x", e2eHostReconnectReasonLimit+1)},
		{name: "a reason at the limit is kept", value: strings.Repeat("x", e2eHostReconnectReasonLimit), want: strings.Repeat("x", e2eHostReconnectReasonLimit)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("BOSS_HOST_E2E_RECONNECT_REASON", tt.value)
			if got := resolveE2EHostReconnectReason(); got != tt.want {
				t.Fatalf("resolveE2EHostReconnectReason() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestE2EHostReconnectSeedDetailMatchesProduction pins that the seeded screen is
// formatted by the production helper, so what a scenario captures is the real
// wording rather than a fixture's imitation of it — and that an unseeded reason
// still yields a non-empty, non-leaky sentence.
func TestE2EHostReconnectSeedDetailMatchesProduction(t *testing.T) {
	t.Setenv("BOSS_HOST_E2E_RECONNECT_REASON", "remotehost: ssh tunnel to user@build-box exited: signal: terminated")
	detail := hostReconnectDetail(resolveE2EHostReconnectReason())
	if !strings.Contains(detail, "Last tunnel failure: remotehost: ssh tunnel to user@build-box exited: signal: terminated") {
		t.Fatalf("seeded detail = %q, want the production reason line", detail)
	}

	t.Setenv("BOSS_HOST_E2E_RECONNECT_REASON", "")
	if got := hostReconnectDetail(resolveE2EHostReconnectReason()); got != hostReconnectFallbackDetail {
		t.Fatalf("unseeded detail = %q, want the production fallback %q", got, hostReconnectFallbackDetail)
	}
}
