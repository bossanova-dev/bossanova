package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultSettings(t *testing.T) {
	s := DefaultSettings()
	if s.DangerouslySkipPermissions {
		t.Error("expected DangerouslySkipPermissions=false by default")
	}
	if s.WorktreeBaseDir == "" {
		t.Error("expected non-empty WorktreeBaseDir")
	}
}

func TestLoadMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nonexistent.json")
	s, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("expected no error for missing file, got %v", err)
	}
	if s.DangerouslySkipPermissions {
		t.Error("expected defaults for missing file")
	}
}

func TestSaveAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "settings.json")
	original := Settings{
		DangerouslySkipPermissions: true,
		WorktreeBaseDir:            "/custom/worktrees",
		PollIntervalSeconds:        60,
	}

	if err := SaveTo(path, original); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}

	loaded, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}

	if loaded.DangerouslySkipPermissions != original.DangerouslySkipPermissions {
		t.Errorf("DangerouslySkipPermissions: got %v, want %v",
			loaded.DangerouslySkipPermissions, original.DangerouslySkipPermissions)
	}
	if loaded.WorktreeBaseDir != original.WorktreeBaseDir {
		t.Errorf("WorktreeBaseDir: got %q, want %q",
			loaded.WorktreeBaseDir, original.WorktreeBaseDir)
	}
	if loaded.PollIntervalSeconds != original.PollIntervalSeconds {
		t.Errorf("PollIntervalSeconds: got %d, want %d",
			loaded.PollIntervalSeconds, original.PollIntervalSeconds)
	}
}

func TestLoadMalformedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte("{bad json"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := LoadFrom(path)
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
	// Should return defaults on error.
	if s.DangerouslySkipPermissions {
		t.Error("expected defaults on parse error")
	}
}

func TestSaveCreatesDirectories(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b", "c", "settings.json")
	s := Settings{WorktreeBaseDir: "/test"}
	if err := SaveTo(path, s); err != nil {
		t.Fatalf("SaveTo with nested dirs: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file not created: %v", err)
	}
}

func TestDisplayPollInterval(t *testing.T) {
	tests := []struct {
		name     string
		seconds  int
		expected time.Duration
	}{
		{
			name:     "zero returns default 2m",
			seconds:  0,
			expected: 2 * time.Minute,
		},
		{
			name:     "negative returns default 2m",
			seconds:  -5,
			expected: 2 * time.Minute,
		},
		{
			name:     "custom value",
			seconds:  60,
			expected: 60 * time.Second,
		},
		{
			name:     "minimum value of 1",
			seconds:  1,
			expected: 1 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := Settings{PollIntervalSeconds: tt.seconds}
			got := s.DisplayPollInterval()
			if got != tt.expected {
				t.Errorf("DisplayPollInterval() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestPluginsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	original := Settings{
		WorktreeBaseDir: "/test",
		Plugins: []PluginConfig{
			{
				Name:    "github-issues",
				Path:    "/usr/local/bin/boss-plugin-github",
				Enabled: true,
				Config:  map[string]string{"token": "abc123"},
			},
			{
				Name:    "linear",
				Path:    "/usr/local/bin/boss-plugin-linear",
				Enabled: false,
			},
		},
	}

	if err := SaveTo(path, original); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}

	loaded, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}

	if len(loaded.Plugins) != 2 {
		t.Fatalf("Plugins: got %d, want 2", len(loaded.Plugins))
	}
	if loaded.Plugins[0].Name != "github-issues" {
		t.Errorf("Plugins[0].Name: got %q, want %q", loaded.Plugins[0].Name, "github-issues")
	}
	if !loaded.Plugins[0].Enabled {
		t.Error("Plugins[0].Enabled: got false, want true")
	}
	if loaded.Plugins[0].Config["token"] != "abc123" {
		t.Errorf("Plugins[0].Config[token]: got %q, want %q", loaded.Plugins[0].Config["token"], "abc123")
	}
	if loaded.Plugins[1].Enabled {
		t.Error("Plugins[1].Enabled: got true, want false")
	}
	if loaded.Plugins[1].Config != nil {
		t.Errorf("Plugins[1].Config: got %v, want nil", loaded.Plugins[1].Config)
	}
}

func TestPluginsOmittedWhenEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	original := Settings{WorktreeBaseDir: "/test"}

	if err := SaveTo(path, original); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	// With omitempty, "plugins" should not appear in the JSON.
	if bytes.Contains(data, []byte(`"plugins"`)) {
		t.Error("expected plugins to be omitted from JSON when empty")
	}
}

func TestPollIntervalOmittedFromJSON(t *testing.T) {
	// When PollIntervalSeconds is 0, it should be omitted from JSON (omitempty).
	path := filepath.Join(t.TempDir(), "settings.json")
	original := Settings{WorktreeBaseDir: "/test"}

	if err := SaveTo(path, original); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}

	loaded, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if loaded.PollIntervalSeconds != 0 {
		t.Errorf("PollIntervalSeconds: got %d, want 0 (omitted)", loaded.PollIntervalSeconds)
	}
}

func TestPluginVersionField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	original := Settings{
		WorktreeBaseDir: "/test",
		Plugins: []PluginConfig{
			{
				Name:    "alpha",
				Path:    "/usr/local/bin/bossd-plugin-alpha",
				Enabled: true,
				Version: "1.2.3",
			},
			{
				Name:    "beta",
				Path:    "/usr/local/bin/bossd-plugin-beta",
				Enabled: true,
				// No version specified (should be omitted)
			},
		},
	}

	if err := SaveTo(path, original); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}

	loaded, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}

	if len(loaded.Plugins) != 2 {
		t.Fatalf("Plugins: got %d, want 2", len(loaded.Plugins))
	}
	if loaded.Plugins[0].Version != "1.2.3" {
		t.Errorf("Plugins[0].Version: got %q, want %q", loaded.Plugins[0].Version, "1.2.3")
	}
	if loaded.Plugins[1].Version != "" {
		t.Errorf("Plugins[1].Version: got %q, want empty string", loaded.Plugins[1].Version)
	}
}

func TestPluginVersionBackwardsCompatible(t *testing.T) {
	// Settings JSON without "version" field should load fine (backwards compatibility).
	path := filepath.Join(t.TempDir(), "settings.json")
	jsonData := []byte(`{
		"worktree_base_dir": "/test",
		"plugins": [
			{
				"name": "alpha",
				"path": "/usr/local/bin/bossd-plugin-alpha",
				"enabled": true
			}
		]
	}`)
	if err := os.WriteFile(path, jsonData, 0o644); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}

	if len(loaded.Plugins) != 1 {
		t.Fatalf("Plugins: got %d, want 1", len(loaded.Plugins))
	}
	if loaded.Plugins[0].Version != "" {
		t.Errorf("Plugins[0].Version: got %q, want empty string (omitted field)", loaded.Plugins[0].Version)
	}
	if loaded.Plugins[0].Name != "alpha" {
		t.Errorf("Plugins[0].Name: got %q, want %q", loaded.Plugins[0].Name, "alpha")
	}
	if !loaded.Plugins[0].Enabled {
		t.Error("Plugins[0].Enabled: got false, want true")
	}
}

func TestDiscoverPluginsFindsPlugins(t *testing.T) {
	// Create a temp dir mimicking Homebrew layout:
	//   <cellar>/bin/bossd
	//   <cellar>/libexec/plugins/bossd-plugin-alpha
	//   <cellar>/libexec/plugins/bossd-plugin-beta
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	pluginDir := filepath.Join(tmp, "libexec", "plugins")

	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create two plugin binaries for discovery.
	for _, name := range []string{"bossd-plugin-alpha", "bossd-plugin-beta"} {
		if err := os.WriteFile(filepath.Join(pluginDir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	plugins := discoverPluginsFrom(binDir)
	if len(plugins) != 2 {
		t.Fatalf("got %d plugins, want 2", len(plugins))
	}

	// ReadDir returns entries in alphabetical order (alpha before beta).
	if plugins[0].Name != "alpha" {
		t.Errorf("plugins[0].Name: got %q, want %q", plugins[0].Name, "alpha")
	}
	if plugins[1].Name != "beta" {
		t.Errorf("plugins[1].Name: got %q, want %q", plugins[1].Name, "beta")
	}

	for _, p := range plugins {
		if !p.Enabled {
			t.Errorf("plugin %q: Enabled should be true", p.Name)
		}
		if !filepath.IsAbs(p.Path) {
			t.Errorf("plugin %q: path should be absolute, got %q", p.Name, p.Path)
		}
	}
}

func TestDiscoverPluginsEmptyWhenNoPlugins(t *testing.T) {
	// An empty bin dir with no ../libexec/plugins/ should return nil.
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	plugins := discoverPluginsFrom(binDir)
	if len(plugins) != 0 {
		t.Errorf("got %d plugins, want 0", len(plugins))
	}
}

func TestDiscoverPluginsEmptyWhenDirMissing(t *testing.T) {
	// When the libexec/plugins dir doesn't exist at all.
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "nonexistent", "bin")

	plugins := discoverPluginsFrom(binDir)
	if len(plugins) != 0 {
		t.Errorf("got %d plugins, want 0", len(plugins))
	}
}

func TestDiscoverPluginsFallsBackToSameDir(t *testing.T) {
	// Dev mode: plugins sit alongside the binary in the same directory.
	// No ../libexec/plugins/ exists.
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"bossd-plugin-alpha", "bossd-plugin-beta"} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	plugins := discoverPluginsFrom(binDir)
	if len(plugins) != 2 {
		t.Fatalf("got %d plugins, want 2", len(plugins))
	}
	if plugins[0].Name != "alpha" {
		t.Errorf("plugins[0].Name: got %q, want %q", plugins[0].Name, "alpha")
	}
	if plugins[1].Name != "beta" {
		t.Errorf("plugins[1].Name: got %q, want %q", plugins[1].Name, "beta")
	}
}

func TestDiscoverPluginsPrefersLibexec(t *testing.T) {
	// When plugins exist in both ../libexec/plugins/ and the binary dir,
	// the libexec path should win.
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	libexecDir := filepath.Join(tmp, "libexec", "plugins")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(libexecDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Place alpha in both locations.
	if err := os.WriteFile(filepath.Join(binDir, "bossd-plugin-alpha"), []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(libexecDir, "bossd-plugin-alpha"), []byte("libexec"), 0o755); err != nil {
		t.Fatal(err)
	}

	plugins := discoverPluginsFrom(binDir)
	if len(plugins) != 1 {
		t.Fatalf("got %d plugins, want 1", len(plugins))
	}
	if !strings.Contains(plugins[0].Path, "libexec") {
		t.Errorf("expected libexec path, got %q", plugins[0].Path)
	}
}

func TestDedupPluginConfigs(t *testing.T) {
	cases := []struct {
		name        string
		in          []PluginConfig
		wantNames   []string
		wantPaths   []string
		wantDropped bool
	}{
		{
			name:        "empty",
			in:          nil,
			wantNames:   nil,
			wantDropped: false,
		},
		{
			name: "single entry",
			in: []PluginConfig{
				{Name: "repair", Path: "/a", Enabled: true},
			},
			wantNames:   []string{"repair"},
			wantPaths:   []string{"/a"},
			wantDropped: false,
		},
		{
			name: "no duplicates",
			in: []PluginConfig{
				{Name: "repair", Path: "/a", Enabled: true},
				{Name: "linear", Path: "/b", Enabled: true},
			},
			wantNames:   []string{"repair", "linear"},
			wantPaths:   []string{"/a", "/b"},
			wantDropped: false,
		},
		{
			name: "duplicate keeps first",
			in: []PluginConfig{
				{Name: "repair", Path: "/first", Enabled: true},
				{Name: "linear", Path: "/b", Enabled: true},
				{Name: "repair", Path: "/second", Enabled: true},
			},
			wantNames:   []string{"repair", "linear"},
			wantPaths:   []string{"/first", "/b"},
			wantDropped: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, dropped := DedupPluginConfigs(tc.in)
			if dropped != tc.wantDropped {
				t.Errorf("dropped = %v, want %v", dropped, tc.wantDropped)
			}
			if len(got) != len(tc.wantNames) {
				t.Fatalf("got %d entries, want %d", len(got), len(tc.wantNames))
			}
			for i, want := range tc.wantNames {
				if got[i].Name != want {
					t.Errorf("entry %d name = %q, want %q", i, got[i].Name, want)
				}
				if got[i].Path != tc.wantPaths[i] {
					t.Errorf("entry %d path = %q, want %q", i, got[i].Path, tc.wantPaths[i])
				}
			}
		})
	}
}
