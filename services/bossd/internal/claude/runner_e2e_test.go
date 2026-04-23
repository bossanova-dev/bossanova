package claude

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// fakeClaudePath returns the absolute path to testdata/fake_claude.sh.
func fakeClaudePath(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(file), "testdata", "fake_claude.sh")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("fake_claude.sh not found at %s: %v", path, err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("fake_claude.sh is not executable: %s (mode %v)", path, info.Mode())
	}
	return path
}

// runnerWithFakeClaude builds a Runner whose CommandFactory replaces the
// "claude" binary with fake_claude.sh, forwarding the supplied env vars into
// the spawned subprocess. Returns the runner plus the log directory (for
// path assertions).
func runnerWithFakeClaude(t *testing.T, fakeEnv map[string]string) (*Runner, string) {
	t.Helper()
	fake := fakeClaudePath(t)
	logDir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "settings.json")

	factory := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name == "claude" {
			name = fake
		}
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Env = append(os.Environ(), envSlice(fakeEnv)...)
		return cmd
	}

	r := NewRunner(
		zerolog.Nop(),
		WithCommandFactory(factory),
		WithLogDir(logDir),
		WithConfigPath(configPath),
	)
	return r, logDir
}

func envSlice(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

// waitForHistory polls History until it reaches at least n lines or times out.
func waitForHistory(t *testing.T, r *Runner, sid string, n int, timeout time.Duration) []OutputLine {
	t.Helper()
	deadline := time.After(timeout)
	for {
		if lines := r.History(sid); len(lines) >= n {
			return lines
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %d history lines (got %d)", n, len(r.History(sid)))
			return nil
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// waitForExit2 is a local helper (runner_test.go already defines waitForExit).
func waitForExit2(t *testing.T, r *Runner, sid string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		if !r.IsRunning(sid) {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for session %s to exit", sid)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestE2E_ClaudeRunner_EmitsOutputLines(t *testing.T) {
	r, _ := runnerWithFakeClaude(t, map[string]string{"FAKE_CLAUDE_LINES": "5"})
	sid, err := r.Start(context.Background(), t.TempDir(), "plan", nil, "")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitForExit2(t, r, sid, 5*time.Second)

	lines := r.History(sid)
	if len(lines) != 5 {
		t.Fatalf("expected 5 history lines, got %d: %+v", len(lines), lines)
	}
	for i, l := range lines {
		want := fmt.Sprintf(`"line":%d`, i+1)
		if !strings.Contains(l.Text, want) {
			t.Errorf("line %d: missing %q in %q", i, want, l.Text)
		}
	}
}

func TestE2E_ClaudeRunner_WritesLogFile(t *testing.T) {
	r, logDir := runnerWithFakeClaude(t, map[string]string{"FAKE_CLAUDE_LINES": "3"})
	sid, err := r.Start(context.Background(), t.TempDir(), "plan", nil, "test-log-session")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForExit2(t, r, sid, 5*time.Second)

	logPath := filepath.Join(logDir, "test-log-session.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log %s: %v", logPath, err)
	}
	if !strings.Contains(string(data), `"line":1`) {
		t.Errorf("log missing first line: %q", string(data))
	}
}

func TestE2E_ClaudeRunner_RingBufferRetention(t *testing.T) {
	r, _ := runnerWithFakeClaude(t, map[string]string{"FAKE_CLAUDE_LINES": "1500"})
	sid, err := r.Start(context.Background(), t.TempDir(), "plan", nil, "")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForExit2(t, r, sid, 15*time.Second)

	lines := r.History(sid)
	if len(lines) != DefaultRingBufferSize {
		t.Errorf("expected ring buffer to retain exactly %d lines, got %d", DefaultRingBufferSize, len(lines))
	}
	// First retained line should be the 501st emitted (1500 - 1000 + 1).
	if !strings.Contains(lines[0].Text, `"line":501`) {
		t.Errorf("expected oldest retained line to be line 501, got: %q", lines[0].Text)
	}
	// Last line should be line 1500.
	if !strings.Contains(lines[len(lines)-1].Text, `"line":1500`) {
		t.Errorf("expected newest retained line to be line 1500, got: %q", lines[len(lines)-1].Text)
	}
}

func TestE2E_ClaudeRunner_MultiSubscriber(t *testing.T) {
	// START_DELAY_MS lets subscribers register before the first emission;
	// otherwise line 1 can be broadcast before Subscribe is called.
	r, _ := runnerWithFakeClaude(t, map[string]string{
		"FAKE_CLAUDE_LINES":          "5",
		"FAKE_CLAUDE_DELAY_MS":       "050",
		"FAKE_CLAUDE_START_DELAY_MS": "500",
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sid, err := r.Start(ctx, t.TempDir(), "plan", nil, "multi-sub")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	subCtx, subCancel := context.WithCancel(ctx)
	defer subCancel()
	subA, err := r.Subscribe(subCtx, sid)
	if err != nil {
		t.Fatalf("Subscribe A: %v", err)
	}
	subB, err := r.Subscribe(subCtx, sid)
	if err != nil {
		t.Fatalf("Subscribe B: %v", err)
	}

	collect := func(ch <-chan OutputLine) []OutputLine {
		var got []OutputLine
		deadline := time.After(5 * time.Second)
		for {
			select {
			case l, ok := <-ch:
				if !ok {
					return got
				}
				got = append(got, l)
			case <-deadline:
				return got
			}
		}
	}

	doneA := make(chan []OutputLine)
	doneB := make(chan []OutputLine)
	go func() { doneA <- collect(subA) }()
	go func() { doneB <- collect(subB) }()

	waitForExit2(t, r, sid, 5*time.Second)
	linesA := <-doneA
	linesB := <-doneB

	if len(linesA) != 5 {
		t.Errorf("subscriber A: expected 5 lines, got %d", len(linesA))
	}
	if len(linesB) != 5 {
		t.Errorf("subscriber B: expected 5 lines, got %d", len(linesB))
	}
}

func TestE2E_ClaudeRunner_GracefulShutdown(t *testing.T) {
	// Long-running subprocess that exits cleanly on SIGTERM (default trap).
	r, _ := runnerWithFakeClaude(t, map[string]string{
		"FAKE_CLAUDE_LINES":    "1000",
		"FAKE_CLAUDE_DELAY_MS": "050",
	})
	sid, err := r.Start(context.Background(), t.TempDir(), "plan", nil, "graceful")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Give it a moment to start emitting.
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	if err := r.Stop(sid); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed > 10*time.Second {
		t.Errorf("Stop took %v, expected well under 10s (graceful path)", elapsed)
	}
	if r.IsRunning(sid) {
		t.Error("expected IsRunning=false after Stop")
	}
}

func TestE2E_ClaudeRunner_ForceKillOnTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("force-kill test waits 10s for graceful-shutdown timeout")
	}
	r, _ := runnerWithFakeClaude(t, map[string]string{
		"FAKE_CLAUDE_LINES":          "1",
		"FAKE_CLAUDE_IGNORE_SIGTERM": "1",
	})
	sid, err := r.Start(context.Background(), t.TempDir(), "plan", nil, "force-kill")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for the fake to emit its line so we know it's running.
	waitForHistory(t, r, sid, 1, 5*time.Second)

	start := time.Now()
	if err := r.Stop(sid); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	elapsed := time.Since(start)

	// Stop waits up to 10s graceful, then Kills. Expect ~10s.
	if elapsed < 9*time.Second {
		t.Errorf("expected Stop to wait ~10s before force-kill, took %v", elapsed)
	}
	if elapsed > 15*time.Second {
		t.Errorf("force-kill took too long: %v (expected <15s)", elapsed)
	}
	if r.IsRunning(sid) {
		t.Error("expected IsRunning=false after force-kill")
	}
}

func TestE2E_ClaudeRunner_ResumeFlag(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	r, _ := runnerWithFakeClaude(t, map[string]string{
		"FAKE_CLAUDE_LINES":          "1",
		"FAKE_CLAUDE_ECHO_ARGS_FILE": argsFile,
	})

	resumeID := "resume-uuid-xyz"
	sid, err := r.Start(context.Background(), t.TempDir(), "plan", &resumeID, "")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForExit2(t, r, sid, 5*time.Second)

	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args file: %v", err)
	}
	argStr := string(args)
	if !strings.Contains(argStr, "--resume") {
		t.Errorf("--resume flag not passed to fake claude: %q", argStr)
	}
	if !strings.Contains(argStr, resumeID) {
		t.Errorf("resume ID %q not passed to fake claude: %q", resumeID, argStr)
	}
}

func TestE2E_ClaudeRunner_NonZeroExit(t *testing.T) {
	r, _ := runnerWithFakeClaude(t, map[string]string{
		"FAKE_CLAUDE_LINES": "1",
		"FAKE_CLAUDE_EXIT":  "7",
	})
	sid, err := r.Start(context.Background(), t.TempDir(), "plan", nil, "exit-fail")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForExit2(t, r, sid, 5*time.Second)

	exitErr := r.ExitError(sid)
	if exitErr == nil {
		t.Fatal("expected non-nil ExitError for non-zero exit")
	}
	var ee *exec.ExitError
	if !errors.As(exitErr, &ee) {
		t.Fatalf("expected *exec.ExitError, got %T: %v", exitErr, exitErr)
	}
	if ee.ExitCode() != 7 {
		t.Errorf("expected exit code 7, got %d", ee.ExitCode())
	}
}
