package config

import (
	"os"
	"path/filepath"
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
			name:     "zero returns default 30s",
			seconds:  0,
			expected: 30 * time.Second,
		},
		{
			name:     "negative returns default 30s",
			seconds:  -5,
			expected: 30 * time.Second,
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
