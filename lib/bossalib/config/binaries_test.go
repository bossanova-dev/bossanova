package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveTrustedExecutableRejectsWorldWritable(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "boss")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o777); err != nil {
		t.Fatal(err)
	}
	// 0o777 is group/world-writable, so isTrustedPath must reject it even
	// though the file exists and is named correctly.
	if ok, _ := isTrustedPath(bin); ok {
		t.Skip("platform does not enforce perms (e.g. Windows); resolver trust is a no-op here")
	}
	t.Setenv("PATH", dir)
	if got := ResolveTrustedExecutable("boss"); got != "" {
		t.Fatalf("ResolveTrustedExecutable returned untrusted path %q, want \"\"", got)
	}
}

func TestResolveTrustedExecutableFindsTrustedOnPath(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "boss")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	got := ResolveTrustedExecutable("boss")
	if got == "" {
		t.Fatal("ResolveTrustedExecutable returned \"\" for a trusted PATH binary")
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("ResolveTrustedExecutable returned non-absolute path %q", got)
	}
}

func TestResolveTrustedExecutableMissingIsEmpty(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if got := ResolveTrustedExecutable("definitely-not-a-real-binary-xyz"); got != "" {
		t.Fatalf("missing binary returned %q, want \"\"", got)
	}
}

func TestResolveMcpBinaryPrefersBossMcp(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"boss-mcp", "mcp"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	got := ResolveMcpBinary()
	if filepath.Base(got) != "boss-mcp" {
		t.Fatalf("ResolveMcpBinary() = %q, want path with base \"boss-mcp\"", got)
	}
}

func TestResolveMcpBinaryFallsBackToMcp(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mcp"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	got := ResolveMcpBinary()
	if got == "" {
		t.Fatal("ResolveMcpBinary() = \"\", want non-empty path to mcp")
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("ResolveMcpBinary() = %q, want absolute path", got)
	}
}

func TestResolveMcpBinaryMissingIsEmpty(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if got := ResolveMcpBinary(); got != "" {
		t.Fatalf("ResolveMcpBinary() = %q, want \"\" when neither binary found", got)
	}
}
