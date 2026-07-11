package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGuardRealDefaultWrite(t *testing.T) {
	realDefault := filepath.Join("/home", "dev", "Library", "Application Support", "bossanova", "settings.json")
	envPath := filepath.Join("/home", "dev", "code", "bossanova", ".config", "settings.json")
	tests := []struct {
		name          string
		path          string
		inProcessTest bool
		sentinelSet   bool
		realDefault   string
		envPath       string
		wantErr       bool
	}{
		{
			name:          "real default under in-process test is refused",
			path:          realDefault,
			inProcessTest: true,
			realDefault:   realDefault,
			envPath:       envPath,
			wantErr:       true,
		},
		{
			name:        "real default under subprocess sentinel is refused",
			path:        realDefault,
			sentinelSet: true,
			realDefault: realDefault,
			envPath:     envPath,
			wantErr:     true,
		},
		{
			name:        "real default with no test markers is allowed",
			path:        realDefault,
			realDefault: realDefault,
			envPath:     envPath,
		},
		{
			name:          "env path under in-process test is refused",
			path:          envPath,
			inProcessTest: true,
			realDefault:   realDefault,
			envPath:       envPath,
			wantErr:       true,
		},
		{
			name:        "env path under subprocess sentinel is allowed (its own temp target)",
			path:        envPath,
			sentinelSet: true,
			realDefault: realDefault,
			envPath:     envPath,
		},
		{
			name:          "temp path under in-process test is allowed",
			path:          "/tmp/some/settings.json",
			inProcessTest: true,
			realDefault:   realDefault,
			envPath:       envPath,
		},
		{
			name:          "uncleaned real default under in-process test is refused",
			path:          filepath.Join("/home", "dev", "Library", "Application Support", "bossanova", "..", "bossanova", "settings.json"),
			inProcessTest: true,
			realDefault:   realDefault,
			envPath:       envPath,
			wantErr:       true,
		},
		{
			name:          "uncleaned env path under in-process test is refused",
			path:          filepath.Join("/home", "dev", "code", "bossanova", ".config", "..", ".config", "settings.json"),
			inProcessTest: true,
			realDefault:   realDefault,
			envPath:       envPath,
			wantErr:       true,
		},
		{
			name:          "no real default and no env path is allowed",
			path:          realDefault,
			inProcessTest: true,
			realDefault:   "",
			envPath:       "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := guardRealDefaultWrite(tt.path, tt.inProcessTest, tt.sentinelSet, tt.realDefault, tt.envPath)
			if tt.wantErr != (err != nil) {
				t.Fatalf("guardRealDefaultWrite() err = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

// TestSaveRefusesRealDefaultUnderTest proves the end-to-end guard: with no
// BOSS_SETTINGS_PATH and no HOME redirection, a Save under `go test` errors
// instead of overwriting the developer's real settings.json.
func TestSaveRefusesRealDefaultUnderTest(t *testing.T) {
	t.Setenv(settingsPathEnv, "") // ensure unset within this test
	if realDefaultPath == "" {
		t.Skip("no real default path on this platform")
	}

	before, beforeErr := os.Stat(realDefaultPath)

	err := Save(DefaultSettings())
	if err == nil {
		t.Fatal("Save() = nil, want guard error refusing the real default path")
	}
	if !strings.Contains(err.Error(), settingsPathEnv) {
		t.Fatalf("Save() error = %q, want mention of %s", err, settingsPathEnv)
	}

	// The real file must be untouched: either still absent, or unchanged size/mtime.
	after, afterErr := os.Stat(realDefaultPath)
	if (beforeErr == nil) != (afterErr == nil) {
		t.Fatalf("real settings.json existence changed: before=%v after=%v", beforeErr, afterErr)
	}
	if beforeErr == nil && afterErr == nil {
		if !before.ModTime().Equal(after.ModTime()) || before.Size() != after.Size() {
			t.Fatal("real settings.json was modified by Save() under test")
		}
	}
}

func TestRefusesDefaultSettingsWrite(t *testing.T) {
	for _, tt := range []struct {
		name  string
		value string
		want  bool
	}{
		{name: "unset", want: false},
		{name: "other value", value: "true", want: false},
		{name: "sentinel", value: "1", want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(refuseDefaultEnv, tt.value)
			if got := refusesDefaultSettingsWrite(); got != tt.want {
				t.Fatalf("refusesDefaultSettingsWrite() = %t, want %t", got, tt.want)
			}
		})
	}
}

// TestExplicitSettingsPathRoundTrips confirms the normal isolated test pattern
// (BOSS_SETTINGS_PATH pointed at a temp file) still reads and writes correctly,
// and that reads (LoadFrom) are never blocked by the write guard.
func TestExplicitSettingsPathRoundTrips(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	t.Setenv(settingsPathEnv, p)

	want := DefaultSettings()
	want.PollIntervalSeconds = 99
	if err := Save(want); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	got, err := LoadFrom(p)
	if err != nil {
		t.Fatalf("LoadFrom() error: %v", err)
	}
	if got.PollIntervalSeconds != 99 {
		t.Fatalf("PollIntervalSeconds = %d, want 99", got.PollIntervalSeconds)
	}
}
