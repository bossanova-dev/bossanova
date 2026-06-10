package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGuardRealDefaultWrite(t *testing.T) {
	realDefault := filepath.Join("/home", "dev", "Library", "Application Support", "bossanova", "settings.json")
	tests := []struct {
		name        string
		path        string
		inTest      bool
		realDefault string
		wantErr     bool
	}{
		{
			name:        "real default under test is refused",
			path:        realDefault,
			inTest:      true,
			realDefault: realDefault,
			wantErr:     true,
		},
		{
			name:        "real default not under test is allowed",
			path:        realDefault,
			inTest:      false,
			realDefault: realDefault,
		},
		{
			name:        "temp path under test is allowed",
			path:        "/tmp/some/settings.json",
			inTest:      true,
			realDefault: realDefault,
		},
		{
			name:        "uncleaned real default under test is refused",
			path:        filepath.Join("/home", "dev", "Library", "Application Support", "bossanova", "..", "bossanova", "settings.json"),
			inTest:      true,
			realDefault: realDefault,
			wantErr:     true,
		},
		{
			name:        "no real default known is allowed",
			path:        realDefault,
			inTest:      true,
			realDefault: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := guardRealDefaultWrite(tt.path, tt.inTest, tt.realDefault)
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
