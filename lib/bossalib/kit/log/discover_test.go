package log

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRotatedPathsOrdersBackupThenCurrent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	logs := filepath.Join(dir, "bossanova", "logs")
	if err := os.MkdirAll(logs, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"bossd-2026-08-04T20-44-46.080.log", "bossd.log", "bosso.log"} {
		if err := os.WriteFile(filepath.Join(logs, name), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := RotatedPaths("bossd")
	if err != nil {
		t.Fatalf("RotatedPaths: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 paths, got %d: %v", len(got), got)
	}
	if !strings.HasSuffix(got[0], "bossd-2026-08-04T20-44-46.080.log") {
		t.Errorf("backup must sort first, got %q", got[0])
	}
	if !strings.HasSuffix(got[1], "bossd.log") {
		t.Errorf("current must sort last, got %q", got[1])
	}
}

func TestRotatedPathsMissingDirIsNotAnError(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "nope"))
	got, err := RotatedPaths("bossd")
	if err != nil {
		t.Fatalf("missing dir must not error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want no paths, got %v", got)
	}
}

func TestTailRotatingSpillsIntoBackup(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	logs := filepath.Join(dir, "bossanova", "logs")
	if err := os.MkdirAll(logs, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logs, "bossd-2026-08-04T20-44-46.080.log"), []byte("old1\nold2\nold3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logs, "bossd.log"), []byte("new1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := TailRotating("bossd", 3)
	if err != nil {
		t.Fatalf("TailRotating: %v", err)
	}
	if got != "old2\nold3\nnew1" {
		t.Errorf("want backup spill, got %q", got)
	}
}

func TestTailRotatingAtStopsAtCurrentLogSnapshot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	logs := filepath.Join(dir, "bossanova", "logs")
	if err := os.MkdirAll(logs, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(logs, "bossd.log")
	if err := os.WriteFile(path, []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("before\nafter\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := TailRotatingAt("bossd", 10, info.Size(), info)
	if err != nil {
		t.Fatal(err)
	}
	if got != "before" {
		t.Errorf("want snapshot tail, got %q", got)
	}
}

func TestTailRotatingSnapshotExcludesPostSnapshotBackups(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	logs := filepath.Join(dir, "bossanova", "logs")
	if err := os.MkdirAll(logs, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logs, "bossd-2026-08-04T20-44-46.080.log"), []byte("older\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(logs, "bossd.log")
	if err := os.WriteFile(path, []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	backupInfos, err := BackupInfos("bossd")
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("after snapshot\n"); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, filepath.Join(logs, "bossd-2026-08-04T20-44-47.080.log")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("first replacement\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, filepath.Join(logs, "bossd-2026-08-04T20-44-48.080.log")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("second replacement\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := TailRotatingSnapshot("bossd", 10, info.Size(), info, backupInfos)
	if err != nil {
		t.Fatalf("TailRotatingSnapshot: %v", err)
	}
	if got != "older\nbefore" {
		t.Errorf("want only snapshot backlog, got %q", got)
	}
}

func TestTailRotatingHandoffPreservesRotatedReplacements(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	logs := filepath.Join(dir, "bossanova", "logs")
	if err := os.MkdirAll(logs, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(logs, "bossd.log")
	if err := os.WriteFile(path, []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("before\nafter snapshot\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, filepath.Join(logs, "bossd-2026-08-04T20-44-47.080.log")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("first replacement\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, filepath.Join(logs, "bossd-2026-08-04T20-44-48.080.log")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("second replacement\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := TailRotatingHandoff("bossd", 10, info.Size(), info, false)
	if err != nil {
		t.Fatalf("TailRotatingHandoff: %v", err)
	}
	if got != "before\nfirst replacement" {
		t.Errorf("want every rotated handoff record, got %q", got)
	}
}

func TestTailRotatingHandoffKeepsRequestedBacklogWhenSnapshotEndsPartial(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	logs := filepath.Join(dir, "bossanova", "logs")
	if err := os.MkdirAll(logs, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(logs, "bossd.log")
	if err := os.WriteFile(path, []byte("first\nsecond\nthird\npartial"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	got, err := TailRotatingHandoff("bossd", 3, info.Size(), info, true)
	if err != nil {
		t.Fatalf("TailRotatingHandoff: %v", err)
	}
	if got != "first\nsecond\nthird" {
		t.Errorf("want three complete snapshot records, got %q", got)
	}
}

func TestRotatingFileRetention(t *testing.T) {
	sink := newRotatingFile(filepath.Join(t.TempDir(), "bossd.log"))
	t.Cleanup(func() { _ = sink.Close() })
	if sink.MaxBackups != 20 {
		t.Errorf("MaxBackups = %d, want 20", sink.MaxBackups)
	}
	if sink.MaxSize != 5 {
		t.Errorf("MaxSize = %d, want 5 MB", sink.MaxSize)
	}
	if sink.Compress {
		t.Error("compressed backups cannot be discovered and tailed")
	}
}

func TestRotatingFileBackupsStayVisibleToTail(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	sink := newRotatingFile(Path("bossd"))
	t.Cleanup(func() { _ = sink.Close() })
	for _, marker := range []string{"first backup", "second backup"} {
		if _, err := sink.Write([]byte(marker + "\n")); err != nil {
			t.Fatal(err)
		}
		if err := sink.Rotate(); err != nil {
			t.Fatal(err)
		}
		// Backup names have millisecond precision. Cross the next boundary
		// after Rotate returns so the next rotation cannot overwrite this one.
		time.Sleep(time.Until(time.Now().Truncate(time.Millisecond).Add(time.Millisecond)))
	}
	if _, err := sink.Write([]byte("live file\n")); err != nil {
		t.Fatal(err)
	}
	paths, err := RotatedPaths("bossd")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 3 {
		t.Fatalf("want two backups and the live file, got %v", paths)
	}
	for _, path := range paths[:2] {
		if !strings.HasSuffix(path, ".log") || path == Path("bossd") {
			t.Errorf("want uncompressed backup, got %q", path)
		}
	}
	if paths[2] != Path("bossd") {
		t.Errorf("want live file last, got %v", paths)
	}
	got, err := TailRotating("bossd", 3)
	if err != nil {
		t.Fatal(err)
	}
	if want := "first backup\nsecond backup\nlive file"; got != want {
		t.Errorf("TailRotating = %q, want %q", got, want)
	}
}
