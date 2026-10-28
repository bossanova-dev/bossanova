package repo

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/adapter/cloudrpc"

	"github.com/spf13/cobra"

	daemonrpc "github.com/recurser/boss/internal/adapter/daemonrpc"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// repoUpdateSubcommand returns a fresh `boss repo update` command from the real
// newTestGroup().RepoCommand() tree, so the test exercises the registered flag set.
func repoUpdateSubcommand(t *testing.T) *cobra.Command {
	t.Helper()
	for _, sub := range newTestGroup().RepoCommand().Commands() {
		if sub.Name() == "update" {
			return sub
		}
	}
	t.Fatal("newTestGroup().RepoCommand() has no update subcommand")
	return nil
}

func TestRepoUpdateWorktreeBaseDirFlag_Repos_AC6(t *testing.T) {
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
		if !strings.Contains(err.Error(), "boss repo show repo-1") {
			t.Fatalf("error = %v, want it to point at boss repo show repo-1", err)
		}
	})
}

// TestRepoUpdateTrustedFlag pins `boss repo update --trusted=<bool>` (BOS-1562):
// both values map onto the request, and an unset flag leaves trust alone.
func TestRepoUpdateTrustedFlag(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		args []string
		want *bool
	}{
		{args: []string{"--trusted"}, want: ptrBool(true)},
		{args: []string{"--trusted=true"}, want: ptrBool(true)},
		{args: []string{"--trusted=false"}, want: ptrBool(false)},
		{args: []string{"--name", "renamed"}, want: nil},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			t.Parallel()
			cmd := repoUpdateSubcommand(t)
			if err := cmd.Flags().Parse(tc.args); err != nil {
				t.Fatalf("parse flags: %v", err)
			}
			req, err := buildRepoUpdateRequest(cmd, "repo-1")
			if err != nil {
				t.Fatalf("buildRepoUpdateRequest() error = %v", err)
			}
			switch {
			case tc.want == nil && req.IsTrusted != nil:
				t.Fatalf("IsTrusted = %v, want unset", *req.IsTrusted)
			case tc.want != nil && (req.IsTrusted == nil || *req.IsTrusted != *tc.want):
				t.Fatalf("IsTrusted = %v, want %v", req.IsTrusted, *tc.want)
			}
		})
	}
}

func ptrBool(v bool) *bool { return &v }

// TestPrintUpdatedRepoOmitsTrustForRemoteClient: the cloud proxy carries no
// repo trust, so a remote update's repo always decodes as untrusted. Printing
// "Trusted: false" for it would misreport a trusted repo; a local update,
// which does see trust, still prints it.
func TestPrintUpdatedRepoOmitsTrustForRemoteClient(t *testing.T) {
	t.Parallel()
	repo := &pb.Repo{Id: "repo-1", DisplayName: "r", IsTrusted: true}

	var remote bytes.Buffer
	printUpdatedRepo(&remote, cloudrpc.NewRemote("https://bosso.invalid", "token"), repo)
	if strings.Contains(remote.String(), "Trusted") {
		t.Fatalf("remote repo update printed a trust line it cannot see:\n%s", remote.String())
	}

	var local bytes.Buffer
	printUpdatedRepo(&local, daemonrpc.NewLocalWithToken(filepath.Join(t.TempDir(), "bossd.sock"), ""), repo)
	if !strings.Contains(local.String(), "Trusted:                true") {
		t.Fatalf("local repo update did not print its trust:\n%s", local.String())
	}
}
