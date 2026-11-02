package server

import (
	"context"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	gitpkg "github.com/recurser/bossd/internal/adapter/git"
	"github.com/recurser/bossd/internal/db"
)

// createTrackerSession drives a CreateSession with a tracker id + explicit
// branch name (no pr_number), the shape a Linear/Sentry dispatch uses. A
// distinct branch keeps sendExistingSessionForBranch from intercepting so the
// BOS-236 tracker dedup guard is what's under test.
func (h *createSessionStreamHarness) createTrackerSession(t *testing.T, title, trackerID, branch string, force bool) error {
	t.Helper()

	agentName := "claude"
	tID := trackerID
	br := branch
	stream, err := h.client.CreateSession(context.Background(), connect.NewRequest(&pb.CreateSessionRequest{
		RepoId:     h.repo.ID,
		Title:      title,
		Plan:       "do work",
		AgentName:  &agentName,
		TrackerId:  &tID,
		BranchName: &br,
		Detach:     true,
		Force:      force,
	}))
	if err != nil {
		return err
	}

	for stream.Receive() {
		_ = stream.Msg()
	}
	return stream.Err()
}

func TestCreateSessionDuplicateActiveTrackerReturnsAlreadyExists(t *testing.T) {
	t.Parallel()

	h := newCreateSessionStreamHarness(t, &setupStreamWorktree{}, &setupStreamAgent{})
	trackerID := "BOS-236"
	existing, err := h.sessions.Create(context.Background(), db.CreateSessionParams{
		RepoID:     h.repo.ID,
		Title:      "first dispatch",
		Plan:       "do work",
		BaseBranch: "main",
		BranchName: "bos-236-first",
		TrackerID:  &trackerID,
		AgentName:  "claude",
	})
	if err != nil {
		t.Fatalf("create existing session: %v", err)
	}

	err = h.createTrackerSession(t, "second dispatch", trackerID, "bos-236-second", false)
	if err == nil {
		t.Fatal("second CreateSession error = nil, want AlreadyExists")
	}
	if got := connect.CodeOf(err); got != connect.CodeAlreadyExists {
		t.Fatalf("Connect code = %v, want %v; err = %v", got, connect.CodeAlreadyExists, err)
	}
	if !strings.Contains(err.Error(), existing.ID) {
		t.Fatalf("error = %q, want to contain existing session id %q", err.Error(), existing.ID)
	}
	requireConflictEqual(t, requireCreateSessionConflict(t, err), &pb.CreateSessionConflict{
		Reason:            pb.CreateSessionConflictReason_CREATE_SESSION_CONFLICT_REASON_DUPLICATE_ACTIVE_SESSION,
		ExistingSessionId: existing.ID,
		MatchedTarget:     "tracker issue",
	})
	sessions, listErr := h.sessions.List(context.Background(), h.repo.ID)
	if listErr != nil {
		t.Fatalf("list sessions: %v", listErr)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions len = %d, want 1 (no new worktree/branch)", len(sessions))
	}
}

func TestCreateSessionTerminalTrackerSessionDoesNotBlock(t *testing.T) {
	t.Parallel()

	h := newCreateSessionStreamHarness(t, &setupStreamWorktree{}, &setupStreamAgent{})
	trackerID := "BOS-236"
	existing, err := h.sessions.Create(context.Background(), db.CreateSessionParams{
		RepoID:     h.repo.ID,
		Title:      "first dispatch",
		Plan:       "do work",
		BaseBranch: "main",
		BranchName: "bos-236-first",
		TrackerID:  &trackerID,
		AgentName:  "claude",
	})
	if err != nil {
		t.Fatalf("create existing session: %v", err)
	}
	blocked := int(machine.Blocked)
	if _, err := h.sessions.Update(context.Background(), existing.ID, db.UpdateSessionParams{State: &blocked}); err != nil {
		t.Fatalf("move existing to blocked: %v", err)
	}

	if err := h.createTrackerSession(t, "second dispatch", trackerID, "bos-236-second", false); err != nil {
		t.Fatalf("second CreateSession error = %v, want nil (terminal session must not block)", err)
	}
	sessions, listErr := h.sessions.List(context.Background(), h.repo.ID)
	if listErr != nil {
		t.Fatalf("list sessions: %v", listErr)
	}
	if len(sessions) != 2 {
		t.Fatalf("sessions len = %d, want 2", len(sessions))
	}
}

func TestCreateSessionForceBypassesTrackerDedup(t *testing.T) {
	t.Parallel()

	h := newCreateSessionStreamHarness(t, &setupStreamWorktree{}, &setupStreamAgent{})
	trackerID := "BOS-236"
	if _, err := h.sessions.Create(context.Background(), db.CreateSessionParams{
		RepoID:     h.repo.ID,
		Title:      "first dispatch",
		Plan:       "do work",
		BaseBranch: "main",
		BranchName: "bos-236-first",
		TrackerID:  &trackerID,
		AgentName:  "claude",
	}); err != nil {
		t.Fatalf("create existing session: %v", err)
	}

	if err := h.createTrackerSession(t, "second dispatch", trackerID, "bos-236-second", true); err != nil {
		t.Fatalf("force CreateSession error = %v, want nil", err)
	}
	sessions, listErr := h.sessions.List(context.Background(), h.repo.ID)
	if listErr != nil {
		t.Fatalf("list sessions: %v", listErr)
	}
	if len(sessions) != 2 {
		t.Fatalf("sessions len = %d, want 2 (force must create a second)", len(sessions))
	}
}

func TestCreateSessionConcurrentTrackerCreatesOneWinner(t *testing.T) {
	t.Parallel()

	h := newCreateSessionStreamHarness(t, &setupStreamWorktree{}, &setupStreamAgent{})
	trackerID := "BOS-236"

	var wg sync.WaitGroup
	errs := make([]error, 2)
	branches := []string{"bos-236-a", "bos-236-b"}
	for i := range 2 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			errs[idx] = h.createTrackerSession(t, "dispatch", trackerID, branches[idx], false)
		}(i)
	}
	wg.Wait()

	var successes, alreadyExists int
	for _, err := range errs {
		switch {
		case err == nil:
			successes++
		case connect.CodeOf(err) == connect.CodeAlreadyExists:
			alreadyExists++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if successes != 1 || alreadyExists != 1 {
		t.Fatalf("got %d successes / %d AlreadyExists, want exactly 1 / 1", successes, alreadyExists)
	}
	sessions, listErr := h.sessions.List(context.Background(), h.repo.ID)
	if listErr != nil {
		t.Fatalf("list sessions: %v", listErr)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions len = %d, want exactly 1 winner", len(sessions))
	}
}

// A create for a branch whose session is still starting (no worktree yet) is
// refused with SESSION_STARTING naming that session, a cause no request flag
// bypasses.
func TestCreateSessionStartingBranchCarriesSessionStartingConflict(t *testing.T) {
	t.Parallel()

	h := newCreateSessionStreamHarness(t, &setupStreamWorktree{}, &setupStreamAgent{})
	starting, err := h.sessions.Create(context.Background(), db.CreateSessionParams{
		RepoID:     h.repo.ID,
		Title:      "still starting",
		Plan:       "do work",
		BaseBranch: "main",
		BranchName: "bos-2143-starting",
		AgentName:  "claude",
	})
	if err != nil {
		t.Fatalf("create starting session: %v", err)
	}

	err = h.createBranchSession(context.Background(), h.repo.ID, "second", "bos-2143-starting")
	requireConflictEqual(t, requireCreateSessionConflict(t, err), &pb.CreateSessionConflict{
		Reason:            pb.CreateSessionConflictReason_CREATE_SESSION_CONFLICT_REASON_SESSION_STARTING,
		ExistingSessionId: starting.ID,
		BranchName:        "bos-2143-starting",
	})
}

// A real local branch collision is refused with BRANCH_EXISTS, the cause
// force_branch bypasses.
func TestCreateSessionBranchCollisionCarriesBranchExistsConflict(t *testing.T) {
	t.Parallel()

	h := newCreateSessionStreamHarness(t, &setupStreamWorktree{createErr: gitpkg.ErrBranchExists}, &setupStreamAgent{})

	err := h.createBranchSession(context.Background(), h.repo.ID, "collides", "bos-2143-existing")
	requireConflictEqual(t, requireCreateSessionConflict(t, err), &pb.CreateSessionConflict{
		Reason:     pb.CreateSessionConflictReason_CREATE_SESSION_CONFLICT_REASON_BRANCH_EXISTS,
		BranchName: "bos-2143-existing",
	})
	if !strings.Contains(err.Error(), "branch already exists for this session title") {
		t.Fatalf("error = %q, want the unchanged branch-exists text", err)
	}
}
