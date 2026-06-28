package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestIsTrustedPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission hardening is a no-op on Windows")
	}
	dir := t.TempDir()

	safe := filepath.Join(dir, "safe")
	if err := os.WriteFile(safe, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if ok, reason := isTrustedPath(safe); !ok {
		t.Errorf("0755 file should be trusted, got reason %q", reason)
	}

	groupWritable := filepath.Join(dir, "gw")
	if err := os.WriteFile(groupWritable, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(groupWritable, 0o775); err != nil {
		t.Fatal(err)
	}
	if ok, _ := isTrustedPath(groupWritable); ok {
		t.Error("group-writable file (0775) must be untrusted")
	}

	worldWritable := filepath.Join(dir, "ww")
	if err := os.WriteFile(worldWritable, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(worldWritable, 0o757); err != nil {
		t.Fatal(err)
	}
	if ok, _ := isTrustedPath(worldWritable); ok {
		t.Error("world-writable file (0757) must be untrusted")
	}

	if ok, _ := isTrustedPath(filepath.Join(dir, "missing")); ok {
		t.Error("nonexistent path must be untrusted")
	}
}
