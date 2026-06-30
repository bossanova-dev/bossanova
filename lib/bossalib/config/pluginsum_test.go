package config

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeBin(t *testing.T, dir, name string, content []byte) (path, sum string) {
	t.Helper()
	path = filepath.Join(dir, name)
	if err := os.WriteFile(path, content, 0o755); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(content)
	return path, hex.EncodeToString(h[:])
}

func TestLoadPluginSumsAndVerify(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("manifest dir trust check relies on Unix perms")
	}
	dir := t.TempDir()
	binPath, binSum := writeBin(t, dir, "bossd-plugin-claude", []byte("real binary"))

	manifest := "# bossd plugin checksums\n" + binSum + "  bossd-plugin-claude\n"
	if err := os.WriteFile(filepath.Join(dir, pluginSumFile), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	sums, err := loadPluginSums(dir)
	if err != nil {
		t.Fatalf("loadPluginSums: %v", err)
	}
	if ok, reason := verifyPluginChecksum(binPath, sums); !ok {
		t.Errorf("matching binary should verify, got %q", reason)
	}

	// Tampered binary -> mismatch.
	if err := os.WriteFile(binPath, []byte("evil binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if ok, _ := verifyPluginChecksum(binPath, sums); ok {
		t.Error("tampered binary must fail verification")
	}

	// Binary absent from manifest -> reject.
	other, _ := writeBin(t, dir, "bossd-plugin-rogue", []byte("rogue"))
	if ok, _ := verifyPluginChecksum(other, sums); ok {
		t.Error("binary missing from manifest must be rejected")
	}
}

func TestLoadPluginSumsErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("manifest dir trust check relies on Unix perms")
	}
	// Missing manifest.
	if _, err := loadPluginSums(t.TempDir()); err == nil {
		t.Error("missing manifest must error")
	}
	// Malformed line.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, pluginSumFile), []byte("not-a-valid-line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPluginSums(dir); err == nil {
		t.Error("malformed manifest must error")
	} else if !strings.Contains(err.Error(), "malformed manifest line 1") {
		t.Fatalf("malformed manifest error = %q, want line 1", err.Error())
	}
}
