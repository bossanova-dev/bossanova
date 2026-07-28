package sessionports

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"
)

// TestRunCommandTimeoutSurfacesDeadline proves the derived-context deadline is
// reported as context.DeadlineExceeded (not an opaque "signal: killed"), which
// is what lets the darwin socket source distinguish an untrustworthy timeout
// from a benign non-zero lsof exit.
func TestRunCommandTimeoutSurfacesDeadline(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not available")
	}
	_, err := runCommand(context.Background(), time.Millisecond, "sleep", "10")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout should surface context.DeadlineExceeded, got %v", err)
	}
}

// TestRunCommandParentCancelSurfacesCancel proves parent-context cancellation
// propagates through the derived context as context.Canceled.
func TestRunCommandParentCancelSurfacesCancel(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not available")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := runCommand(ctx, 5*time.Second, "sleep", "10")
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled parent should surface a context error, got %v", err)
	}
}

// TestRunCommandNonZeroExitKeepsOutput proves a clean non-zero exit (the shape
// lsof produces when some PIDs have no listeners) returns its stdout plus an
// *exec.ExitError, never a context error — so the caller treats it as
// authoritative evidence rather than a timeout.
func TestRunCommandNonZeroExitKeepsOutput(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	out, err := runCommand(context.Background(), 5*time.Second, "sh", "-c", "printf hello; exit 1")
	if string(out) != "hello" {
		t.Fatalf("stdout should be preserved on non-zero exit, got %q", out)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("non-zero exit should be an *exec.ExitError, got %v", err)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Fatalf("clean non-zero exit must not look like a context error, got %v", err)
	}
}
