package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/recurser/bossalib/config"
	"github.com/spf13/cobra"
)

// setupTestConfigEnv creates a temporary config directory and sets env vars
// to make config.Path() use it. Returns cleanup function.
func setupTestConfigEnv(t *testing.T) (settingsPath string, cleanup func()) {
	t.Helper()
	tempHome := t.TempDir()

	var configDir string
	if runtime.GOOS == "darwin" {
		configDir = filepath.Join(tempHome, "Library", "Application Support", "bossanova")
	} else {
		configDir = filepath.Join(tempHome, ".config", "bossanova")
	}
	settingsPath = filepath.Join(configDir, "settings.json")

	// Set USER_CONFIG_DIR or HOME to make config.Path() use our temp dir
	oldHome := os.Getenv("HOME")
	oldXDG := os.Getenv("XDG_CONFIG_HOME")

	_ = os.Setenv("HOME", tempHome)
	if runtime.GOOS != "darwin" {
		_ = os.Setenv("XDG_CONFIG_HOME", filepath.Join(tempHome, ".config"))
	}

	cleanup = func() {
		_ = os.Setenv("HOME", oldHome)
		_ = os.Setenv("XDG_CONFIG_HOME", oldXDG)
	}

	return settingsPath, cleanup
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
		DangerouslySkipPermissions: true,
		WorktreeBaseDir:            "/custom/worktrees",
		PollIntervalSeconds:        120,
		Plugins: []config.PluginConfig{
			{
				Name:    "alpha",
				Path:    "/old/path/alpha",
				Enabled: false,
				Version: "0.0.1",
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

	if !s.DangerouslySkipPermissions {
		t.Error("DangerouslySkipPermissions: got false, want true (preserved)")
	}
	if s.WorktreeBaseDir != "/custom/worktrees" {
		t.Errorf("WorktreeBaseDir: got %q, want %q (preserved)", s.WorktreeBaseDir, "/custom/worktrees")
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
