package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResolveBossdPath(t *testing.T) {
	// Create a temp directory with a bossd binary.
	tmpDir := t.TempDir()
	bossdPath := filepath.Join(tmpDir, "bossd")
	if err := os.WriteFile(bossdPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("create temp bossd: %v", err)
	}

	// Temporarily add tmpDir to PATH.
	origPath := os.Getenv("PATH")
	defer func() { _ = os.Setenv("PATH", origPath) }()
	if err := os.Setenv("PATH", tmpDir+":"+origPath); err != nil {
		t.Fatalf("set PATH: %v", err)
	}

	// ResolveBossdPath should find it in PATH.
	found, err := ResolveBossdPath()
	if err != nil {
		t.Fatalf("ResolveBossdPath: %v", err)
	}

	// The path should be absolute and point to our temp bossd.
	if !filepath.IsAbs(found) {
		t.Errorf("expected absolute path, got %s", found)
	}
	if filepath.Base(found) != "bossd" {
		t.Errorf("expected bossd, got %s", filepath.Base(found))
	}
}

func TestIsSocketReachable(t *testing.T) {
	// Test with a non-existent socket.
	if isSocketReachable("/tmp/nonexistent-socket-12345.sock") {
		t.Error("expected socket to be unreachable")
	}
}

func TestWaitForSocket(t *testing.T) {
	// Test timeout with non-existent socket.
	start := time.Now()
	if waitForSocket("/tmp/nonexistent-socket-12345.sock", 100*time.Millisecond) {
		t.Error("expected waitForSocket to timeout")
	}
	elapsed := time.Since(start)
	if elapsed < 100*time.Millisecond {
		t.Errorf("expected timeout >= 100ms, got %v", elapsed)
	}
}

// TestResolveBossdPath_PrefersExecutableDir verifies that ResolveBossdPath
// returns the bossd binary that lives next to the running executable,
// even when a different bossd is also on PATH.
//
// Catches:
//   - daemon.go:79 CONDITIONALS_NEGATION (`err == nil` → `err != nil`):
//     mutated code would skip the executable-dir branch entirely.
//   - daemon.go:82 CONDITIONALS_NEGATION (`err == nil` → `err != nil`):
//     mutated code would not return the candidate even when it exists.
func TestResolveBossdPath_PrefersExecutableDir(t *testing.T) {
	// Determine the directory where the test binary lives.
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable not available: %v", err)
	}
	exeDir := filepath.Dir(exe)
	candidate := filepath.Join(exeDir, "bossd")

	// Skip if a real bossd already lives next to the test binary — we don't
	// want to clobber it. Otherwise create a fake one and clean up after.
	if _, err := os.Stat(candidate); err == nil {
		t.Skipf("bossd already exists at %s; skipping to avoid clobber", candidate)
	}
	if err := os.WriteFile(candidate, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("create executable-dir bossd: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(candidate) })

	// Put a *different* bossd on PATH at a separate location to ensure the
	// function chooses the executable-dir one.
	pathDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(pathDir, "bossd"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("create PATH bossd: %v", err)
	}
	origPath := os.Getenv("PATH")
	t.Cleanup(func() { _ = os.Setenv("PATH", origPath) })
	if err := os.Setenv("PATH", pathDir+":"+origPath); err != nil {
		t.Fatalf("set PATH: %v", err)
	}

	got, err := ResolveBossdPath()
	if err != nil {
		t.Fatalf("ResolveBossdPath: %v", err)
	}
	if got != candidate {
		t.Errorf("ResolveBossdPath = %q, want %q (executable-dir bossd should win over PATH)", got, candidate)
	}
}
