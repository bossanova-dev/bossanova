package claude

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func testRunner(t *testing.T) *Runner {
	t.Helper()
	logDir := t.TempDir()
	return NewRunner(
		zerolog.Nop(),
		WithCommandFactory(func(ctx context.Context, name string, args ...string) *exec.Cmd {
			// Replace "claude" with a shell script that reads stdin and echoes numbered lines.
			return exec.CommandContext(ctx, "sh", "-c",
				`cat > /dev/null; for i in $(seq 1 10); do echo "line $i"; done`)
		}),
		WithLogDir(logDir),
	)
}

func TestStartStop(t *testing.T) {
	r := testRunner(t)
	ctx := context.Background()
	workDir := t.TempDir()

	sid, err := r.Start(ctx, workDir, "test plan", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if sid == "" {
		t.Fatal("expected non-empty session ID")
	}

	// Process should be running (or recently started).
	// Give it a moment to actually start.
	time.Sleep(50 * time.Millisecond)

	// Stop the process.
	if err := r.Stop(sid); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// After stop, IsRunning should be false.
	if r.IsRunning(sid) {
		t.Error("expected IsRunning=false after Stop")
	}
}

func TestIsRunning(t *testing.T) {
	r := testRunner(t)
	ctx := context.Background()
	workDir := t.TempDir()

	// Not running before start.
	if r.IsRunning("nonexistent") {
		t.Error("expected IsRunning=false for unknown session")
	}

	sid, err := r.Start(ctx, workDir, "test plan", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for process to finish naturally (the echo script is fast).
	time.Sleep(200 * time.Millisecond)

	// Should no longer be running (process exited).
	if r.IsRunning(sid) {
		t.Error("expected IsRunning=false after process exits")
	}
}

func TestHistory(t *testing.T) {
	r := testRunner(t)
	ctx := context.Background()
	workDir := t.TempDir()

	sid, err := r.Start(ctx, workDir, "test plan", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for process to finish and output to be captured.
	time.Sleep(200 * time.Millisecond)

	lines := r.History(sid)
	if len(lines) != 10 {
		t.Fatalf("expected 10 history lines, got %d", len(lines))
	}

	for i, line := range lines {
		expected := fmt.Sprintf("line %d", i+1)
		if line.Text != expected {
			t.Errorf("line %d: got %q, want %q", i, line.Text, expected)
		}
	}
}

func TestLogFileWritten(t *testing.T) {
	logDir := t.TempDir()
	r := NewRunner(
		zerolog.Nop(),
		WithCommandFactory(func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "sh", "-c",
				`cat > /dev/null; echo "hello"; echo "world"`)
		}),
		WithLogDir(logDir),
	)

	ctx := context.Background()
	workDir := t.TempDir()

	sid, err := r.Start(ctx, workDir, "test", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for process to finish.
	time.Sleep(200 * time.Millisecond)

	// Check log file exists at logDir/<sessionID>.log.
	logPath := filepath.Join(logDir, sid+".log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}

	content := strings.TrimSpace(string(data))
	if content != "hello\nworld" {
		t.Errorf("log content: got %q, want %q", content, "hello\nworld")
	}
}

func TestDefaultLogPath(t *testing.T) {
	r := NewRunner(zerolog.Nop(),
		WithCommandFactory(func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "sh", "-c", `cat > /dev/null; echo "ok"`)
		}),
	)

	ctx := context.Background()
	workDir := t.TempDir()

	_, err := r.Start(ctx, workDir, "test", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for process to finish.
	time.Sleep(200 * time.Millisecond)

	// Default log path should be workDir/.boss/claude.log.
	logPath := filepath.Join(workDir, ".boss", "claude.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read default log file: %v", err)
	}

	if !strings.Contains(string(data), "ok") {
		t.Errorf("expected log to contain 'ok', got %q", string(data))
	}
}

func TestHistoryUnknownSession(t *testing.T) {
	r := NewRunner(zerolog.Nop())
	lines := r.History("nonexistent")
	if lines != nil {
		t.Errorf("expected nil for unknown session, got %v", lines)
	}
}

func TestSubscriber(t *testing.T) {
	r := testRunner(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	workDir := t.TempDir()

	sid, err := r.Start(ctx, workDir, "test plan", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	ch, err := r.Subscribe(ctx, sid)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Collect lines from subscriber.
	var received []OutputLine
	timeout := time.After(2 * time.Second)
	for {
		select {
		case line, ok := <-ch:
			if !ok {
				goto done
			}
			received = append(received, line)
		case <-timeout:
			t.Fatal("timed out waiting for subscriber lines")
		}
	}
done:

	if len(received) < 1 {
		t.Fatal("expected at least 1 subscriber line")
	}

	// Verify we got output lines.
	if received[0].Text != "line 1" {
		t.Errorf("first subscriber line: got %q, want %q", received[0].Text, "line 1")
	}
}

func TestSubscribeUnknownSession(t *testing.T) {
	r := NewRunner(zerolog.Nop())
	_, err := r.Subscribe(context.Background(), "nonexistent")
	if err == nil {
		t.Error("expected error for unknown session")
	}
}

func TestStopUnknownSession(t *testing.T) {
	r := NewRunner(zerolog.Nop())
	if err := r.Stop("nonexistent"); err == nil {
		t.Error("expected error for unknown session")
	}
}

// TestRingBufferOverflow verifies that the oldest entries are evicted when
// the ring buffer exceeds its capacity.
func TestRingBufferOverflow(t *testing.T) {
	rb := newRingBuffer(5) // Small buffer for testing.

	// Write 8 entries (3 more than capacity).
	for i := 0; i < 8; i++ {
		rb.add(OutputLine{Text: fmt.Sprintf("line-%d", i)})
	}

	lines := rb.lines()
	if len(lines) != 5 {
		t.Fatalf("expected 5 lines, got %d", len(lines))
	}

	// Should have lines 3-7 (oldest 0-2 evicted).
	for i, line := range lines {
		expected := fmt.Sprintf("line-%d", i+3)
		if line.Text != expected {
			t.Errorf("line %d: got %q, want %q", i, line.Text, expected)
		}
	}
}

// TestRingBufferUnderflow verifies partial buffers return correct results.
func TestRingBufferUnderflow(t *testing.T) {
	rb := newRingBuffer(100)

	rb.add(OutputLine{Text: "a"})
	rb.add(OutputLine{Text: "b"})
	rb.add(OutputLine{Text: "c"})

	lines := rb.lines()
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}

	if lines[0].Text != "a" || lines[1].Text != "b" || lines[2].Text != "c" {
		t.Errorf("unexpected lines: %v", lines)
	}
}

// TestRingBufferEmpty verifies empty buffer returns nil.
func TestRingBufferEmpty(t *testing.T) {
	rb := newRingBuffer(10)
	lines := rb.lines()
	if lines != nil {
		t.Errorf("expected nil for empty buffer, got %v", lines)
	}
}

// TestRingBufferExactCapacity verifies buffer at exact capacity.
func TestRingBufferExactCapacity(t *testing.T) {
	rb := newRingBuffer(3)

	rb.add(OutputLine{Text: "x"})
	rb.add(OutputLine{Text: "y"})
	rb.add(OutputLine{Text: "z"})

	lines := rb.lines()
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}

	if lines[0].Text != "x" || lines[1].Text != "y" || lines[2].Text != "z" {
		t.Errorf("unexpected: %v", lines)
	}
}

// TestRingBuffer1000Overflow verifies the default buffer size behavior.
func TestRingBuffer1000Overflow(t *testing.T) {
	rb := newRingBuffer(DefaultRingBufferSize)

	// Write 1200 entries.
	for i := 0; i < 1200; i++ {
		rb.add(OutputLine{Text: fmt.Sprintf("entry-%d", i)})
	}

	lines := rb.lines()
	if len(lines) != DefaultRingBufferSize {
		t.Fatalf("expected %d lines, got %d", DefaultRingBufferSize, len(lines))
	}

	// Oldest should be entry-200, newest should be entry-1199.
	if lines[0].Text != "entry-200" {
		t.Errorf("oldest: got %q, want %q", lines[0].Text, "entry-200")
	}
	if lines[len(lines)-1].Text != "entry-1199" {
		t.Errorf("newest: got %q, want %q", lines[len(lines)-1].Text, "entry-1199")
	}
}

func TestStartWithResume(t *testing.T) {
	logDir := t.TempDir()
	resumeID := "prev-session-123"
	var capturedArgs []string

	r := NewRunner(
		zerolog.Nop(),
		WithCommandFactory(func(ctx context.Context, name string, args ...string) *exec.Cmd {
			capturedArgs = args
			return exec.CommandContext(ctx, "sh", "-c", "cat > /dev/null; echo resumed")
		}),
		WithLogDir(logDir),
	)

	ctx := context.Background()
	workDir := t.TempDir()

	_, err := r.Start(ctx, workDir, "continue work", &resumeID)
	if err != nil {
		t.Fatalf("Start with resume: %v", err)
	}

	// Verify the --resume flag was passed.
	found := false
	for i, arg := range capturedArgs {
		if arg == "--resume" && i+1 < len(capturedArgs) && capturedArgs[i+1] == resumeID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected --resume %s in args, got %v", resumeID, capturedArgs)
	}
}
