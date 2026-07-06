package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/agentruntime"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// fakeCodexShell runs /bin/sh -c "$script" instead of the real codex binary so
// ExitStatus tests can drive a usage-cap / auth / clean exit through the real
// PostExit hook without codex installed.
func fakeCodexShell(t *testing.T, script string) agentruntime.CommandFactory {
	t.Helper()
	return func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "/bin/sh", "-c", script)
	}
}

func waitExit(t *testing.T, s *Server, sid string) *bossanovav1.AgentExitStatusResponse {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		exit, err := s.ExitStatus(context.Background(), &bossanovav1.AgentExitStatusRequest{SessionId: sid})
		if err == nil && exit.IsComplete {
			return exit
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("never observed exit")
	return nil
}

func TestExitStatusPropagatesUsageLimited(t *testing.T) {
	isolateCaptureDir(t)
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")
	r := NewRunner(zerolog.Nop(), WithCommandFactory(fakeCodexShell(t, "echo 'stream error: usage_limit_reached, resets at 15:00'; exit 1")))
	srv := &Server{logger: zerolog.Nop(), runner: r}

	start, err := srv.StartRun(context.Background(), &bossanovav1.StartAgentRunRequest{WorkDir: dir, SessionId: "sid-cap", LogPath: logPath})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	exit := waitExit(t, srv, start.SessionId)

	if exit.GetFailureClass() != "usage_exhausted" {
		t.Errorf("FailureClass = %q, want usage_exhausted", exit.GetFailureClass())
	}
	if exit.GetResetAt() == nil {
		t.Error("ResetAt unset, want a parsed reset time for a 15:00 banner")
	}
	if exit.GetExitError() == "" {
		t.Error("ExitError empty, want the usage-limited message")
	}
}

func TestExitStatusUsageLimitedUnparseableResetOmitsResetAt(t *testing.T) {
	isolateCaptureDir(t)
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")
	r := NewRunner(zerolog.Nop(), WithCommandFactory(fakeCodexShell(t, "echo 'stream error: usage_limit_reached, try again later'; exit 1")))
	srv := &Server{logger: zerolog.Nop(), runner: r}

	start, _ := srv.StartRun(context.Background(), &bossanovav1.StartAgentRunRequest{WorkDir: dir, SessionId: "sid-cap2", LogPath: logPath})
	exit := waitExit(t, srv, start.SessionId)

	if exit.GetFailureClass() != "usage_exhausted" {
		t.Errorf("FailureClass = %q, want usage_exhausted", exit.GetFailureClass())
	}
	if exit.GetResetAt() != nil {
		t.Errorf("ResetAt = %v, want unset for an unparseable reset", exit.GetResetAt())
	}
}

func TestExitStatusPropagatesAuthInvalidated(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")
	r := NewRunner(zerolog.Nop(), WithCommandFactory(fakeCodexShell(t, "echo 'HTTP error: 401 Unauthorized'; exit 1")))
	srv := &Server{logger: zerolog.Nop(), runner: r}

	start, _ := srv.StartRun(context.Background(), &bossanovav1.StartAgentRunRequest{WorkDir: dir, SessionId: "sid-auth", LogPath: logPath})
	exit := waitExit(t, srv, start.SessionId)

	if exit.GetFailureClass() != "auth_invalidated" {
		t.Errorf("FailureClass = %q, want auth_invalidated", exit.GetFailureClass())
	}
	if exit.GetResetAt() != nil {
		t.Errorf("ResetAt = %v, want unset for an auth exit", exit.GetResetAt())
	}
	if exit.GetExitError() == "" {
		t.Error("ExitError empty, want the auth-required message")
	}
}

func TestExitStatusCleanExitLeavesNewFieldsUnset(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")
	r := NewRunner(zerolog.Nop(), WithCommandFactory(fakeCodexShell(t, "echo done; exit 0")))
	srv := &Server{logger: zerolog.Nop(), runner: r}

	start, _ := srv.StartRun(context.Background(), &bossanovav1.StartAgentRunRequest{WorkDir: dir, SessionId: "sid-clean", LogPath: logPath})
	exit := waitExit(t, srv, start.SessionId)

	if exit.FailureClass != nil {
		t.Errorf("FailureClass = %q, want unset on clean exit", exit.GetFailureClass())
	}
	if exit.GetResetAt() != nil {
		t.Errorf("ResetAt = %v, want unset on clean exit", exit.GetResetAt())
	}
	if exit.GetExitError() != "" {
		t.Errorf("ExitError = %q, want empty on clean exit", exit.GetExitError())
	}
}
