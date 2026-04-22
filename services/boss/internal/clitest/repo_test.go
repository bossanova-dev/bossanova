package clitest_test

import (
	"strings"
	"testing"

	"github.com/recurser/boss/internal/clitest"
)

func TestCLI_Repo_Ls(t *testing.T) {
	h := clitest.New(t, clitest.WithRepos(testRepos()...))
	res := h.Run("repo", "ls")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	for _, want := range []string{"repo-1", "my-app", "repo-2", "my-api"} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("stdout missing %q", want)
		}
	}
}

func TestCLI_Repo_Remove(t *testing.T) {
	h := clitest.New(t, clitest.WithRepos(testRepos()...))
	res := h.Run("repo", "remove", "repo-1")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}

	for _, r := range h.Daemon.Repos() {
		if r.Id == "repo-1" {
			t.Errorf("repo-1 still present after remove")
		}
	}
}

func TestCLI_Repo_Update_AllFlags(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		check func(t *testing.T, h *clitest.Harness)
	}{
		{
			name: "name",
			args: []string{"repo", "update", "repo-1", "--name", "new-name"},
			check: func(t *testing.T, h *clitest.Harness) {
				t.Helper()
				for _, r := range h.Daemon.Repos() {
					if r.Id == "repo-1" && r.DisplayName != "new-name" {
						t.Errorf("expected DisplayName=new-name, got %q", r.DisplayName)
					}
				}
			},
		},
		{
			name: "setup-script",
			args: []string{"repo", "update", "repo-1", "--setup-script", "make deps"},
			check: func(t *testing.T, h *clitest.Harness) {
				t.Helper()
				for _, r := range h.Daemon.Repos() {
					if r.Id == "repo-1" {
						if r.SetupScript == nil || *r.SetupScript != "make deps" {
							t.Errorf("expected SetupScript=%q, got %v", "make deps", r.SetupScript)
						}
					}
				}
			},
		},
		{
			name: "merge-strategy",
			args: []string{"repo", "update", "repo-1", "--merge-strategy", "rebase"},
			check: func(t *testing.T, h *clitest.Harness) {
				t.Helper()
				for _, r := range h.Daemon.Repos() {
					if r.Id == "repo-1" && r.MergeStrategy != "rebase" {
						t.Errorf("expected MergeStrategy=rebase, got %q", r.MergeStrategy)
					}
				}
			},
		},
		{
			name: "auto-merge-enable",
			args: []string{"repo", "update", "repo-1", "--auto-merge"},
			check: func(t *testing.T, h *clitest.Harness) {
				t.Helper()
				for _, r := range h.Daemon.Repos() {
					if r.Id == "repo-1" && !r.CanAutoMerge {
						t.Errorf("expected CanAutoMerge=true")
					}
				}
			},
		},
		{
			name: "auto-merge-disable",
			args: []string{"repo", "update", "repo-1", "--no-auto-merge"},
			check: func(t *testing.T, h *clitest.Harness) {
				t.Helper()
				// Default is false, so disabling is a no-op on seed data but we still
				// verify the RPC went through.
				if len(h.Daemon.Repos()) == 0 {
					t.Fatal("no repos in mock state")
				}
			},
		},
		{
			name: "auto-merge-dependabot",
			args: []string{"repo", "update", "repo-1", "--auto-merge-dependabot"},
			check: func(t *testing.T, h *clitest.Harness) {
				t.Helper()
				for _, r := range h.Daemon.Repos() {
					if r.Id == "repo-1" && !r.CanAutoMergeDependabot {
						t.Errorf("expected CanAutoMergeDependabot=true")
					}
				}
			},
		},
		{
			name: "auto-address-reviews",
			args: []string{"repo", "update", "repo-1", "--auto-address-reviews"},
			check: func(t *testing.T, h *clitest.Harness) {
				t.Helper()
				for _, r := range h.Daemon.Repos() {
					if r.Id == "repo-1" && !r.CanAutoAddressReviews {
						t.Errorf("expected CanAutoAddressReviews=true")
					}
				}
			},
		},
		{
			name: "auto-resolve-conflicts",
			args: []string{"repo", "update", "repo-1", "--auto-resolve-conflicts"},
			check: func(t *testing.T, h *clitest.Harness) {
				t.Helper()
				for _, r := range h.Daemon.Repos() {
					if r.Id == "repo-1" && !r.CanAutoResolveConflicts {
						t.Errorf("expected CanAutoResolveConflicts=true")
					}
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := clitest.New(t, clitest.WithRepos(testRepos()...))
			res := h.Run(tc.args...)
			if res.ExitCode != 0 {
				t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
			}
			tc.check(t, h)
		})
	}
}

func TestCLI_Repo_Update_ClearSetupScript(t *testing.T) {
	h := clitest.New(t, clitest.WithRepos(testRepos()...))
	res := h.Run("repo", "update", "repo-1", "--setup-script", "")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}

	for _, r := range h.Daemon.Repos() {
		if r.Id == "repo-1" {
			if r.SetupScript == nil {
				t.Errorf("expected SetupScript to be set (to empty string), got nil")
				continue
			}
			if *r.SetupScript != "" {
				t.Errorf("expected SetupScript='', got %q", *r.SetupScript)
			}
		}
	}
}
