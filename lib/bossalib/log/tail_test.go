package log

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTailMissingFile(t *testing.T) {
	got, err := Tail(filepath.Join(t.TempDir(), "does-not-exist.log"), 10)
	if err != nil {
		t.Fatalf("missing file should return nil error, got: %v", err)
	}
	if got != "" {
		t.Errorf("missing file should return empty string, got: %q", got)
	}
}

func TestTailEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.log")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Tail(path, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("empty file should return empty string, got: %q", got)
	}
}

func TestTailShortFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "short.log")
	content := "one\ntwo\nthree\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Tail(path, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != content {
		t.Errorf("short file should return all content; got %q want %q", got, content)
	}
}

func TestTailLongFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "long.log")
	var sb strings.Builder
	for i := 1; i <= 500; i++ {
		fmt.Fprintf(&sb, "line-%03d\n", i)
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Tail(path, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "line-496\nline-497\nline-498\nline-499\nline-500\n"
	if got != want {
		t.Errorf("long file tail mismatch; got %q want %q", got, want)
	}
}

func TestTailLargeFileChunked(t *testing.T) {
	// Exercises the chunk-from-end path (> 1 MB).
	path := filepath.Join(t.TempDir(), "large.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 50000; i++ {
		if _, err := fmt.Fprintf(f, "line-%05d with some padding to make the file large\n", i); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() <= 1<<20 {
		t.Fatalf("test setup: expected file > 1MB, got %d bytes", info.Size())
	}

	got, err := Tail(path, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d: %q", len(lines), got)
	}
	if !strings.HasPrefix(lines[2], "line-50000 ") {
		t.Errorf("last line should be line-50000, got: %q", lines[2])
	}
	if !strings.HasPrefix(lines[0], "line-49998 ") {
		t.Errorf("first tailed line should be line-49998, got: %q", lines[0])
	}
}

func TestTailZeroMaxLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "anything.log")
	if err := os.WriteFile(path, []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Tail(path, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("maxLines=0 should return empty string, got: %q", got)
	}
}
