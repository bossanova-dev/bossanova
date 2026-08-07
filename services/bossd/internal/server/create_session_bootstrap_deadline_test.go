package server

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	gitpkg "github.com/recurser/bossd/internal/git"
)

// TestCreateSessionBootstrapDeadlineFailsAndCleansUp is regression test B for
// BOS-717 (acceptance criterion 2).
//
// The bootstrap is wedged inside WorktreeManager.Create — the shape of a setup
// script that never returns. The bootstrap deadline must fire, fail the session
// with a deadline error, and leave nothing behind: no row stuck in
// CreatingWorktree, no worktree, no branch.
//
// This fails on pre-BOS-717 code, where StartSession ran under the caller's
// context with no deadline of its own: the create simply never returned, so the
// "bootstrap returned within the deadline" assertion below never fires and every
// cleanup assertion after it is unreachable.
func TestCreateSessionBootstrapDeadlineFailsAndCleansUp(t *testing.T) {
	t.Parallel()

	const bootstrapTimeout = 300 * time.Millisecond

	escape := make(chan struct{})
	worktrees := &setupStreamWorktree{}
	worktrees.createFn = func(ctx context.Context, opts gitpkg.CreateOpts) (*gitpkg.CreateResult, error) {
		// Mirror the real Create: `git worktree add` lands and reports what it
		// made, and only THEN does the setup script hang forever. Reporting is
		// what leaves the row able to name — and prove ownership of — the branch.
		if opts.OnWorktreeReady != nil {
			opts.OnWorktreeReady(ctx, "bos-717-deadline", "/tmp/worktrees/repo/bos-717-deadline")
		}
		// This returns only when the bootstrap deadline cancels it (or the test
		// gives up and releases it).
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-escape:
			return nil, context.Canceled
		}
	}
	t.Cleanup(func() { close(escape) })

	h := newCreateSessionStreamHarness(t, worktrees, &setupStreamAgent{})
	h.lifecycle.SetBootstrapTimeout(bootstrapTimeout)

	done := make(chan error, 1)
	go func() {
		done <- h.createBranchSession(context.Background(), h.repo.ID, "wedged setup", "bos-717-deadline")
	}()

	var err error
	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("bootstrap never returned: it has no deadline of its own")
	}

	if err == nil {
		t.Fatal("CreateSession error = nil, want a bootstrap deadline failure")
	}
	if !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("CreateSession error = %q, want it to name the deadline", err.Error())
	}

	sessions, listErr := h.sessions.List(context.Background(), h.repo.ID)
	if listErr != nil {
		t.Fatalf("list sessions: %v", listErr)
	}
	if len(sessions) != 0 {
		t.Fatalf("sessions len = %d, want 0 (no row stuck in CreatingWorktree); got state %v", len(sessions), sessions[0].State)
	}

	reaped, purged := worktrees.cleanupRecords()
	if !slices.Contains(purged, "bos-717-deadline") {
		t.Fatalf("purged worktrees = %v, want the session's branch", purged)
	}
	if !slices.Contains(reaped, "bos-717-deadline") {
		t.Fatalf("reaped branches = %v, want the session's fresh branch deleted", reaped)
	}
}

// createTitleOnlySession drives a CreateSession with NEITHER a branch name nor
// a PR — the plain `boss new --repo … --prompt …` / TUI "new session" shape, and
// the shape the 2026-08-06 outage happened on. The branch is derived from the
// title inside WorktreeManager.Create, so the session row carries an empty
// branch_name until that returns.
func (h *createSessionStreamHarness) createTitleOnlySession(ctx context.Context, title string) error {
	agentName := "claude"
	stream, err := h.client.CreateSession(ctx, connect.NewRequest(&pb.CreateSessionRequest{
		RepoId:    h.repo.ID,
		Title:     title,
		Plan:      "do work",
		AgentName: &agentName,
		Detach:    true,
	}))
	if err != nil {
		return err
	}
	for stream.Receive() {
		_ = stream.Msg()
	}
	return stream.Err()
}

// TestCreateSessionBootstrapDeadlineCleansUpATitleDerivedBranch is regression
// test B for the shape the incident actually took (AC 2).
//
// TestCreateSessionBootstrapDeadlineFailsAndCleansUp above passes an explicit
// branch name, so the session row knows its branch from the moment it is
// inserted. A title-only create does not: WorktreeManager.Create derives the
// branch (sanitized title + uniquifying suffix) and the row learns it only when
// Create RETURNS — which, on the deadline path, it never does usefully. Without
// the OnWorktreeReady hook the failure cleanup has no branch to name, so it
// skips BOTH the branch reap and the worktree purge, and AC 2's "no orphaned
// worktree" is violated on the commonest create in the product.
//
// The fake mirrors the real Create: `worktree add` lands (hook fires), then the
// setup script hangs until the deadline kills it.
func TestCreateSessionBootstrapDeadlineCleansUpATitleDerivedBranch(t *testing.T) {
	t.Parallel()

	const (
		bootstrapTimeout = 300 * time.Millisecond
		derivedBranch    = "wedged-title-only"
	)

	escape := make(chan struct{})
	worktrees := &setupStreamWorktree{}
	worktrees.createFn = func(ctx context.Context, opts gitpkg.CreateOpts) (*gitpkg.CreateResult, error) {
		if opts.BranchName != "" {
			t.Errorf("createFn BranchName = %q, want empty (this create derives its branch)", opts.BranchName)
		}
		if opts.OnWorktreeReady == nil {
			t.Error("CreateOpts.OnWorktreeReady is nil; the caller cannot learn the derived branch")
		} else {
			opts.OnWorktreeReady(ctx, derivedBranch, "/tmp/worktrees/repo/"+derivedBranch)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-escape:
			return nil, context.Canceled
		}
	}
	t.Cleanup(func() { close(escape) })

	h := newCreateSessionStreamHarness(t, worktrees, &setupStreamAgent{})
	h.lifecycle.SetBootstrapTimeout(bootstrapTimeout)

	done := make(chan error, 1)
	go func() { done <- h.createTitleOnlySession(context.Background(), "wedged title only") }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("CreateSession error = nil, want a bootstrap deadline failure")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("bootstrap never returned: it has no deadline of its own")
	}

	sessions, listErr := h.sessions.List(context.Background(), h.repo.ID)
	if listErr != nil {
		t.Fatalf("list sessions: %v", listErr)
	}
	if len(sessions) != 0 {
		t.Fatalf("sessions len = %d, want 0 (no row stuck in CreatingWorktree)", len(sessions))
	}

	reaped, purged := worktrees.cleanupRecords()
	if !slices.Contains(purged, derivedBranch) {
		t.Fatalf("purged worktrees = %v, want the derived branch %q — the worktree is orphaned", purged, derivedBranch)
	}
	if !slices.Contains(reaped, derivedBranch) {
		t.Fatalf("reaped branches = %v, want the derived branch %q — the branch is orphaned", reaped, derivedBranch)
	}
}

// TestCreateSessionClientDisconnectReapsTheFreshBranch covers the sibling
// discarding exit: the client goes away mid-create (Ctrl-C during `boss new`, a
// dropped web stream), so the session is deleted.
//
// That path used to call the CONSERVATIVE cleanup entry and throw away the
// lifecycle result, so a fresh branch git had already made — now nameable,
// because OnWorktreeReady recorded it — was purged as a worktree but left
// behind as a branch. Both discarding exits must apply the same ownership rule.
func TestCreateSessionClientDisconnectReapsTheFreshBranch(t *testing.T) {
	t.Parallel()

	const derivedBranch = "bos-717-disconnected"

	worktrees := &setupStreamWorktree{}
	worktrees.createFn = func(ctx context.Context, opts gitpkg.CreateOpts) (*gitpkg.CreateResult, error) {
		if opts.OnWorktreeReady != nil {
			opts.OnWorktreeReady(ctx, derivedBranch, "/tmp/worktrees/repo/"+derivedBranch)
		}
		// Emitting setup output is what gets the server into streamSetupOutput,
		// where the failing emit below simulates the client going away.
		if opts.SetupScriptOutput != nil {
			_, _ = io.WriteString(opts.SetupScriptOutput, "installing deps\n")
		}
		// Fail after the add. The hook above has recorded both the branch and
		// the worktree path, so the row can name what to clean up AND prove it
		// owns the branch — which is what this exit has to act on.
		return nil, errors.New("client vanished mid-bootstrap")
	}

	h := newCreateSessionStreamHarness(t, worktrees, &setupStreamAgent{})

	agentName := "claude"
	clientGone := errors.New("client disconnected")
	err := h.server.StreamCreateSession(context.Background(), &pb.CreateSessionRequest{
		RepoId:    h.repo.ID,
		Title:     "disconnecting client",
		Plan:      "do work",
		AgentName: &agentName,
		Detach:    true,
	}, func(resp *pb.CreateSessionResponse) error {
		if resp.GetSetupOutput() != nil {
			return clientGone
		}
		return nil
	})
	if err == nil {
		t.Fatal("StreamCreateSession error = nil, want the disconnect error")
	}

	sessions, listErr := h.sessions.List(context.Background(), h.repo.ID)
	if listErr != nil {
		t.Fatalf("list sessions: %v", listErr)
	}
	if len(sessions) != 0 {
		t.Fatalf("sessions len = %d, want 0 (the abandoned session is deleted)", len(sessions))
	}

	reaped, purged := worktrees.cleanupRecords()
	if !slices.Contains(purged, derivedBranch) {
		t.Fatalf("purged worktrees = %v, want %q", purged, derivedBranch)
	}
	if !slices.Contains(reaped, derivedBranch) {
		t.Fatalf("reaped branches = %v, want %q — the disconnect exit orphaned the branch", reaped, derivedBranch)
	}
}

// TestCreateSessionFailureBeforeTheWorktreeKeepsAnExplicitBranch is the
// data-loss guard on the failure cleanup.
//
// ReapLocalBranches is `git branch -D`, which does not ask. A row's branch_name
// is written at INSERT time from the request, BEFORE Create has looked at
// whether such a branch already exists — so on an explicit-branch create (a
// Linear suggested branch, `--branch`) the name alone proves nothing. Only a
// recorded worktree path does, because that is written after `git worktree add`
// succeeded.
//
// Here the create fails the way the newly-added deadline makes routine: inside
// Create, BEFORE the worktree exists. Re-running a ticket produces the same
// deterministic branch name, so the branch this would delete is a previous
// session's, complete with unpushed commits.
func TestCreateSessionFailureBeforeTheWorktreeKeepsAnExplicitBranch(t *testing.T) {
	t.Parallel()

	worktrees := &setupStreamWorktree{}
	worktrees.createFn = func(context.Context, gitpkg.CreateOpts) (*gitpkg.CreateResult, error) {
		// Fetch died — a network blip, a `cannot lock ref`, or the bootstrap
		// deadline firing mid-fetch. Note this is NOT ErrBranchExists: Create
		// never reached the existence check.
		return nil, errors.New("fetch origin/main: context deadline exceeded")
	}

	h := newCreateSessionStreamHarness(t, worktrees, &setupStreamAgent{})

	if err := h.createBranchSession(context.Background(), h.repo.ID, "retry of a ticket", "feat-ticket-123"); err == nil {
		t.Fatal("CreateSession error = nil, want the fetch failure")
	}

	reaped, _ := worktrees.cleanupRecords()
	if len(reaped) != 0 {
		t.Fatalf("reaped branches = %v, want none — the create never made a worktree, so the branch may predate it and carry unpushed work", reaped)
	}
}

// TestCreateSessionBranchExistsDoesNotReapTheBranch is the named case of the
// pre-worktree-failure rule: a create refused BECAUSE the branch already existed
// must never delete that pre-existing branch. Create returns before
// OnWorktreeReady can fire, so the row never records a worktree path and the
// cleanup has no evidence the branch is ours — which is the whole point.
func TestCreateSessionBranchExistsDoesNotReapTheBranch(t *testing.T) {
	t.Parallel()

	worktrees := &setupStreamWorktree{createErr: gitpkg.ErrBranchExists}
	h := newCreateSessionStreamHarness(t, worktrees, &setupStreamAgent{})

	if err := h.createBranchSession(context.Background(), h.repo.ID, "collides", "bos-717-existing"); err == nil {
		t.Fatal("CreateSession error = nil, want ErrBranchExists")
	}

	reaped, _ := worktrees.cleanupRecords()
	if len(reaped) != 0 {
		t.Fatalf("reaped branches = %v, want none (the branch predates this attempt)", reaped)
	}
}
