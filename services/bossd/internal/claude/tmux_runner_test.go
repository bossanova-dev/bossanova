package claude

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossd/internal/tmux"
)

// tmuxCallRecorder records tmux command invocations.
type tmuxCallRecorder struct {
	calls       [][]string
	successFunc func(args []string) bool
}

func (r *tmuxCallRecorder) factory(ctx context.Context, name string, args ...string) *exec.Cmd {
	full := append([]string{name}, args...)
	r.calls = append(r.calls, full)
	if r.successFunc != nil && !r.successFunc(full) {
		return exec.CommandContext(ctx, "false")
	}
	return exec.CommandContext(ctx, "true")
}

func TestTmuxRunner_SessionName(t *testing.T) {
	tests := []struct {
		sessionID string
		expected  string
	}{
		{"abc123", "autopilot-abc123"},
		{"abcdef012345", "autopilot-abcdef012345"},
		{"abcdef0123456789", "autopilot-abcdef012345"},
	}

	for _, tt := range tests {
		result := tmuxSessionName(tt.sessionID)
		if result != tt.expected {
			t.Errorf("tmuxSessionName(%q) = %q, want %q", tt.sessionID, result, tt.expected)
		}
	}
}

func TestTmuxRunner_Start_CreatesSession(t *testing.T) {
	logDir := t.TempDir()
	recorder := &tmuxCallRecorder{successFunc: func(args []string) bool { return true }}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(recorder.factory))

	r := NewTmuxRunner(
		tmuxClient,
		zerolog.Nop(),
		WithTmuxLogDir(logDir),
		WithTmuxConfigPath(filepath.Join(t.TempDir(), "settings.json")),
	)

	ctx := context.Background()
	workDir := t.TempDir()

	sid, err := r.Start(ctx, workDir, "test plan", nil, "test-session-123")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if sid != "test-session-123" {
		t.Errorf("expected session ID 'test-session-123', got %q", sid)
	}

	// Verify tmux new-session was called.
	found := false
	for _, call := range recorder.calls {
		for _, arg := range call {
			if arg == "new-session" {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("expected tmux new-session call")
	}

	// Verify plan file was written.
	planPath := filepath.Join(logDir, "test-session-123.plan")
	data, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatalf("read plan file: %v", err)
	}
	if string(data) != "test plan" {
		t.Errorf("plan content: got %q, want %q", string(data), "test plan")
	}

	// Verify the wrapper command references claude with correct args and
	// reads the plan from a file via stdin redirect (not piped via cat).
	var newSessionCall []string
	for _, call := range recorder.calls {
		for _, arg := range call {
			if arg == "new-session" {
				newSessionCall = call
				break
			}
		}
	}
	if newSessionCall != nil {
		wrapperFound := false
		for _, arg := range newSessionCall {
			if strings.Contains(arg, "claude") && strings.Contains(arg, "--print") && strings.Contains(arg, "--session-id") {
				wrapperFound = true
				// Plan should be read from file via stdin redirect, not piped via cat.
				if strings.Contains(arg, "cat ") {
					t.Error("wrapper should use stdin redirect from plan file, not pipe via cat")
				}
				// Should contain stdin redirect from the plan file.
				expectedRedirect := "< '" + planPath + "'"
				if !strings.Contains(arg, expectedRedirect) {
					t.Errorf("wrapper should contain stdin redirect from plan file, got: %s", arg)
				}
				break
			}
		}
		if !wrapperFound {
			t.Errorf("expected wrapper command with claude args in new-session call, got %v", newSessionCall)
		}
	}

	// Verify IsRunning reports true.
	if !r.IsRunning(sid) {
		t.Error("expected IsRunning=true after Start")
	}
}

func TestTmuxRunner_Start_DuplicateSession(t *testing.T) {
	logDir := t.TempDir()
	recorder := &tmuxCallRecorder{successFunc: func(args []string) bool { return true }}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(recorder.factory))

	r := NewTmuxRunner(
		tmuxClient,
		zerolog.Nop(),
		WithTmuxLogDir(logDir),
		WithTmuxConfigPath(filepath.Join(t.TempDir(), "settings.json")),
	)

	ctx := context.Background()
	workDir := t.TempDir()

	_, err := r.Start(ctx, workDir, "plan 1", nil, "dup-session")
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}

	_, err = r.Start(ctx, workDir, "plan 2", nil, "dup-session")
	if err == nil {
		t.Error("expected error for duplicate session")
	}
}

func TestTmuxRunner_CompletionDetection(t *testing.T) {
	logDir := t.TempDir()

	// Track whether has-session has been called after done file is created.
	doneFileWritten := false
	recorder := &tmuxCallRecorder{
		successFunc: func(args []string) bool {
			// has-session should fail once done file exists (session ended).
			for _, arg := range args {
				if arg == "has-session" && doneFileWritten {
					return false
				}
			}
			return true
		},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(recorder.factory))

	r := NewTmuxRunner(
		tmuxClient,
		zerolog.Nop(),
		WithTmuxLogDir(logDir),
		WithTmuxConfigPath(filepath.Join(t.TempDir(), "settings.json")),
	)

	ctx := context.Background()
	workDir := t.TempDir()

	sid, err := r.Start(ctx, workDir, "test", nil, "completion-test")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Write output to the log file.
	logPath := filepath.Join(logDir, "completion-test.log")
	if err := os.WriteFile(logPath, []byte("line 1\nline 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Write the done file with exit code 0.
	donePath := filepath.Join(logDir, "completion-test.done")
	if err := os.WriteFile(donePath, []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	doneFileWritten = true

	// Wait for completion to be detected.
	deadline := time.After(5 * time.Second)
	for r.IsRunning(sid) {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for completion detection")
		case <-time.After(50 * time.Millisecond):
		}
	}

	// Should have no exit error for code 0.
	if exitErr := r.ExitError(sid); exitErr != nil {
		t.Errorf("expected nil ExitError, got %v", exitErr)
	}

	// Wait a bit for log tailing to process.
	time.Sleep(200 * time.Millisecond)

	// Verify history captured log output.
	lines := r.History(sid)
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 history lines, got %d", len(lines))
	}
	if lines[0].Text != "line 1" {
		t.Errorf("first line: got %q, want %q", lines[0].Text, "line 1")
	}
}

func TestTmuxRunner_CompletionNonZeroExit(t *testing.T) {
	logDir := t.TempDir()
	recorder := &tmuxCallRecorder{successFunc: func(args []string) bool { return true }}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(recorder.factory))

	r := NewTmuxRunner(
		tmuxClient,
		zerolog.Nop(),
		WithTmuxLogDir(logDir),
		WithTmuxConfigPath(filepath.Join(t.TempDir(), "settings.json")),
	)

	ctx := context.Background()
	workDir := t.TempDir()

	sid, err := r.Start(ctx, workDir, "test", nil, "exit-code-test")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Write done file with non-zero exit code.
	donePath := filepath.Join(logDir, "exit-code-test.done")
	if err := os.WriteFile(donePath, []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(5 * time.Second)
	for r.IsRunning(sid) {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for completion")
		case <-time.After(50 * time.Millisecond):
		}
	}

	exitErr := r.ExitError(sid)
	if exitErr == nil {
		t.Fatal("expected non-nil ExitError for exit code 1")
	}
	if !strings.Contains(exitErr.Error(), "code 1") {
		t.Errorf("expected error to mention exit code, got %v", exitErr)
	}
}

func TestTmuxRunner_Stop_KillsSession(t *testing.T) {
	logDir := t.TempDir()

	// After kill-session, has-session should report false.
	killed := false
	recorder := &tmuxCallRecorder{
		successFunc: func(args []string) bool {
			for _, arg := range args {
				if arg == "kill-session" {
					killed = true
					return true
				}
				if arg == "has-session" && killed {
					return false
				}
			}
			return true
		},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(recorder.factory))

	r := NewTmuxRunner(
		tmuxClient,
		zerolog.Nop(),
		WithTmuxLogDir(logDir),
		WithTmuxConfigPath(filepath.Join(t.TempDir(), "settings.json")),
	)

	ctx := context.Background()
	workDir := t.TempDir()

	sid, err := r.Start(ctx, workDir, "test", nil, "stop-test")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := r.Stop(sid); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if r.IsRunning(sid) {
		t.Error("expected IsRunning=false after Stop")
	}

	// Verify kill-session was called.
	killFound := false
	for _, call := range recorder.calls {
		for _, arg := range call {
			if arg == "kill-session" {
				killFound = true
			}
		}
	}
	if !killFound {
		t.Error("expected tmux kill-session call")
	}
}

func TestTmuxRunner_Stop_Unknown(t *testing.T) {
	recorder := &tmuxCallRecorder{successFunc: func(args []string) bool { return true }}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(recorder.factory))

	r := NewTmuxRunner(tmuxClient, zerolog.Nop())
	if err := r.Stop("nonexistent"); err == nil {
		t.Error("expected error for unknown session")
	}
}

func TestTmuxRunner_SubscribeUnknown(t *testing.T) {
	recorder := &tmuxCallRecorder{successFunc: func(args []string) bool { return true }}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(recorder.factory))

	r := NewTmuxRunner(tmuxClient, zerolog.Nop())
	_, err := r.Subscribe(context.Background(), "nonexistent")
	if err == nil {
		t.Error("expected error for unknown session")
	}
}

func TestTmuxRunner_HistoryUnknown(t *testing.T) {
	recorder := &tmuxCallRecorder{successFunc: func(args []string) bool { return true }}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(recorder.factory))

	r := NewTmuxRunner(tmuxClient, zerolog.Nop())
	lines := r.History("nonexistent")
	if lines != nil {
		t.Errorf("expected nil for unknown session, got %v", lines)
	}
}

func TestTmuxRunner_FallbackWhenTmuxUnavailable(t *testing.T) {
	logDir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "settings.json")

	// tmux not available — all tmux commands fail.
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(
		func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "false")
		},
	))

	fallback := NewRunner(
		zerolog.Nop(),
		WithCommandFactory(func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "sh", "-c",
				`cat > /dev/null; for i in $(seq 1 5); do echo "fallback $i"; done`)
		}),
		WithLogDir(logDir),
		WithConfigPath(configPath),
	)

	r := NewTmuxRunner(
		tmuxClient,
		zerolog.Nop(),
		WithTmuxLogDir(logDir),
		WithTmuxConfigPath(configPath),
		WithTmuxFallback(fallback),
	)

	ctx := context.Background()
	workDir := t.TempDir()

	sid, err := r.Start(ctx, workDir, "test plan", nil, "fallback-test")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for fallback process to finish.
	deadline := time.After(5 * time.Second)
	for r.IsRunning(sid) {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for fallback to finish")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Should get history from fallback runner.
	lines := r.History(sid)
	if len(lines) < 1 {
		t.Fatal("expected at least 1 history line from fallback")
	}
	if !strings.Contains(lines[0].Text, "fallback") {
		t.Errorf("expected fallback output, got %q", lines[0].Text)
	}
}

func TestTmuxRunner_TmuxSessionDisappears(t *testing.T) {
	logDir := t.TempDir()

	// has-session returns false immediately (simulating crash).
	callCount := 0
	recorder := &tmuxCallRecorder{
		successFunc: func(args []string) bool {
			for _, arg := range args {
				if arg == "has-session" {
					callCount++
					// Let the first has-session check fail (session crashed).
					return false
				}
			}
			return true
		},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(recorder.factory))

	r := NewTmuxRunner(
		tmuxClient,
		zerolog.Nop(),
		WithTmuxLogDir(logDir),
		WithTmuxConfigPath(filepath.Join(t.TempDir(), "settings.json")),
	)

	ctx := context.Background()
	workDir := t.TempDir()

	sid, err := r.Start(ctx, workDir, "test", nil, "crash-test")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for completion to be detected via tmux session disappearance.
	deadline := time.After(5 * time.Second)
	for r.IsRunning(sid) {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for crash detection")
		case <-time.After(50 * time.Millisecond):
		}
	}

	exitErr := r.ExitError(sid)
	if exitErr == nil {
		t.Fatal("expected non-nil ExitError when tmux session disappears")
	}
	if !strings.Contains(exitErr.Error(), "disappeared") {
		t.Errorf("expected error to mention 'disappeared', got %v", exitErr)
	}
}

func TestTmuxRunner_ShellQuote(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"hello", "'hello'"},
		{"it's", "'it'\"'\"'s'"},
		{"/path/to/file", "'/path/to/file'"},
		{"", "''"},
	}

	for _, tt := range tests {
		result := shellQuote(tt.input)
		if result != tt.expected {
			t.Errorf("shellQuote(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

func TestTmuxRunner_ExitErrorUnknown(t *testing.T) {
	recorder := &tmuxCallRecorder{successFunc: func(args []string) bool { return true }}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(recorder.factory))

	r := NewTmuxRunner(tmuxClient, zerolog.Nop())
	if exitErr := r.ExitError("nonexistent"); exitErr != nil {
		t.Errorf("expected nil for unknown session, got %v", exitErr)
	}
}

func TestTmuxRunner_IsRunningUnknown(t *testing.T) {
	recorder := &tmuxCallRecorder{successFunc: func(args []string) bool { return true }}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(recorder.factory))

	r := NewTmuxRunner(tmuxClient, zerolog.Nop())
	if r.IsRunning("nonexistent") {
		t.Error("expected IsRunning=false for unknown session")
	}
}
