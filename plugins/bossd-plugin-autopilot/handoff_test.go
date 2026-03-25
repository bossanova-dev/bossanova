package main

import (
	"os"
	"path/filepath"
	"strings"
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
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(dir) })
				return dir, time.Now().Add(-1 * time.Hour)
			},
			wantEmpty: true,
		},
		{
			name: "one new file returns its path",
			setup: func(t *testing.T) (string, time.Time) {
				dir := "testdata_handoff_one"
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(dir) })
				since := time.Now()
				f, err := os.Create(filepath.Join(dir, "handoff-001.md"))
				if err != nil {
					t.Fatal(err)
				}
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
				// Set mtime to future.
				futureTime := since.Add(1 * time.Hour)
				if err := os.Chtimes(f.Name(), futureTime, futureTime); err != nil {
					t.Fatal(err)
				}
				return dir, since
			},
			wantFile: true,
		},
		{
			name: "multiple files picks newest by mtime",
			setup: func(t *testing.T) (string, time.Time) {
				dir := "testdata_handoff_multi"
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(dir) })
				since := time.Now()

				// Older file.
				f1, err := os.Create(filepath.Join(dir, "handoff-old.md"))
				if err != nil {
					t.Fatal(err)
				}
				if err := f1.Close(); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(f1.Name(), since.Add(1*time.Hour), since.Add(1*time.Hour)); err != nil {
					t.Fatal(err)
				}

				// Newer file.
				f2, err := os.Create(filepath.Join(dir, "handoff-new.md"))
				if err != nil {
					t.Fatal(err)
				}
				if err := f2.Close(); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(f2.Name(), since.Add(2*time.Hour), since.Add(2*time.Hour)); err != nil {
					t.Fatal(err)
				}

				return dir, since
			},
			wantFile: true,
		},
		{
			name: "old files only returns empty string",
			setup: func(t *testing.T) (string, time.Time) {
				dir := "testdata_handoff_old"
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(dir) })

				f, err := os.Create(filepath.Join(dir, "handoff-ancient.md"))
				if err != nil {
					t.Fatal(err)
				}
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
				// Set mtime to the past.
				pastTime := time.Now().Add(-2 * time.Hour)
				if err := os.Chtimes(f.Name(), pastTime, pastTime); err != nil {
					t.Fatal(err)
				}

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
			name: "absolute directory path works",
			setup: func(t *testing.T) (string, time.Time) {
				dir := t.TempDir()
				since := time.Now()
				f, err := os.Create(filepath.Join(dir, "handoff-abs.md"))
				if err != nil {
					t.Fatal(err)
				}
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
				futureTime := since.Add(1 * time.Hour)
				if err := os.Chtimes(f.Name(), futureTime, futureTime); err != nil {
					t.Fatal(err)
				}
				return dir, since
			},
			wantFile: true,
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
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(dir) })

				since := time.Now()

				// Old file (before since).
				f1, err := os.Create(filepath.Join(dir, "handoff-before.md"))
				if err != nil {
					t.Fatal(err)
				}
				if err := f1.Close(); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(f1.Name(), since.Add(-1*time.Hour), since.Add(-1*time.Hour)); err != nil {
					t.Fatal(err)
				}

				// New file (after since).
				f2, err := os.Create(filepath.Join(dir, "handoff-after.md"))
				if err != nil {
					t.Fatal(err)
				}
				if err := f2.Close(); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(f2.Name(), since.Add(1*time.Hour), since.Add(1*time.Hour)); err != nil {
					t.Fatal(err)
				}

				return dir, since
			},
			wantFile: true,
		},
		{
			name: "subdirectories are skipped",
			setup: func(t *testing.T) (string, time.Time) {
				dir := "testdata_handoff_subdir"
				if err := os.MkdirAll(filepath.Join(dir, "subdir"), 0o755); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(dir) })
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

func TestSynthesizeHandoff(t *testing.T) {
	t.Run("creates file with expected content", func(t *testing.T) {
		dir := t.TempDir()
		path, err := synthesizeHandoff(dir, "docs/plans/test.md", 2)
		if err != nil {
			t.Fatal(err)
		}
		if path == "" {
			t.Fatal("expected non-empty path")
		}

		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("failed to read synthesized file: %v", err)
		}
		content := string(data)

		if !strings.Contains(content, "Synthesized: true") {
			t.Error("expected 'Synthesized: true' marker in content")
		}
		if !strings.Contains(content, "docs/plans/test.md") {
			t.Error("expected plan path in content")
		}
		if !strings.Contains(content, "Leg 2") {
			t.Error("expected leg number in content")
		}
	})

	t.Run("creates nested directories", func(t *testing.T) {
		base := t.TempDir()
		dir := filepath.Join(base, "deep", "nested", "handoffs")
		path, err := synthesizeHandoff(dir, "plan.md", 1)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("file does not exist at %q", path)
		}
	})

	t.Run("rejects paths with ..", func(t *testing.T) {
		_, err := synthesizeHandoff("docs/../../../etc", "plan.md", 1)
		if err == nil {
			t.Fatal("expected error for path with '..'")
		}
	})
}

func TestSynthesizeHandoffFilename(t *testing.T) {
	dir := t.TempDir()
	path, err := synthesizeHandoff(dir, "plan.md", 3)
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(path)
	if !strings.Contains(name, "synthesized-handoff-leg-3") {
		t.Errorf("filename %q does not contain expected pattern", name)
	}
	if !strings.HasSuffix(name, ".md") {
		t.Errorf("filename %q does not end with .md", name)
	}
}

func TestScanHandoffDirPicksNewest(t *testing.T) {
	dir := "testdata_handoff_newest"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

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
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		mtime := since.Add(f.offset)
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
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
