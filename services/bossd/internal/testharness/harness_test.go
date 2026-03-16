package testharness_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/testharness"
)

func TestHarness_PingViaClient(t *testing.T) {
	h := testharness.New(t)

	// Register a repo via the RPC client.
	resp, err := h.Client.RegisterRepo(context.Background(), connect.NewRequest(&pb.RegisterRepoRequest{
		DisplayName:       "test-repo",
		LocalPath:         "/tmp/test-repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}))
	if err != nil {
		t.Fatalf("register repo: %v", err)
	}
	if resp.Msg.Repo == nil {
		t.Fatal("expected repo in response")
	}
	if resp.Msg.Repo.DisplayName != "test-repo" {
		t.Fatalf("expected display name 'test-repo', got %q", resp.Msg.Repo.DisplayName)
	}

	// List repos to verify persistence.
	listResp, err := h.Client.ListRepos(context.Background(), connect.NewRequest(&pb.ListReposRequest{}))
	if err != nil {
		t.Fatalf("list repos: %v", err)
	}
	if len(listResp.Msg.Repos) != 1 {
		t.Fatalf("expected 1 repo, got %d", len(listResp.Msg.Repos))
	}
}

func TestHarness_CreateSession(t *testing.T) {
	h := testharness.New(t)

	// Register a repo first.
	repoResp, err := h.Client.RegisterRepo(context.Background(), connect.NewRequest(&pb.RegisterRepoRequest{
		DisplayName:       "test-repo",
		LocalPath:         "/tmp/test-repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}))
	if err != nil {
		t.Fatalf("register repo: %v", err)
	}
	repoID := repoResp.Msg.Repo.Id

	// Create a session.
	sessResp, err := h.Client.CreateSession(context.Background(), connect.NewRequest(&pb.CreateSessionRequest{
		RepoId: repoID,
		Title:  "Fix login bug",
		Plan:   "Fix the login bug in auth.go",
	}))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	sess := sessResp.Msg.Session
	if sess == nil {
		t.Fatal("expected session in response")
	}
	if sess.Title != "Fix login bug" {
		t.Fatalf("expected title 'Fix login bug', got %q", sess.Title)
	}

	// Verify the mock worktree was created.
	if len(h.Git.CreateCalls) != 1 {
		t.Fatalf("expected 1 worktree create call, got %d", len(h.Git.CreateCalls))
	}

	// Verify the mock Claude process was started.
	if sess.ClaudeSessionId == nil || *sess.ClaudeSessionId == "" {
		t.Fatal("expected Claude session ID to be set")
	}

	// Session should be in ImplementingPlan state.
	if sess.State != pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN {
		t.Fatalf("expected state IMPLEMENTING_PLAN, got %v", sess.State)
	}
}
