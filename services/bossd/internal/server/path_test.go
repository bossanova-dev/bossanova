package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/recurser/bossalib/config"
)

func TestDefaultSocketPathForSettingsUsesSocketPath(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "custom.sock")

	got, err := DefaultSocketPathForSettings(config.Settings{SocketPath: socketPath})
	if err != nil {
		t.Fatalf("DefaultSocketPathForSettings() returned error: %v", err)
	}
	if got != socketPath {
		t.Fatalf("DefaultSocketPathForSettings() = %q, want %q", got, socketPath)
	}
}

func TestDefaultSocketPathForSettingsUsesAppDataDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")

	got, err := DefaultSocketPathForSettings(config.Settings{AppDataDir: dir})
	if err != nil {
		t.Fatalf("DefaultSocketPathForSettings() returned error: %v", err)
	}
	want := filepath.Join(dir, "bossd.sock")
	if got != want {
		t.Fatalf("DefaultSocketPathForSettings() = %q, want %q", got, want)
	}
}

func TestDefaultSocketPathForSettingsRejectsRelativeSocketPath(t *testing.T) {
	_, err := DefaultSocketPathForSettings(config.Settings{SocketPath: "relative/bossd.sock"})
	if err == nil {
		t.Fatal("DefaultSocketPathForSettings() error = nil, want relative path error")
	}
}

func TestDefaultSocketPathSitsUnderConfigAppDataDir(t *testing.T) {
	got, err := DefaultSocketPath()
	if err != nil {
		t.Fatalf("DefaultSocketPath() returned error: %v", err)
	}

	base, err := config.DefaultAppDataDir()
	if err != nil {
		t.Fatalf("config.DefaultAppDataDir() returned error: %v", err)
	}
	want := filepath.Join(base, "bossd.sock")
	if got != want {
		t.Fatalf("DefaultSocketPath() = %q, want %q", got, want)
	}
}

func TestDefaultSocketPathForSettingsFallsBackToDefault(t *testing.T) {
	isolateHomeEnv(t)

	got, err := DefaultSocketPathForSettings(config.Settings{})
	if err != nil {
		t.Fatalf("DefaultSocketPathForSettings() returned error: %v", err)
	}

	want, err := DefaultSocketPath()
	if err != nil {
		t.Fatalf("DefaultSocketPath() returned error: %v", err)
	}
	if got != want {
		t.Fatalf("DefaultSocketPathForSettings() = %q, want fallback %q", got, want)
	}
}

func TestListenRejectsRegularFileSocketPathWithoutRemovingIt(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "bossd.sock")
	contents := []byte("not a socket")
	if err := os.WriteFile(socketPath, contents, 0o600); err != nil {
		t.Fatalf("write regular file: %v", err)
	}

	s := New(Config{})
	err := s.Listen(socketPath)
	if err == nil {
		if s.listener != nil {
			_ = s.listener.Close()
		}
		t.Fatal("Listen() error = nil, want regular file rejection")
	}

	got, readErr := os.ReadFile(socketPath)
	if readErr != nil {
		t.Fatalf("regular file was not preserved: %v", readErr)
	}
	if string(got) != string(contents) {
		t.Fatalf("regular file contents = %q, want %q", got, contents)
	}
}

func isolateHomeEnv(t *testing.T) {
	t.Helper()

	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("USERPROFILE", filepath.Join(root, "userprofile"))
	t.Setenv("APPDATA", filepath.Join(root, "appdata"))
	t.Setenv("LOCALAPPDATA", filepath.Join(root, "localappdata"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg-config"))
}
