//go:build integration

package server

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/tmux"
)

// requireTmux skips the test cleanly when the tmux binary is not on PATH —
// CI without tmux installed should not fail this suite, just skip.
func requireTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
}

// buildStubClaude builds the testdata stub-claude binary and returns the
// absolute path of the resulting binary. The output lives in t.TempDir() so
// each test gets its own copy and cleanup is automatic.
func buildStubClaude(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "stub-claude")
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Dir = "testdata/stub-claude"
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build stub-claude: %v: %s", err, b)
	}
	return out
}

// promoteToClaude renames the stub binary to "claude" inside its parent
// directory and prepends that directory to PATH so the daemon's tmux exec
// of "claude" picks up the stub. t.Setenv automatically restores PATH at
// test cleanup.
func promoteToClaude(t *testing.T, stub string) {
	t.Helper()
	claudePath := filepath.Join(filepath.Dir(stub), "claude")
	if err := os.Rename(stub, claudePath); err != nil {
		t.Fatalf("rename stub: %v", err)
	}
	t.Setenv("PATH", filepath.Dir(claudePath)+":"+os.Getenv("PATH"))
}

// pathToProjectKey mirrors Claude Code's project-directory encoding (both
// "/" and "." become "-"). Duplicated here rather than imported from
// internal/status to keep this integration test honest about the on-disk
// layout it depends on.
func pathToProjectKey(path string) string {
	return strings.NewReplacer("/", "-", ".", "-").Replace(path)
}

// TestWakeChatIntegration_FreshFallback_NoTranscript exercises the
// fresh-fallback branch end-to-end against a real tmux server and the
// stub-claude binary. With no transcript on disk, WakeChat must spawn a
// tmux session running `claude --session-id <id>`.
func TestWakeChatIntegration_FreshFallback_NoTranscript(t *testing.T) {
	requireTmux(t)
	stub := buildStubClaude(t)

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	promoteToClaude(t, stub)
	// Keep the stub alive long enough that HasSession can observe the
	// tmux session before the stub exits and tmux tears it down.
	t.Setenv("STUB_CLAUDE_TICK_MS", "30000")

	wd := t.TempDir()
	chat := &models.AgentChat{ID: "c1", SessionID: "s1", AgentSessionID: "agent-fresh"}
	sess := &models.Session{ID: "s1", RepoID: "r123", WorktreePath: wd}
	tmuxClient := tmux.NewClient()
	s := &Server{
		agentChats: &chatStoreFake{chat: chat},
		sessions:   &sessionStoreFake{sess: sess},
		tmux:       tmuxClient,
	}
	tmuxName := tmux.ChatSessionName("r123", "agent-fresh")
	t.Cleanup(func() {
		_ = tmuxClient.KillSession(context.Background(), tmuxName)
	})

	resp, err := s.WakeChat(context.Background(), connect.NewRequest(&pb.WakeChatRequest{AgentSessionId: "agent-fresh"}))
	if err != nil {
		t.Fatalf("WakeChat: %v", err)
	}
	if resp.Msg.Outcome != pb.WakeChatResponse_OUTCOME_FRESH_FALLBACK {
		t.Fatalf("got %v, want FRESH_FALLBACK", resp.Msg.Outcome)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if tmuxClient.HasSession(context.Background(), tmuxName) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !tmuxClient.HasSession(context.Background(), tmuxName) {
		t.Fatalf("tmux session %q never appeared", tmuxName)
	}
}

// TestWakeChatIntegration_ResumedWhenTranscriptExists pre-creates a
// transcript file at the path the live oracle (status.TranscriptExists)
// inspects and verifies that WakeChat reports OUTCOME_RESUMED.
func TestWakeChatIntegration_ResumedWhenTranscriptExists(t *testing.T) {
	requireTmux(t)
	stub := buildStubClaude(t)
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	promoteToClaude(t, stub)
	t.Setenv("STUB_CLAUDE_TICK_MS", "30000")

	wd := t.TempDir()
	// Pre-create the transcript so the pre-flight stat returns true.
	projectDir := filepath.Join(tmpHome, ".claude", "projects", pathToProjectKey(wd))
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "agent-resume.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	chat := &models.AgentChat{ID: "c1", SessionID: "s1", AgentSessionID: "agent-resume"}
	sess := &models.Session{ID: "s1", RepoID: "r123", WorktreePath: wd}
	tmuxClient := tmux.NewClient()
	s := &Server{
		agentChats: &chatStoreFake{chat: chat},
		sessions:   &sessionStoreFake{sess: sess},
		tmux:       tmuxClient,
	}
	tmuxName := tmux.ChatSessionName("r123", "agent-resume")
	t.Cleanup(func() {
		_ = tmuxClient.KillSession(context.Background(), tmuxName)
	})

	resp, err := s.WakeChat(context.Background(), connect.NewRequest(&pb.WakeChatRequest{AgentSessionId: "agent-resume"}))
	if err != nil {
		t.Fatalf("WakeChat: %v", err)
	}
	if resp.Msg.Outcome != pb.WakeChatResponse_OUTCOME_RESUMED {
		t.Fatalf("got %v, want RESUMED", resp.Msg.Outcome)
	}
}
