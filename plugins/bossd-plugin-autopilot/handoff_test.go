package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestScanHandoffDir(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(t *testing.T) (dir string, since time.Time)
		wantFile  bool
		wantEmpty bool
		wantErr   bool
	}{
		{
			name: "no files returns empty string",
			setup: func(t *testing.T) (string, time.Time) {
				dir := "testdata_handoff_empty"
				os.MkdirAll(dir, 0o755)
				t.Cleanup(func() { os.RemoveAll(dir) })
				return dir, time.Now().Add(-1 * time.Hour)
			},
			wantEmpty: true,
		},
		{
			name: "one new file returns its path",
			setup: func(t *testing.T) (string, time.Time) {
				dir := "testdata_handoff_one"
				os.MkdirAll(dir, 0o755)
				t.Cleanup(func() { os.RemoveAll(dir) })
				since := time.Now()
				f, err := os.Create(filepath.Join(dir, "handoff-001.md"))
				if err != nil {
					t.Fatal(err)
				}
				f.Close()
				// Set mtime to future.
				futureTime := since.Add(1 * time.Hour)
				os.Chtimes(f.Name(), futureTime, futureTime)
				return dir, since
			},
			wantFile: true,
		},
		{
			name: "multiple files picks newest by mtime",
			setup: func(t *testing.T) (string, time.Time) {
				dir := "testdata_handoff_multi"
				os.MkdirAll(dir, 0o755)
				t.Cleanup(func() { os.RemoveAll(dir) })
				since := time.Now()

				// Older file.
				f1, err := os.Create(filepath.Join(dir, "handoff-old.md"))
				if err != nil {
					t.Fatal(err)
				}
				f1.Close()
				os.Chtimes(f1.Name(), since.Add(1*time.Hour), since.Add(1*time.Hour))

				// Newer file.
				f2, err := os.Create(filepath.Join(dir, "handoff-new.md"))
				if err != nil {
					t.Fatal(err)
				}
				f2.Close()
				os.Chtimes(f2.Name(), since.Add(2*time.Hour), since.Add(2*time.Hour))

				return dir, since
			},
			wantFile: true,
		},
		{
			name: "old files only returns empty string",
			setup: func(t *testing.T) (string, time.Time) {
				dir := "testdata_handoff_old"
				os.MkdirAll(dir, 0o755)
				t.Cleanup(func() { os.RemoveAll(dir) })

				f, err := os.Create(filepath.Join(dir, "handoff-ancient.md"))
				if err != nil {
					t.Fatal(err)
				}
				f.Close()
				// Set mtime to the past.
				pastTime := time.Now().Add(-2 * time.Hour)
				os.Chtimes(f.Name(), pastTime, pastTime)

				// Since is after the file's mtime.
				return dir, time.Now().Add(-1 * time.Hour)
			},
			wantEmpty: true,
		},
		{
			name: "directory does not exist returns error",
			setup: func(t *testing.T) (string, time.Time) {
				return "testdata_handoff_nonexistent", time.Now()
			},
			wantErr: true,
		},
		{
			name: "absolute directory path returns error",
			setup: func(t *testing.T) (string, time.Time) {
				return "/tmp/absolute-handoff-dir", time.Now()
			},
			wantErr: true,
		},
		{
			name: "directory with .. returns error",
			setup: func(t *testing.T) (string, time.Time) {
				return "docs/../../../etc", time.Now()
			},
			wantErr: true,
		},
		{
			name: "mixed old and new files returns only new",
			setup: func(t *testing.T) (string, time.Time) {
				dir := "testdata_handoff_mixed"
				os.MkdirAll(dir, 0o755)
				t.Cleanup(func() { os.RemoveAll(dir) })

				since := time.Now()

				// Old file (before since).
				f1, err := os.Create(filepath.Join(dir, "handoff-before.md"))
				if err != nil {
					t.Fatal(err)
				}
				f1.Close()
				os.Chtimes(f1.Name(), since.Add(-1*time.Hour), since.Add(-1*time.Hour))

				// New file (after since).
				f2, err := os.Create(filepath.Join(dir, "handoff-after.md"))
				if err != nil {
					t.Fatal(err)
				}
				f2.Close()
				os.Chtimes(f2.Name(), since.Add(1*time.Hour), since.Add(1*time.Hour))

				return dir, since
			},
			wantFile: true,
		},
		{
			name: "subdirectories are skipped",
			setup: func(t *testing.T) (string, time.Time) {
				dir := "testdata_handoff_subdir"
				os.MkdirAll(filepath.Join(dir, "subdir"), 0o755)
				t.Cleanup(func() { os.RemoveAll(dir) })
				return dir, time.Now().Add(-1 * time.Hour)
			},
			wantEmpty: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, since := tt.setup(t)

			result, err := scanHandoffDir(dir, since)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tt.wantEmpty {
				if result != "" {
					t.Errorf("expected empty string, got %q", result)
				}
				return
			}

			if tt.wantFile {
				if result == "" {
					t.Fatal("expected non-empty path, got empty string")
				}
				// Verify the file exists.
				if _, err := os.Stat(result); os.IsNotExist(err) {
					t.Errorf("returned path %q does not exist", result)
				}
			}
		})
	}
}

func TestScanHandoffDirPicksNewest(t *testing.T) {
	dir := "testdata_handoff_newest"
	os.MkdirAll(dir, 0o755)
	t.Cleanup(func() { os.RemoveAll(dir) })

	since := time.Now()

	// Create three files with ascending mtimes.
	files := []struct {
		name   string
		offset time.Duration
	}{
		{"first.md", 1 * time.Hour},
		{"second.md", 2 * time.Hour},
		{"third.md", 3 * time.Hour},
	}

	for _, f := range files {
		path := filepath.Join(dir, f.name)
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		file.Close()
		mtime := since.Add(f.offset)
		os.Chtimes(path, mtime, mtime)
	}

	result, err := scanHandoffDir(dir, since)
	if err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(dir, "third.md")
	if result != want {
		t.Errorf("got %q, want %q (newest file)", result, want)
	}
}
