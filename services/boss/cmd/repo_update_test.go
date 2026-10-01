package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// repoUpdateSubcommand returns a fresh `boss repo update` command from the real
// repoCmd() tree, so the test exercises the registered flag set.
func repoUpdateSubcommand(t *testing.T) *cobra.Command {
	t.Helper()
	for _, sub := range repoCmd().Commands() {
		if sub.Name() == "update" {
			return sub
		}
	}
	t.Fatal("repoCmd() has no update subcommand")
	return nil
}

func TestRepoUpdateWorktreeBaseDirFlag(t *testing.T) {
	t.Parallel()

	t.Run("flag is registered", func(t *testing.T) {
		t.Parallel()
		if repoUpdateSubcommand(t).Flags().Lookup("worktree-base-dir") == nil {
			t.Fatal("boss repo update has no --worktree-base-dir flag")
		}
	})

	t.Run("flag maps onto the request", func(t *testing.T) {
		t.Parallel()
		cmd := repoUpdateSubcommand(t)
		if err := cmd.Flags().Parse([]string{"--worktree-base-dir", "/abs/dir"}); err != nil {
			t.Fatalf("parse flags: %v", err)
		}
		req, err := buildRepoUpdateRequest(cmd, "repo-1")
		if err != nil {
			t.Fatalf("buildRepoUpdateRequest() error = %v", err)
		}
		if req.GetId() != "repo-1" {
			t.Fatalf("Id = %q, want repo-1", req.GetId())
		}
		if req.WorktreeBaseDir == nil || req.GetWorktreeBaseDir() != "/abs/dir" {
			t.Fatalf("WorktreeBaseDir = %v, want /abs/dir", req.WorktreeBaseDir)
		}
	})

	t.Run("unset flag leaves the field nil", func(t *testing.T) {
		t.Parallel()
		cmd := repoUpdateSubcommand(t)
		if err := cmd.Flags().Parse([]string{"--name", "renamed"}); err != nil {
			t.Fatalf("parse flags: %v", err)
		}
		req, err := buildRepoUpdateRequest(cmd, "repo-1")
		if err != nil {
			t.Fatalf("buildRepoUpdateRequest() error = %v", err)
		}
		if req.WorktreeBaseDir != nil {
			t.Fatalf("WorktreeBaseDir = %q, want nil", req.GetWorktreeBaseDir())
		}
	})

	t.Run("no flags still errors and names the new flag", func(t *testing.T) {
		t.Parallel()
		_, err := buildRepoUpdateRequest(repoUpdateSubcommand(t), "repo-1")
		if err == nil {
			t.Fatal("buildRepoUpdateRequest() error = nil, want no-flags error")
		}
		if !strings.Contains(err.Error(), "--worktree-base-dir") {
			t.Fatalf("error = %v, want it to mention --worktree-base-dir", err)
		}
	})
}
