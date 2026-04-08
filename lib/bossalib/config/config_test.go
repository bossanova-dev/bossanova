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

func TestAutopilotConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	original := Settings{
		WorktreeBaseDir: "/test",
		Autopilot: AutopilotConfig{
			Skills: AutopilotSkills{
				Plan:      "custom-plan",
				Implement: "custom-implement",
				Handoff:   "custom-handoff",
				Resume:    "custom-resume",
				Verify:    "custom-verify",
				Land:      "custom-land",
			},
			HandoffDir:          "my/handoffs",
			PollIntervalSeconds: 10,
			MaxFlightLegs:       5,
			ConfirmLand:         true,
		},
	}

	if err := SaveTo(path, original); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}

	loaded, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}

	ap := loaded.Autopilot
	if ap.Skills.Plan != "custom-plan" {
		t.Errorf("Skills.Plan: got %q, want %q", ap.Skills.Plan, "custom-plan")
	}
	if ap.Skills.Implement != "custom-implement" {
		t.Errorf("Skills.Implement: got %q, want %q", ap.Skills.Implement, "custom-implement")
	}
	if ap.Skills.Handoff != "custom-handoff" {
		t.Errorf("Skills.Handoff: got %q, want %q", ap.Skills.Handoff, "custom-handoff")
	}
	if ap.Skills.Resume != "custom-resume" {
		t.Errorf("Skills.Resume: got %q, want %q", ap.Skills.Resume, "custom-resume")
	}
	if ap.Skills.Verify != "custom-verify" {
		t.Errorf("Skills.Verify: got %q, want %q", ap.Skills.Verify, "custom-verify")
	}
	if ap.Skills.Land != "custom-land" {
		t.Errorf("Skills.Land: got %q, want %q", ap.Skills.Land, "custom-land")
	}
	if ap.HandoffDir != "my/handoffs" {
		t.Errorf("HandoffDir: got %q, want %q", ap.HandoffDir, "my/handoffs")
	}
	if ap.PollIntervalSeconds != 10 {
		t.Errorf("PollIntervalSeconds: got %d, want 10", ap.PollIntervalSeconds)
	}
	if ap.MaxFlightLegs != 5 {
		t.Errorf("MaxFlightLegs: got %d, want 5", ap.MaxFlightLegs)
	}
	if !ap.ConfirmLand {
		t.Error("ConfirmLand: got false, want true")
	}
}

func TestAutopilotConfigDefaults(t *testing.T) {
	var c AutopilotConfig

	if got := c.HandoffDirectory(); got != "docs/handoffs" {
		t.Errorf("HandoffDirectory(): got %q, want %q", got, "docs/handoffs")
	}
	if got := c.PollInterval(); got != 5*time.Second {
		t.Errorf("PollInterval(): got %v, want %v", got, 5*time.Second)
	}
	if got := c.MaxLegs(); got != 20 {
		t.Errorf("MaxLegs(): got %d, want 20", got)
	}
}

func TestAutopilotConfigPartialOverrides(t *testing.T) {
	c := AutopilotConfig{
		HandoffDir:    "custom/dir",
		MaxFlightLegs: 10,
		// PollIntervalSeconds left at zero — should use default
	}

	if got := c.HandoffDirectory(); got != "custom/dir" {
		t.Errorf("HandoffDirectory(): got %q, want %q", got, "custom/dir")
	}
	if got := c.PollInterval(); got != 5*time.Second {
		t.Errorf("PollInterval(): got %v, want %v (default)", got, 5*time.Second)
	}
	if got := c.MaxLegs(); got != 10 {
		t.Errorf("MaxLegs(): got %d, want 10", got)
	}
}

func TestAutopilotSkillNameDefaults(t *testing.T) {
	var c AutopilotConfig

	tests := []struct {
		step     string
		expected string
	}{
		{"plan", "boss-create-tasks"},
		{"implement", "boss-implement"},
		{"handoff", "boss-handoff"},
		{"resume", "boss-resume"},
		{"verify", "boss-verify"},
		{"land", "boss-finalize"},
		{"unknown", ""},
	}

	for _, tt := range tests {
		t.Run(tt.step, func(t *testing.T) {
			if got := c.SkillName(tt.step); got != tt.expected {
				t.Errorf("SkillName(%q): got %q, want %q", tt.step, got, tt.expected)
			}
		})
	}
}

func TestAutopilotSkillNameOverrides(t *testing.T) {
	c := AutopilotConfig{
		Skills: AutopilotSkills{
			Plan:   "my-plan-skill",
			Verify: "my-verify-skill",
		},
	}

	if got := c.SkillName("plan"); got != "my-plan-skill" {
		t.Errorf("SkillName(plan): got %q, want %q", got, "my-plan-skill")
	}
	if got := c.SkillName("verify"); got != "my-verify-skill" {
		t.Errorf("SkillName(verify): got %q, want %q", got, "my-verify-skill")
	}
	// Non-overridden steps should still return defaults.
	if got := c.SkillName("implement"); got != "boss-implement" {
		t.Errorf("SkillName(implement): got %q, want %q", got, "boss-implement")
	}
}

func TestAutopilotConfigOmittedWhenZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	original := Settings{WorktreeBaseDir: "/test"}

	if err := SaveTo(path, original); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	if bytes.Contains(data, []byte(`"autopilot"`)) {
		t.Error("expected autopilot to be omitted from JSON when zero value")
	}
}

func TestAutopilotBackwardsCompatible(t *testing.T) {
	// Settings JSON without an "autopilot" key should load fine.
	path := filepath.Join(t.TempDir(), "settings.json")
	jsonData := []byte(`{"dangerously_skip_permissions": false, "worktree_base_dir": "/test"}`)
	if err := os.WriteFile(path, jsonData, 0o644); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}

	if loaded.WorktreeBaseDir != "/test" {
		t.Errorf("WorktreeBaseDir: got %q, want %q", loaded.WorktreeBaseDir, "/test")
	}
	// AutopilotConfig should be zero value, defaults should still work.
	if got := loaded.Autopilot.HandoffDirectory(); got != "docs/handoffs" {
		t.Errorf("HandoffDirectory(): got %q, want %q", got, "docs/handoffs")
	}
	if got := loaded.Autopilot.MaxLegs(); got != 20 {
		t.Errorf("MaxLegs(): got %d, want 20", got)
	}
}

func TestPluginVersionField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	original := Settings{
		WorktreeBaseDir: "/test",
		Plugins: []PluginConfig{
			{
				Name:    "autopilot",
				Path:    "/usr/local/bin/bossd-plugin-autopilot",
				Enabled: true,
				Version: "1.2.3",
			},
			{
				Name:    "dependabot",
				Path:    "/usr/local/bin/bossd-plugin-dependabot",
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
				"name": "autopilot",
				"path": "/usr/local/bin/bossd-plugin-autopilot",
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
	if loaded.Plugins[0].Name != "autopilot" {
		t.Errorf("Plugins[0].Name: got %q, want %q", loaded.Plugins[0].Name, "autopilot")
	}
	if !loaded.Plugins[0].Enabled {
		t.Error("Plugins[0].Enabled: got false, want true")
	}
}

func TestDiscoverPluginsFindsPlugins(t *testing.T) {
	// Create a temp dir mimicking Homebrew layout:
	//   <cellar>/bin/bossd
	//   <cellar>/libexec/plugins/bossd-plugin-autopilot
	//   <cellar>/libexec/plugins/bossd-plugin-repair
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	pluginDir := filepath.Join(tmp, "libexec", "plugins")

	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create two of three known plugin binaries.
	for _, name := range []string{"bossd-plugin-autopilot", "bossd-plugin-repair"} {
		if err := os.WriteFile(filepath.Join(pluginDir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	plugins := discoverPluginsFrom(binDir)
	if len(plugins) != 2 {
		t.Fatalf("got %d plugins, want 2", len(plugins))
	}

	// ReadDir returns entries in alphabetical order (autopilot before repair).
	if plugins[0].Name != "autopilot" {
		t.Errorf("plugins[0].Name: got %q, want %q", plugins[0].Name, "autopilot")
	}
	if plugins[1].Name != "repair" {
		t.Errorf("plugins[1].Name: got %q, want %q", plugins[1].Name, "repair")
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

	for _, name := range []string{"bossd-plugin-autopilot", "bossd-plugin-dependabot"} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	plugins := discoverPluginsFrom(binDir)
	if len(plugins) != 2 {
		t.Fatalf("got %d plugins, want 2", len(plugins))
	}
	if plugins[0].Name != "autopilot" {
		t.Errorf("plugins[0].Name: got %q, want %q", plugins[0].Name, "autopilot")
	}
	if plugins[1].Name != "dependabot" {
		t.Errorf("plugins[1].Name: got %q, want %q", plugins[1].Name, "dependabot")
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

	// Place autopilot in both locations.
	if err := os.WriteFile(filepath.Join(binDir, "bossd-plugin-autopilot"), []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(libexecDir, "bossd-plugin-autopilot"), []byte("libexec"), 0o755); err != nil {
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
