package factoryhost

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHostFileExists(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	file := filepath.Join(dir, "gate.mjs")
	if err := os.WriteFile(file, []byte("// gate\n"), 0o600); err != nil {
		t.Fatalf("write regular file: %v", err)
	}
	subDir := filepath.Join(dir, "subdir")
	if err := os.Mkdir(subDir, 0o755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}

	h := New()
	if !h.FileExists(file) {
		t.Errorf("FileExists(%q) = false, want true for regular file", file)
	}
	if h.FileExists(subDir) {
		t.Errorf("FileExists(%q) = true, want false for directory", subDir)
	}
	missing := filepath.Join(dir, "does-not-exist")
	if h.FileExists(missing) {
		t.Errorf("FileExists(%q) = true, want false for missing path", missing)
	}
}

func TestHostLookPath(t *testing.T) {
	binDir := t.TempDir()
	exePath := filepath.Join(binDir, "factoryhost-test-bin")
	if err := os.WriteFile(exePath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write executable: %v", err)
	}
	t.Setenv("PATH", binDir)

	h := New()
	got, ok := h.LookPath("factoryhost-test-bin")
	if !ok {
		t.Fatalf("LookPath(factoryhost-test-bin) ok = false, want true")
	}
	if got != exePath {
		t.Errorf("LookPath(factoryhost-test-bin) = %q, want %q", got, exePath)
	}
	if gotMissing, okMissing := h.LookPath("factoryhost-missing-bin"); okMissing || gotMissing != "" {
		t.Errorf("LookPath(factoryhost-missing-bin) = (%q, %v), want (\"\", false)", gotMissing, okMissing)
	}
}

func TestHostSkillRootAndHomeDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	h := New()
	if got := h.HomeDir(); got != home {
		t.Errorf("HomeDir() = %q, want %q", got, home)
	}
	root, err := h.SkillRoot("claude")
	if err != nil {
		t.Fatalf("SkillRoot(claude) error = %v", err)
	}
	if root == "" {
		t.Error("SkillRoot(claude) = empty, want non-empty path")
	}
	if _, err := h.SkillRoot("unknown-agent"); err == nil {
		t.Error("SkillRoot(unknown-agent) error = nil, want error")
	}
}
