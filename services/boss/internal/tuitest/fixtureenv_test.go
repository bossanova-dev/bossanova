package tuitest

import (
	"reflect"
	"strings"
	"testing"
)

func TestFilterProofEnvAllowsWhitelistedFamilies(t *testing.T) {
	requested := map[string]string{
		"BOSS_CLOUD_ACCESS_E2E_SEQUENCE":  "active",
		"BOSS_GITHUB_APP_E2E_INSTALL_URL": "https://example.test",
		"BOSS_AUTH_E2E_EMAIL":             "proof@example.com",
	}
	allowed, rejected := FilterProofEnv(requested)
	if len(rejected) != 0 {
		t.Fatalf("no key should be rejected, got %v", rejected)
	}
	if !reflect.DeepEqual(allowed, requested) {
		t.Fatalf("allowed = %v, want all requested", allowed)
	}
}

func TestFilterProofEnvRejectsNonWhitelistedSorted(t *testing.T) {
	requested := map[string]string{
		"ZZZ_EVIL":                       "1",
		"AAA_ALSO_EVIL":                  "2",
		"PATH":                           "/bin",
		"BOSS_CLOUD_ACCESS_E2E_SEQUENCE": "active",
	}
	allowed, rejected := FilterProofEnv(requested)
	// Only the whitelisted family survives.
	if _, ok := allowed["BOSS_CLOUD_ACCESS_E2E_SEQUENCE"]; !ok || len(allowed) != 1 {
		t.Fatalf("allowed should contain only the whitelisted key, got %v", allowed)
	}
	// Rejected keys must be sorted for deterministic error messages.
	want := []string{"AAA_ALSO_EVIL", "PATH", "ZZZ_EVIL"}
	if !reflect.DeepEqual(rejected, want) {
		t.Fatalf("rejected = %v, want sorted %v", rejected, want)
	}
}

func TestBaseHarnessEnvStripsSeedEnvCarrier(t *testing.T) {
	environ := []string{
		"BOSS_PROOF_TUI_SEED_ENV={\"BOSS_AUTH_E2E_EMAIL\":\"x@y.z\"}",
		"PATH=/bin",
	}
	got := BaseHarnessEnv(environ)
	for _, e := range got {
		if strings.HasPrefix(e, "BOSS_PROOF_TUI_SEED_ENV=") {
			t.Fatalf("BaseHarnessEnv must strip the raw seed-env carrier, got %v", got)
		}
	}
	// Unrelated vars survive.
	var sawPath bool
	for _, e := range got {
		if e == "PATH=/bin" {
			sawPath = true
		}
	}
	if !sawPath {
		t.Fatalf("BaseHarnessEnv dropped an unrelated var: %v", got)
	}
}

// TestBaseHarnessEnvStripsAuthE2EFamily pins that every BOSS_AUTH_E2E_* var the
// e2e auth store reads is stripped from the ambient environ. The whitelist is a
// prefix match, but BaseHarnessEnv strips by exact key: a var added to the store
// and not to this list would be inherited from the developer's shell and could
// never be overridden per scenario. BOS-659 added NEEDS_RELOGIN.
func TestBaseHarnessEnvStripsAuthE2EFamily(t *testing.T) {
	stripped := []string{
		"BOSS_AUTH_E2E_EMAIL=ambient@example.com",
		"BOSS_AUTH_E2E_LOGIN_EMAIL=ambient@example.com",
		"BOSS_AUTH_E2E_NEEDS_RELOGIN=refresh_outcome_unknown",
		"BOSS_AUTH_E2E_LOGOUT_ERROR=1",
	}
	got := BaseHarnessEnv(append([]string{"PATH=/bin"}, stripped...))
	for _, want := range stripped {
		key := strings.SplitN(want, "=", 2)[0] + "="
		for _, e := range got {
			if strings.HasPrefix(e, key) {
				t.Fatalf("BaseHarnessEnv must strip %s, got %v", key, got)
			}
		}
	}
}

// TestHostE2EFamilySurvivesFilterAndIsStripped pins both halves of the BOS-713
// pipeline for the --host reconnect seed: a scenario may request it (the bridge
// must not reject the key at boot), and the ambient copy must never survive into
// the subprocess on its own. Getting only one half right is silently broken —
// stripped-but-not-whitelisted aborts boot, whitelisted-but-not-stripped lets a
// developer's shell park every proof run behind a wait screen.
func TestHostE2EFamilySurvivesFilterAndIsStripped(t *testing.T) {
	requested := map[string]string{
		"BOSS_HOST_E2E_RECONNECT":        "user@build-box",
		"BOSS_HOST_E2E_RECONNECT_POLLS":  "3",
		"BOSS_HOST_E2E_RECONNECT_REASON": "remotehost: ssh tunnel to user@build-box exited: signal: terminated",
	}
	allowed, rejected := FilterProofEnv(requested)
	if len(rejected) != 0 {
		t.Fatalf("the BOSS_HOST_E2E_ family must be forwardable, got rejected %v", rejected)
	}
	if !reflect.DeepEqual(allowed, requested) {
		t.Fatalf("allowed = %v, want all requested", allowed)
	}

	got := BaseHarnessEnv([]string{
		"PATH=/bin",
		"BOSS_HOST_E2E_RECONNECT=ambient@example.com",
		"BOSS_HOST_E2E_RECONNECT_POLLS=99",
		"BOSS_HOST_E2E_RECONNECT_REASON=an ambient developer's own words",
	})
	for _, e := range got {
		if strings.HasPrefix(e, "BOSS_HOST_E2E_") {
			t.Fatalf("BaseHarnessEnv must strip the ambient BOSS_HOST_E2E_ family, got %v", got)
		}
	}
}

func TestProofEnvWhitelistFamilies(t *testing.T) {
	want := []string{"BOSS_CLOUD_ACCESS_E2E_", "BOSS_GITHUB_APP_E2E_", "BOSS_AUTH_E2E_", "BOSS_HOST_E2E_", "BOSS_PROOF_UPGRADE_"}
	if !reflect.DeepEqual(ProofEnvWhitelist, want) {
		t.Fatalf("ProofEnvWhitelist = %v, want %v", ProofEnvWhitelist, want)
	}
}

// TestBaseHarnessEnvStripsRPCDeadline pins the BOS-723 half of the same
// contract: an ambient BOSS_RPC_DEADLINE_E2E must never reach the boss
// subprocess, because it would shrink the client's unary RPC bound for every
// proof run — including scenarios that have no wedged daemon and would then
// fail on a daemon-down screen nothing in them asked for. The wedged-daemon
// preset supplies its own value through DefaultEnv, which is appended after
// this filter runs.
func TestBaseHarnessEnvStripsRPCDeadline(t *testing.T) {
	got := BaseHarnessEnv([]string{"PATH=/bin", "BOSS_RPC_DEADLINE_E2E=1ms"})
	for _, e := range got {
		if strings.HasPrefix(e, "BOSS_RPC_DEADLINE_E2E=") {
			t.Fatalf("BaseHarnessEnv must strip BOSS_RPC_DEADLINE_E2E, got %v", got)
		}
	}
}

// TestRPCDeadlineIsNotScenarioRequestable pins the other half: a scenario's
// fixture.env may not set the bound either. The knob belongs to the preset, so
// an agent-authored scenario file cannot quietly disarm — or over-tighten — the
// client bound the ticket exists to add.
func TestRPCDeadlineIsNotScenarioRequestable(t *testing.T) {
	_, rejected := FilterProofEnv(map[string]string{"BOSS_RPC_DEADLINE_E2E": "1ms"})
	if len(rejected) != 1 || rejected[0] != "BOSS_RPC_DEADLINE_E2E" {
		t.Fatalf("BOSS_RPC_DEADLINE_E2E should be rejected as scenario env, got rejected=%v", rejected)
	}
}
