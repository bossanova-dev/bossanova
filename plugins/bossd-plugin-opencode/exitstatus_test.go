package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/recurser/bossalib/agenterr"
	"github.com/recurser/bossalib/agentruntime"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// fakeOpencodeShell runs /bin/sh -c "$script" instead of the real opencode
// binary so the run-lifecycle RPCs (StartRun → ExitStatus) can be driven through
// the real runner + PostExit hook without opencode installed. It mirrors the
// codex twin's fakeCodexShell.
func fakeOpencodeShell(t *testing.T, script string) agentruntime.CommandFactory {
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

// TestStartRunReturnsDiscoveredSessionID proves the wired StartRun spawns the
// runner and returns opencode's own echoed `sessionID` (via SessionIDFromOutput)
// rather than the caller-provided hint — the session-id capture seam BOS-437
// live validation exercises end-to-end.
func TestStartRunReturnsDiscoveredSessionID(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")
	r := NewRunner(zerolog.Nop(), WithCommandFactory(fakeOpencodeShell(t,
		`printf '%s\n' '{"type":"step_start","sessionID":"ses_wired0001abcd","part":{"type":"step-start"}}'; exit 0`)))
	srv := &Server{logger: zerolog.Nop(), runner: r}

	start, err := srv.StartRun(context.Background(), &bossanovav1.StartAgentRunRequest{
		WorkDir: dir, SessionId: "caller-hint-ignored", LogPath: logPath,
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if start.SessionId != "ses_wired0001abcd" {
		t.Errorf("StartRun.SessionId = %q, want the echoed ses_wired0001abcd (not the caller hint)", start.SessionId)
	}
	waitExit(t, srv, start.SessionId)
	hintExit, err := srv.ExitStatus(context.Background(), &bossanovav1.AgentExitStatusRequest{SessionId: "caller-hint-ignored"})
	if err != nil {
		t.Fatalf("ExitStatus caller hint: %v", err)
	}
	if !hintExit.GetIsComplete() || hintExit.GetExitError() != "" {
		t.Fatalf("ExitStatus caller hint = %+v, want clean completed state", hintExit)
	}
}

func TestExitStatusPropagatesAuthInvalidated(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")
	r := NewRunner(zerolog.Nop(), WithCommandFactory(fakeOpencodeShell(t,
		`printf '%s\n' '{"type":"error","sessionID":"ses_x","error":{"statusCode":401,"message":"unauthorized"}}'; exit 1`)))
	srv := &Server{logger: zerolog.Nop(), runner: r}

	start, _ := srv.StartRun(context.Background(), &bossanovav1.StartAgentRunRequest{WorkDir: dir, SessionId: "sid-auth", LogPath: logPath})
	exit := waitExit(t, srv, start.SessionId)

	if exit.GetFailureClass() != agenterr.KindAuthInvalidated.String() {
		t.Errorf("FailureClass = %q, want %q", exit.GetFailureClass(), agenterr.KindAuthInvalidated.String())
	}
	if exit.GetExitError() == "" {
		t.Error("ExitError empty, want the auth-required message")
	}
}

// TestExitStatusPropagatesRateLimited is the opencode-specific branch the codex
// twin lacks: a structured 429 with no usage banner is a retryable rate limit,
// surfaced as KindRateLimited (BOS-406 contract), not usage exhaustion.
func TestExitStatusPropagatesRateLimited(t *testing.T) {
	isolateCaptureDir(t)
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")
	r := NewRunner(zerolog.Nop(), WithCommandFactory(fakeOpencodeShell(t,
		`printf '%s\n' '{"type":"error","sessionID":"ses_x","error":{"statusCode":429,"message":"rate limit exceeded, please retry"}}'; exit 1`)))
	srv := &Server{logger: zerolog.Nop(), runner: r}

	start, _ := srv.StartRun(context.Background(), &bossanovav1.StartAgentRunRequest{WorkDir: dir, SessionId: "sid-rate", LogPath: logPath})
	exit := waitExit(t, srv, start.SessionId)

	if exit.GetFailureClass() != agenterr.KindRateLimited.String() {
		t.Errorf("FailureClass = %q, want %q", exit.GetFailureClass(), agenterr.KindRateLimited.String())
	}
	if exit.GetResetAt() != nil {
		t.Errorf("ResetAt = %v, want unset for a rate limit (no reset time)", exit.GetResetAt())
	}
}

func TestExitStatusPropagatesUsageLimited(t *testing.T) {
	isolateCaptureDir(t)
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")
	r := NewRunner(zerolog.Nop(), WithCommandFactory(fakeOpencodeShell(t,
		`printf '%s\n' '{"type":"error","sessionID":"ses_x","error":{"statusCode":429,"message":"usage_limit_reached, resets at 15:00"}}'; exit 1`)))
	srv := &Server{logger: zerolog.Nop(), runner: r}

	start, _ := srv.StartRun(context.Background(), &bossanovav1.StartAgentRunRequest{WorkDir: dir, SessionId: "sid-usage", LogPath: logPath})
	exit := waitExit(t, srv, start.SessionId)

	if exit.GetFailureClass() != agenterr.KindUsageExhausted.String() {
		t.Errorf("FailureClass = %q, want %q", exit.GetFailureClass(), agenterr.KindUsageExhausted.String())
	}
	if exit.GetResetAt() == nil {
		t.Error("ResetAt unset, want a parsed reset time for a 15:00 usage banner")
	}
}

func TestExitStatusCleanExitLeavesFailureFieldsUnset(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")
	r := NewRunner(zerolog.Nop(), WithCommandFactory(fakeOpencodeShell(t,
		`printf '%s\n' '{"type":"step_finish","sessionID":"ses_x","part":{"type":"step-finish"}}'; exit 0`)))
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

// TestStopRunMissingSessionReturnsNotFound asserts the wired StopRun maps an
// unknown-session stop to codes.NotFound, mirroring the codex twin.
func TestStopRunMissingSessionReturnsNotFound(t *testing.T) {
	s := newTestServer(t)
	_, err := s.StopRun(context.Background(), &bossanovav1.StopAgentRunRequest{SessionId: "never-started"})
	if err == nil {
		t.Fatal("StopRun for unknown session: want error, got nil")
	}
	if got := status.Code(err); got != codes.NotFound {
		t.Errorf("StopRun status code = %v, want NotFound", got)
	}
}

// TestIsRunningReportsRunnerState asserts IsRunning reflects the runner's state;
// with no started runs every id reports false.
func TestIsRunningReportsRunnerState(t *testing.T) {
	s := newTestServer(t)
	resp, err := s.IsRunning(context.Background(), &bossanovav1.IsAgentRunningRequest{SessionId: "anything"})
	if err != nil {
		t.Fatalf("IsRunning: %v", err)
	}
	if resp.Running {
		t.Error("IsRunning = true for unknown session, want false")
	}
}

// TestExitStatusUnknownSessionIsComplete documents the headless polling contract:
// a session the runner never knew reports complete with no error.
func TestExitStatusUnknownSessionIsComplete(t *testing.T) {
	s := newTestServer(t)
	resp, err := s.ExitStatus(context.Background(), &bossanovav1.AgentExitStatusRequest{SessionId: "unknown"})
	if err != nil {
		t.Fatalf("ExitStatus: %v", err)
	}
	if !resp.IsComplete {
		t.Error("IsComplete = false for unknown session, want true")
	}
	if resp.ExitError != "" {
		t.Errorf("ExitError = %q for unknown session, want empty", resp.ExitError)
	}
}
