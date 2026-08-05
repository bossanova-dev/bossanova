package tuidriver_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/recurser/boss/internal/tuidriver"
)

// TestDriver_Dimensions verifies New applies the documented width/height
// defaults only when the option is zero, and otherwise honors the caller's
// explicit value.
func TestDriver_Dimensions(t *testing.T) {
	tests := []struct {
		name                  string
		width, height         int
		wantWidth, wantHeight int
	}{
		{"both zero use defaults", 0, 0, 120, 30},
		{"both explicit are kept", 50, 40, 50, 40},
		{"zero width defaults, height kept", 0, 24, 120, 24},
		{"width kept, zero height defaults", 80, 0, 80, 30},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := tuidriver.New(tuidriver.Options{
				Command: "cat",
				Width:   tt.width,
				Height:  tt.height,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			defer func() { _ = d.Close() }()

			if got := d.Width(); got != tt.wantWidth {
				t.Errorf("Width() = %d, want %d", got, tt.wantWidth)
			}
			if got := d.Height(); got != tt.wantHeight {
				t.Errorf("Height() = %d, want %d", got, tt.wantHeight)
			}
		})
	}
}

// TestDriver_EnvApplied verifies that a non-nil Options.Env is passed through
// to the child process. The child prints an env var that exists only in the
// supplied Env; if New failed to apply it the marker would be absent.
func TestDriver_EnvApplied(t *testing.T) {
	d, err := tuidriver.New(tuidriver.Options{
		Command: "sh",
		Args:    []string{"-c", "echo env-is:$TUIDRIVER_MARKER"},
		Env:     []string{"TUIDRIVER_MARKER=present-42"},
		Width:   80,
		Height:  24,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = d.Close() }()

	if err := d.WaitForText(5*time.Second, "env-is:present-42"); err != nil {
		t.Fatalf("expected supplied Env to reach the child: %v", err)
	}
}

// TestDriver_DirApplied verifies that a non-empty Options.Dir sets the child's
// working directory. The child cats a file by its relative name, which only
// resolves if it ran inside Dir.
func TestDriver_DirApplied(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "dir-marker.txt"), []byte("dir-content-99"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	d, err := tuidriver.New(tuidriver.Options{
		Command: "cat",
		Args:    []string{"dir-marker.txt"},
		Dir:     dir,
		Width:   80,
		Height:  24,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = d.Close() }()

	if err := d.WaitForText(5*time.Second, "dir-content-99"); err != nil {
		t.Fatalf("expected child to run in Options.Dir: %v", err)
	}
}

func TestDriver_SimpleCommand(t *testing.T) {
	// Spawn "echo hello" and verify the output appears on screen.
	d, err := tuidriver.New(tuidriver.Options{
		Command: "echo",
		Args:    []string{"hello-from-tuidriver"},
		Width:   80,
		Height:  24,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = d.Close() }()

	err = d.WaitForText(5*time.Second, "hello-from-tuidriver")
	if err != nil {
		t.Fatal(err)
	}
}

func TestDriver_InteractiveCommand(t *testing.T) {
	// Spawn "cat" (interactive), write to it, and verify echo.
	d, err := tuidriver.New(tuidriver.Options{
		Command: "cat",
		Width:   80,
		Height:  24,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = d.Close() }()

	if err := d.SendString("test-input\n"); err != nil {
		t.Fatalf("SendString: %v", err)
	}

	err = d.WaitForText(5*time.Second, "test-input")
	if err != nil {
		t.Fatal(err)
	}
}

// TestDriver_ReadLoopStaysAliveAfterRead guards readLoop's error check
// (driver.go:118 `if err != nil { return }`). "cat" keeps its stdin open,
// so after echoing input the process is still running and readLoop must keep
// looping with Done() open. A mutant that returns on a *successful* read
// (err == nil) exits readLoop after the first echo and closes d.done early,
// which this test catches.
func TestDriver_ReadLoopStaysAliveAfterRead(t *testing.T) {
	d, err := tuidriver.New(tuidriver.Options{
		Command: "cat",
		Width:   80,
		Height:  24,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = d.Close() }()

	if err := d.SendString("keepalive-marker\n"); err != nil {
		t.Fatalf("SendString: %v", err)
	}
	// Reaching the text proves readLoop performed a successful read (n>0,
	// err==nil) and wrote it to the emulator.
	if err := d.WaitForText(5*time.Second, "keepalive-marker"); err != nil {
		t.Fatal(err)
	}
	// cat is still running (stdin open), so readLoop must not have returned;
	// d.done must remain open. A premature return closes it.
	select {
	case <-d.Done():
		t.Fatal("readLoop exited after a successful read; cat process should still be running")
	case <-time.After(250 * time.Millisecond):
	}
}

// TestDriver_CloseAllowsGracefulInterruptCleanup verifies Close gives a
// Ctrl-C-aware process time to run its shutdown handler before force-killing it.
func TestDriver_CloseAllowsGracefulInterruptCleanup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "graceful-shutdown")
	d, err := tuidriver.New(tuidriver.Options{
		Command: "sh",
		Args: []string{
			"-c",
			`printf ready; trap 'sleep 0.2; printf graceful > "$1"; exit' INT; while :; do sleep 1; done`,
			"sh",
			marker,
		},
		Width:  80,
		Height: 24,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := d.WaitForText(5*time.Second, "ready"); err != nil {
		_ = d.Close()
		t.Fatalf("wait for child readiness: %v", err)
	}

	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read graceful-shutdown marker: %v", err)
	}
	if string(got) != "graceful" {
		t.Errorf("graceful-shutdown marker = %q, want %q", got, "graceful")
	}
}

// TestDriver_CloseReportsPTYCloseError verifies that Close preserves a PTY
// close failure. Calling Close again makes the already-closed PTY return that
// failure; callers need it to learn teardown was not completely clean.
func TestDriver_CloseReportsPTYCloseError(t *testing.T) {
	d, err := tuidriver.New(tuidriver.Options{
		Command: "true",
		Width:   80,
		Height:  24,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := d.Close(); err == nil {
		t.Fatal("second Close returned nil despite the already-closed PTY")
	}
}

func TestDriver_ScreenContains(t *testing.T) {
	d, err := tuidriver.New(tuidriver.Options{
		Command: "echo",
		Args:    []string{"unique-marker-xyz"},
		Width:   80,
		Height:  24,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = d.Close() }()

	if err := d.WaitForText(5*time.Second, "unique-marker-xyz"); err != nil {
		t.Fatal(err)
	}

	if !d.ScreenContains("unique-marker-xyz") {
		t.Fatalf("ScreenContains returned false; screen:\n%s", d.Screen())
	}
	if d.ScreenContains("nonexistent-text") {
		t.Fatal("ScreenContains returned true for absent text")
	}
}

// TestDriver_CloseRaceRegression exercises the vt.Close vs responseLoop
// race by creating and closing drivers back-to-back. Pre-fix this tripped
// -race on the emulator's `closed` bool; post-fix the pipe writer is
// closed directly and -race stays quiet.
func TestDriver_CloseRaceRegression(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping race-stress regression in -short mode")
	}
	for i := range 30 {
		d, err := tuidriver.New(tuidriver.Options{
			Command: "true",
			Width:   80,
			Height:  24,
		})
		if err != nil {
			t.Fatalf("iter %d: New: %v", i, err)
		}
		if err := d.Close(); err != nil {
			t.Fatalf("iter %d: Close: %v", i, err)
		}
	}
}
