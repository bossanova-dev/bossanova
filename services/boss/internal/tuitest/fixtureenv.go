package tuitest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
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
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return err
	}
	contents, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), contents, 0o644); err != nil {
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

// BaseHarnessEnv strips harness-managed env vars from environ, returning a
// filtered copy. This prevents developer environment variables from leaking
// into the boss subprocess under test.
func BaseHarnessEnv(environ []string) []string {
	var env []string
	for _, e := range environ {
		if strings.HasPrefix(e, "BOSS_SOCKET=") ||
			strings.HasPrefix(e, "BOSS_SETTINGS_PATH=") ||
			strings.HasPrefix(e, "BOSS_SKIP_SKILLS=") ||
			strings.HasPrefix(e, "BOSS_AUTH_E2E_EMAIL=") ||
			strings.HasPrefix(e, "BOSS_AUTH_E2E_LOGIN_EMAIL=") ||
			strings.HasPrefix(e, "BOSS_SKIP_PROVIDER_STARTUP_DAEMON_RESTART=") ||
			strings.HasPrefix(e, "BOSS_CLOUD_ACCESS_E2E_SEQUENCE=") ||
			strings.HasPrefix(e, "BOSS_CLOUD_ACCESS_E2E_CHECKOUT_URL=") ||
			strings.HasPrefix(e, "BOSS_CLOUD_ACCESS_E2E_CHECKOUT_ERROR=") ||
			strings.HasPrefix(e, "BOSS_CLOUD_ACCESS_E2E_REFRESH_INTERVAL=") ||
			strings.HasPrefix(e, "BOSS_GITHUB_APP_E2E_INSTALLED_REPOS=") ||
			strings.HasPrefix(e, "BOSS_GITHUB_APP_E2E_INSTALL_AFTER_POLLS=") ||
			strings.HasPrefix(e, "BOSS_GITHUB_APP_E2E_INSTALL_URL=") ||
			strings.HasPrefix(e, "HOME=") ||
			strings.HasPrefix(e, "XDG_CONFIG_HOME=") {
			continue
		}
		env = append(env, e)
	}
	return env
}
