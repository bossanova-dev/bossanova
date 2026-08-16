package gatecmd

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRun_PlainCommand_ExitZero(t *testing.T) {
	res := Run(context.Background(), Options{
		Command: "true",
		Timeout: 5 * time.Second,
	})
	if !res.Passed {
		t.Fatalf("expected Passed=true, got false (err=%v)", res.Err)
	}
	if res.Err != nil {
		t.Fatalf("expected nil error, got %v", res.Err)
	}
	if res.Failure != FailureNone {
		t.Fatalf("Failure = %v, want FailureNone", res.Failure)
	}
}

func TestRun_PlainCommand_ExitNonZero(t *testing.T) {
	res := Run(context.Background(), Options{
		Command: "false",
		Timeout: 5 * time.Second,
	})
	if res.Passed {
		t.Fatal("expected Passed=false")
	}
	if res.Err == nil {
		t.Fatal("expected non-nil error")
	}
}

func TestRun_ZeroTimeoutUsesDefault(t *testing.T) {
	res := Run(context.Background(), Options{
		Command: "true",
		Timeout: 0,
	})
	if !res.Passed {
		t.Fatalf("expected Passed=true, got false (err=%v)", res.Err)
	}
	if res.Err != nil {
		t.Fatalf("expected nil error, got %v", res.Err)
	}
}

func TestRun_RelativePathScript_ExitZero(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "script.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Command starts with "./" → direct exec against RepoPath (no shell).
	res := Run(context.Background(), Options{
		Command:  "./script.sh",
		RepoPath: dir,
		Timeout:  5 * time.Second,
	})
	if !res.Passed {
		t.Fatalf("expected Passed=true, got false (err=%v)", res.Err)
	}
	if res.Err != nil {
		t.Fatalf("expected nil error, got %v", res.Err)
	}
}

func TestRun_AbsolutePathScript_ExitNonZero(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fail.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Command is an absolute path → direct exec.
	res := Run(context.Background(), Options{
		Command:  script,
		RepoPath: dir,
		Timeout:  5 * time.Second,
	})
	if res.Passed {
		t.Fatal("expected Passed=false")
	}
	if res.Err == nil {
		t.Fatal("expected non-nil error")
	}
}

func TestRun_EnvVars(t *testing.T) {
	dir := t.TempDir()
	// EXPECTED_PATH is set in the test process env; os.Environ() picks it up.
	t.Setenv("EXPECTED_PATH", dir)

	const body = `#!/bin/sh
[ "$REPO_DIR" = "$EXPECTED_PATH" ] || exit 1
[ "$WORKTREE_DIR" = "$EXPECTED_PATH" ] || exit 2
[ "$BOSS_WORKTREE_DIR" = "$EXPECTED_PATH" ] || exit 3
[ "${LINEAR_API_KEY+x}" = "x" ] || exit 4
[ "${SENTRY_API_KEY+x}" = "x" ] || exit 5
[ "${SENTRY_ORG+x}" = "x" ] || exit 6
[ "$LINEAR_API_KEY" = "lin-key" ] || exit 7
[ "$SENTRY_API_KEY" = "sent-key" ] || exit 8
[ "$SENTRY_ORG" = "my-org" ] || exit 9
exit 0
`
	script := filepath.Join(dir, "check-env.sh")
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	res := Run(context.Background(), Options{
		Command:      "./check-env.sh",
		RepoPath:     dir,
		LinearAPIKey: "lin-key",
		SentryAPIKey: "sent-key",
		SentryOrg:    "my-org",
		Timeout:      5 * time.Second,
	})
	if !res.Passed {
		t.Fatalf("expected Passed=true, got false (err=%v)", res.Err)
	}
	if res.Err != nil {
		t.Fatalf("expected nil error, got %v", res.Err)
	}
}

// TestRun_EnvVars_BlankKeysPresent verifies that blank key fields are still
// passed as set-but-empty variables (not unset).
func TestRun_EnvVars_BlankKeysPresent(t *testing.T) {
	dir := t.TempDir()

	const body = `#!/bin/sh
[ "${LINEAR_API_KEY+x}" = "x" ] || exit 1
[ "${SENTRY_API_KEY+x}" = "x" ] || exit 2
[ "${SENTRY_ORG+x}" = "x" ] || exit 3
[ "$LINEAR_API_KEY" = "" ] || exit 4
[ "$SENTRY_API_KEY" = "" ] || exit 5
[ "$SENTRY_ORG" = "" ] || exit 6
exit 0
`
	script := filepath.Join(dir, "check-blank.sh")
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	res := Run(context.Background(), Options{
		Command:  "./check-blank.sh",
		RepoPath: dir,
		// LinearAPIKey, SentryAPIKey, SentryOrg intentionally empty
		Timeout: 5 * time.Second,
	})
	if !res.Passed {
		t.Fatalf("expected Passed=true, got false (err=%v)", res.Err)
	}
	if res.Err != nil {
		t.Fatalf("expected nil error, got %v", res.Err)
	}
}

func TestRun_ProofAnthropicAPIKeyOverridesAmbientValue(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROOF_ANTHROPIC_API_KEY", "ambient-key")

	const body = `#!/bin/sh
[ "$PROOF_ANTHROPIC_API_KEY" = "injected-key" ] || exit 1
exit 0
`
	script := filepath.Join(dir, "check-proof-key.sh")
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	res := Run(context.Background(), Options{
		Command:              "./check-proof-key.sh",
		RepoPath:             dir,
		ProofAnthropicAPIKey: "injected-key",
		Timeout:              5 * time.Second,
	})
	if !res.Passed || res.Err != nil {
		t.Fatalf("Run = %+v; want pass", res)
	}
}

func TestRun_ProofAnthropicAPIKeyEmptyDoesNotOverrideAmbientValue(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROOF_ANTHROPIC_API_KEY", "ambient-key")

	const body = `#!/bin/sh
[ "$PROOF_ANTHROPIC_API_KEY" = "ambient-key" ] || exit 1
exit 0
`
	script := filepath.Join(dir, "check-proof-key.sh")
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	res := Run(context.Background(), Options{
		Command:  "./check-proof-key.sh",
		RepoPath: dir,
		Timeout:  5 * time.Second,
	})
	if !res.Passed || res.Err != nil {
		t.Fatalf("Run = %+v; want pass", res)
	}
}

func TestRun_ProofAnthropicAPIKeyEmptyLeavesKeyUnsetWithoutAmbientValue(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROOF_ANTHROPIC_API_KEY", "")
	if err := os.Unsetenv("PROOF_ANTHROPIC_API_KEY"); err != nil {
		t.Fatal(err)
	}

	const body = `#!/bin/sh
[ "${PROOF_ANTHROPIC_API_KEY+x}" != "x" ] || exit 1
exit 0
`
	script := filepath.Join(dir, "check-proof-key-unset.sh")
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	res := Run(context.Background(), Options{
		Command:  "./check-proof-key-unset.sh",
		RepoPath: dir,
		Timeout:  5 * time.Second,
	})
	if !res.Passed || res.Err != nil {
		t.Fatalf("Run = %+v; want pass", res)
	}
}

func TestRun_ExtraEnvOverridesAndUnsetEnvRemovesInheritedValues(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GATE_OVERLAY_TEST", "inherited-value")
	t.Setenv("GATE_REMOVE_TEST", "must-not-reach-child")

	const body = `#!/bin/sh
[ "$GATE_OVERLAY_TEST" = "daemon-value" ] || exit 1
[ "${GATE_REMOVE_TEST+x}" != "x" ] || exit 2
exit 0
`
	script := filepath.Join(dir, "check-overlay.sh")
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	res := Run(context.Background(), Options{
		Command:  "./check-overlay.sh",
		RepoPath: dir,
		ExtraEnv: map[string]string{
			"GATE_OVERLAY_TEST": "daemon-value",
		},
		UnsetEnv: []string{"GATE_REMOVE_TEST"},
		Timeout:  5 * time.Second,
	})
	if !res.Passed || res.Err != nil {
		t.Fatalf("Run = %+v; want pass", res)
	}
}

func TestCommandEnvHandlesSparseInheritedEnvironment(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestCommandEnvSparseInheritedEnvironmentHelper$")
	cmd.Env = []string{"GATECMD_SPARSE_ENV_HELPER=1"}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("commandEnv with sparse inherited environment failed: %v\n%s", err, output)
	}
}

func TestCommandEnvSparseInheritedEnvironmentHelper(t *testing.T) {
	if os.Getenv("GATECMD_SPARSE_ENV_HELPER") != "1" {
		return
	}

	got := commandEnv(Options{RepoPath: "/repo"})
	want := map[string]string{
		"GATECMD_SPARSE_ENV_HELPER": "1",
		"REPO_DIR":                  "/repo",
		"WORKTREE_DIR":              "/repo",
		"BOSS_WORKTREE_DIR":         "/repo",
		"LINEAR_API_KEY":            "",
		"SENTRY_API_KEY":            "",
		"SENTRY_ORG":                "",
	}
	if len(got) != len(want) {
		t.Fatalf("commandEnv returned %d entries, want %d: %q", len(got), len(want), got)
	}
	for _, entry := range got {
		key, value, found := strings.Cut(entry, "=")
		if !found {
			t.Fatalf("commandEnv returned malformed entry %q", entry)
		}
		if wantValue, ok := want[key]; !ok || value != wantValue {
			t.Errorf("commandEnv entry %q = %q, want %q (known key: %v)", key, value, wantValue, ok)
		}
	}
}

func TestRun_Timeout(t *testing.T) {
	res := Run(context.Background(), Options{
		Command: "sleep 5",
		Timeout: 50 * time.Millisecond,
	})
	if res.Passed {
		t.Fatal("expected Passed=false")
	}
	if res.Err == nil {
		t.Fatal("expected non-nil error")
	}
	if !strings.Contains(res.Err.Error(), "timed out") {
		t.Fatalf("expected timeout message in error, got %q", res.Err.Error())
	}
}

func TestRun_EmptyCommand(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		{name: "empty", command: ""},
		{name: "whitespace", command: "   "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := Run(context.Background(), Options{
				Command: tt.command,
				Timeout: 5 * time.Second,
			})
			if res.Passed {
				t.Fatal("expected Passed=false")
			}
			if res.Err == nil {
				t.Fatal("expected non-nil error")
			}
		})
	}
}

func TestRun_CaptureOutput(t *testing.T) {
	var buf bytes.Buffer
	res := Run(context.Background(), Options{
		Command: "echo stdout-line; echo stderr-line >&2",
		Timeout: 5 * time.Second,
		Output:  &buf,
	})
	if !res.Passed {
		t.Fatalf("expected Passed=true, got false (err=%v)", res.Err)
	}
	if res.Err != nil {
		t.Fatalf("expected nil error, got %v", res.Err)
	}
	out := buf.String()
	if !strings.Contains(out, "stdout-line") {
		t.Fatalf("expected stdout-line in output, got %q", out)
	}
	if !strings.Contains(out, "stderr-line") {
		t.Fatalf("expected stderr-line in output, got %q", out)
	}
}

// TestRun_Classification is the BOS-881 matrix: every way a gate can end,
// mapped to its FailureKind and to whether the gate produced a verdict at all.
// The 127 row is the regression this ticket exists for — a gate whose
// interpreter is missing must NOT look like a gate that decided "no work".
func TestRun_Classification(t *testing.T) {
	dir := t.TempDir()

	nonExecutable := filepath.Join(dir, "not-executable.sh")
	if err := os.WriteFile(nonExecutable, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	exitFour := filepath.Join(dir, "exit-four.sh")
	if err := os.WriteFile(exitFour, []byte("#!/bin/sh\nexit 4\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name            string
		command         string
		repoPath        string // "" → dir
		timeout         time.Duration
		wantPassed      bool
		wantFailure     FailureKind
		wantExitCode    int
		wantCouldNotRun bool
	}{
		{
			name:         "exit 0 passes",
			command:      "true",
			wantPassed:   true,
			wantFailure:  FailureNone,
			wantExitCode: 0,
		},
		{
			name:            "exit 1 is a real gate decision",
			command:         "false",
			wantFailure:     FailureNonZeroExit,
			wantExitCode:    1,
			wantCouldNotRun: false,
		},
		{
			name:            "any other non-zero exit is a real gate decision",
			command:         exitFour,
			wantFailure:     FailureNonZeroExit,
			wantExitCode:    4,
			wantCouldNotRun: false,
		},
		{
			// The BOS-880 shape: `sh -c` launches, node is gone, exit 127.
			name:            "shell exit 127 means the command could not be found",
			command:         "bos881-definitely-not-a-real-binary --check",
			wantFailure:     FailureCommandNotFound,
			wantExitCode:    shellExitNotFound,
			wantCouldNotRun: true,
		},
		{
			name:            "shell exit 126 means the command is not executable",
			command:         "'" + nonExecutable + "'",
			wantFailure:     FailureNotExecutable,
			wantExitCode:    shellExitNotExecutable,
			wantCouldNotRun: true,
		},
		{
			name:            "direct exec of a missing absolute path could not be launched",
			command:         filepath.Join(dir, "no-such-gate.sh"),
			wantFailure:     FailureCommandNotFound,
			wantExitCode:    -1,
			wantCouldNotRun: true,
		},
		{
			name:            "direct exec of a non-executable file could not be launched",
			command:         nonExecutable,
			wantFailure:     FailureNotExecutable,
			wantExitCode:    -1,
			wantCouldNotRun: true,
		},
		{
			name:            "timeout",
			command:         "sleep 30",
			timeout:         50 * time.Millisecond,
			wantFailure:     FailureTimeout,
			wantExitCode:    -1,
			wantCouldNotRun: true,
		},
		{
			name:            "empty command",
			command:         "   ",
			wantFailure:     FailureEmptyCommand,
			wantExitCode:    -1,
			wantCouldNotRun: true,
		},
		{
			// A gate killed by a signal exits with no STATUS at all: os/exec
			// reports an *exec.ExitError whose ExitCode is -1. It chose
			// nothing, so it must not be filed as "the gate said no" — the
			// same conflation the 127 row exists to prevent, arriving through
			// the OOM killer / an operator kill / a segfaulting interpreter.
			name:            "a gate killed by a signal produced no verdict",
			command:         "kill -9 $$",
			wantFailure:     FailureSignaled,
			wantExitCode:    -1,
			wantCouldNotRun: true,
		},
		{
			// The spawn itself fails before the shell exists. os/exec surfaces
			// a chdir failure as fs.ErrNotExist, so it narrows to
			// CommandNotFound rather than the generic FailureLaunch — either
			// way it is a could-not-run, which is what the outcome turns on.
			name:            "a gate whose working directory is missing never launches",
			command:         "true",
			repoPath:        filepath.Join(dir, "no-such-cwd"),
			wantFailure:     FailureCommandNotFound,
			wantExitCode:    -1,
			wantCouldNotRun: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			timeout := tt.timeout
			if timeout == 0 {
				timeout = 5 * time.Second
			}
			repoPath := tt.repoPath
			if repoPath == "" {
				repoPath = dir
			}
			res := Run(context.Background(), Options{
				Command:  tt.command,
				RepoPath: repoPath,
				Timeout:  timeout,
			})
			if res.Passed != tt.wantPassed {
				t.Errorf("Passed = %v, want %v (err=%v)", res.Passed, tt.wantPassed, res.Err)
			}
			if res.Failure != tt.wantFailure {
				t.Errorf("Failure = %v, want %v (err=%v)", res.Failure, tt.wantFailure, res.Err)
			}
			if res.ExitCode != tt.wantExitCode {
				t.Errorf("ExitCode = %d, want %d", res.ExitCode, tt.wantExitCode)
			}
			if res.CouldNotRun() != tt.wantCouldNotRun {
				t.Errorf("CouldNotRun() = %v, want %v", res.CouldNotRun(), tt.wantCouldNotRun)
			}
			if !tt.wantPassed && res.Err == nil {
				t.Error("Err = nil, want a descriptive error on every failure path")
			}
		})
	}
}

// TestRun_MissingInterpreterIsNotAGateDecision states the ticket's headline
// invariant on its own so a future refactor cannot quietly fold exit 127 back
// into the "gate said no" bucket.
func TestRun_MissingInterpreterIsNotAGateDecision(t *testing.T) {
	res := Run(context.Background(), Options{
		Command: "bos881-missing-interpreter /some/gate.mjs",
		Timeout: 5 * time.Second,
	})
	if res.Passed {
		t.Fatal("a missing interpreter must not pass the gate")
	}
	if res.Failure == FailureNonZeroExit {
		t.Fatal("a missing interpreter was classified as a real gate decision (FailureNonZeroExit)")
	}
	if !res.CouldNotRun() {
		t.Fatalf("CouldNotRun() = false for %v; want true", res.Failure)
	}
}

// TestRun_ParentCancellationIsNotAGateDecision covers the other way a gate dies
// without an exit status: the CALLER's context is cancelled (daemon shutdown, a
// client abandoning RunNow). exec kills the process, so the error is an
// *exec.ExitError with ExitCode -1 while ctx.Err() is context.Canceled rather
// than DeadlineExceeded — which means the timeout arm does not catch it, and
// without the negative-code guard it would be recorded as a healthy `gated`.
func TestRun_ParentCancellationIsNotAGateDecision(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	defer cancel()

	res := Run(ctx, Options{Command: "sleep 30", Timeout: 30 * time.Second})
	if res.Passed {
		t.Fatal("a cancelled gate must not pass")
	}
	if res.Failure == FailureNonZeroExit {
		t.Fatal("a cancelled gate was classified as a real gate decision (FailureNonZeroExit)")
	}
	if !res.CouldNotRun() {
		t.Fatalf("CouldNotRun() = false for %v; want true", res.Failure)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 (no exit status was observed)", res.ExitCode)
	}
}

func TestFailureKind_StringAndCouldNotRun(t *testing.T) {
	tests := []struct {
		kind         FailureKind
		wantString   string
		wantCouldNot bool
	}{
		{FailureNone, "none", false},
		{FailureEmptyCommand, "empty_command", true},
		{FailureTimeout, "timeout", true},
		{FailureCommandNotFound, "command_not_found", true},
		{FailureNotExecutable, "not_executable", true},
		{FailureLaunch, "launch_failure", true},
		{FailureSignaled, "signaled", true},
		{FailureNonZeroExit, "non_zero_exit", false},
		{FailureKind(99), "unknown", false},
	}
	for _, tt := range tests {
		if got := tt.kind.String(); got != tt.wantString {
			t.Errorf("FailureKind(%d).String() = %q, want %q", tt.kind, got, tt.wantString)
		}
		if got := tt.kind.CouldNotRun(); got != tt.wantCouldNot {
			t.Errorf("FailureKind(%d).CouldNotRun() = %v, want %v", tt.kind, got, tt.wantCouldNot)
		}
	}
}
