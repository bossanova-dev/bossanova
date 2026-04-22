package log

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	zlog "github.com/rs/zerolog/log"
)

func TestSetup(t *testing.T) {
	// Point XDG_STATE_HOME at a temp dir so we don't touch the developer's real state dir.
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	closer := Setup("test-service")
	t.Cleanup(func() { _ = closer.Close() })

	// Logger should be usable and should also write to the file.
	zlog.Info().Msg("test log after Setup")

	path := LogPath("test-service")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected log file at %s, got error: %v", path, err)
	}
	if !strings.Contains(string(data), "test log after Setup") {
		t.Errorf("log file %s did not contain expected entry; got: %s", path, data)
	}
}

func TestSetupFileOnly(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	closer := SetupFileOnly("test-service-fileonly")
	t.Cleanup(func() { _ = closer.Close() })

	zlog.Info().Msg("file-only log entry")

	path := LogPath("test-service-fileonly")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected log file at %s, got error: %v", path, err)
	}
	if !strings.Contains(string(data), "file-only log entry") {
		t.Errorf("log file %s did not contain expected entry; got: %s", path, data)
	}
}

func TestLogPathXDG(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/tmp/xdg")
	got := LogPath("bossd")
	want := filepath.Join("/tmp/xdg", "bossanova", "logs", "bossd.log")
	if got != want {
		t.Errorf("LogPath: got %q, want %q", got, want)
	}
}

func TestLogPathFallback(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	got := LogPath("boss")
	want := filepath.Join(home, ".local", "state", "bossanova", "logs", "boss.log")
	if got != want {
		t.Errorf("LogPath fallback: got %q, want %q", got, want)
	}
}
