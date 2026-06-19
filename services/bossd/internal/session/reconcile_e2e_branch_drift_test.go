package session_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/vcs"
	bossgit "github.com/recurser/bossd/internal/git"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/testharness"
	"github.com/rs/zerolog"
)

func TestE2E_Reconcile_MatchesDriftedWorktreeBranch(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()

	repoID, _ := reconcileRegisterRepo(t, h, ctx, "reconcile-branch-drift")
	originURL := "https://github.com/recurser/reconcile-branch-drift.git"
	if _, err := h.DB.ExecContext(ctx, `UPDATE repos SET origin_url = ? WHERE id = ?`, originURL, repoID); err != nil {
		t.Fatalf("set repo origin URL: %v", err)
	}
	repoDir := reconcileBranchDriftInitRepo(t)
	mgr := bossgit.NewManager(zerolog.Nop())

	result, err := mgr.Create(ctx, bossgit.CreateOpts{
		RepoPath:        repoDir,
		BaseBranch:      "main",
		WorktreeBaseDir: filepath.Join(t.TempDir(), "worktrees"),
		RepoName:        "reconcile-branch-drift",
		Title:           "original worktree branch",
	})
	if err != nil {
		t.Fatalf("Create worktree: %v", err)
	}
	originalBranch := result.BranchName
	driftedBranch := "dave/won-1208-foo"
	reconcileBranchDriftGit(t, result.WorktreePath, "checkout", "-b", driftedBranch)

	sessionID, _ := h.SeedSessionInState(t, ctx, repoID,
		pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN, "stale branch session", "p")
	if _, err := h.DB.ExecContext(ctx, `
		UPDATE sessions
		SET branch_name = ?, worktree_path = ?, pr_number = NULL, pr_url = NULL
		WHERE id = ?`,
		originalBranch, result.WorktreePath, sessionID,
	); err != nil {
		t.Fatalf("seed drifted session row: %v", err)
	}

	h.VCS.OpenPRs = []vcs.PRSummary{{
		Number:     354,
		HeadBranch: driftedBranch,
		Title:      "[WON-1208] Fix the thing",
		State:      vcs.PRStateOpen,
	}}

	n, err := session.NewPRAssociationResolver(h.Sessions, h.Repos, h.VCS, zerolog.Nop()).
		WithBranchResolver(mgr).
		Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 reconciled session, got %d", n)
	}

	got, err := h.Client.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: sessionID}))
	if err != nil {
		t.Fatalf("get session post: %v", err)
	}
	if got.Msg.Session.PrNumber == nil || *got.Msg.Session.PrNumber != 354 {
		t.Fatalf("expected PR 354 attached, got %v", got.Msg.Session.PrNumber)
	}
	if got.Msg.Session.BranchName != driftedBranch {
		t.Fatalf("branch_name = %q, want %q", got.Msg.Session.BranchName, driftedBranch)
	}
	if got.Msg.Session.Title != "[WON-1208] Fix the thing" {
		t.Fatalf("title = %q, want %q", got.Msg.Session.Title, "[WON-1208] Fix the thing")
	}
	wantURL := "https://github.com/recurser/reconcile-branch-drift/pull/354"
	if got.Msg.Session.PrUrl == nil || *got.Msg.Session.PrUrl != wantURL {
		t.Fatalf("pr_url = %v, want %q", got.Msg.Session.PrUrl, wantURL)
	}
}

func reconcileBranchDriftInitRepo(t *testing.T) string {
	t.Helper()

	bareDir := t.TempDir()
	reconcileBranchDriftGit(t, bareDir, "init", "--bare", "-b", "main")

	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
		{"commit", "--allow-empty", "-m", "init"},
		{"remote", "add", "origin", bareDir},
		{"push", "-u", "origin", "main"},
	} {
		reconcileBranchDriftGit(t, dir, args...)
	}
	return dir
}

func reconcileBranchDriftGit(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}
