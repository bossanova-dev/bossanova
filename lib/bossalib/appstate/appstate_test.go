package appstate

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDirHandlesBaseDirError(t *testing.T) {
	tests := []struct {
		name      string
		homeDir   func() (string, error)
		wantError bool
	}{
		{
			name: "base dir error propagates",
			// On linux with no XDG_STATE_HOME, baseDir falls back to the home
			// dir; a failing home lookup makes baseDir return an error, so Dir
			// must also return an error (line 23 err != nil branch).
			homeDir:   func() (string, error) { return "", os.ErrPermission },
			wantError: true,
		},
		{
			name: "base dir success creates directory",
			// A valid home dir means baseDir succeeds, so Dir must proceed past
			// line 23 and return a directory with no error.
			homeDir:   nil, // set below to a temp dir per-run
			wantError: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := tc.homeDir
			if home == nil {
				root := t.TempDir()
				home = func() (string, error) { return root, nil }
			}

			currentGOOS = "linux"
			getenv = func(string) string { return "" }
			userHomeDir = home
			userConfigDir = func() (string, error) { return "/unused", nil }
			mkdirAll = os.MkdirAll
			t.Cleanup(func() {
				currentGOOS = runtime.GOOS
				getenv = os.Getenv
				userHomeDir = os.UserHomeDir
				userConfigDir = os.UserConfigDir
				mkdirAll = os.MkdirAll
			})

			dir, err := Dir()
			if tc.wantError {
				if err == nil {
					t.Fatalf("Dir() = %q, want error", dir)
				}
				if dir != "" {
					t.Fatalf("Dir() returned %q on error, want empty string", dir)
				}
				return
			}
			if err != nil {
				t.Fatalf("Dir() returned error: %v", err)
			}
			if dir == "" {
				t.Fatal("Dir() returned empty path without error")
			}
			if info, statErr := os.Stat(dir); statErr != nil {
				t.Fatalf("state dir missing: %v", statErr)
			} else if !info.IsDir() {
				t.Fatalf("state path is not a directory: %s", dir)
			}
		})
	}
}

func TestBaseDirLinuxUsesXDGStateHome(t *testing.T) {
	got, err := baseDir("linux", func(key string) string {
		if key == "XDG_STATE_HOME" {
			return "/tmp/state"
		}
		return ""
	}, func() (string, error) {
		t.Fatal("home should not be used")
		return "", nil
	}, nil)
	if err != nil {
		t.Fatalf("baseDir returned error: %v", err)
	}
	if got != "/tmp/state" {
		t.Fatalf("baseDir = %q, want %q", got, "/tmp/state")
	}
}

func TestBaseDirLinuxFallsBackToHome(t *testing.T) {
	got, err := baseDir("linux", func(string) string { return "" }, func() (string, error) {
		return "/home/alice", nil
	}, nil)
	if err != nil {
		t.Fatalf("baseDir returned error: %v", err)
	}
	want := filepath.Join("/home/alice", ".local", "state")
	if got != want {
		t.Fatalf("baseDir = %q, want %q", got, want)
	}
}

func TestBaseDirRejectsRelativeBase(t *testing.T) {
	_, err := baseDir("linux", func(key string) string {
		if key == "XDG_STATE_HOME" {
			return "relative"
		}
		return ""
	}, nil, nil)
	if err == nil {
		t.Fatal("expected relative base path error")
	}
}

func TestPathCreatesDirectoryAndRejectsTraversal(t *testing.T) {
	stateRoot := t.TempDir()
	currentGOOS = "linux"
	getenv = func(key string) string {
		if key == "XDG_STATE_HOME" {
			return stateRoot
		}
		return ""
	}
	userHomeDir = func() (string, error) { return "/unused", nil }
	userConfigDir = func() (string, error) { return "/unused", nil }
	mkdirAll = os.MkdirAll
	t.Cleanup(func() {
		currentGOOS = runtime.GOOS
		getenv = os.Getenv
		userHomeDir = os.UserHomeDir
		userConfigDir = os.UserConfigDir
		mkdirAll = os.MkdirAll
	})

	got, err := Path("workos-refresh.lock")
	if err != nil {
		t.Fatalf("Path returned error: %v", err)
	}
	if filepath.Base(got) != "workos-refresh.lock" {
		t.Fatalf("Path base = %q", filepath.Base(got))
	}
	if info, err := os.Stat(filepath.Dir(got)); err != nil {
		t.Fatalf("state dir missing: %v", err)
	} else if !info.IsDir() {
		t.Fatalf("state path is not a directory: %s", filepath.Dir(got))
	}

	if _, err := Path("../bad"); err == nil {
		t.Fatal("expected traversal to be rejected")
	}
}
