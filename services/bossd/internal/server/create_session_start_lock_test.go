package server

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/db"
	gitpkg "github.com/recurser/bossd/internal/git"
)

// createBranchSession drives a CreateSession with an explicit branch name and no
// PR, the shape that routes through WorktreeManager.Create (so the blocking
// createFn hook below is reached). repoID is explicit so one harness can drive
// creates against two different repos.
func (h *createSessionStreamHarness) createBranchSession(ctx context.Context, repoID, title, branch string) error {
	agentName := "claude"
	br := branch
	stream, err := h.client.CreateSession(ctx, connect.NewRequest(&pb.CreateSessionRequest{
		RepoId:     repoID,
		Title:      title,
		Plan:       "do work",
		AgentName:  &agentName,
		BranchName: &br,
		Detach:     true,
	}))
	if err != nil {
		return err
	}
	for stream.Receive() {
		_ = stream.Msg()
	}
	return stream.Err()
}

// TestCreateSessionHungBootstrapDoesNotBlockOtherRepo is regression test A for
// BOS-717 (acceptance criterion 1).
//
// One session's bootstrap is deliberately wedged inside WorktreeManager.Create —
// the exact shape of the 2026-08-06 outage, where Create() blocked forever after
// the setup script. A second CreateSession for a DIFFERENT repo must still
// complete.
//
// This fails on pre-BOS-717 code: session.LockStartPath was a single
// process-global, untimed mutex acquired before sessions.Create and released
// only after StartSession returned, so the wedged bootstrap held it forever and
// the second create hung with no log line and no session row until the daemon
// was restarted. Here it surfaces as a DeadlineExceeded on the second create.
func TestCreateSessionHungBootstrapDoesNotBlockOtherRepo(t *testing.T) {
	t.Parallel()

	const wedgedRepoPath = "/tmp/repo-wedged"

	release := make(chan struct{})
	entered := make(chan struct{})
	worktrees := &setupStreamWorktree{}
	worktrees.createFn = func(ctx context.Context, opts gitpkg.CreateOpts) (*gitpkg.CreateResult, error) {
		if opts.RepoPath != wedgedRepoPath {
			return &gitpkg.CreateResult{WorktreePath: "/tmp/worktrees/repo/branch", BranchName: opts.BranchName}, nil
		}
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return &gitpkg.CreateResult{WorktreePath: "/tmp/worktrees/wedged/branch", BranchName: opts.BranchName}, nil
	}

	h := newCreateSessionStreamHarness(t, worktrees, &setupStreamAgent{})

	wedgedRepo, err := h.repos.Create(context.Background(), db.CreateRepoParams{
		DisplayName:       "wedged",
		LocalPath:         wedgedRepoPath,
		OriginURL:         "https://github.com/org/wedged",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	})
	if err != nil {
		t.Fatalf("create wedged repo: %v", err)
	}

	wedgedDone := make(chan error, 1)
	wedgedCtx, cancelWedged := context.WithCancel(context.Background())
	go func() {
		wedgedDone <- h.createBranchSession(wedgedCtx, wedgedRepo.ID, "wedged session", "bos-717-wedged")
	}()
	// Unwedge before the harness drains background work, and join so the RPC is
	// finished before the test DB closes.
	t.Cleanup(func() {
		close(release)
		cancelWedged()
		select {
		case <-wedgedDone:
		case <-time.After(30 * time.Second):
			t.Error("wedged CreateSession never returned after release")
		}
	})

	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("wedged bootstrap never entered WorktreeManager.Create")
	}

	// The wedged bootstrap now holds whatever the create path serializes on. A
	// create for a different repo must not wait on it.
	otherCtx, cancelOther := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelOther()
	if err := h.createBranchSession(otherCtx, h.repo.ID, "unblocked session", "bos-717-other"); err != nil {
		t.Fatalf("CreateSession for a second repo failed while another repo's bootstrap was wedged: %v", err)
	}

	sessions, err := h.sessions.List(context.Background(), h.repo.ID)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions for unblocked repo = %d, want 1", len(sessions))
	}
}
