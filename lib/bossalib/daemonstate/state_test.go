package daemonstate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteReadAndRemoveMetadata(t *testing.T) {
	dir := t.TempDir()
	startedAt := time.Date(2026, 6, 6, 12, 30, 0, 0, time.UTC)
	want := Metadata{
		PID:            12345,
		ExecutablePath: "/opt/homebrew/bin/bossd",
		SettingsPath:   "/tmp/profile/settings.json",
		SocketPath:     "/tmp/profile/bossd.sock",
		StartedAt:      startedAt,
		FileLimitSoft:  4096,
	}

	if err := Write(dir, want); err != nil {
		t.Fatalf("Write() returned error: %v", err)
	}

	got, err := Read(dir)
	if err != nil {
		t.Fatalf("Read() returned error: %v", err)
	}
	if got != want {
		t.Fatalf("Read() = %#v, want %#v", got, want)
	}
	// The achieved FD soft limit must survive the round-trip so a low cap is
	// visible in daemon state without grepping logs (BOS-465).
	if got.FileLimitSoft != 4096 {
		t.Fatalf("Read().FileLimitSoft = %d, want 4096", got.FileLimitSoft)
	}

	raw, err := os.ReadFile(filepath.Join(dir, MetadataFileName))
	if err != nil {
		t.Fatalf("read metadata file: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("metadata is not JSON: %v", err)
	}
	if decoded["pid"] == nil || decoded["executable_path"] == nil || decoded["settings_path"] == nil || decoded["socket_path"] == nil {
		t.Fatalf("metadata JSON missing expected fields: %s", string(raw))
	}
	if decoded["file_limit_soft"] == nil {
		t.Fatalf("metadata JSON missing file_limit_soft: %s", string(raw))
	}

	if err := Remove(dir); err != nil {
		t.Fatalf("Remove() returned error: %v", err)
	}
	if _, err := Read(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Read() after Remove() error = %v, want os.ErrNotExist", err)
	}
}

func TestWriteUsesOwnerOnlyPermissionsAndLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, Metadata{PID: 1}); err != nil {
		t.Fatalf("Write() returned error: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, MetadataFileName))
	if err != nil {
		t.Fatalf("stat metadata file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("metadata file perm = %o, want 600", perm)
	}

	// The atomic temp file must be renamed into place, not left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != MetadataFileName {
			t.Fatalf("unexpected leftover file %q in state dir", e.Name())
		}
	}
}

func TestReadRejectsCorruptMetadata(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, MetadataFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write corrupt metadata: %v", err)
	}

	_, err := Read(dir)
	if err == nil {
		t.Fatal("Read() of corrupt metadata returned nil error")
	}
	// A decode failure must be distinguishable from a missing file so callers
	// don't treat corruption as "no daemon running".
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Read() of corrupt metadata = %v, want a non-ErrNotExist error", err)
	}
}

func TestPathCleansAppDataDir(t *testing.T) {
	got := Path(filepath.Join(string(filepath.Separator), "tmp", "boss", "..", "boss"))
	want := filepath.Join(string(filepath.Separator), "tmp", "boss", MetadataFileName)
	if got != want {
		t.Fatalf("Path() = %q, want %q", got, want)
	}
}
