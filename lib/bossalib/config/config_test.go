package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func boolPtr(value bool) *bool {
	return &value
}

func TestDefaultSettings(t *testing.T) {
	s := DefaultSettings()
	if s.WorktreeBaseDir == "" {
		t.Error("expected non-empty WorktreeBaseDir")
	}
}

func TestNotificationsEnabled(t *testing.T) {
	tests := []struct {
		name  string
		value *bool
		want  bool
	}{
		{name: "nil defaults to enabled", want: true},
		{name: "true remains enabled", value: boolPtr(true), want: true},
		{name: "false disables", value: boolPtr(false), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NotificationsEnabled(Settings{NotificationsEnabled: tt.value}); got != tt.want {
				t.Errorf("NotificationsEnabled() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestSettingsNotificationsEnabledJSON(t *testing.T) {
	tests := []struct {
		name          string
		input         []byte
		want          *bool
		wantJSONField bool
	}{
		{name: "absent defaults to enabled", input: []byte(`{}`), wantJSONField: false},
		{name: "false round trips", input: []byte(`{"notifications_enabled":false}`), want: boolPtr(false), wantJSONField: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var settings Settings
			if err := json.Unmarshal(tt.input, &settings); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if (settings.NotificationsEnabled == nil) != (tt.want == nil) {
				t.Fatalf("NotificationsEnabled = %v, want %v", settings.NotificationsEnabled, tt.want)
			}
			if tt.want != nil && *settings.NotificationsEnabled != *tt.want {
				t.Fatalf("NotificationsEnabled = %t, want %t", *settings.NotificationsEnabled, *tt.want)
			}

			data, err := json.Marshal(settings)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if got := strings.Contains(string(data), `"notifications_enabled":false`); got != tt.wantJSONField {
				t.Fatalf("notifications_enabled false JSON field = %t, want %t: %s", got, tt.wantJSONField, data)
			}
		})
	}
}

func TestUsageStalenessWindow(t *testing.T) {
	var c ManagedAccountsConfig
	if got := c.UsageStalenessWindow(); got != 30*time.Minute {
		t.Errorf("UsageStalenessWindow default: got %v, want %v", got, 30*time.Minute)
	}
	c.UsageStalenessWindowMinutes = 45
	if got := c.UsageStalenessWindow(); got != 45*time.Minute {
		t.Errorf("UsageStalenessWindow set: got %v, want %v", got, 45*time.Minute)
	}
	c.UsageStalenessWindowMinutes = -5
	if got := c.UsageStalenessWindow(); got != 30*time.Minute {
		t.Errorf("UsageStalenessWindow negative: got %v, want %v (default)", got, 30*time.Minute)
	}
}

func TestDefaultAgent_Default(t *testing.T) {
	s := DefaultSettings()
	if s.DefaultAgent != "claude" {
		t.Errorf("DefaultAgent: got %q, want %q", s.DefaultAgent, "claude")
	}
}

func TestLoadMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nonexistent.json")
	s, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("expected no error for missing file, got %v", err)
	}
	if s.WorktreeBaseDir == "" {
		t.Error("expected defaults for missing file")
	}
}

func TestSaveAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "settings.json")
	original := Settings{
		WorktreeBaseDir:     "/custom/worktrees",
		DefaultAgent:        "opencode",
		PollIntervalSeconds: 60,
		LoginShell:          "/opt/homebrew/bin/fish",
		SkillsDeclinedByAgent: map[string]bool{
			"codex": true,
		},
		SkillsDeclinedManifestByAgent: map[string]string{
			"codex": "declined-manifest",
		},
		SkillsInstalledManifestByAgent: map[string]string{
			"claude": "installed-manifest",
		},
	}

	if err := SaveTo(path, original); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}

	loaded, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}

	if loaded.WorktreeBaseDir != original.WorktreeBaseDir {
		t.Errorf("WorktreeBaseDir: got %q, want %q",
			loaded.WorktreeBaseDir, original.WorktreeBaseDir)
	}
	if loaded.DefaultAgent != original.DefaultAgent {
		t.Errorf("DefaultAgent: got %q, want %q",
			loaded.DefaultAgent, original.DefaultAgent)
	}
	if loaded.PollIntervalSeconds != original.PollIntervalSeconds {
		t.Errorf("PollIntervalSeconds: got %d, want %d",
			loaded.PollIntervalSeconds, original.PollIntervalSeconds)
	}
	if loaded.LoginShell != original.LoginShell {
		t.Errorf("LoginShell: got %q, want %q", loaded.LoginShell, original.LoginShell)
	}
	if !loaded.SkillsDeclinedByAgent["codex"] {
		t.Errorf("SkillsDeclinedByAgent[codex]: got false, want true")
	}
	if loaded.SkillsDeclinedManifestByAgent["codex"] != "declined-manifest" {
		t.Errorf("SkillsDeclinedManifestByAgent[codex]: got %q", loaded.SkillsDeclinedManifestByAgent["codex"])
	}
	if loaded.SkillsInstalledManifestByAgent["claude"] != "installed-manifest" {
		t.Errorf("SkillsInstalledManifestByAgent[claude]: got %q", loaded.SkillsInstalledManifestByAgent["claude"])
	}
}

// TestSaveToReturnsRenameError pins the rename error branch in SaveTo
// (config.go:678, `os.Rename(tmpName, path); err != nil`). The temp file is
// written and fsynced successfully, then renamed into place atomically; when the
// destination is an existing directory that final rename fails (EISDIR/ENOTDIR on
// both Linux and macOS). SaveTo must surface that error rather than reporting a
// write that never landed — the CONDITIONALS_NEGATION mutant drops the failure and
// returns nil.
func TestSaveToReturnsRenameError(t *testing.T) {
	// A directory at the destination path forces os.Rename(file, dir) to fail.
	target := filepath.Join(t.TempDir(), "settings-is-a-dir")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	if err := SaveTo(target, DefaultSettings()); err == nil {
		t.Fatal("SaveTo to an existing directory = nil, want a rename error")
	}
}

func TestPathUsesBossSettingsPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profile", "settings.json")
	t.Setenv("BOSS_SETTINGS_PATH", path)

	got, err := Path()
	if err != nil {
		t.Fatalf("Path() returned error: %v", err)
	}
	if got != path {
		t.Fatalf("Path() = %q, want %q", got, path)
	}
}

func TestPathRejectsRelativeBossSettingsPath(t *testing.T) {
	t.Setenv("BOSS_SETTINGS_PATH", "relative/settings.json")

	_, err := Path()
	if err == nil {
		t.Fatal("Path() error = nil, want relative path error")
	}
	if !strings.Contains(err.Error(), "BOSS_SETTINGS_PATH must be absolute") {
		t.Fatalf("Path() error = %q, want BOSS_SETTINGS_PATH absolute error", err.Error())
	}
}

func TestSettingsRuntimePathsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	in := DefaultSettings()
	in.AppDataDir = filepath.Join(t.TempDir(), "data")
	in.SocketPath = filepath.Join(t.TempDir(), "bossd.sock")

	if err := SaveTo(path, in); err != nil {
		t.Fatalf("SaveTo() returned error: %v", err)
	}

	out, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom() returned error: %v", err)
	}
	if out.AppDataDir != in.AppDataDir {
		t.Fatalf("AppDataDir = %q, want %q", out.AppDataDir, in.AppDataDir)
	}
	if out.SocketPath != in.SocketPath {
		t.Fatalf("SocketPath = %q, want %q", out.SocketPath, in.SocketPath)
	}
}

func TestConfiguredAppDataDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")

	got, ok, err := ConfiguredAppDataDir(Settings{AppDataDir: dir})
	if err != nil {
		t.Fatalf("ConfiguredAppDataDir() returned error: %v", err)
	}
	if !ok {
		t.Fatal("ConfiguredAppDataDir() ok = false, want true")
	}
	if got != dir {
		t.Fatalf("ConfiguredAppDataDir() = %q, want %q", got, dir)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("configured app data dir was not created: %v", err)
	}
}

func TestConfiguredAppDataDirRejectsRelativePath(t *testing.T) {
	_, _, err := ConfiguredAppDataDir(Settings{AppDataDir: "relative/data"})
	if err == nil {
		t.Fatal("ConfiguredAppDataDir() error = nil, want relative path error")
	}
	if !strings.Contains(err.Error(), "app_data_dir must be absolute") {
		t.Fatalf("ConfiguredAppDataDir() error = %q, want absolute path error", err.Error())
	}
}

func TestConfiguredSocketPath(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "bossd.sock")

	got, ok, err := ConfiguredSocketPath(Settings{SocketPath: socketPath})
	if err != nil {
		t.Fatalf("ConfiguredSocketPath() returned error: %v", err)
	}
	if !ok {
		t.Fatal("ConfiguredSocketPath() ok = false, want true")
	}
	if got != socketPath {
		t.Fatalf("ConfiguredSocketPath() = %q, want %q", got, socketPath)
	}
}

func TestConfiguredSocketPathDefaultsToAppDataDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")

	got, ok, err := ConfiguredSocketPath(Settings{AppDataDir: dir})
	if err != nil {
		t.Fatalf("ConfiguredSocketPath() returned error: %v", err)
	}
	if !ok {
		t.Fatal("ConfiguredSocketPath() ok = false, want true")
	}
	want := filepath.Join(dir, "bossd.sock")
	if got != want {
		t.Fatalf("ConfiguredSocketPath() = %q, want %q", got, want)
	}
}

func TestConfiguredSocketPathRejectsRelativePath(t *testing.T) {
	_, _, err := ConfiguredSocketPath(Settings{SocketPath: "relative/bossd.sock"})
	if err == nil {
		t.Fatal("ConfiguredSocketPath() error = nil, want relative path error")
	}
	if !strings.Contains(err.Error(), "socket_path must be absolute") {
		t.Fatalf("ConfiguredSocketPath() error = %q, want absolute path error", err.Error())
	}
}

func TestPostHogSettingsDefaultDisabled(t *testing.T) {
	s := DefaultSettings()

	if s.EventTracingEnabled {
		t.Fatal("EventTracingEnabled default = true, want false")
	}
	if s.PostHogProjectToken != "" {
		t.Fatalf("PostHogProjectToken default = %q, want empty", s.PostHogProjectToken)
	}
	if s.PostHogHost != "" {
		t.Fatalf("PostHogHost default = %q, want empty", s.PostHogHost)
	}
}

func TestPostHogSettingsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	in := DefaultSettings()
	in.EventTracingEnabled = true
	in.PostHogProjectToken = "phc_test"
	in.PostHogHost = "https://k.bossanova.dev"

	if err := SaveTo(path, in); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	out, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}

	if !out.EventTracingEnabled {
		t.Fatal("EventTracingEnabled did not round-trip")
	}
	if out.PostHogProjectToken != "phc_test" {
		t.Fatalf("PostHogProjectToken = %q", out.PostHogProjectToken)
	}
	if out.PostHogHost != "https://k.bossanova.dev" {
		t.Fatalf("PostHogHost = %q", out.PostHogHost)
	}
}

func TestEnsureInstalledAtBackfillsMissingTimestamp(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	settings := DefaultSettings()

	got, changed := settings.EnsureInstalledAt(now)
	if !changed {
		t.Fatal("EnsureInstalledAt changed = false, want true")
	}
	if !got.InstalledAt.Equal(now) {
		t.Fatalf("InstalledAt = %s, want %s", got.InstalledAt, now)
	}
}

func TestEnsureInstalledAtPreservesExistingTimestamp(t *testing.T) {
	installedAt := time.Date(2026, 6, 1, 9, 30, 0, 0, time.UTC)
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	settings := DefaultSettings()
	settings.InstalledAt = installedAt

	got, changed := settings.EnsureInstalledAt(now)
	if changed {
		t.Fatal("EnsureInstalledAt changed = true, want false")
	}
	if !got.InstalledAt.Equal(installedAt) {
		t.Fatalf("InstalledAt = %s, want %s", got.InstalledAt, installedAt)
	}
}

func TestCloudGuestOfferSettingsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	installedAt := time.Date(2026, 6, 1, 9, 30, 0, 0, time.UTC)
	settings := DefaultSettings()
	settings.InstalledAt = installedAt
	settings.BossCloudGuestOfferHidden = true

	if err := SaveTo(path, settings); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	loaded, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if !loaded.InstalledAt.Equal(installedAt) {
		t.Fatalf("InstalledAt = %s, want %s", loaded.InstalledAt, installedAt)
	}
	if !loaded.BossCloudGuestOfferHidden {
		t.Fatal("BossCloudGuestOfferHidden = false, want true")
	}
}

func TestEnsureBossCloudValueDeliveredAtBackfillsMissingTimestamp(t *testing.T) {
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	settings := DefaultSettings()

	got, changed := settings.EnsureBossCloudValueDeliveredAt(now)
	if !changed {
		t.Fatal("EnsureBossCloudValueDeliveredAt changed = false, want true")
	}
	if !got.BossCloudValueDeliveredAt.Equal(now) {
		t.Fatalf("BossCloudValueDeliveredAt = %s, want %s", got.BossCloudValueDeliveredAt, now)
	}
}

func TestEnsureBossCloudValueDeliveredAtPreservesExistingTimestamp(t *testing.T) {
	deliveredAt := time.Date(2026, 6, 1, 9, 30, 0, 0, time.UTC)
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	settings := DefaultSettings()
	settings.BossCloudValueDeliveredAt = deliveredAt

	got, changed := settings.EnsureBossCloudValueDeliveredAt(now)
	if changed {
		t.Fatal("EnsureBossCloudValueDeliveredAt changed = true, want false")
	}
	if !got.BossCloudValueDeliveredAt.Equal(deliveredAt) {
		t.Fatalf("BossCloudValueDeliveredAt = %s, want %s", got.BossCloudValueDeliveredAt, deliveredAt)
	}
}

func TestBossCloudValueDeliveredAtRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	deliveredAt := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	settings := DefaultSettings()

	// fresh Settings has a zero BossCloudValueDeliveredAt
	if !settings.BossCloudValueDeliveredAt.IsZero() {
		t.Fatalf("new Settings should have zero BossCloudValueDeliveredAt, got %v", settings.BossCloudValueDeliveredAt)
	}

	settings.BossCloudValueDeliveredAt = deliveredAt

	if err := SaveTo(path, settings); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	loaded, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if !loaded.BossCloudValueDeliveredAt.Equal(deliveredAt) {
		t.Fatalf("BossCloudValueDeliveredAt = %s, want %s", loaded.BossCloudValueDeliveredAt, deliveredAt)
	}
}

func TestLoadFrom_BackfillsDefaultAgent(t *testing.T) {
	// LoadFrom must guarantee a non-empty DefaultAgent on two file shapes:
	//   1. Legacy file: omits default_agent entirely (older binary).
	//   2. Hand-edited file: sets default_agent to "" explicitly.
	// Both must resolve to "claude" so downstream callers never see empty.
	cases := []struct {
		name string
		json string
	}{
		{
			name: "key omitted",
			json: `{"dangerously_skip_permissions": false, "worktree_base_dir": "/legacy/worktrees"}`,
		},
		{
			name: "key present but empty",
			json: `{"dangerously_skip_permissions": false, "worktree_base_dir": "/legacy/worktrees", "default_agent": ""}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			if err := os.WriteFile(path, []byte(tc.json), 0o644); err != nil {
				t.Fatal(err)
			}
			loaded, err := LoadFrom(path)
			if err != nil {
				t.Fatalf("LoadFrom: %v", err)
			}
			if loaded.DefaultAgent != "claude" {
				t.Errorf("DefaultAgent: got %q, want %q", loaded.DefaultAgent, "claude")
			}
		})
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
	if s.WorktreeBaseDir == "" {
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

func TestSaveToWritesAtomicWholeFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	original := Settings{
		WorktreeBaseDir: "/test",
		Plugins: []PluginConfig{
			{Name: "claude", Path: "/old/bossd-plugin-claude", Enabled: true},
		},
	}
	if err := SaveTo(path, original); err != nil {
		t.Fatalf("initial SaveTo: %v", err)
	}

	updated := Settings{
		WorktreeBaseDir: "/test",
		Plugins: []PluginConfig{
			{Name: "claude", Path: "/new/bossd-plugin-claude", Enabled: true},
			{Name: "codex", Path: "/new/bossd-plugin-codex", Enabled: false},
		},
	}
	if err := SaveTo(path, updated); err != nil {
		t.Fatalf("updated SaveTo: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var loaded Settings
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("settings JSON is not a whole valid file: %v\n%s", err, data)
	}
	if len(loaded.Plugins) != 2 {
		t.Fatalf("loaded %d plugins, want whole rewritten plugin list", len(loaded.Plugins))
	}
	if loaded.Plugins[0].Path != "/new/bossd-plugin-claude" || loaded.Plugins[1].Path != "/new/bossd-plugin-codex" {
		t.Fatalf("loaded plugin paths = %+v, want updated paths", loaded.Plugins)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp") {
			t.Fatalf("temporary settings file left behind: %s", entry.Name())
		}
	}
}

func TestSyncDirReturnsOpenError(t *testing.T) {
	err := syncDir(filepath.Join(t.TempDir(), "missing"))
	if err == nil {
		t.Fatal("syncDir() error = nil, want missing directory error")
	}
	if !os.IsNotExist(err) {
		t.Fatalf("syncDir() error = %v, want not-exist error", err)
	}
}

func TestSyncDirectoryReturnsOnlyUnexpectedSyncError(t *testing.T) {
	unexpected := io.ErrUnexpectedEOF
	other := errors.New("sync failed")

	for _, tt := range []struct {
		name    string
		syncErr error
		wantErr error
	}{
		{name: "success"},
		{name: "unexpected EOF", syncErr: unexpected},
		{name: "other error", syncErr: other, wantErr: other},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := syncDirectory(syncFile{err: tt.syncErr}); !errors.Is(err, tt.wantErr) {
				t.Fatalf("syncDirectory() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

type syncFile struct{ err error }

func (f syncFile) Sync() error { return f.err }

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

	plugins, _ := discoverPluginsFrom(binDir, activePolicy())
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

func TestMergeDiscoveredPluginsAddsUnregistered(t *testing.T) {
	// A plugin binary that exists on disk but isn't yet in the config (e.g. a
	// freshly-built bossd-plugin-sentry) should be appended so it loads without
	// a hand-edit. The existing claude entry — with a user-customized path —
	// must be preserved untouched rather than replaced by the discovered one.
	existing := []PluginConfig{
		{Name: "claude", Path: "/custom/claude", Enabled: true},
	}
	discovered := []PluginConfig{
		{Name: "claude", Path: "/bin/bossd-plugin-claude", Enabled: true},
		{Name: "sentry", Path: "/bin/bossd-plugin-sentry", Enabled: true},
	}

	merged, added := MergeDiscoveredPlugins(existing, discovered)

	if len(added) != 1 || added[0] != "sentry" {
		t.Fatalf("added = %v, want [sentry]", added)
	}
	if len(merged) != 2 {
		t.Fatalf("merged has %d entries, want 2: %+v", len(merged), merged)
	}
	if merged[0].Name != "claude" || merged[0].Path != "/custom/claude" {
		t.Errorf("existing claude entry changed: %+v", merged[0])
	}
	if merged[1].Name != "sentry" || merged[1].Path != "/bin/bossd-plugin-sentry" {
		t.Errorf("sentry entry = %+v, want discovered sentry", merged[1])
	}
}

func TestMergeDiscoveredPluginsPreservesDisabledEntry(t *testing.T) {
	// A user who disabled a plugin must keep it disabled even though discovery
	// finds the binary on disk and reports Enabled: true.
	existing := []PluginConfig{{Name: "sentry", Path: "/bin/sentry", Enabled: false}}
	discovered := []PluginConfig{{Name: "sentry", Path: "/bin/sentry", Enabled: true}}

	merged, added := MergeDiscoveredPlugins(existing, discovered)

	if len(added) != 0 {
		t.Fatalf("added = %v, want none", added)
	}
	if len(merged) != 1 || merged[0].Enabled {
		t.Fatalf("merged = %+v, want sentry still disabled", merged)
	}
}

func TestMergeDiscoveredPluginsNoChangeWhenAllRegistered(t *testing.T) {
	existing := []PluginConfig{{Name: "claude"}, {Name: "sentry"}}
	discovered := []PluginConfig{{Name: "sentry"}, {Name: "claude"}}

	merged, added := MergeDiscoveredPlugins(existing, discovered)

	if len(added) != 0 {
		t.Errorf("added = %v, want none", added)
	}
	if len(merged) != 2 {
		t.Errorf("merged has %d entries, want 2", len(merged))
	}
}

func TestFilterNonDiscoverablePluginsDropsStubRunner(t *testing.T) {
	cfgs := []PluginConfig{{Name: "claude"}, {Name: "stub-runner"}, {Name: "sentry"}}

	filtered, dropped := FilterNonDiscoverablePlugins(cfgs)

	if want := []string{"stub-runner"}; !slices.Equal(dropped, want) {
		t.Errorf("dropped = %v, want %v", dropped, want)
	}
	if len(filtered) != 2 {
		t.Fatalf("filtered has %d entries, want 2: %+v", len(filtered), filtered)
	}
	for _, c := range filtered {
		if c.Name == "stub-runner" {
			t.Errorf("stub-runner survived filtering: %+v", filtered)
		}
	}
}

// TestFilterNonDiscoverablePluginsKeepsOpencode guards the BOS-437 completion of
// the OpenCode epic: with the launch path wired and live-validated, the opencode
// binary is a functional agent runner and must NO LONGER be filtered out of a
// persisted/auto-discovered config — it is discovered like claude/codex.
func TestFilterNonDiscoverablePluginsKeepsOpencode(t *testing.T) {
	cfgs := []PluginConfig{{Name: "claude"}, {Name: "opencode"}, {Name: "codex"}}

	filtered, dropped := FilterNonDiscoverablePlugins(cfgs)

	if len(dropped) != 0 {
		t.Errorf("dropped = %v, want none (opencode is now discoverable)", dropped)
	}
	if len(filtered) != 3 {
		t.Fatalf("filtered has %d entries, want 3: %+v", len(filtered), filtered)
	}
	var sawOpencode bool
	for _, c := range filtered {
		if c.Name == "opencode" {
			sawOpencode = true
		}
	}
	if !sawOpencode {
		t.Errorf("opencode was filtered out, want it to survive: %+v", filtered)
	}
}

func TestFilterNonDiscoverablePluginsNoChangeWhenClean(t *testing.T) {
	cfgs := []PluginConfig{{Name: "claude"}, {Name: "sentry"}}

	filtered, dropped := FilterNonDiscoverablePlugins(cfgs)

	if len(dropped) != 0 {
		t.Errorf("dropped = %v, want none", dropped)
	}
	if len(filtered) != 2 {
		t.Errorf("filtered has %d entries, want 2", len(filtered))
	}
}

func TestDiscoverPluginsIgnoresNonExecutablePluginFiles(t *testing.T) {
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	pluginDir := filepath.Join(tmp, "libexec", "plugins")

	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}

	executablePath := filepath.Join(pluginDir, "bossd-plugin-alpha")
	if err := os.WriteFile(executablePath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "bossd-plugin-stale"), []byte("not runnable\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	plugins, _ := discoverPluginsFrom(binDir, activePolicy())
	if len(plugins) != 1 {
		t.Fatalf("got %d plugins, want 1: %+v", len(plugins), plugins)
	}
	if plugins[0].Name != "alpha" || plugins[0].Path != executablePath || !plugins[0].Enabled {
		t.Fatalf("plugin = %+v, want enabled alpha at %s", plugins[0], executablePath)
	}
}

func TestDiscoverPluginsIgnoresStubRunner(t *testing.T) {
	// The deterministic stub agent runner is test-only (NO_DISTRIBUTE) and is
	// loaded via explicit config by the E2E harness -- it must never be
	// auto-discovered into a real or dev daemon's agent picker.
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"bossd-plugin-claude", "bossd-plugin-stub-runner"} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	plugins, _ := discoverPluginsFrom(binDir, activePolicy())
	if len(plugins) != 1 {
		t.Fatalf("got %d plugins, want 1 (stub-runner excluded): %+v", len(plugins), plugins)
	}
	if plugins[0].Name != "claude" {
		t.Fatalf("plugins[0].Name = %q, want %q", plugins[0].Name, "claude")
	}
}

func TestDiscoverPluginsEmptyWhenNoPlugins(t *testing.T) {
	// An empty bin dir with no ../libexec/plugins/ should return nil.
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	plugins, _ := discoverPluginsFrom(binDir, activePolicy())
	if len(plugins) != 0 {
		t.Errorf("got %d plugins, want 0", len(plugins))
	}
}

func TestDiscoverPluginsEmptyWhenDirMissing(t *testing.T) {
	// When the libexec/plugins dir doesn't exist at all.
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "nonexistent", "bin")

	plugins, _ := discoverPluginsFrom(binDir, activePolicy())
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

	plugins, _ := discoverPluginsFrom(binDir, activePolicy())
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

func TestDiscoverPluginsFindsDevBinFromWorkingDirectory(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	pluginPath := filepath.Join(binDir, "bossd-plugin-codex")
	if err := os.WriteFile(pluginPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write plugin: %v", err)
	}
	serviceDir := filepath.Join(root, "services", "boss")
	if err := os.MkdirAll(serviceDir, 0o755); err != nil {
		t.Fatalf("mkdir service dir: %v", err)
	}
	t.Chdir(serviceDir)

	plugins, _ := discoverDevPluginsFromCWD(activePolicy())
	if len(plugins) != 1 {
		t.Fatalf("plugins = %v, want one plugin", plugins)
	}
	if plugins[0].Name != "codex" || plugins[0].Path != pluginPath || !plugins[0].Enabled {
		t.Fatalf("plugin = %+v, want enabled codex at %s", plugins[0], pluginPath)
	}
}

func TestDiscoverPluginsFallsBackToUserPluginDirWhenNoDevPluginsExist(t *testing.T) {
	root := t.TempDir()
	settingsPath := filepath.Join(root, "settings.json")
	appDataDir := filepath.Join(root, "appdata")
	pluginDir := filepath.Join(appDataDir, "plugins")
	pluginPath := filepath.Join(pluginDir, "bossd-plugin-codex")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatalf("mkdir plugin dir: %v", err)
	}
	if err := os.WriteFile(pluginPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write plugin: %v", err)
	}
	settings := DefaultSettings()
	settings.AppDataDir = appDataDir
	if err := SaveTo(settingsPath, settings); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	t.Setenv(settingsPathEnv, settingsPath)
	cwd := filepath.Join(root, "workspace", "services", "boss")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatalf("mkdir cwd: %v", err)
	}
	t.Chdir(cwd)

	plugins := DiscoverPlugins()
	if len(plugins) != 1 {
		t.Fatalf("plugins = %v, want one user plugin", plugins)
	}
	if plugins[0].Name != "codex" || plugins[0].Path != pluginPath || !plugins[0].Enabled {
		t.Fatalf("plugin = %+v, want enabled codex at %s", plugins[0], pluginPath)
	}
}

func TestRepairConfig_IdleRepairThreshold(t *testing.T) {
	t.Run("returns default when unset", func(t *testing.T) {
		c := RepairConfig{}
		got := c.IdleRepairThreshold()
		if got != 5*time.Minute {
			t.Errorf("IdleRepairThreshold() = %v, want %v", got, 5*time.Minute)
		}
	})
	t.Run("returns configured value when set", func(t *testing.T) {
		c := RepairConfig{IdleRepairThresholdMinutes: 12}
		got := c.IdleRepairThreshold()
		if got != 12*time.Minute {
			t.Errorf("IdleRepairThreshold() = %v, want %v", got, 12*time.Minute)
		}
	})
	t.Run("zero falls back to default", func(t *testing.T) {
		c := RepairConfig{IdleRepairThresholdMinutes: 0}
		got := c.IdleRepairThreshold()
		if got != 5*time.Minute {
			t.Errorf("IdleRepairThreshold() = %v, want %v", got, 5*time.Minute)
		}
	})
}

func TestRepairConfig_CooldownDuration(t *testing.T) {
	t.Run("returns default when unset", func(t *testing.T) {
		c := RepairConfig{}
		got := c.CooldownDuration()
		if got != 1*time.Minute {
			t.Errorf("CooldownDuration() = %v, want %v", got, 1*time.Minute)
		}
	})
	t.Run("returns configured value when set", func(t *testing.T) {
		c := RepairConfig{CooldownMinutes: 7}
		got := c.CooldownDuration()
		if got != 7*time.Minute {
			t.Errorf("CooldownDuration() = %v, want %v", got, 7*time.Minute)
		}
	})
	t.Run("zero falls back to default", func(t *testing.T) {
		c := RepairConfig{CooldownMinutes: 0}
		got := c.CooldownDuration()
		if got != 1*time.Minute {
			t.Errorf("CooldownDuration() = %v, want %v", got, 1*time.Minute)
		}
	})
}

func TestManagedAccountsConfig_DefaultCooldown(t *testing.T) {
	t.Run("returns default when unset", func(t *testing.T) {
		c := ManagedAccountsConfig{}
		got := c.DefaultCooldown()
		if got != 60*time.Minute {
			t.Errorf("DefaultCooldown() = %v, want %v", got, 60*time.Minute)
		}
	})
	t.Run("returns configured value when set", func(t *testing.T) {
		c := ManagedAccountsConfig{DefaultCooldownMinutes: 5}
		got := c.DefaultCooldown()
		if got != 5*time.Minute {
			t.Errorf("DefaultCooldown() = %v, want %v", got, 5*time.Minute)
		}
	})
	t.Run("zero falls back to default", func(t *testing.T) {
		c := ManagedAccountsConfig{DefaultCooldownMinutes: 0}
		got := c.DefaultCooldown()
		if got != 60*time.Minute {
			t.Errorf("DefaultCooldown() = %v, want %v", got, 60*time.Minute)
		}
	})
}

func TestManagedAccountsConfig_ManagedAccountsEnabled(t *testing.T) {
	t.Run("nil defaults to true", func(t *testing.T) {
		c := ManagedAccountsConfig{}
		if !c.ManagedAccountsEnabled() {
			t.Error("ManagedAccountsEnabled() = false, want true when Enabled is nil")
		}
	})
	t.Run("explicit false", func(t *testing.T) {
		f := false
		c := ManagedAccountsConfig{Enabled: &f}
		if c.ManagedAccountsEnabled() {
			t.Error("ManagedAccountsEnabled() = true, want false when Enabled = &false")
		}
	})
	t.Run("explicit true", func(t *testing.T) {
		tr := true
		c := ManagedAccountsConfig{Enabled: &tr}
		if !c.ManagedAccountsEnabled() {
			t.Error("ManagedAccountsEnabled() = false, want true when Enabled = &true")
		}
	})
}

// TestManagedAccountsConfig_KillSwitchRoundTrip pins the global kill-switch (BOS-176)
// through Save/Load: an absent `rotation.enabled` key reads as ON (default-ON per
// D4), while an explicit false persists and reloads as OFF; DefaultSettings is ON.
func TestManagedAccountsConfig_KillSwitchRoundTrip(t *testing.T) {
	if !DefaultSettings().ManagedAccounts.ManagedAccountsEnabled() {
		t.Error("DefaultSettings().ManagedAccounts.ManagedAccountsEnabled() = false, want true (default-ON)")
	}

	t.Run("explicit false persists as disabled", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "settings.json")
		s := DefaultSettings()
		f := false
		s.ManagedAccounts.Enabled = &f
		if err := SaveTo(path, s); err != nil {
			t.Fatalf("SaveTo: %v", err)
		}
		loaded, err := LoadFrom(path)
		if err != nil {
			t.Fatalf("LoadFrom: %v", err)
		}
		if loaded.ManagedAccounts.ManagedAccountsEnabled() {
			t.Error("reloaded ManagedAccountsEnabled() = true, want false after saving enabled=false")
		}
	})

	t.Run("absent key reloads as enabled", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "settings.json")
		if err := SaveTo(path, DefaultSettings()); err != nil {
			t.Fatalf("SaveTo: %v", err)
		}
		loaded, err := LoadFrom(path)
		if err != nil {
			t.Fatalf("LoadFrom: %v", err)
		}
		if !loaded.ManagedAccounts.ManagedAccountsEnabled() {
			t.Error("reloaded ManagedAccountsEnabled() = false, want true when key absent")
		}
	})
}

func TestManagedAccountsConfig_MaxRotations(t *testing.T) {
	t.Run("zero falls back to default", func(t *testing.T) {
		c := ManagedAccountsConfig{}
		if got := c.MaxRotations(); got != 3 {
			t.Errorf("MaxRotations() = %d, want 3", got)
		}
	})
	t.Run("returns configured value when set", func(t *testing.T) {
		c := ManagedAccountsConfig{MaxRotationsPerRun: 5}
		if got := c.MaxRotations(); got != 5 {
			t.Errorf("MaxRotations() = %d, want 5", got)
		}
	})
}

func TestManagedAccountsConfig_ParkSweepInterval(t *testing.T) {
	t.Run("zero falls back to default", func(t *testing.T) {
		c := ManagedAccountsConfig{}
		if got := c.ParkSweepInterval(); got != 60*time.Second {
			t.Errorf("ParkSweepInterval() = %v, want %v", got, 60*time.Second)
		}
	})
	t.Run("returns configured value when set", func(t *testing.T) {
		c := ManagedAccountsConfig{ParkSweepIntervalSeconds: 30}
		if got := c.ParkSweepInterval(); got != 30*time.Second {
			t.Errorf("ParkSweepInterval() = %v, want %v", got, 30*time.Second)
		}
	})
}

func TestManagedAccountsConfig_ProactiveDefaults(t *testing.T) {
	t.Run("zero value: disabled and 5m interval", func(t *testing.T) {
		c := ManagedAccountsConfig{}
		if c.ProactiveRotationEnabled() {
			t.Error("ProactiveRotationEnabled() = true, want false for zero value")
		}
		if got := c.ProactiveSweepInterval(); got != 5*time.Minute {
			t.Errorf("ProactiveSweepInterval() = %v, want %v", got, 5*time.Minute)
		}
	})
	t.Run("true pointer enables", func(t *testing.T) {
		enabled := true
		c := ManagedAccountsConfig{ProactiveRotation: &enabled}
		if !c.ProactiveRotationEnabled() {
			t.Error("ProactiveRotationEnabled() = false, want true when pointer is true")
		}
	})
	t.Run("false pointer disables", func(t *testing.T) {
		disabled := false
		c := ManagedAccountsConfig{ProactiveRotation: &disabled}
		if c.ProactiveRotationEnabled() {
			t.Error("ProactiveRotationEnabled() = true, want false when pointer is false")
		}
	})
	t.Run("configured interval overrides default", func(t *testing.T) {
		c := ManagedAccountsConfig{ProactiveSweepIntervalSeconds: 30}
		if got := c.ProactiveSweepInterval(); got != 30*time.Second {
			t.Errorf("ProactiveSweepInterval() = %v, want %v", got, 30*time.Second)
		}
	})
}

func TestManagedAccountsConfig_FailoverProxyEnabled(t *testing.T) {
	t.Run("zero value: enabled (default-ON)", func(t *testing.T) {
		c := ManagedAccountsConfig{}
		if !c.FailoverProxyEnabled() {
			t.Error("FailoverProxyEnabled() = false, want true for zero value")
		}
	})
	t.Run("true pointer enables", func(t *testing.T) {
		enabled := true
		c := ManagedAccountsConfig{FailoverProxy: &enabled}
		if !c.FailoverProxyEnabled() {
			t.Error("FailoverProxyEnabled() = false, want true when pointer is true")
		}
	})
	t.Run("false pointer disables", func(t *testing.T) {
		disabled := false
		c := ManagedAccountsConfig{FailoverProxy: &disabled}
		if c.FailoverProxyEnabled() {
			t.Error("FailoverProxyEnabled() = true, want false when pointer is false")
		}
	})
}

func TestManagedAccountsConfig_FailoverProxyPort(t *testing.T) {
	t.Run("zero value falls back to default 44127", func(t *testing.T) {
		c := ManagedAccountsConfig{}
		if got := c.FailoverProxyPort(); got != 44127 {
			t.Errorf("FailoverProxyPort() = %d, want 44127 for zero value", got)
		}
	})
	t.Run("negative falls back to default 44127", func(t *testing.T) {
		c := ManagedAccountsConfig{FailoverProxyPortSetting: -1}
		if got := c.FailoverProxyPort(); got != 44127 {
			t.Errorf("FailoverProxyPort() = %d, want 44127 for negative value", got)
		}
	})
	t.Run("returns configured value when set", func(t *testing.T) {
		c := ManagedAccountsConfig{FailoverProxyPortSetting: 51000}
		if got := c.FailoverProxyPort(); got != 51000 {
			t.Errorf("FailoverProxyPort() = %d, want 51000", got)
		}
	})
	t.Run("parses failover_proxy_port from JSON", func(t *testing.T) {
		var c ManagedAccountsConfig
		if err := json.Unmarshal([]byte(`{"failover_proxy_port":50505}`), &c); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if c.FailoverProxyPortSetting != 50505 {
			t.Errorf("FailoverProxyPortSetting = %d, want 50505", c.FailoverProxyPortSetting)
		}
		if got := c.FailoverProxyPort(); got != 50505 {
			t.Errorf("FailoverProxyPort() = %d, want 50505", got)
		}
	})
	t.Run("absent JSON key defaults to 44127", func(t *testing.T) {
		var c ManagedAccountsConfig
		if err := json.Unmarshal([]byte(`{}`), &c); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if got := c.FailoverProxyPort(); got != 44127 {
			t.Errorf("FailoverProxyPort() = %d, want 44127 when key absent", got)
		}
	})
}

func TestRepairConfig_PollInterval(t *testing.T) {
	t.Run("returns default when unset", func(t *testing.T) {
		c := RepairConfig{}
		got := c.PollInterval()
		if got != 5*time.Second {
			t.Errorf("PollInterval() = %v, want %v", got, 5*time.Second)
		}
	})
	t.Run("returns configured value when set", func(t *testing.T) {
		c := RepairConfig{PollIntervalSeconds: 30}
		got := c.PollInterval()
		if got != 30*time.Second {
			t.Errorf("PollInterval() = %v, want %v", got, 30*time.Second)
		}
	})
	t.Run("zero falls back to default", func(t *testing.T) {
		c := RepairConfig{PollIntervalSeconds: 0}
		got := c.PollInterval()
		if got != 5*time.Second {
			t.Errorf("PollInterval() = %v, want %v", got, 5*time.Second)
		}
	})
}

func TestRepairConfig_SkillName(t *testing.T) {
	t.Run("returns default when unset", func(t *testing.T) {
		c := RepairConfig{}
		got := c.SkillName()
		if got != "boss-repair" {
			t.Errorf("SkillName() = %q, want %q", got, "boss-repair")
		}
	})
	t.Run("returns configured value when set", func(t *testing.T) {
		c := RepairConfig{Skills: RepairSkills{Repair: "custom-repair"}}
		got := c.SkillName()
		if got != "custom-repair" {
			t.Errorf("SkillName() = %q, want %q", got, "custom-repair")
		}
	})
	t.Run("empty falls back to default", func(t *testing.T) {
		c := RepairConfig{Skills: RepairSkills{Repair: ""}}
		got := c.SkillName()
		if got != "boss-repair" {
			t.Errorf("SkillName() = %q, want %q", got, "boss-repair")
		}
	})
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

	plugins, _ := discoverPluginsFrom(binDir, activePolicy())
	if len(plugins) != 1 {
		t.Fatalf("got %d plugins, want 1", len(plugins))
	}
	if !strings.Contains(plugins[0].Path, "libexec") {
		t.Errorf("expected libexec path, got %q", plugins[0].Path)
	}
}

func TestPluginConfigBool(t *testing.T) {
	cases := []struct {
		name    string
		plugins []PluginConfig
		plugin  string
		key     string
		want    bool
	}{
		{
			name:    "missing plugin returns false",
			plugins: []PluginConfig{{Name: "linear"}},
			plugin:  "claude",
			key:     "dangerously_skip_permissions",
			want:    false,
		},
		{
			name:    "nil settings safe",
			plugins: nil,
			plugin:  "claude",
			key:     "dangerously_skip_permissions",
			want:    false,
		},
		{
			name: "missing key returns false",
			plugins: []PluginConfig{
				{Name: "claude", Config: map[string]string{"other": "true"}},
			},
			plugin: "claude",
			key:    "dangerously_skip_permissions",
			want:   false,
		},
		{
			name: "value true returns true",
			plugins: []PluginConfig{
				{Name: "claude", Config: map[string]string{"dangerously_skip_permissions": "true"}},
			},
			plugin: "claude",
			key:    "dangerously_skip_permissions",
			want:   true,
		},
		{
			name: "value false returns false",
			plugins: []PluginConfig{
				{Name: "claude", Config: map[string]string{"dangerously_skip_permissions": "false"}},
			},
			plugin: "claude",
			key:    "dangerously_skip_permissions",
			want:   false,
		},
		{
			name: "non-canonical value returns false",
			plugins: []PluginConfig{
				{Name: "claude", Config: map[string]string{"dangerously_skip_permissions": "yes"}},
			},
			plugin: "claude",
			key:    "dangerously_skip_permissions",
			want:   false,
		},
		{
			name: "nil Config map returns false",
			plugins: []PluginConfig{
				{Name: "claude"},
			},
			plugin: "claude",
			key:    "dangerously_skip_permissions",
			want:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := Settings{Plugins: tc.plugins}
			got := PluginConfigBool(&s, tc.plugin, tc.key)
			if got != tc.want {
				t.Errorf("PluginConfigBool(%q,%q) = %v, want %v", tc.plugin, tc.key, got, tc.want)
			}
		})
	}

	// Nil pointer must not panic.
	if got := PluginConfigBool(nil, "claude", "x"); got {
		t.Errorf("nil settings: got %v, want false", got)
	}
}

func TestSetPluginConfigBool(t *testing.T) {
	t.Run("true sets the key", func(t *testing.T) {
		s := Settings{Plugins: []PluginConfig{{Name: "claude"}}}
		SetPluginConfigBool(&s, "claude", "dangerously_skip_permissions", true)
		if s.Plugins[0].Config["dangerously_skip_permissions"] != "true" {
			t.Errorf("got %q, want %q", s.Plugins[0].Config["dangerously_skip_permissions"], "true")
		}
	})

	t.Run("false deletes the key", func(t *testing.T) {
		s := Settings{Plugins: []PluginConfig{{
			Name:   "claude",
			Config: map[string]string{"dangerously_skip_permissions": "true", "other": "v"},
		}}}
		SetPluginConfigBool(&s, "claude", "dangerously_skip_permissions", false)
		if _, ok := s.Plugins[0].Config["dangerously_skip_permissions"]; ok {
			t.Errorf("expected key deleted, got %v", s.Plugins[0].Config)
		}
		if s.Plugins[0].Config["other"] != "v" {
			t.Errorf("unrelated key clobbered: %v", s.Plugins[0].Config)
		}
	})

	t.Run("true on missing plugin appends entry", func(t *testing.T) {
		s := Settings{Plugins: []PluginConfig{{Name: "linear"}}}
		SetPluginConfigBool(&s, "claude", "dangerously_skip_permissions", true)
		if len(s.Plugins) != 2 {
			t.Fatalf("expected 2 plugins, got %d: %+v", len(s.Plugins), s.Plugins)
		}
		if s.Plugins[1].Name != "claude" {
			t.Errorf("appended plugin name = %q, want %q", s.Plugins[1].Name, "claude")
		}
		if s.Plugins[1].Config["dangerously_skip_permissions"] != "true" {
			t.Errorf("config not seeded: %+v", s.Plugins[1].Config)
		}
	})

	t.Run("false on missing plugin is no-op", func(t *testing.T) {
		s := Settings{Plugins: []PluginConfig{{Name: "linear"}}}
		SetPluginConfigBool(&s, "claude", "dangerously_skip_permissions", false)
		if len(s.Plugins) != 1 || s.Plugins[0].Name != "linear" {
			t.Errorf("plugins list mutated: %+v", s.Plugins)
		}
	})

	t.Run("nil settings safe", func(t *testing.T) {
		SetPluginConfigBool(nil, "claude", "dangerously_skip_permissions", true)
	})
}

func TestProvidersAcknowledgedRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	s := DefaultSettings()
	s.ProvidersAcknowledged = true
	if err := SaveTo(path, s); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.ProvidersAcknowledged {
		t.Errorf("ProvidersAcknowledged not persisted")
	}
}

func TestSetPluginConfigStringRoundTrip(t *testing.T) {
	s := DefaultSettings()
	SetPluginConfigString(&s, "codex", "model", "o1-pro")
	got := PluginConfigString(&s, "codex", "model")
	if got != "o1-pro" {
		t.Errorf("got %q want o1-pro", got)
	}
	SetPluginConfigString(&s, "codex", "model", "")
	got = PluginConfigString(&s, "codex", "model")
	if got != "" {
		t.Errorf("got %q want empty after delete", got)
	}
}

func TestSetPluginConfigEnumRejectsInvalid(t *testing.T) {
	s := DefaultSettings()
	allowed := []string{"workspace-write", "read-only", "danger-full-access"}

	if err := SetPluginConfigEnum(&s, "codex", "sandbox", "workspace-write", allowed); err != nil {
		t.Fatalf("valid value rejected: %v", err)
	}
	if err := SetPluginConfigEnum(&s, "codex", "sandbox", "nonsense", allowed); err == nil {
		t.Fatal("invalid value accepted")
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

// TestDedupPluginConfigs_SingleEntryReturnsInputUnchanged pins the len(cfgs)<=1
// boundary: a single-entry slice must take the early-return path and hand back
// the original backing array (not a freshly built copy). If the boundary is
// mutated to len(cfgs)<1, a single entry would fall through into the dedup loop
// where out is allocated with make, so the returned slice would no longer share
// storage with the input.
func TestDedupPluginConfigs_SingleEntryReturnsInputUnchanged(t *testing.T) {
	in := []PluginConfig{{Name: "repair", Path: "/a", Enabled: true}}
	got, dropped := DedupPluginConfigs(in)
	if dropped {
		t.Fatalf("dropped = true, want false for single entry")
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	if &got[0] != &in[0] {
		t.Fatalf("single entry must return the same backing slice (early return), got a copy")
	}
}

// TestSetPluginConfigString_ExistingPluginBothBranches covers both sides of the
// `value == ""` branch for a plugin that already exists in s.Plugins: a
// non-empty value sets the key, and an empty value deletes it. Negating the
// comparison would swap these behaviours.
func TestSetPluginConfigString_ExistingPluginBothBranches(t *testing.T) {
	s := Settings{Plugins: []PluginConfig{
		{Name: "codex", Config: map[string]string{"existing": "keep"}},
	}}

	// Non-empty value: should set the key without deleting others.
	SetPluginConfigString(&s, "codex", "model", "o1-pro")
	if got := PluginConfigString(&s, "codex", "model"); got != "o1-pro" {
		t.Fatalf("after set: model = %q, want o1-pro", got)
	}
	if got := PluginConfigString(&s, "codex", "existing"); got != "keep" {
		t.Fatalf("non-empty set must not delete other keys: existing = %q, want keep", got)
	}

	// Empty value: should delete the key, leaving others intact.
	SetPluginConfigString(&s, "codex", "model", "")
	if got := PluginConfigString(&s, "codex", "model"); got != "" {
		t.Fatalf("after delete: model = %q, want empty", got)
	}
	if _, ok := s.Plugins[0].Config["model"]; ok {
		t.Fatalf("empty value must delete the key from the config map")
	}
	if got := PluginConfigString(&s, "codex", "existing"); got != "keep" {
		t.Fatalf("delete must not remove other keys: existing = %q, want keep", got)
	}
}

func TestSettings_ErrorTrackingEnabled_Default(t *testing.T) {
	var s Settings
	if s.ErrorTrackingEnabled {
		t.Fatal("ErrorTrackingEnabled default = true, want false")
	}
}

func TestSettings_ErrorTrackingEnabled_RoundTrip(t *testing.T) {
	in := Settings{ErrorTrackingEnabled: true}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"error_tracking_enabled":true`) {
		t.Fatalf("marshal did not use error_tracking_enabled JSON key: %s", data)
	}
	var out Settings
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !out.ErrorTrackingEnabled {
		t.Fatal("ErrorTrackingEnabled did not round-trip")
	}
}

func TestSettings_ErrorTrackingEnabled_ForwardCompat(t *testing.T) {
	// Regression: a config.json written before this field existed
	// must still load cleanly and leave ErrorTrackingEnabled=false.
	legacy := []byte(`{"worktree_base_dir":"/x","event_tracing_enabled":true}`)
	var s Settings
	if err := json.Unmarshal(legacy, &s); err != nil {
		t.Fatalf("unmarshal legacy config: %v", err)
	}
	if s.ErrorTrackingEnabled {
		t.Error("legacy config produced ErrorTrackingEnabled=true")
	}
	if !s.EventTracingEnabled {
		t.Error("EventTracingEnabled from legacy config got lost")
	}
}

func TestSettings_ErrorTrackingEnabled_OmittedWhenFalse(t *testing.T) {
	s := Settings{}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "error_tracking_enabled") {
		t.Errorf("default-false field leaked into JSON: %s", data)
	}
}

func TestUserPluginDirUsesConfiguredAppDataDir(t *testing.T) {
	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	appDataDir := filepath.Join(t.TempDir(), "data")
	t.Setenv("BOSS_SETTINGS_PATH", settingsPath)

	if err := SaveTo(settingsPath, Settings{AppDataDir: appDataDir}); err != nil {
		t.Fatalf("SaveTo() returned error: %v", err)
	}

	got, err := UserPluginDir()
	if err != nil {
		t.Fatalf("UserPluginDir() returned error: %v", err)
	}
	want := filepath.Join(appDataDir, "plugins")
	if got != want {
		t.Fatalf("UserPluginDir() = %q, want %q", got, want)
	}
}

func TestUserPluginDirDoesNotCreateWorktreeBaseDir(t *testing.T) {
	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	worktreeBaseDir := filepath.Join(t.TempDir(), "worktrees")
	appDataDir := filepath.Join(t.TempDir(), "data")
	t.Setenv("BOSS_SETTINGS_PATH", settingsPath)

	if err := SaveTo(settingsPath, Settings{
		WorktreeBaseDir: worktreeBaseDir,
		AppDataDir:      appDataDir,
	}); err != nil {
		t.Fatalf("SaveTo() returned error: %v", err)
	}

	if _, err := UserPluginDir(); err != nil {
		t.Fatalf("UserPluginDir() returned error: %v", err)
	}
	if _, err := os.Stat(worktreeBaseDir); !os.IsNotExist(err) {
		t.Fatalf("UserPluginDir() created WorktreeBaseDir %q", worktreeBaseDir)
	}
}

func TestUserPluginDirFallsBackToUserConfigDir(t *testing.T) {
	settingsPath := filepath.Join(t.TempDir(), "missing-settings.json")
	homeDir := t.TempDir()
	xdgConfigHome := filepath.Join(t.TempDir(), "xdg-config")
	t.Setenv("BOSS_SETTINGS_PATH", settingsPath)
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_CONFIG_HOME", xdgConfigHome)

	got, err := UserPluginDir()
	if err != nil {
		t.Fatalf("UserPluginDir() returned error: %v", err)
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("UserConfigDir() returned error: %v", err)
	}
	want := filepath.Join(configDir, "bossanova", "plugins")
	if got != want {
		t.Fatalf("UserPluginDir() = %q, want %q", got, want)
	}
}

// TestResolveInitEnvSettingsPath exercises the env-gating logic captured at
// package init: an absolute BOSS_SETTINGS_PATH is cleaned and returned, while an
// unset or relative value yields "". The absolute case pins both conditions
// (non-empty AND absolute) so neither can be inverted without the test noticing.
func TestResolveInitEnvSettingsPath(t *testing.T) {
	t.Run("absolute is cleaned and returned", func(t *testing.T) {
		t.Setenv(settingsPathEnv, "/tmp/boss/../settings.json")
		if got, want := resolveInitEnvSettingsPath(), filepath.Clean("/tmp/boss/../settings.json"); got != want {
			t.Fatalf("resolveInitEnvSettingsPath() = %q, want %q", got, want)
		}
	})
	t.Run("relative is rejected", func(t *testing.T) {
		t.Setenv(settingsPathEnv, "relative/settings.json")
		if got := resolveInitEnvSettingsPath(); got != "" {
			t.Fatalf("resolveInitEnvSettingsPath() = %q, want empty for relative path", got)
		}
	})
	t.Run("empty is rejected", func(t *testing.T) {
		t.Setenv(settingsPathEnv, "")
		if got := resolveInitEnvSettingsPath(); got != "" {
			t.Fatalf("resolveInitEnvSettingsPath() = %q, want empty when unset", got)
		}
	})
}

// TestDiscoverPluginsFromUsesOSExecutable covers the binDir=="" fallback, which
// locates plugins relative to the running binary via os.Executable. The osExecutable
// seam points at a real file in a temp dir holding a plugin binary; discovery must
// resolve the executable, walk to its directory, and find the plugin. If either the
// os.Executable error guard or the EvalSymlinks error guard were inverted, the
// success path would return nil instead of the plugin.
func TestDiscoverPluginsFromUsesOSExecutable(t *testing.T) {
	tmp := t.TempDir()
	exe := filepath.Join(tmp, "boss")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "bossd-plugin-zeta"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	orig := osExecutable
	t.Cleanup(func() { osExecutable = orig })
	osExecutable = func() (string, error) { return exe, nil }

	plugins, _ := discoverPluginsFrom("", activePolicy())
	if len(plugins) != 1 {
		t.Fatalf("got %d plugins, want 1", len(plugins))
	}
	if plugins[0].Name != "zeta" {
		t.Errorf("plugins[0].Name = %q, want %q", plugins[0].Name, "zeta")
	}
}

// TestDiscoverPluginsFromOSExecutableError verifies the binDir=="" fallback bails out
// when os.Executable fails, rather than scanning an unrelated directory.
func TestDiscoverPluginsFromOSExecutableError(t *testing.T) {
	orig := osExecutable
	t.Cleanup(func() { osExecutable = orig })
	osExecutable = func() (string, error) { return "", os.ErrNotExist }

	plugins, _ := discoverPluginsFrom("", activePolicy())
	if plugins != nil {
		t.Fatalf("discoverPluginsFrom(\"\") = %v, want nil when os.Executable fails", plugins)
	}
}

func TestScanForPluginsRejectsUntrustedDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("perms hardening no-op on Windows")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	binPath := filepath.Join(dir, "bossd-plugin-x")
	if err := os.WriteFile(binPath, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, rej := scanForPlugins(dir, discoveryPolicy{requireSafePerms: true})
	if len(got) != 0 {
		t.Errorf("expected no accepted plugins from world-writable dir, got %d", len(got))
	}
	if len(rej) != 1 {
		t.Fatalf("expected 1 rejection, got %d", len(rej))
	}
}

func TestScanForPluginsChecksumEnforced(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("manifest dir trust check relies on Unix perms")
	}
	dir := t.TempDir()
	binPath, sum := writeBin(t, dir, "bossd-plugin-claude", []byte("real"))
	_ = binPath
	if err := os.WriteFile(filepath.Join(dir, pluginSumFile),
		[]byte(sum+"  bossd-plugin-claude\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, rej := scanForPlugins(dir, discoveryPolicy{requireSafePerms: true, verifyChecksums: true})
	if len(got) != 1 || len(rej) != 0 {
		t.Fatalf("matching binary should be accepted: got=%d rej=%d", len(got), len(rej))
	}
	if err := os.WriteFile(filepath.Join(dir, "bossd-plugin-claude"), []byte("evil"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, rej = scanForPlugins(dir, discoveryPolicy{requireSafePerms: true, verifyChecksums: true})
	if len(got) != 0 || len(rej) != 1 {
		t.Fatalf("tampered binary must be rejected: got=%d rej=%d", len(got), len(rej))
	}
}

func TestScanForPluginsMissingManifestFailsClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("perms hardening no-op on Windows")
	}
	dir := t.TempDir()
	binPath, _ := writeBin(t, dir, "bossd-plugin-claude", []byte("real"))
	_ = binPath
	got, rej := scanForPlugins(dir, discoveryPolicy{requireSafePerms: true, verifyChecksums: true})
	if len(got) != 0 {
		t.Errorf("release build with no manifest must load 0 plugins, got %d", len(got))
	}
	if len(rej) != 1 {
		t.Errorf("expected 1 rejection for missing manifest, got %d", len(rej))
	}
}

func TestVerifyConfiguredPluginsEnforcesChecksum(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("manifest dir trust check relies on Unix perms")
	}
	dir := t.TempDir()
	binPath, sum := writeBin(t, dir, "bossd-plugin-claude", []byte("real"))
	if err := os.WriteFile(filepath.Join(dir, pluginSumFile),
		[]byte(sum+"  bossd-plugin-claude\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgs := []PluginConfig{{Name: "claude", Path: binPath, Enabled: true}}
	policy := discoveryPolicy{requireSafePerms: true, verifyChecksums: true}

	got, rej := verifyConfiguredPlugins(cfgs, policy)
	if len(got) != 1 || len(rej) != 0 {
		t.Fatalf("matching configured binary should be accepted: got=%d rej=%d", len(got), len(rej))
	}

	// A binary swapped after `config init` persisted the path must be rejected.
	if err := os.WriteFile(binPath, []byte("evil"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, rej = verifyConfiguredPlugins(cfgs, policy)
	if len(got) != 0 || len(rej) != 1 {
		t.Fatalf("tampered configured binary must be rejected: got=%d rej=%d", len(got), len(rej))
	}
}

func TestVerifyConfiguredPluginsMissingManifestFailsClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("perms hardening no-op on Windows")
	}
	dir := t.TempDir()
	binPath, _ := writeBin(t, dir, "bossd-plugin-claude", []byte("real"))
	cfgs := []PluginConfig{{Name: "claude", Path: binPath, Enabled: true}}
	got, rej := verifyConfiguredPlugins(cfgs, discoveryPolicy{requireSafePerms: true, verifyChecksums: true})
	if len(got) != 0 || len(rej) != 1 {
		t.Fatalf("configured binary with no manifest must fail closed: got=%d rej=%d", len(got), len(rej))
	}
}

func TestVerifyConfiguredPluginsDevSkipsChecksumButEnforcesTrust(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("perms hardening no-op on Windows")
	}
	dir := t.TempDir()
	binPath, _ := writeBin(t, dir, "bossd-plugin-claude", []byte("real"))
	cfgs := []PluginConfig{{Name: "claude", Path: binPath, Enabled: true}}
	got, rej := verifyConfiguredPlugins(cfgs, discoveryPolicy{requireSafePerms: true, verifyChecksums: false})
	if len(got) != 1 || len(rej) != 0 {
		t.Fatalf("dev build must accept trusted configured plugin without manifest: got=%d rej=%d", len(got), len(rej))
	}

	if err := os.Chmod(binPath, 0o777); err != nil {
		t.Fatal(err)
	}
	got, rej = verifyConfiguredPlugins(cfgs, discoveryPolicy{requireSafePerms: true, verifyChecksums: false})
	if len(got) != 0 || len(rej) != 1 {
		t.Fatalf("dev build must reject untrusted configured plugin file: got=%d rej=%d", len(got), len(rej))
	}
	if !strings.Contains(rej[0].Reason, "group/world-writable") {
		t.Fatalf("rejection reason = %q, want group/world-writable", rej[0].Reason)
	}

	if err := os.Chmod(binPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	got, rej = verifyConfiguredPlugins(cfgs, discoveryPolicy{requireSafePerms: true, verifyChecksums: false})
	if len(got) != 0 || len(rej) != 1 {
		t.Fatalf("dev build must reject untrusted configured plugin directory: got=%d rej=%d", len(got), len(rej))
	}
	if !strings.Contains(rej[0].Reason, "untrusted plugin directory") {
		t.Fatalf("rejection reason = %q, want untrusted plugin directory", rej[0].Reason)
	}
}

func TestScanForPluginsDevSkipsChecksum(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("perms hardening no-op on Windows")
	}
	dir := t.TempDir()
	binPath, _ := writeBin(t, dir, "bossd-plugin-claude", []byte("real"))
	_ = binPath
	got, rej := scanForPlugins(dir, discoveryPolicy{requireSafePerms: true, verifyChecksums: false})
	if len(got) != 1 || len(rej) != 0 {
		t.Fatalf("dev build should accept without manifest: got=%d rej=%d", len(got), len(rej))
	}
}

func TestManagedAccountsConfig_AutoRotateChatsEnabled(t *testing.T) {
	boolPtr := func(b bool) *bool { return &b }
	cases := []struct {
		name   string
		cfg    ManagedAccountsConfig
		repoID string
		want   bool
	}{
		{"default is ON (D4)", ManagedAccountsConfig{}, "r1", true},
		{"global false disables", ManagedAccountsConfig{AutoRotateChats: boolPtr(false)}, "r1", false},
		{"global true enables", ManagedAccountsConfig{AutoRotateChats: boolPtr(true)}, "r1", true},
		{"per-repo false overrides global default", ManagedAccountsConfig{
			AutoRotateChatsPerRepo: map[string]bool{"r1": false},
		}, "r1", false},
		{"per-repo true overrides global false", ManagedAccountsConfig{
			AutoRotateChats:        boolPtr(false),
			AutoRotateChatsPerRepo: map[string]bool{"r1": true},
		}, "r1", true},
		{"other repo unaffected by per-repo entry", ManagedAccountsConfig{
			AutoRotateChatsPerRepo: map[string]bool{"r1": false},
		}, "r2", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.AutoRotateChatsEnabled(tc.repoID); got != tc.want {
				t.Fatalf("AutoRotateChatsEnabled(%q) = %v, want %v", tc.repoID, got, tc.want)
			}
		})
	}
}

func TestManagedAccountsConfig_ChatRotateMinInterval(t *testing.T) {
	if got := (ManagedAccountsConfig{}).ChatRotateMinInterval(); got != 10*time.Minute {
		t.Fatalf("default interval = %v, want 10m", got)
	}
	if got := (ManagedAccountsConfig{ChatRotateMinIntervalMinutes: 3}).ChatRotateMinInterval(); got != 3*time.Minute {
		t.Fatalf("configured interval = %v, want 3m", got)
	}
}

func TestManagedAccountsDefaults(t *testing.T) {
	var c ManagedAccountsConfig // zero value: both *bool nil
	if !c.ManagedAccountsEnabled() {
		t.Errorf("ManagedAccountsEnabled() = false, want true (nil ⇒ on)")
	}
	if !c.FailoverProxyEnabled() {
		t.Errorf("FailoverProxyEnabled() = false, want true (nil ⇒ on, flipped)")
	}
}

func TestManagedAccountsExplicitFalse(t *testing.T) {
	no := false
	c := ManagedAccountsConfig{Enabled: &no, FailoverProxy: &no}
	if c.ManagedAccountsEnabled() {
		t.Error("ManagedAccountsEnabled() = true, want false")
	}
	if c.FailoverProxyEnabled() {
		t.Error("FailoverProxyEnabled() = true, want false")
	}
}

func TestSettingsManagedAccountsKey(t *testing.T) {
	var s Settings
	if err := json.Unmarshal([]byte(`{"managed_accounts":{"enabled":false}}`), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if s.ManagedAccounts.ManagedAccountsEnabled() {
		t.Error("managed_accounts.enabled:false did not disable")
	}
}

func TestSettingsLegacyRotationKey(t *testing.T) {
	var s Settings
	if err := json.Unmarshal([]byte(`{"rotation":{"enabled":false}}`), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if s.ManagedAccounts.ManagedAccountsEnabled() {
		t.Error("legacy rotation.enabled:false was not honored via back-compat")
	}
}

func TestSettingsManagedAccountsWinsOverLegacy(t *testing.T) {
	var s Settings
	raw := `{"rotation":{"enabled":false},"managed_accounts":{"enabled":true}}`
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !s.ManagedAccounts.ManagedAccountsEnabled() {
		t.Error("managed_accounts should win over legacy rotation")
	}
}

func TestSettingsSerializesManagedAccountsKey(t *testing.T) {
	no := false
	s := Settings{ManagedAccounts: ManagedAccountsConfig{Enabled: &no}}
	out, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `"managed_accounts"`) {
		t.Errorf("serialized settings missing managed_accounts key: %s", out)
	}
	if strings.Contains(string(out), `"rotation"`) {
		t.Errorf("serialized settings should not emit legacy rotation key: %s", out)
	}
}
