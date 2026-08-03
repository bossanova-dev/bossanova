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

func TestProofEnvWhitelistFamilies(t *testing.T) {
	want := []string{"BOSS_CLOUD_ACCESS_E2E_", "BOSS_GITHUB_APP_E2E_", "BOSS_AUTH_E2E_", "BOSS_PROOF_UPGRADE_"}
	if !reflect.DeepEqual(ProofEnvWhitelist, want) {
		t.Fatalf("ProofEnvWhitelist = %v, want %v", ProofEnvWhitelist, want)
	}
}
