package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/recurser/bossalib/config"
	"github.com/spf13/cobra"
)

// setupTestConfigEnv points config.Path() at an isolated temp settings file via
// BOSS_SETTINGS_PATH so the test can never touch the developer's real
// settings.json. It uses t.Setenv, so the env is restored automatically; the
// returned cleanup is a no-op kept for call-site compatibility.
func setupTestConfigEnv(t *testing.T) (settingsPath string, cleanup func()) {
	t.Helper()
	settingsPath = filepath.Join(t.TempDir(), "settings.json")
	t.Setenv("BOSS_SETTINGS_PATH", settingsPath)
	return settingsPath, func() {}
}

func TestConfigInitValidPlugins(t *testing.T) {
	settingsPath, cleanup := setupTestConfigEnv(t)
	defer cleanup()

	// Create temp plugin directory with 3 plugin binaries
	pluginDir := t.TempDir()
	plugins := []string{
		"bossd-plugin-alpha",
		"bossd-plugin-beta",
		"bossd-plugin-gamma",
	}
	for _, name := range plugins {
		path := filepath.Join(pluginDir, name)
		if err := os.WriteFile(path, []byte("dummy"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Run config init
	cmd := &cobra.Command{}
	cmd.Flags().String("plugin-dir", "", "")
	_ = cmd.Flags().Set("plugin-dir", pluginDir)

	if err := runConfigInit(cmd); err != nil {
		t.Fatalf("runConfigInit: %v", err)
	}

	// Load settings and verify
	s, err := config.LoadFrom(settingsPath)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}

	if len(s.Plugins) != 3 {
		t.Fatalf("Plugins: got %d, want 3", len(s.Plugins))
	}

	// Verify all plugins are present and enabled
	pluginNames := map[string]bool{
		"alpha": false,
		"beta":  false,
		"gamma": false,
	}
	for _, p := range s.Plugins {
		if _, ok := pluginNames[p.Name]; !ok {
			t.Errorf("unexpected plugin: %s", p.Name)
			continue
		}
		pluginNames[p.Name] = true
		if !p.Enabled {
			t.Errorf("plugin %s: expected Enabled=true", p.Name)
		}
		expectedPath := filepath.Join(pluginDir, "bossd-plugin-"+p.Name)
		absExpectedPath, _ := filepath.Abs(expectedPath)
		if p.Path != absExpectedPath {
			t.Errorf("plugin %s: Path=%q, want %q", p.Name, p.Path, absExpectedPath)
		}
		if p.Version == "" {
			t.Errorf("plugin %s: Version is empty", p.Name)
		}
	}

	for name, found := range pluginNames {
		if !found {
			t.Errorf("plugin %s not found in settings", name)
		}
	}
}

func TestConfigInitPreservesExistingSettings(t *testing.T) {
	settingsPath, cleanup := setupTestConfigEnv(t)
	defer cleanup()
	worktreeBaseDir := filepath.Join(t.TempDir(), "worktrees")

	// Create temp plugin directory with 2 plugins
	pluginDir := t.TempDir()
	plugins := []string{
		"bossd-plugin-alpha",
		"bossd-plugin-beta",
	}
	for _, name := range plugins {
		path := filepath.Join(pluginDir, name)
		if err := os.WriteFile(path, []byte("dummy"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Create existing settings with non-plugin config
	existingSettings := config.Settings{
		WorktreeBaseDir:     worktreeBaseDir,
		PollIntervalSeconds: 120,
		Plugins: []config.PluginConfig{
			{
				Name:    "alpha",
				Path:    "/old/path/alpha",
				Enabled: false,
				Version: "0.0.1",
				Config:  map[string]string{"dangerously_skip_permissions": "true"},
			},
		},
	}
	if err := config.SaveTo(settingsPath, existingSettings); err != nil {
		t.Fatal(err)
	}

	// Run config init
	cmd := &cobra.Command{}
	cmd.Flags().String("plugin-dir", "", "")
	_ = cmd.Flags().Set("plugin-dir", pluginDir)

	if err := runConfigInit(cmd); err != nil {
		t.Fatalf("runConfigInit: %v", err)
	}

	// Load settings and verify non-plugin settings preserved
	s, err := config.LoadFrom(settingsPath)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}

	if s.WorktreeBaseDir != worktreeBaseDir {
		t.Errorf("WorktreeBaseDir: got %q, want %q (preserved)", s.WorktreeBaseDir, worktreeBaseDir)
	}
	if s.PollIntervalSeconds != 120 {
		t.Errorf("PollIntervalSeconds: got %d, want 120 (preserved)", s.PollIntervalSeconds)
	}

	// Verify plugin entries updated
	if len(s.Plugins) != 2 {
		t.Fatalf("Plugins: got %d, want 2", len(s.Plugins))
	}

	// Check alpha was updated
	var alpha *config.PluginConfig
	for i := range s.Plugins {
		if s.Plugins[i].Name == "alpha" {
			alpha = &s.Plugins[i]
			break
		}
	}
	if alpha == nil {
		t.Fatal("alpha plugin not found")
	}
	if alpha.Enabled {
		t.Error("alpha: expected Enabled=false (preserved, not re-enabled by config init)")
	}
	expectedPath, _ := filepath.Abs(filepath.Join(pluginDir, "bossd-plugin-alpha"))
	if alpha.Path != expectedPath {
		t.Errorf("alpha: Path=%q, want %q (updated)", alpha.Path, expectedPath)
	}
}

func TestConfigInitMissingDirectory(t *testing.T) {
	_, cleanup := setupTestConfigEnv(t)
	defer cleanup()

	pluginDir := filepath.Join(t.TempDir(), "nonexistent")

	cmd := &cobra.Command{}
	cmd.Flags().String("plugin-dir", "", "")
	_ = cmd.Flags().Set("plugin-dir", pluginDir)

	err := runConfigInit(cmd)
	if err == nil {
		t.Fatal("expected error for missing directory")
	}
	// Check error message contains the path
	if err.Error() != "plugin directory not found: "+pluginDir {
		t.Errorf("expected 'not found' error, got: %v", err)
	}
}

func TestConfigInitEmptyDirectory(t *testing.T) {
	settingsPath, cleanup := setupTestConfigEnv(t)
	defer cleanup()

	pluginDir := t.TempDir() // empty directory

	cmd := &cobra.Command{}
	cmd.Flags().String("plugin-dir", "", "")
	_ = cmd.Flags().Set("plugin-dir", pluginDir)

	// Should succeed but print warning (tested via stderr in integration tests)
	if err := runConfigInit(cmd); err != nil {
		t.Fatalf("runConfigInit: %v", err)
	}

	// Settings should be created with defaults (no plugins)
	s, err := config.LoadFrom(settingsPath)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}

	if len(s.Plugins) != 0 {
		t.Errorf("Plugins: got %d, want 0 (empty dir)", len(s.Plugins))
	}
}

func TestConfigInitIdempotent(t *testing.T) {
	settingsPath, cleanup := setupTestConfigEnv(t)
	defer cleanup()

	// Create temp plugin directory
	pluginDir := t.TempDir()
	plugins := []string{
		"bossd-plugin-alpha",
	}
	for _, name := range plugins {
		path := filepath.Join(pluginDir, name)
		if err := os.WriteFile(path, []byte("dummy"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cmd := &cobra.Command{}
	cmd.Flags().String("plugin-dir", "", "")
	_ = cmd.Flags().Set("plugin-dir", pluginDir)

	// Run twice
	if err := runConfigInit(cmd); err != nil {
		t.Fatalf("runConfigInit (first): %v", err)
	}

	s1, err := config.LoadFrom(settingsPath)
	if err != nil {
		t.Fatalf("LoadFrom (first): %v", err)
	}

	if err := runConfigInit(cmd); err != nil {
		t.Fatalf("runConfigInit (second): %v", err)
	}

	s2, err := config.LoadFrom(settingsPath)
	if err != nil {
		t.Fatalf("LoadFrom (second): %v", err)
	}

	// Settings should be unchanged after second run
	if len(s1.Plugins) != len(s2.Plugins) {
		t.Errorf("Plugins count changed: first=%d, second=%d", len(s1.Plugins), len(s2.Plugins))
	}
	if len(s2.Plugins) > 0 {
		if s1.Plugins[0].Path != s2.Plugins[0].Path {
			t.Errorf("Plugin path changed on second run")
		}
	}
}

// newConfigInitCmd builds the minimal cobra command runConfigInit reads its
// flag from, so the handler can be driven without the whole CLI tree.
func newConfigInitCmd(t *testing.T, pluginDir string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.Flags().String("plugin-dir", "", "")
	if err := cmd.Flags().Set("plugin-dir", pluginDir); err != nil {
		t.Fatalf("set --plugin-dir: %v", err)
	}
	return cmd
}

// writePluginBinaries populates a directory the way the official installer does.
func writePluginBinaries(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatalf("write %s: %v", n, err)
		}
	}
	return dir
}

// readPluginEnabledFlags reads the raw JSON rather than round-tripping through
// config.Settings, so the assertion is about the bytes `config init` wrote into
// the user's settings.json.
func readPluginEnabledFlags(t *testing.T, path string) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var parsed struct {
		Plugins []struct {
			Name    string `json:"name"`
			Enabled bool   `json:"enabled"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse settings %s: %v", raw, err)
	}
	got := make(map[string]bool, len(parsed.Plugins))
	for _, p := range parsed.Plugins {
		got[p.Name] = p.Enabled
	}
	return got
}

// TestConfigInitWritesExperimentalPluginDisabled pins BOS-1145 at the site the
// official installer runs: `boss config init --plugin-dir` scans the directory
// the installer populated, so it DOES author an opencode entry. bossd's gate
// would turn it off at load anyway, but leaving "enabled": true in the file the
// installer writes is a misleading half-state.
func TestConfigInitWritesExperimentalPluginDisabled(t *testing.T) {
	settingsPath, cleanup := setupTestConfigEnv(t)
	defer cleanup()
	dir := writePluginBinaries(t, "bossd-plugin-claude", "bossd-plugin-opencode")

	if err := runConfigInit(newConfigInitCmd(t, dir)); err != nil {
		t.Fatalf("runConfigInit: %v", err)
	}

	got := readPluginEnabledFlags(t, settingsPath)
	if enabled, ok := got["claude"]; !ok || !enabled {
		t.Errorf("claude enabled = %v (present=%v), want true", enabled, ok)
	}
	if enabled, ok := got["opencode"]; !ok {
		t.Errorf("opencode entry missing; it must still be listed so it can be turned on: %v", got)
	} else if enabled {
		t.Error(`opencode enabled = true, want false (experimental plugins are opt-in)`)
	}
}

// TestConfigInitNamesTheDisabledExperimentalPlugin pins the operator-facing half
// of BOS-1145. `config init` writes "enabled": false for opencode, but its only
// output was "Configured N plugins" — a count that includes the plugin it just
// turned off, so it reads as "all of these are on". The other two places this
// state surfaces (a bossd log line and the settings reference) do not reach the
// person who just ran the installer's own command, and settings.json cannot
// carry a comment, so this line is the only hint at the point of use. It must
// name both the plugin and the settings key that turns it on.
func TestConfigInitNamesTheDisabledExperimentalPlugin(t *testing.T) {
	_, cleanup := setupTestConfigEnv(t)
	defer cleanup()
	dir := writePluginBinaries(t, "bossd-plugin-claude", "bossd-plugin-opencode")

	var runErr error
	out := captureStdout(t, func() {
		runErr = runConfigInit(newConfigInitCmd(t, dir))
	})
	if runErr != nil {
		t.Fatalf("runConfigInit: %v", runErr)
	}

	if !strings.Contains(out, "opencode") {
		t.Errorf("output does not name the plugin it disabled; got:\n%s", out)
	}
	if !strings.Contains(out, "experimental_plugins") {
		t.Errorf("output does not name the settings key that would enable it; got:\n%s", out)
	}
	if strings.Contains(out, "claude,") || strings.Contains(out, "left off: claude") {
		t.Errorf("a non-experimental plugin was reported as left off; got:\n%s", out)
	}
}

// TestConfigInitSaysNothingWhenTheUserHasOptedIn is the other half: the notice
// is keyed on the opt-in list, so a user who already opted in must not be told
// to opt in again. The seed deliberately pairs a present experimental_plugins
// entry with "enabled": false, because that is the pair config init itself
// authors — it writes the entry disabled and never rewrites the flag, so the
// persisted flag stays false however the user opts in.
func TestConfigInitSaysNothingWhenTheUserHasOptedIn(t *testing.T) {
	settingsPath, cleanup := setupTestConfigEnv(t)
	defer cleanup()
	dir := writePluginBinaries(t, "bossd-plugin-claude", "bossd-plugin-opencode")

	s := config.DefaultSettings()
	s.ExperimentalPlugins = []string{"opencode"}
	s.Plugins = []config.PluginConfig{{Name: "opencode", Path: "/stale/bossd-plugin-opencode", Enabled: false}}
	if err := config.SaveTo(settingsPath, s); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	var runErr error
	out := captureStdout(t, func() {
		runErr = runConfigInit(newConfigInitCmd(t, dir))
	})
	if runErr != nil {
		t.Fatalf("runConfigInit: %v", runErr)
	}

	if strings.Contains(out, "left off") {
		t.Errorf("told an opted-in user to opt in again; got:\n%s", out)
	}
}

// TestConfigInitSaysNothingWhenTheOptInIsPrefixed pins that the notice asks the
// gate rather than re-deriving membership: ApplyExperimentalPluginGate trims the
// bossd-plugin- prefix on both sides, so the fully-qualified spelling is a valid
// opt-in that the daemon honours. A hand-rolled membership test here would pass
// every other case in this file and still nag this user.
func TestConfigInitSaysNothingWhenTheOptInIsPrefixed(t *testing.T) {
	settingsPath, cleanup := setupTestConfigEnv(t)
	defer cleanup()
	dir := writePluginBinaries(t, "bossd-plugin-claude", "bossd-plugin-opencode")

	s := config.DefaultSettings()
	s.ExperimentalPlugins = []string{"bossd-plugin-opencode"}
	if err := config.SaveTo(settingsPath, s); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	var runErr error
	out := captureStdout(t, func() {
		runErr = runConfigInit(newConfigInitCmd(t, dir))
	})
	if runErr != nil {
		t.Fatalf("runConfigInit: %v", runErr)
	}

	if strings.Contains(out, "left off") {
		t.Errorf("told a user who opted in by qualified name to opt in again; got:\n%s", out)
	}
}

// TestConfigInitNamesALegacyEnabledExperimentalPlugin covers the installed base
// BOS-1145 exists to repair: an entry carrying "enabled": true from before the
// gate, with no experimental_plugins opt-in. The daemon forces it off at load
// and deliberately does not persist that, so the stored flag stays true and says
// nothing about what will actually run. The notice must still fire, or the one
// user who most needs it is the one who never sees it.
func TestConfigInitNamesALegacyEnabledExperimentalPlugin(t *testing.T) {
	settingsPath, cleanup := setupTestConfigEnv(t)
	defer cleanup()
	dir := writePluginBinaries(t, "bossd-plugin-claude", "bossd-plugin-opencode")

	s := config.DefaultSettings()
	s.Plugins = []config.PluginConfig{{Name: "opencode", Path: "/stale/bossd-plugin-opencode", Enabled: true}}
	if err := config.SaveTo(settingsPath, s); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	var runErr error
	out := captureStdout(t, func() {
		runErr = runConfigInit(newConfigInitCmd(t, dir))
	})
	if runErr != nil {
		t.Fatalf("runConfigInit: %v", runErr)
	}

	if !strings.Contains(out, "opencode") {
		t.Errorf("output does not name the plugin the daemon will force off; got:\n%s", out)
	}
	if !strings.Contains(out, "experimental_plugins") {
		t.Errorf("output does not name the settings key that would enable it; got:\n%s", out)
	}
}

// TestConfigInitPreservesExistingExperimentalEntry guards the documented
// preserve-existing branch: config init repairs an existing entry's path and
// version but must never rewrite its Enabled flag, in either direction.
func TestConfigInitPreservesExistingExperimentalEntry(t *testing.T) {
	settingsPath, cleanup := setupTestConfigEnv(t)
	defer cleanup()
	dir := writePluginBinaries(t, "bossd-plugin-claude", "bossd-plugin-opencode")

	s := config.DefaultSettings()
	s.Plugins = []config.PluginConfig{
		{Name: "opencode", Path: "/stale/bossd-plugin-opencode", Enabled: true},
		{Name: "claude", Path: "/stale/bossd-plugin-claude", Enabled: false},
	}
	if err := config.SaveTo(settingsPath, s); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	if err := runConfigInit(newConfigInitCmd(t, dir)); err != nil {
		t.Fatalf("runConfigInit: %v", err)
	}

	got := readPluginEnabledFlags(t, settingsPath)
	if !got["opencode"] {
		t.Errorf("opencode enabled = false, want the user's existing true preserved: %v", got)
	}
	if got["claude"] {
		t.Errorf("claude enabled = true, want the user's existing false preserved: %v", got)
	}

	loaded, err := config.LoadFrom(settingsPath)
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	for _, p := range loaded.Plugins {
		if want := filepath.Join(dir, "bossd-plugin-"+p.Name); p.Path != want {
			t.Errorf("%s.Path = %q, want the repaired %q", p.Name, p.Path, want)
		}
	}
}
