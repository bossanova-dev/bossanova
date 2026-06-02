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

// TestTailExactlyAtThreshold ensures a file at exactly tailReadAllThreshold
// (1MB) is handled correctly. Both the read-all and chunked paths must
// produce identical output for the boundary size.
// Catches mutation: size <= threshold → size < threshold (boundary flips at 1MB).
func TestTailExactlyAtThreshold(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boundary.log")
	// Build a file whose size equals exactly 1MB. Each line is 16 bytes
	// ("lineNNNNNNNNNNN\n" = "line" (4) + 11 digits + "\n" (1) = 16). 1MB / 16 = 65536 lines.
	const lineLen = 16
	const totalLines = (1 << 20) / lineLen
	var sb strings.Builder
	sb.Grow(1 << 20)
	for i := range totalLines {
		fmt.Fprintf(&sb, "line%011d\n", i)
	}
	if sb.Len() != 1<<20 {
		t.Fatalf("test setup: built %d bytes, want %d", sb.Len(), 1<<20)
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Tail(path, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d: %q", len(lines), got)
	}
	wantLast := fmt.Sprintf("line%011d", totalLines-1)
	if lines[2] != wantLast {
		t.Errorf("last line = %q, want %q", lines[2], wantLast)
	}
	wantFirst := fmt.Sprintf("line%011d", totalLines-3)
	if lines[0] != wantFirst {
		t.Errorf("first tailed line = %q, want %q", lines[0], wantFirst)
	}
}

// TestTailExactlyMaxLines covers lastLines boundary: when the file has
// exactly maxLines lines, no truncation should occur.
// Catches mutation: len(lines) > n → len(lines) >= n (would slice unnecessarily,
// but result is the same so this is more of a regression guard).
func TestTailExactlyMaxLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exact.log")
	content := "a\nb\nc\nd\ne\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Tail(path, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != content {
		t.Errorf("got %q, want %q (all 5 lines preserved)", got, content)
	}
}

// TestTailMoreLinesThanRequested catches the lastLines slicing boundary:
// asking for n=5 from a file with 6 lines must drop exactly the first.
// Catches mutation: len(lines) > n → len(lines) < n (would not slice,
// returning all 6 lines instead of last 5).
func TestTailMoreLinesThanRequested(t *testing.T) {
	path := filepath.Join(t.TempDir(), "more.log")
	content := "1\n2\n3\n4\n5\n6\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Tail(path, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "2\n3\n4\n5\n6\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestTailChunkedLongLinesArithmetic forces the chunked read path (file > 1MB)
// with lines that are each LARGER than a single 64KB read chunk. Because every
// newline lands in its own chunk pass, the exact value of `needed` in
//
//	needed := maxLines + 1   (tail.go:55)
//
// becomes load-bearing: the +1 accounts for the trailing newline that yields an
// empty final segment. If the `+` mutates to `-`, `*` or `/` (ARITHMETIC_BASE),
// or the break test `count >= needed` (tail.go:64) flips to `count < needed`
// (CONDITIONALS_NEGATION), the loop stops one newline too early and the first
// returned "line" is a truncated tail of a long line ("yyy...") rather than the
// expected marker. We assert the exact first/last line prefixes so any such
// early-stop is observable.
func TestTailChunkedLongLinesArithmetic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "longlines.log")
	const lineFill = 80000 // > 64KB chunk, so each line spans multiple chunk reads
	const numLines = 16    // 16 * ~80KB = ~1.28MB, comfortably over the 1MB threshold
	var sb strings.Builder
	for i := range numLines {
		fmt.Fprintf(&sb, "MARK%02d:%s\n", i, strings.Repeat("y", lineFill))
	}
	if sb.Len() <= 1<<20 {
		t.Fatalf("test setup: file must exceed 1MB to hit chunked path, got %d", sb.Len())
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Tail(path, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected exactly 3 lines, got %d", len(lines))
	}
	// Last 3 markers of 16 lines (0..15) are MARK13, MARK14, MARK15.
	if !strings.HasPrefix(lines[0], "MARK13:") {
		t.Errorf("first tailed line = %.10q, want prefix %q", lines[0], "MARK13:")
	}
	if !strings.HasPrefix(lines[2], "MARK15:") {
		t.Errorf("last tailed line = %.10q, want prefix %q", lines[2], "MARK15:")
	}
}

// TestTailChunkedFewerLinesThanRequested forces the chunked read path with a
// file that contains FEWER lines than requested, so the break condition on
// tail.go:64 never fires and the loop must run all the way down to offset 0.
//
// This pins the loop-guard boundary `for offset > 0` (tail.go:56): the real
// code terminates when offset reaches 0, but the CONDITIONALS_BOUNDARY mutant
// `offset >= 0` would execute one more iteration with a zero-length read that
// never advances offset, spinning forever. Under that mutant this test hangs
// and is killed by the test timeout; under correct code it returns promptly
// with all lines intact.
func TestTailChunkedFewerLinesThanRequested(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fewlines.log")
	const lineFill = 150000 // big lines so few of them still exceed 1MB
	const numLines = 8
	var sb strings.Builder
	for i := range numLines {
		fmt.Fprintf(&sb, "ROW%02d:%s\n", i, strings.Repeat("z", lineFill))
	}
	if sb.Len() <= 1<<20 {
		t.Fatalf("test setup: file must exceed 1MB to hit chunked path, got %d", sb.Len())
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	// Ask for far more lines than exist so the newline-count break never trips
	// and the loop is forced to walk to offset 0.
	got, err := Tail(path, 50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != numLines {
		t.Fatalf("expected all %d lines, got %d", numLines, len(lines))
	}
	if !strings.HasPrefix(lines[0], "ROW00:") {
		t.Errorf("first line = %.10q, want prefix %q", lines[0], "ROW00:")
	}
	if !strings.HasPrefix(lines[numLines-1], "ROW07:") {
		t.Errorf("last line = %.10q, want prefix %q", lines[numLines-1], "ROW07:")
	}
}

// TestTailLargeFileExactRequestedLines covers the chunked read path's
// `needed = maxLines + 1` arithmetic — if mutated to `maxLines - 1` or
// `maxLines * 1`, the loop would terminate too early and return a
// partial first line as the leading "line".
func TestTailLargeFileExactRequestedLines(t *testing.T) {
	// File > 1MB to force chunked path. First line is huge (>64KB,
	// spans multiple chunk reads), then 5 short lines at the end.
	path := filepath.Join(t.TempDir(), "long-first-line.log")
	var sb strings.Builder
	// 1.5MB of "x" with no newlines.
	bigLine := strings.Repeat("x", 1500000)
	sb.WriteString(bigLine)
	sb.WriteString("\n")
	for i := 1; i <= 5; i++ {
		fmt.Fprintf(&sb, "tail-%d\n", i)
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Tail(path, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "tail-1\ntail-2\ntail-3\ntail-4\ntail-5\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
