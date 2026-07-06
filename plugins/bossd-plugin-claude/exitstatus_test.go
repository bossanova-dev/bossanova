package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

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
	r := NewRunner(zerolog.Nop(), WithCommandFactory(fakeClaude(t, "echo 'Claude usage limit reached. resets at 15:00'; exit 1")))
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
	r := NewRunner(zerolog.Nop(), WithCommandFactory(fakeClaude(t, "echo 'Claude usage limit reached. try again later'; exit 1")))
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

// TestExitStatusAuthTailLeavesNewFieldsUnset pins the claude scope decision:
// an auth-only tail is NOT upgraded (claude has no auth sentinel), so
// failure_class stays unset even though the exit was non-zero.
func TestExitStatusAuthTailLeavesNewFieldsUnset(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")
	r := NewRunner(zerolog.Nop(), WithCommandFactory(fakeClaude(t, "echo 'authentication token has been invalidated, please sign in again'; exit 1")))
	srv := &Server{logger: zerolog.Nop(), runner: r}

	start, _ := srv.StartRun(context.Background(), &bossanovav1.StartAgentRunRequest{WorkDir: dir, SessionId: "sid-auth", LogPath: logPath})
	exit := waitExit(t, srv, start.SessionId)

	if exit.FailureClass != nil {
		t.Errorf("FailureClass = %q, want unset (claude auth out of scope)", exit.GetFailureClass())
	}
	if exit.GetResetAt() != nil {
		t.Errorf("ResetAt = %v, want unset", exit.GetResetAt())
	}
}

func TestExitStatusCleanExitLeavesNewFieldsUnset(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")
	r := NewRunner(zerolog.Nop(), WithCommandFactory(fakeClaude(t, "echo done; exit 0")))
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
