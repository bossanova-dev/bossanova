package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// TestRunnerEndToEndWithFakeCodex drives the codex Runner against a hermetic
// fake_codex.sh instead of the real codex binary. The fake binary emits a
// `thread.started` JSONL event and echoes its stdin, so this exercises:
//
//   - argv construction (codex exec --json --skip-git-repo-check)
//   - stdin write (the plan)
//   - SessionIDFromOutput discovery (the runner returns "fake-uuid-0001"
//     parsed from thread.started, not the caller's "sess-1" hint)
//   - log file capture (the proof-of-stdin line lands in the per-session log)
//   - clean exit reporting (IsRunning → false; no error)
func TestRunnerEndToEndWithFakeCodex(t *testing.T) {
	fakeBin, err := filepath.Abs("testdata/fake_codex.sh")
	if err != nil {
		t.Fatalf("abs fake_codex path: %v", err)
	}
	if _, err := os.Stat(fakeBin); err != nil {
		t.Skipf("testdata/fake_codex.sh not present: %v", err)
	}

	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")

	// Build a Runner whose CommandFactory ignores the would-be codex argv
	// and exec's the fake binary directly. We still let buildArgv compute
	// argv to exercise the per-Start argv pathway end-to-end.
	r := NewRunner(zerolog.Nop(), WithCommandFactory(func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, fakeBin)
	}))

	plan := "say hi"
	sid, err := r.Start(context.Background(), dir, plan, nil, "sess-1", logPath)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if sid == "" {
		t.Fatal("Start returned empty session ID; expected SessionIDFromOutput to discover one from thread.started")
	}
	if sid == "sess-1" {
		t.Errorf("session id = %q; expected the discovered fake-uuid value (caller hint should be replaced)", sid)
	}

	// Wait for the subprocess to exit cleanly.
	deadline := time.After(3 * time.Second)
	for r.IsRunning(sid) {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for fake_codex.sh to exit")
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}
	if exitErr := r.ExitError(sid); exitErr != nil {
		t.Fatalf("ExitError = %v, want nil for clean fake_codex exit", exitErr)
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	logStr := string(logBytes)
	if !strings.Contains(logStr, "fake codex received: "+plan) {
		t.Errorf("log file does not contain stdin proof line; log:\n%s", logStr)
	}
}

// TestRunnerResumePropagatesArgvAndStdin asserts the resume path:
//
//   - argv shape is `codex exec resume <UUID> --json --skip-git-repo-check`
//     (resume is a positional subcommand, NOT a --resume flag — Lane 0 spike)
//   - the follow-up prompt is piped to stdin alongside the resume argv,
//     same as fresh runs (codex exec resume reads stdin)
//
// Without this test, a regression that drops stdin on the resume path
// would silently send empty work to codex and the daemon would be unaware.
func TestRunnerResumePropagatesArgvAndStdin(t *testing.T) {
	fakeBin, err := filepath.Abs("testdata/fake_codex.sh")
	if err != nil {
		t.Fatalf("abs fake_codex path: %v", err)
	}
	if _, err := os.Stat(fakeBin); err != nil {
		t.Skipf("testdata/fake_codex.sh not present: %v", err)
	}

	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")
	argvLog := filepath.Join(dir, "argv.txt")
	t.Setenv("FAKE_CODEX_ARGV_LOG", argvLog)

	// agentruntime calls cmdFactory(ctx, argv[0], argv[1:]...) — `name` is
	// the binary name ("codex"), `args` is everything after. Forward args
	// verbatim to the fake so it can record them into FAKE_CODEX_ARGV_LOG.
	r := NewRunner(zerolog.Nop(), WithCommandFactory(func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, fakeBin, args...)
	}))

	resumeID := "uuid-resume-target"
	followUp := "now do step two"
	sid, err := r.Start(context.Background(), dir, followUp, &resumeID, "ignored-hint", logPath)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for the subprocess to exit cleanly.
	deadline := time.After(3 * time.Second)
	for r.IsRunning(sid) {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for fake_codex.sh to exit")
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}
	if exitErr := r.ExitError(sid); exitErr != nil {
		t.Fatalf("ExitError = %v, want nil", exitErr)
	}

	// Argv assertion: the recorded argv must contain `exec resume <UUID> --json --skip-git-repo-check`
	argvBytes, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	argv := strings.Split(strings.TrimSpace(string(argvBytes)), "\n")
	wantArgv := []string{"exec", "resume", resumeID, "--json", "--skip-git-repo-check"}
	if !reflect.DeepEqual(argv, wantArgv) {
		t.Errorf("argv = %v, want %v", argv, wantArgv)
	}

	// Stdin assertion: the follow-up prompt must reach the subprocess.
	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(logBytes), "fake codex received: "+followUp) {
		t.Errorf("log file does not contain follow-up prompt; log:\n%s", logBytes)
	}
}
