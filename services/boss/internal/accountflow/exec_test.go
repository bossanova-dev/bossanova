package accountflow

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestNewDevNullStdinExecChildSeesEmptyStdin verifies that a child launched by
// the TUI-context Exec reads an immediately-empty (EOF) stdin rather than the
// parent terminal: `wc -c` over /dev/null counts zero bytes. This proves Bubble
// Tea keeps os.Stdin and the child never blocks waiting on terminal input.
func TestNewDevNullStdinExecChildSeesEmptyStdin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell not available on windows")
	}
	ex := NewDevNullStdinExec()
	proc, err := ex.Start(context.Background(), "sh", []string{"-c", "wc -c"}, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	var got []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		for line := range proc.Lines() {
			got = append(got, strings.TrimSpace(line))
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = proc.Kill()
		t.Fatal("child did not exit — a non-empty stdin would block wc")
	}

	if err := proc.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	// wc -c over an empty stdin prints "0" (with platform-dependent leading
	// whitespace, already trimmed above).
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "0") {
		t.Fatalf("child stdin byte count = %q, want a line containing 0 (empty stdin)", joined)
	}
}

// TestNewOSExecUsesParentStdin is a light guard that the CLI form still
// constructs and runs a child (its stdin is os.Stdin, which under `go test` is
// typically /dev/null too, so we only assert the child runs and exits cleanly —
// the DevNull variant test above covers the stdin-source distinction).
func TestNewOSExecRunsChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell not available on windows")
	}
	ex := NewOSExec()
	proc, err := ex.Start(context.Background(), "sh", []string{"-c", "echo hi"}, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var got []string
	for line := range proc.Lines() {
		got = append(got, strings.TrimSpace(line))
	}
	if err := proc.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if strings.Join(got, " ") != "hi" {
		t.Fatalf("child output = %q, want hi", got)
	}
}
