package tuitest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// ConfigDirForHome returns the bossanova config directory under a given HOME.
// config.Path() resolves through os.UserConfigDir(), which is HOME-derived
// on every supported platform.
func ConfigDirForHome(home string) string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "bossanova")
	}
	return filepath.Join(home, ".config", "bossanova")
}

// WriteSeedSettings writes a settings.json with JSON-marshaled settings into
// the bossanova config directory under home, creating directories as needed.
func WriteSeedSettings(home string, settings map[string]any) error {
	configDir := ConfigDirForHome(home)
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return err
	}
	contents, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), contents, 0o600); err != nil {
		return err
	}
	return nil
}

// SeedSettingsAcknowledged writes a minimal settings.json with
// ProvidersAcknowledged=true into the per-test HOME so the boss subprocess
// skips the first-run onboarding gate.
func SeedSettingsAcknowledged(home, worktreeBaseDir string) error {
	settings := map[string]any{
		"providers_acknowledged": true,
	}
	if worktreeBaseDir != "" {
		settings["worktree_base_dir"] = worktreeBaseDir
	}
	return WriteSeedSettings(home, settings)
}

// SeedSettingsAcknowledgedWithOverrides writes acknowledged settings plus
// validated proof-only overrides supplied by a scenario fixture.
func SeedSettingsAcknowledgedWithOverrides(home, worktreeBaseDir string, overrides map[string]any) error {
	settings := map[string]any{
		"providers_acknowledged": true,
	}
	if worktreeBaseDir != "" {
		settings["worktree_base_dir"] = worktreeBaseDir
	}
	for key, value := range overrides {
		settings[key] = value
	}
	return WriteSeedSettings(home, settings)
}

// SeedFirstRunSettings writes a settings.json that points boss at the test
// daemon socket but leaves providers unacknowledged. This makes the first-run
// onboarding gate fire (boss skips the daemon-startup preflight only when
// BOSS_SOCKET is set, so onboarding harnesses leave it unset) while boss still
// resolves the mock daemon directly via socket_path — no socket proxy needed.
func SeedFirstRunSettings(home, socketPath string) error {
	return WriteSeedSettings(home, map[string]any{
		"providers_acknowledged": false,
		"socket_path":            socketPath,
	})
}

// ProofEnvWhitelist is the set of env key prefixes a proof scenario may forward
// into the boss subprocess. Security boundary: scenario files are agent-authored,
// so only these harness-managed families are permitted — never arbitrary
// developer env. The E2E families intentionally mirror the prefixes BaseHarnessEnv
// strips (BOSS_CLOUD_ACCESS_E2E_*, BOSS_GITHUB_APP_E2E_*, BOSS_AUTH_E2E_*,
// BOSS_HOST_E2E_*): the harness strips them from the ambient environ, then the
// bridge re-adds only the validated, scenario-requested subset. BOSS_HOST_E2E_* is
// the --host transport family (BOS-713): it stages the reconnecting wait screen a
// dropped ssh tunnel produces, which the harness cannot reach for real because it
// has no network and never runs --host. BOSS_PROOF_UPGRADE_* is a proof-only
// display toggle family (not an E2E fake): it lets a scenario force a
// deterministic upgrade-banner state (e.g. the rate-limit line) without draining
// a real GitHub quota. Values only flip on-screen text, never behavior a user
// cares about, so forwarding them from an agent-authored scenario is safe.
// BOSS_ORG_E2E_* (BOS-1061) is the organization / repo-organization-mapping
// family: it seeds the caller's organization list, the mapping in force, and a
// membership refusal, none of which the harness can reach for real because it
// never signs in to bosso.
var ProofEnvWhitelist = []string{"BOSS_CLOUD_ACCESS_E2E_", "BOSS_GITHUB_APP_E2E_", "BOSS_AUTH_E2E_", "BOSS_HOST_E2E_", "BOSS_ORG_E2E_", "BOSS_PROOF_UPGRADE_"}

// ProofEnvAllowedKeys are exact proof-only env keys whose behavior is narrow
// enough to expose to agent-authored scenarios without admitting a whole family.
var ProofEnvAllowedKeys = []string{
	"BOSS_PROOF_SETTINGS_EVENT_TRACING",
	"BOSS_PROOF_SETTINGS_POSTHOG_HOST",
	"BOSS_PROOF_SETTINGS_SAVE_FAILURE",
}

// FilterProofEnv splits a requested env map into allowed (keys that prefix-match
// ProofEnvWhitelist) and rejected (keys that don't). Env validation lives ONLY
// in Go — the Node side forwards raw — so there is a single source of truth (the
// PathToProjectKey Go/TS duplication drift is the documented anti-pattern this
// avoids). rejected is sorted so a boot-abort error message is deterministic.
func FilterProofEnv(requested map[string]string) (allowed map[string]string, rejected []string) {
	allowed = make(map[string]string, len(requested))
	for k, v := range requested {
		ok := false
		for _, prefix := range ProofEnvWhitelist {
			if strings.HasPrefix(k, prefix) {
				ok = true
				break
			}
		}
		for _, allowedKey := range ProofEnvAllowedKeys {
			if k == allowedKey {
				ok = true
				break
			}
		}
		if ok {
			allowed[k] = v
		} else {
			rejected = append(rejected, k)
		}
	}
	sort.Strings(rejected)
	return allowed, rejected
}

// BaseHarnessEnv strips harness-managed env vars from environ, returning a
// filtered copy. This prevents developer environment variables from leaking
// into the boss subprocess under test.
func BaseHarnessEnv(environ []string) []string {
	var env []string
	for _, e := range environ {
		if strings.HasPrefix(e, "BOSS_SOCKET=") ||
			// BOSS_PROOF_TUI_SEED_ENV is the proof bridge's RAW scenario-env carrier
			// (read + validated by the bridge in Go). Strip it so the unvalidated
			// JSON blob never reaches the boss subprocess — only the whitelisted
			// subset the bridge re-adds does (the "append only the allowed subset"
			// security boundary).
			strings.HasPrefix(e, "BOSS_PROOF_TUI_SEED_ENV=") ||
			strings.HasPrefix(e, "BOSS_SETTINGS_PATH=") ||
			strings.HasPrefix(e, "BOSS_SKIP_SKILLS=") ||
			strings.HasPrefix(e, "BOSS_AUTH_E2E_EMAIL=") ||
			strings.HasPrefix(e, "BOSS_AUTH_E2E_LOGIN_EMAIL=") ||
			strings.HasPrefix(e, "BOSS_AUTH_E2E_LOGOUT_ERROR=") ||
			// BOS-942: the silently-no-op credential save seam. Stripped with
			// the rest of the family — an ambient developer value would make
			// every unrelated proof run's login store nothing at all.
			strings.HasPrefix(e, "BOSS_AUTH_E2E_LOGIN_SAVE_NOOP=") ||
			// BOS-659: the retained-re-login seed. Stripped here like every other
			// BOSS_AUTH_E2E_* var so an ambient developer value can never flag the
			// subprocess's credentials; the bridge re-adds it only when a scenario
			// asks for it.
			strings.HasPrefix(e, "BOSS_AUTH_E2E_NEEDS_RELOGIN=") ||
			// BOS-713: the --host reconnecting-screen seed and its poll count.
			// Stripped like every other E2E family so an ambient developer value
			// can never park an unrelated run behind a full-screen blocking wait;
			// the bridge re-adds them only when a scenario asks for them.
			strings.HasPrefix(e, "BOSS_HOST_E2E_RECONNECT=") ||
			strings.HasPrefix(e, "BOSS_HOST_E2E_RECONNECT_POLLS=") ||
			// BOS-724: the classified failure the reconnecting screen reports.
			// Stripped with the rest of the pair so an ambient value can never
			// put a developer's own words on a captured frame.
			strings.HasPrefix(e, "BOSS_HOST_E2E_RECONNECT_REASON=") ||
			// BOS-714: the --host remote-context seed. Stripped for the same
			// reason as the reconnect pair — an ambient developer value would
			// silently put an unrelated run into a remote context, where attach
			// shells out over ssh and the local-filesystem affordances vanish.
			strings.HasPrefix(e, "BOSS_HOST_E2E_ATTACH_DESTINATION=") ||
			// BOS-723: the e2e-only unary RPC bound. Stripped for the same reason
			// as the rest of the family — an ambient developer value would apply
			// to EVERY proof run, and a short bound would fail unrelated
			// scenarios with a daemon-down screen no step asked for. Only the
			// wedged-daemon preset's DefaultEnv (appended after this filter) may
			// set it, so it is deliberately absent from ProofEnvWhitelist too:
			// scenario-authored env cannot reach into the client's bound.
			strings.HasPrefix(e, "BOSS_RPC_DEADLINE_E2E=") ||
			strings.HasPrefix(e, "BOSS_SKIP_PROVIDER_STARTUP_DAEMON_RESTART=") ||
			strings.HasPrefix(e, "BOSS_CLOUD_ACCESS_E2E_SEQUENCE=") ||
			strings.HasPrefix(e, "BOSS_CLOUD_ACCESS_E2E_ERROR_MESSAGE=") ||
			strings.HasPrefix(e, "BOSS_CLOUD_ACCESS_E2E_CHECKOUT_URL=") ||
			strings.HasPrefix(e, "BOSS_CLOUD_ACCESS_E2E_CHECKOUT_ERROR=") ||
			strings.HasPrefix(e, "BOSS_CLOUD_ACCESS_E2E_REFRESH_INTERVAL=") ||
			strings.HasPrefix(e, "BOSS_GITHUB_APP_E2E_INSTALLED_REPOS=") ||
			strings.HasPrefix(e, "BOSS_GITHUB_APP_E2E_INSTALL_AFTER_POLLS=") ||
			strings.HasPrefix(e, "BOSS_GITHUB_APP_E2E_INSTALL_URL=") ||
			// BOS-1061: the organization list, the mapping in force, and the
			// membership refusal. Stripped like every other E2E family so an
			// ambient developer value can never map an unrelated run's repo to
			// an organization, or stage a refusal no scenario asked for.
			strings.HasPrefix(e, "BOSS_ORG_E2E_ORGANIZATIONS=") ||
			strings.HasPrefix(e, "BOSS_ORG_E2E_MAPPING=") ||
			strings.HasPrefix(e, "BOSS_ORG_E2E_SET_ERROR=") ||
			strings.HasPrefix(e, "BOSS_PROOF_SETTINGS_") ||
			strings.HasPrefix(e, "HOME=") ||
			strings.HasPrefix(e, "XDG_CONFIG_HOME=") {
			continue
		}
		env = append(env, e)
	}
	return env
}
