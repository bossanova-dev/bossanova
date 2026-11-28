package agentruntime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestRunLoginStatusCapturesOutputAndExitCode(t *testing.T) {
	var (
		gotName string
		gotArgs []string
		gotCmd  *exec.Cmd
	)
	factory := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		gotName = name
		gotArgs = append([]string(nil), args...)
		gotCmd = exec.CommandContext(ctx, "/bin/sh", "-c", "printf ok; exit 3")
		return gotCmd
	}

	out := RunLoginStatus(t.Context(), LoginStatusProbe{
		Shell:   "/bin/sh",
		Argv:    []string{"fake-cli", "auth", "status"},
		Factory: factory,
	})

	if out.Err != nil {
		t.Fatalf("unexpected error: %v", out.Err)
	}
	if string(out.Output) != "ok" {
		t.Fatalf("output = %q, want %q", string(out.Output), "ok")
	}
	if out.ExitCode != 3 {
		t.Fatalf("exit code = %d, want 3", out.ExitCode)
	}
	if gotName != "/bin/sh" {
		t.Fatalf("wrapped executable = %q, want /bin/sh", gotName)
	}
	if !slices.Contains(gotArgs, "fake-cli") || !slices.Contains(gotArgs, "status") {
		t.Fatalf("wrapped args missing original argv: %#v", gotArgs)
	}
	if gotCmd == nil {
		t.Fatal("factory was not invoked")
	}
	if gotCmd.Dir != os.TempDir() {
		t.Fatalf("cmd.Dir = %q, want %q", gotCmd.Dir, os.TempDir())
	}
	if gotCmd.WaitDelay <= 0 {
		t.Fatalf("cmd.WaitDelay = %v, want > 0", gotCmd.WaitDelay)
	}
}

func TestRunLoginStatusHonoursCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	factory := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "/bin/sh", "-c", "sleep 60")
	}

	out := RunLoginStatus(ctx, LoginStatusProbe{
		Shell:   "/bin/sh",
		Argv:    []string{"fake-cli", "status"},
		Factory: factory,
	})

	if !errors.Is(out.Err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", out.Err)
	}
}

func TestRunLoginStatusAppliesTimeout(t *testing.T) {
	var hasDeadline bool
	factory := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		_, hasDeadline = ctx.Deadline()
		return exec.CommandContext(ctx, "/bin/sh", "-c", "printf ready")
	}

	out := RunLoginStatus(t.Context(), LoginStatusProbe{
		Shell:   "/bin/sh",
		Argv:    []string{"fake-cli", "status"},
		Factory: factory,
		Timeout: 60 * time.Second,
	})

	if out.Err != nil {
		t.Fatalf("unexpected error: %v", out.Err)
	}
	if !hasDeadline {
		t.Fatal("expected probe context to carry a deadline when Timeout > 0")
	}
	if string(out.Output) != "ready" || out.ExitCode != 0 {
		t.Fatalf("got output=%q exit=%d, want output=%q exit=0", string(out.Output), out.ExitCode, "ready")
	}
}

func TestRunLoginStatusLaunchFailure(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "definitely-missing-binary")

	out := RunLoginStatus(t.Context(), LoginStatusProbe{
		Argv: []string{missing},
	})

	if out.Err == nil {
		t.Fatalf("expected launch failure error, got nil (exit=%d output=%q)", out.ExitCode, string(out.Output))
	}
}

func TestRunLoginStatusEmptyArgv(t *testing.T) {
	out := RunLoginStatus(t.Context(), LoginStatusProbe{})
	if out.Err == nil {
		t.Fatal("expected error for empty argv, got nil")
	}
}
