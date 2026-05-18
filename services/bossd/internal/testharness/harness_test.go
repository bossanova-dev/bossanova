package testharness_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/testharness"
)

func TestHarness_BootsCleanly(t *testing.T) {
	h := testharness.New(t)

	repoID := h.SeedRepo(t, "https://github.com/test/repo.git")
	if repoID == "" {
		t.Fatal("expected repo ID")
	}

	sessionID := h.SeedSession(t, repoID, 42, pb.SessionState_SESSION_STATE_AWAITING_CHECKS)
	if sessionID == "" {
		t.Fatal("expected session ID")
	}
}

func TestHarness_WebhookRefreshUpdatesSharedDisplayTracker(t *testing.T) {
	h := testharness.New(t)

	repoOrigin := "https://github.com/test/repo.git"
	repoID := h.SeedRepo(t, repoOrigin)
	sessionID := h.SeedSession(t, repoID, 42, pb.SessionState_SESSION_STATE_AWAITING_CHECKS)

	mergeable := true
	h.Provider.SetPRStatus(42, &vcs.PRStatus{State: vcs.PRStateOpen, Mergeable: &mergeable})
	success := vcs.CheckConclusionSuccess
	h.Provider.SetCheckResults(42, []vcs.CheckResult{{
		ID:         "ci",
		Name:       "ci",
		Status:     vcs.CheckStatusCompleted,
		Conclusion: &success,
	}})

	h.PostGitHubWebhook(t, "pull_request", nil, 42, repoOrigin)

	entry := h.DisplayTracker.Get(sessionID)
	if entry == nil {
		t.Fatal("expected shared display tracker entry")
	}
	if entry.Status != vcs.DisplayStatusPassing {
		t.Fatalf("display status = %v, want %v", entry.Status, vcs.DisplayStatusPassing)
	}
}

func TestHarness_PingViaClient(t *testing.T) {
	h := testharness.New(t)
	repoDir := testharness.TempRepoDir(t)

	// Register a repo via the RPC client.
	resp, err := h.Client.RegisterRepo(context.Background(), connect.NewRequest(&pb.RegisterRepoRequest{
		DisplayName:       "test-repo",
		LocalPath:         repoDir,
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
	repoDir := testharness.TempRepoDir(t)

	// Register a repo first.
	repoResp, err := h.Client.RegisterRepo(context.Background(), connect.NewRequest(&pb.RegisterRepoRequest{
		DisplayName:       "test-repo",
		LocalPath:         repoDir,
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}))
	if err != nil {
		t.Fatalf("register repo: %v", err)
	}
	repoID := repoResp.Msg.Repo.Id

	// Create a session (streaming RPC).
	stream, err := h.Client.CreateSession(context.Background(), connect.NewRequest(&pb.CreateSessionRequest{
		RepoId: repoID,
		Title:  "Fix login bug",
		Plan:   "Fix the login bug in auth.go",
	}))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	defer stream.Close() //nolint:errcheck // test cleanup

	// Read stream messages until we get SessionCreated.
	var sess *pb.Session
	for stream.Receive() {
		msg := stream.Msg()
		if sc := msg.GetSessionCreated(); sc != nil {
			sess = sc.GetSession()
			break
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if sess == nil {
		t.Fatal("expected session in response")
		return
	}
	if sess.Title != "Fix login bug" {
		t.Fatalf("expected title 'Fix login bug', got %q", sess.Title)
	}

	// Verify the mock worktree was created.
	if len(h.Git.CreateCalls) != 1 {
		t.Fatalf("expected 1 worktree create call, got %d", len(h.Git.CreateCalls))
	}

	// Verify the mock Claude process was started.
	if sess.AgentSessionId == nil || *sess.AgentSessionId == "" {
		t.Fatal("expected agent session ID to be set")
	}

	// Session should be in ImplementingPlan state.
	if sess.State != pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN {
		t.Fatalf("expected state IMPLEMENTING_PLAN, got %v", sess.State)
	}
}
