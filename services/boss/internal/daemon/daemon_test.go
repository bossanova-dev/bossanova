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
