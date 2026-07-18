package tuitest

import (
	"os"
	"path/filepath"
	"testing"
)

// WriteSeedSettings creates the config dir with least-privilege 0700 and writes
// settings.json 0600 (gosec G301/G306). Assert both tightened modes and that
// the settings still round-trip as valid JSON.
func TestWriteSeedSettingsPerms(t *testing.T) {
	home := t.TempDir()
	if err := WriteSeedSettings(home, map[string]any{"providersAcknowledged": true}); err != nil {
		t.Fatalf("WriteSeedSettings: %v", err)
	}

	configDir := ConfigDirForHome(home)
	di, err := os.Stat(configDir)
	if err != nil {
		t.Fatalf("stat config dir: %v", err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Fatalf("config dir perm = %o, want 0700", got)
	}

	settingsPath := filepath.Join(configDir, "settings.json")
	fi, err := os.Stat(settingsPath)
	if err != nil {
		t.Fatalf("stat settings.json: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("settings.json perm = %o, want 0600", got)
	}

	// Still readable and valid.
	if _, err := os.ReadFile(settingsPath); err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
}
