package gatecmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRun_PlainCommand_ExitZero(t *testing.T) {
	passed, err := Run(context.Background(), Options{
		Command: "true",
		Timeout: 5 * time.Second,
	})
	if !passed {
		t.Fatalf("expected passed=true, got false (err=%v)", err)
	}
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

func TestRun_PlainCommand_ExitNonZero(t *testing.T) {
	passed, err := Run(context.Background(), Options{
		Command: "false",
		Timeout: 5 * time.Second,
	})
	if passed {
		t.Fatal("expected passed=false")
	}
	if err == nil {
		t.Fatal("expected non-nil error")
	}
}

func TestRun_ZeroTimeoutUsesDefault(t *testing.T) {
	passed, err := Run(context.Background(), Options{
		Command: "true",
		Timeout: 0,
	})
	if !passed {
		t.Fatalf("expected passed=true, got false (err=%v)", err)
	}
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

func TestRun_RelativePathScript_ExitZero(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "script.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Command starts with "./" → direct exec against RepoPath (no shell).
	passed, err := Run(context.Background(), Options{
		Command:  "./script.sh",
		RepoPath: dir,
		Timeout:  5 * time.Second,
	})
	if !passed {
		t.Fatalf("expected passed=true, got false (err=%v)", err)
	}
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

func TestRun_AbsolutePathScript_ExitNonZero(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fail.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Command is an absolute path → direct exec.
	passed, err := Run(context.Background(), Options{
		Command:  script,
		RepoPath: dir,
		Timeout:  5 * time.Second,
	})
	if passed {
		t.Fatal("expected passed=false")
	}
	if err == nil {
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

	passed, err := Run(context.Background(), Options{
		Command:      "./check-env.sh",
		RepoPath:     dir,
		LinearAPIKey: "lin-key",
		SentryAPIKey: "sent-key",
		SentryOrg:    "my-org",
		Timeout:      5 * time.Second,
	})
	if !passed {
		t.Fatalf("expected passed=true, got false (err=%v)", err)
	}
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
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

	passed, err := Run(context.Background(), Options{
		Command:  "./check-blank.sh",
		RepoPath: dir,
		// LinearAPIKey, SentryAPIKey, SentryOrg intentionally empty
		Timeout: 5 * time.Second,
	})
	if !passed {
		t.Fatalf("expected passed=true, got false (err=%v)", err)
	}
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
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

	passed, err := Run(context.Background(), Options{
		Command:              "./check-proof-key.sh",
		RepoPath:             dir,
		ProofAnthropicAPIKey: "injected-key",
		Timeout:              5 * time.Second,
	})
	if !passed || err != nil {
		t.Fatalf("Run = %v, %v; want pass", passed, err)
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

	passed, err := Run(context.Background(), Options{
		Command:  "./check-proof-key.sh",
		RepoPath: dir,
		Timeout:  5 * time.Second,
	})
	if !passed || err != nil {
		t.Fatalf("Run = %v, %v; want pass", passed, err)
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

	passed, err := Run(context.Background(), Options{
		Command:  "./check-proof-key-unset.sh",
		RepoPath: dir,
		Timeout:  5 * time.Second,
	})
	if !passed || err != nil {
		t.Fatalf("Run = %v, %v; want pass", passed, err)
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

	passed, err := Run(context.Background(), Options{
		Command:  "./check-overlay.sh",
		RepoPath: dir,
		ExtraEnv: map[string]string{
			"GATE_OVERLAY_TEST": "daemon-value",
		},
		UnsetEnv: []string{"GATE_REMOVE_TEST"},
		Timeout:  5 * time.Second,
	})
	if !passed || err != nil {
		t.Fatalf("Run = %v, %v; want pass", passed, err)
	}
}

func TestRun_Timeout(t *testing.T) {
	passed, err := Run(context.Background(), Options{
		Command: "sleep 5",
		Timeout: 50 * time.Millisecond,
	})
	if passed {
		t.Fatal("expected passed=false")
	}
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout message in error, got %q", err.Error())
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
			passed, err := Run(context.Background(), Options{
				Command: tt.command,
				Timeout: 5 * time.Second,
			})
			if passed {
				t.Fatal("expected passed=false")
			}
			if err == nil {
				t.Fatal("expected non-nil error")
			}
		})
	}
}

func TestRun_CaptureOutput(t *testing.T) {
	var buf bytes.Buffer
	passed, err := Run(context.Background(), Options{
		Command: "echo stdout-line; echo stderr-line >&2",
		Timeout: 5 * time.Second,
		Output:  &buf,
	})
	if !passed {
		t.Fatalf("expected passed=true, got false (err=%v)", err)
	}
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "stdout-line") {
		t.Fatalf("expected stdout-line in output, got %q", out)
	}
	if !strings.Contains(out, "stderr-line") {
		t.Fatalf("expected stderr-line in output, got %q", out)
	}
}
