package testharness_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/testharness"
)

func TestE2E_CreateSessionOnNamedBaseBranch_Sessions_AC261401(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	repo, err := h.Client.RegisterRepo(ctx, connect.NewRequest(&pb.RegisterRepoRequest{
		DisplayName: "base branch", LocalPath: testharness.TempRepoDir(t), DefaultBaseBranch: "main", WorktreeBaseDir: t.TempDir(),
	}))
	if err != nil {
		t.Fatal(err)
	}
	h.Git.RemoteBranches = map[string]bool{"integration": true}
	sess := createSessionFromStream(t, h, ctx, &pb.CreateSessionRequest{
		RepoId: repo.Msg.Repo.Id, Title: "named base", Plan: "work", BaseBranch: "integration", Detach: true,
	})
	if sess.BaseBranch != "integration" {
		t.Fatalf("stored base = %q, want integration", sess.BaseBranch)
	}
	if len(h.Git.CreateCalls) != 1 || h.Git.CreateCalls[0].BaseBranch != "integration" {
		t.Fatalf("worktree create = %+v", h.Git.CreateCalls)
	}
	if len(h.VCS.CreateDraftPRCalls) != 1 || h.VCS.CreateDraftPRCalls[0].BaseBranch != "integration" {
		t.Fatalf("draft PR = %+v", h.VCS.CreateDraftPRCalls)
	}
	if len(h.Git.RemoteBranchExistsCalls) != 1 || h.Git.RemoteBranchExistsCalls[0].Branch != "integration" {
		t.Fatalf("origin probes = %+v", h.Git.RemoteBranchExistsCalls)
	}
	stream, err := h.Client.CreateSession(ctx, connect.NewRequest(&pb.CreateSessionRequest{
		RepoId: repo.Msg.Repo.Id, Title: "misspelled base", Plan: "work", BaseBranch: "integrtion", Detach: true,
	}))
	if err == nil {
		for stream.Receive() {
		}
		err = stream.Err()
		_ = stream.Close()
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("missing base error = %v, want InvalidArgument", err)
	}
	rows, err := h.Sessions.List(ctx, repo.Msg.Repo.Id)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || len(h.Git.CreateCalls) != 1 {
		t.Fatalf("rejected create left sessions=%d, worktrees=%d", len(rows), len(h.Git.CreateCalls))
	}
}
