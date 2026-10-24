package upstream

import (
	"os"
	"path/filepath"
	"testing"
)

func envFunc(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestResolveDaemonID_EnvOverrideWins(t *testing.T) {
	dir := t.TempDir()
	id, err := ResolveDaemonID(envFunc(map[string]string{"BOSSD_DAEMON_ID": "explicit-id"}), dir, "host.local")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "explicit-id" {
		t.Fatalf("id = %q, want explicit-id", id)
	}
	// An explicit override must not persist a file.
	if _, statErr := os.Stat(filepath.Join(dir, daemonIDFileName)); !os.IsNotExist(statErr) {
		t.Fatalf("env override must not persist a daemon-id file")
	}
}

func TestResolveDaemonID_GeneratesAndIsStable(t *testing.T) {
	dir := t.TempDir()
	id1, err := ResolveDaemonID(envFunc(nil), dir, "host.local")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id1 == "" || id1 == "host.local" {
		t.Fatalf("expected a generated id distinct from hostname, got %q", id1)
	}
	// Stable across calls even when the hostname changes (the whole point).
	id2, err := ResolveDaemonID(envFunc(nil), dir, "DIFFERENT-host")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id2 != id1 {
		t.Fatalf("daemon id not stable across hostname change: %q != %q", id2, id1)
	}
}

func TestResolveDaemonID_ReadsExistingFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, daemonIDFileName), []byte("  preset-id\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := ResolveDaemonID(envFunc(nil), dir, "host.local")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "preset-id" {
		t.Fatalf("id = %q, want preset-id", id)
	}
}

func TestResolveDaemonID_FallsBackToHostnameOnError(t *testing.T) {
	// An unwritable data dir cannot persist the id → hostname fallback + error.
	id, err := ResolveDaemonID(envFunc(nil), "/nonexistent/path/that/cannot/be/written", "host.local")
	if err == nil {
		t.Fatal("expected an error when the data dir is unwritable")
	}
	if id != "host.local" {
		t.Fatalf("id = %q, want hostname fallback", id)
	}
}
