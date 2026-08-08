package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/db"
	gitpkg "github.com/recurser/bossd/internal/git"
	"github.com/recurser/bossd/internal/session"
)

// stalledSubscriberLines is comfortably more setup output than one subscriber's
// buffer holds — 4 × session.setupBusSubscriberBuffer, which is 256 and
// unexported. Written against a subscriber that has stopped reading, so on the
// pre-BOS-720 io.Pipe the very first line already blocks forever.
const stalledSubscriberLines = 4 * 256

// Regression A for BOS-720. A client that stops draining, then disconnects, must
// neither block the bootstrap nor destroy it.
//
// On the pre-change code both halves fail. The bootstrap wrote into an io.Pipe
// whose only reader was this client, so the RPC *drove* the bootstrap rather
// than observing it; and StreamCreateSession's error exit ran
// cleanupFailedCreateSession, deleting the row and reaping the branch of a
// bootstrap that was still running fine.
func TestCreateSessionClientDisconnectLeavesTheBootstrapRunning(t *testing.T) {
	t.Parallel()

	const branch = "bos-720-disconnect"

	writesDone := make(chan struct{})
	release := make(chan struct{})
	worktrees := &setupStreamWorktree{}
	worktrees.createFn = func(_ context.Context, opts gitpkg.CreateOpts) (*gitpkg.CreateResult, error) {
		for i := range stalledSubscriberLines {
			if opts.SetupScriptOutput != nil {
				_, _ = io.WriteString(opts.SetupScriptOutput, fmt.Sprintf("setup line %d\n", i))
			}
		}
		close(writesDone)
		<-release
		return &gitpkg.CreateResult{
			WorktreePath: "/tmp/worktrees/repo/" + branch,
			BranchName:   branch,
		}, nil
	}

	h := newCreateSessionStreamHarness(t, worktrees, &setupStreamAgent{})

	agentName := "claude"
	branchName := branch
	disconnect := errors.New("client went away")
	streamDone := make(chan error, 1)
	go func() {
		streamDone <- h.server.StreamCreateSession(context.Background(), &pb.CreateSessionRequest{
			RepoId:     h.repo.ID,
			Title:      "async bootstrap",
			Plan:       "do work",
			AgentName:  &agentName,
			BranchName: &branchName,
			Detach:     true,
			DeferPr:    true,
		}, func(resp *pb.CreateSessionResponse) error {
			if resp.GetSetupOutput() != nil {
				return disconnect
			}
			return nil
		})
	}()

	// 1. The RPC returns the client's own error promptly. On the pre-change code
	//    it cannot: the handler is the io.Pipe's only reader, so it is wedged
	//    inside the write it stopped servicing.
	var streamErr error
	select {
	case streamErr = <-streamDone:
	case <-time.After(5 * time.Second):
		t.Fatal("StreamCreateSession never returned: the client is still driving the bootstrap")
	}
	if !errors.Is(streamErr, disconnect) {
		t.Fatalf("StreamCreateSession error = %v, want the emit error %v", streamErr, disconnect)
	}

	// 2. Every setup-output write completed. A dropped line is the deliberate
	//    trade here; a blocked writer is the defect.
	select {
	case <-writesDone:
	case <-time.After(5 * time.Second):
		t.Fatalf("the bootstrap is still blocked writing %d setup lines to a subscriber that stopped reading", stalledSubscriberLines)
	}

	close(release)

	// 4. The target lock is released — but only once the bootstrap settles, so
	//    this acquire is also how the test joins the background work. Bounded
	//    well under session.TargetStartLockTimeout so a regression reads as a
	//    failure rather than a multi-minute hang.
	lockCtx, cancelLock := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelLock()
	unlock, _, lockErr := session.AcquireTargetStart(lockCtx, h.repo.ID, branch, nil)
	if lockErr != nil {
		t.Fatalf("second AcquireTargetStart for the same target: %v", lockErr)
	}
	defer unlock()

	// 3. The row SURVIVES. This is the behaviour that inverts: the departing
	//    client used to take the session with it.
	sessions, listErr := h.sessions.List(context.Background(), h.repo.ID)
	if listErr != nil {
		t.Fatalf("list sessions: %v", listErr)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions len = %d, want 1 (a disconnect must not delete a healthy session)", len(sessions))
	}
	reaped, purged := worktrees.cleanupRecords()
	if len(reaped) != 0 || len(purged) != 0 {
		t.Fatalf("cleanup ran on a successful bootstrap: reaped=%v purged=%v", reaped, purged)
	}
}

// Regression B for BOS-720. A bootstrap that fails AFTER the client is gone
// still cleans up after itself, and does so BEFORE the per-target lock is
// released — so an overlapping create for the same target sees a settled world
// rather than a half-started row about to be deleted.
//
// On the pre-change code the cleanup is the departing RPC's job, running on its
// own cancelled context; once the bootstrap is decoupled from the RPC there is
// nobody left to do it but the runner.
func TestCreateSessionFailedBootstrapCleansUpAfterTheClientIsGone(t *testing.T) {
	t.Parallel()

	const branch = "bos-720-failed-bootstrap"
	bootstrapErr := errors.New("setup script exploded")

	release := make(chan struct{})
	worktrees := &setupStreamWorktree{}
	worktrees.createFn = func(ctx context.Context, opts gitpkg.CreateOpts) (*gitpkg.CreateResult, error) {
		if opts.SetupScriptOutput != nil {
			_, _ = io.WriteString(opts.SetupScriptOutput, "setup starting\n")
		}
		if opts.OnWorktreeReady != nil {
			opts.OnWorktreeReady(ctx, branch, "/tmp/worktrees/repo/"+branch)
		}
		<-release
		return nil, bootstrapErr
	}

	h := newCreateSessionStreamHarness(t, worktrees, &setupStreamAgent{})
	deleted := make(chan string, 4)
	h.server.onSessionDeleted = func(_ context.Context, id string) { deleted <- id }

	agentName := "claude"
	branchName := branch
	disconnect := errors.New("client went away")
	streamDone := make(chan error, 1)
	go func() {
		streamDone <- h.server.StreamCreateSession(context.Background(), &pb.CreateSessionRequest{
			RepoId:     h.repo.ID,
			Title:      "failing bootstrap",
			Plan:       "do work",
			AgentName:  &agentName,
			BranchName: &branchName,
			Detach:     true,
			DeferPr:    true,
		}, func(resp *pb.CreateSessionResponse) error {
			if resp.GetSetupOutput() != nil {
				return disconnect
			}
			return nil
		})
	}()

	select {
	case err := <-streamDone:
		if !errors.Is(err, disconnect) {
			t.Fatalf("StreamCreateSession error = %v, want the emit error %v", err, disconnect)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("StreamCreateSession never returned: the client is still driving the bootstrap")
	}

	// An overlapping create for the same target. It must not proceed until this
	// create's cleanup has finished, so what it observes at the instant it wins
	// the lock is the ordering proof: an empty session list means the delete was
	// already durable.
	type overlap struct {
		sessionsAtAcquire int
		err               error
	}
	overlapped := make(chan overlap, 1)
	acquiring := make(chan struct{})
	go func() {
		lockCtx, cancelLock := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancelLock()
		close(acquiring)
		unlock, _, lockErr := session.AcquireTargetStart(lockCtx, h.repo.ID, branch, nil)
		if lockErr != nil {
			overlapped <- overlap{err: lockErr}
			return
		}
		defer unlock()
		rows, listErr := h.sessions.List(context.Background(), h.repo.ID)
		overlapped <- overlap{sessionsAtAcquire: len(rows), err: listErr}
	}()

	// It is genuinely blocked: the first create still holds the target lock
	// across its wedged bootstrap. The window starts only once the goroutine
	// says it is about to acquire — timing it from here instead would pass
	// vacuously under load, when the goroutine had not yet reached the call.
	<-acquiring
	select {
	case got := <-overlapped:
		t.Fatalf("overlapping create acquired the target lock while the first bootstrap was still running: %+v", got)
	case <-time.After(250 * time.Millisecond):
	}

	close(release)

	var got overlap
	select {
	case got = <-overlapped:
	case <-time.After(30 * time.Second):
		t.Fatal("overlapping create never acquired the target lock: the runner did not release it")
	}
	if got.err != nil {
		t.Fatalf("overlapping create: %v", got.err)
	}
	// 2. Cleanup ran BEFORE the release.
	if got.sessionsAtAcquire != 0 {
		t.Fatalf("sessions at the overlapping acquire = %d, want 0 (cleanup must complete before the target lock is released)", got.sessionsAtAcquire)
	}

	// 1. Cleanup really was cleanupFailedCreateSession: the row is gone and the
	//    deletion was published, so bosso drops it from the read model.
	sessions, listErr := h.sessions.List(context.Background(), h.repo.ID)
	if listErr != nil {
		t.Fatalf("list sessions: %v", listErr)
	}
	if len(sessions) != 0 {
		t.Fatalf("sessions len = %d, want 0 (a failed bootstrap cleans up after itself)", len(sessions))
	}
	select {
	case <-deleted:
	default:
		t.Fatal("onSessionDeleted never fired for the failed bootstrap")
	}
}

// AC1/AC2 for BOS-720: the client holds a usable session id BEFORE the
// bootstrap finishes, and a second frame carries the settled session.
//
// The worktree create blocks until the test releases it, so every assertion in
// the first half runs while the bootstrap is provably still in flight.
func TestCreateSessionEmitsAnAcceptedFrameBeforeTheBootstrapCompletes(t *testing.T) {
	t.Parallel()

	const branch = "bos-720-accepted-frame"

	started := make(chan struct{})
	release := make(chan struct{})
	worktrees := &setupStreamWorktree{}
	worktrees.createFn = func(_ context.Context, _ gitpkg.CreateOpts) (*gitpkg.CreateResult, error) {
		close(started)
		<-release
		return &gitpkg.CreateResult{
			WorktreePath: "/tmp/worktrees/repo/" + branch,
			BranchName:   branch,
		}, nil
	}

	h := newCreateSessionStreamHarness(t, worktrees, &setupStreamAgent{})

	agentName := "claude"
	branchName := branch
	frames := make(chan *pb.SessionCreated, 8)
	streamDone := make(chan error, 1)
	go func() {
		streamDone <- h.server.StreamCreateSession(context.Background(), &pb.CreateSessionRequest{
			RepoId:     h.repo.ID,
			Title:      "accepted early",
			Plan:       "do work",
			AgentName:  &agentName,
			BranchName: &branchName,
			Detach:     true,
			DeferPr:    true,
		}, func(resp *pb.CreateSessionResponse) error {
			if sc := resp.GetSessionCreated(); sc != nil {
				frames <- sc
			}
			return nil
		})
	}()

	// The bootstrap is running and wedged inside the worktree create.
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the bootstrap never started")
	}

	var accepted *pb.SessionCreated
	select {
	case accepted = <-frames:
	case <-time.After(2 * time.Second):
		t.Fatal("no SessionCreated frame arrived while the bootstrap was still running: the create still blocks until it settles")
	}
	if accepted.GetSession().GetId() == "" {
		t.Fatal("accepted frame session id is empty; the client cannot address the session")
	}
	if got := accepted.GetSession().GetState(); got != pb.SessionState_SESSION_STATE_CREATING_WORKTREE {
		t.Fatalf("accepted frame state = %v, want SESSION_STATE_CREATING_WORKTREE", got)
	}
	if accepted.GetAttachedExisting() {
		t.Fatal("accepted frame AttachedExisting = true, want false on a genuine create")
	}

	close(release)

	var settled *pb.SessionCreated
	select {
	case settled = <-frames:
	case <-time.After(20 * time.Second):
		t.Fatal("no settled SessionCreated frame arrived after the bootstrap completed")
	}
	if got, want := settled.GetSession().GetId(), accepted.GetSession().GetId(); got != want {
		t.Fatalf("settled frame session id = %q, want the accepted frame's %q", got, want)
	}
	// The post-bootstrap field the accepted frame could not carry — proof this
	// is the settled frame and not a replay of the first.
	if settled.GetSession().GetWorktreePath() == "" {
		t.Fatal("settled frame worktree path is empty; it is not the post-bootstrap session")
	}
	if accepted.GetSession().GetWorktreePath() != "" {
		t.Fatalf("accepted frame worktree path = %q, want empty (the worktree does not exist yet)", accepted.GetSession().GetWorktreePath())
	}

	select {
	case err := <-streamDone:
		if err != nil {
			t.Fatalf("StreamCreateSession: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("StreamCreateSession never returned")
	}
}

// AC3: the attach short-circuit must stay single-frame. sendExistingSessionForBranch
// returns before any bootstrap is started, so emitting an accepted frame there
// too would tell the client a bootstrap is running when none is.
func TestCreateSessionAttachToExistingStillEmitsExactlyOneFrame(t *testing.T) {
	t.Parallel()

	h := newCreateSessionStreamHarness(t, &setupStreamWorktree{}, &setupStreamAgent{})

	branch := "bos-720-attach"
	if _, err := h.sessions.Create(context.Background(), db.CreateSessionParams{
		RepoID:       h.repo.ID,
		Title:        "existing",
		Plan:         "original plan",
		BaseBranch:   "main",
		BranchName:   branch,
		WorktreePath: t.TempDir(),
		AgentName:    "claude",
	}); err != nil {
		t.Fatalf("seed existing session: %v", err)
	}

	var created []*pb.SessionCreated
	if err := h.server.StreamCreateSession(context.Background(), &pb.CreateSessionRequest{
		RepoId:     h.repo.ID,
		Title:      "attach",
		Plan:       "do work",
		BranchName: &branch,
	}, func(resp *pb.CreateSessionResponse) error {
		if sc := resp.GetSessionCreated(); sc != nil {
			created = append(created, sc)
		}
		return nil
	}); err != nil {
		t.Fatalf("StreamCreateSession: %v", err)
	}

	if len(created) != 1 {
		t.Fatalf("SessionCreated frames on the attach path = %d, want exactly 1", len(created))
	}
	if !created[0].GetAttachedExisting() {
		t.Fatal("AttachedExisting = false, want true on the attach path")
	}
}

// AC3: quick chats skip the worktree/branch/PR bootstrap entirely and never go
// through the runner, so they keep the single terminal frame.
func TestCreateSessionQuickChatStillEmitsExactlyOneFrame(t *testing.T) {
	t.Parallel()

	h := newCreateSessionStreamHarness(t, &setupStreamWorktree{}, &setupStreamAgent{})

	events, err := h.createIsQuickChat(t, "quick chat")
	if err != nil {
		t.Fatalf("CreateSession stream error = %v", err)
	}
	var created int
	for _, e := range events {
		if e.GetSessionCreated() != nil {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("SessionCreated frames on the quick-chat path = %d, want exactly 1", created)
	}
}
