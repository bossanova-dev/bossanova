package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	gitpkg "github.com/recurser/bossd/internal/git"
	"github.com/recurser/bossd/internal/status"
	"github.com/rs/zerolog"
)

// keepCurrentGit is a WorktreeManager that records the branches the sweep
// rebased. It embeds the interface so any method the sweep is not supposed to
// call panics loudly instead of silently succeeding.
type keepCurrentGit struct {
	gitpkg.WorktreeManager

	mu          sync.Mutex
	behind      int
	behindErr   error
	behindCount int
	dirty       string
	statusErr   error
	rebaseErr   error
	rebased     []string
}

func (g *keepCurrentGit) CountBehindBase(_ context.Context, _, _, _ string) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.behindCount++
	return g.behind, g.behindErr
}

func (g *keepCurrentGit) behindCalls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.behindCount
}

func (g *keepCurrentGit) Status(_ context.Context, _ string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.dirty, g.statusErr
}

func (g *keepCurrentGit) RebaseOntoBaseAndPush(_ context.Context, _, branch, _ string) (*gitpkg.RebaseResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.rebaseErr != nil {
		return nil, g.rebaseErr
	}
	g.rebased = append(g.rebased, branch)
	return &gitpkg.RebaseResult{PriorHead: "old", NewHead: "new"}, nil
}

func (g *keepCurrentGit) rebasedBranches() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.rebased...)
}

// keepCurrentSessionStore serves one repo's active sessions.
type keepCurrentSessionStore struct {
	db.SessionStore
	sessions []*models.Session
	err      error
}

func (s *keepCurrentSessionStore) ListActive(_ context.Context, _ string) ([]*models.Session, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.sessions, nil
}

// keepCurrentChatStore returns a fixed chat list for every session.
type keepCurrentChatStore struct {
	db.AgentChatStore
	chats []*models.AgentChat
	err   error
}

func (s *keepCurrentChatStore) ListBySession(_ context.Context, _ string) ([]*models.AgentChat, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.chats, nil
}

// keepCurrentRepoStore answers the sweep's post-gate repo re-read.
type keepCurrentRepoStore struct {
	db.RepoStore
	repo *models.Repo
	err  error
}

func (s *keepCurrentRepoStore) Get(_ context.Context, _ string) (*models.Repo, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.repo, nil
}

func keepCurrentSession() *models.Session {
	pr := 42
	return &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		State:        machine.AwaitingChecks,
		BranchName:   "feature",
		BaseBranch:   "main",
		WorktreePath: "/tmp/wt",
		PRNumber:     &pr,
	}
}

func keepCurrentRepo() *models.Repo {
	return &models.Repo{ID: "repo-1", DefaultBaseBranch: "main", ShouldKeepBranchesCurrent: true}
}

// keepCurrentServer wires the minimum Server surface the sweep touches.
func keepCurrentServer(sess *models.Session, git *keepCurrentGit, chats *keepCurrentChatStore) *Server {
	return &Server{
		sessions:    &keepCurrentSessionStore{sessions: []*models.Session{sess}},
		agentChats:  chats,
		chatStatus:  status.NewTracker(),
		repairLease: status.NewRepairLeaseManager(),
		worktrees:   git,
		logger:      zerolog.Nop(),
	}
}

// runSweep drives one sweep synchronously (the merged session id is empty, so
// no candidate is filtered out as "the branch that just merged").
func runSweep(t *testing.T, s *Server, repo *models.Repo) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.runKeepCurrentSweep(ctx, repo, "main", "")
}

// TestKeepCurrentSweep_RebasesEligibleSession is the positive control for the
// predicate matrix below: with every predicate satisfied the branch is rebased
// and pushed exactly once.
func TestKeepCurrentSweep_RebasesEligibleSession(t *testing.T) {
	git := &keepCurrentGit{behind: 3}
	s := keepCurrentServer(keepCurrentSession(), git, &keepCurrentChatStore{})

	runSweep(t, s, keepCurrentRepo())

	if got := git.rebasedBranches(); len(got) != 1 || got[0] != "feature" {
		t.Fatalf("rebased branches = %v, want [feature]", got)
	}
}

// TestKeepCurrentSweep_SkipMatrix asserts every safety predicate independently
// suppresses the rebase. Each case mutates exactly one variable away from the
// eligible baseline, so a passing case proves that predicate alone is load
// bearing.
func TestKeepCurrentSweep_SkipMatrix(t *testing.T) {
	workingChat := &models.AgentChat{ID: "c1", SessionID: "sess-1", AgentSessionID: "agent-1"}

	cases := []struct {
		name  string
		setup func(t *testing.T, repo *models.Repo, sess *models.Session, git *keepCurrentGit, s *Server)
	}{
		{
			name: "repo opted out",
			setup: func(_ *testing.T, repo *models.Repo, _ *models.Session, _ *keepCurrentGit, _ *Server) {
				repo.ShouldKeepBranchesCurrent = false
			},
		},
		{
			name: "chat working",
			setup: func(_ *testing.T, _ *models.Repo, _ *models.Session, _ *keepCurrentGit, s *Server) {
				s.agentChats = &keepCurrentChatStore{chats: []*models.AgentChat{workingChat}}
				s.chatStatus.Update("agent-1", pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())
			},
		},
		{
			name: "chat awaiting an answer",
			setup: func(_ *testing.T, _ *models.Repo, _ *models.Session, _ *keepCurrentGit, s *Server) {
				s.agentChats = &keepCurrentChatStore{chats: []*models.AgentChat{workingChat}}
				s.chatStatus.Update("agent-1", pb.ChatStatus_CHAT_STATUS_QUESTION, time.Now())
			},
		},
		{
			name: "worktree dirty",
			setup: func(_ *testing.T, _ *models.Repo, _ *models.Session, git *keepCurrentGit, _ *Server) {
				git.dirty = " M main.go"
			},
		},
		{
			name: "repair lease held",
			setup: func(t *testing.T, _ *models.Repo, sess *models.Session, _ *keepCurrentGit, s *Server) {
				if err := s.repairLease.Acquire(sess.ID, "repair-plugin"); err != nil {
					t.Fatalf("Acquire repair lease: %v", err)
				}
			},
		},
		{
			name: "already current",
			setup: func(_ *testing.T, _ *models.Repo, _ *models.Session, git *keepCurrentGit, _ *Server) {
				git.behind = 0
			},
		},
		{
			name: "no open pr",
			setup: func(_ *testing.T, _ *models.Repo, sess *models.Session, _ *keepCurrentGit, _ *Server) {
				sess.PRNumber = nil
			},
		},
		{
			name: "session already merged",
			setup: func(_ *testing.T, _ *models.Repo, sess *models.Session, _ *keepCurrentGit, _ *Server) {
				sess.State = machine.Merged
			},
		},
		{
			name: "no worktree",
			setup: func(_ *testing.T, _ *models.Repo, sess *models.Session, _ *keepCurrentGit, _ *Server) {
				sess.WorktreePath = ""
			},
		},
		{
			name: "targets a different base",
			setup: func(_ *testing.T, _ *models.Repo, sess *models.Session, _ *keepCurrentGit, _ *Server) {
				sess.BaseBranch = "release"
			},
		},
		{
			name: "chat lookup failed",
			setup: func(_ *testing.T, _ *models.Repo, _ *models.Session, _ *keepCurrentGit, s *Server) {
				s.agentChats = &keepCurrentChatStore{err: errors.New("boom")}
			},
		},
		{
			name: "git status failed",
			setup: func(_ *testing.T, _ *models.Repo, _ *models.Session, git *keepCurrentGit, _ *Server) {
				git.statusErr = errors.New("boom")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := keepCurrentRepo()
			sess := keepCurrentSession()
			git := &keepCurrentGit{behind: 3}
			s := keepCurrentServer(sess, git, &keepCurrentChatStore{})
			tc.setup(t, repo, sess, git, s)

			runSweep(t, s, repo)

			if got := git.rebasedBranches(); len(got) != 0 {
				t.Fatalf("rebased %v, want no rebase when %s", got, tc.name)
			}
		})
	}
}

// TestKeepCurrentSweep_ConflictIsASkip verifies a conflicting rebase is a
// benign skip, not an escalation: the git layer has already restored the
// branch, and the sweep must simply move on.
func TestKeepCurrentSweep_ConflictIsASkip(t *testing.T) {
	git := &keepCurrentGit{behind: 2, rebaseErr: gitpkg.ErrRebaseConflict}
	s := keepCurrentServer(keepCurrentSession(), git, &keepCurrentChatStore{})

	runSweep(t, s, keepCurrentRepo())

	if got := git.rebasedBranches(); len(got) != 0 {
		t.Fatalf("rebased %v, want none after a conflict", got)
	}
}

// TestKeepCurrentSweep_SkipsTheMergedSession verifies the branch that just
// merged is never a sweep candidate — its worktree is mid-teardown and it is
// by definition not behind.
func TestKeepCurrentSweep_SkipsTheMergedSession(t *testing.T) {
	git := &keepCurrentGit{behind: 3}
	sess := keepCurrentSession()
	s := keepCurrentServer(sess, git, &keepCurrentChatStore{})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.runKeepCurrentSweep(ctx, keepCurrentRepo(), "main", sess.ID)

	if got := git.rebasedBranches(); len(got) != 0 {
		t.Fatalf("rebased %v, want the merged session skipped", got)
	}
}

// TestKeepCurrentSweep_ChatWithNoLiveStatusIsNotBusy pins the deliberate
// semantic of busyChatStatus: a chat the status tracker knows nothing about —
// never seen, or last heard from more than status.StaleThreshold ago, which
// Tracker.Get collapses to the same nil — is NOT a reason to skip. Idle
// sessions whose agent has long since stopped heartbeating are precisely the
// ones the sweep exists to keep current, so treating an absent heartbeat as
// "busy" would make the feature inert.
func TestKeepCurrentSweep_ChatWithNoLiveStatusIsNotBusy(t *testing.T) {
	git := &keepCurrentGit{behind: 3}
	chats := &keepCurrentChatStore{chats: []*models.AgentChat{
		{ID: "c1", SessionID: "sess-1", AgentSessionID: "agent-1"},
	}}
	s := keepCurrentServer(keepCurrentSession(), git, chats)
	// Deliberately never Update() the tracker for agent-1, so Get returns nil.
	if entry := s.chatStatus.Get("agent-1"); entry != nil {
		t.Fatalf("tracker returned %v for an unseen chat, want nil", entry)
	}

	runSweep(t, s, keepCurrentRepo())

	if got := git.rebasedBranches(); len(got) != 1 || got[0] != "feature" {
		t.Fatalf("rebased branches = %v, want [feature] (no heartbeat is not busy)", got)
	}
}

// TestKeepCurrentSweep_HonoursOptOutWhileQueued verifies the sweep re-reads the
// repo after acquiring the gate: a sweep can sit queued behind other merges for
// minutes, and an operator who opts out in that window must not be swept
// against on the strength of a stale snapshot.
func TestKeepCurrentSweep_HonoursOptOutWhileQueued(t *testing.T) {
	git := &keepCurrentGit{behind: 3}
	s := keepCurrentServer(keepCurrentSession(), git, &keepCurrentChatStore{})

	optedOut := keepCurrentRepo()
	optedOut.ShouldKeepBranchesCurrent = false
	s.repos = &keepCurrentRepoStore{repo: optedOut}

	// The snapshot still says opted-in — only the fresh read disagrees.
	runSweep(t, s, keepCurrentRepo())

	if got := git.rebasedBranches(); len(got) != 0 {
		t.Fatalf("rebased %v after the repo opted out while queued, want none", got)
	}
}

// TestKeepCurrentSweep_AbandonsWhenRepoRereadFails verifies an unreadable or
// deleted repo aborts the sweep rather than falling back to the stale snapshot.
func TestKeepCurrentSweep_AbandonsWhenRepoRereadFails(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store *keepCurrentRepoStore
	}{
		{name: "read failed", store: &keepCurrentRepoStore{err: errors.New("boom")}},
		{name: "repo gone", store: &keepCurrentRepoStore{repo: nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			git := &keepCurrentGit{behind: 3}
			s := keepCurrentServer(keepCurrentSession(), git, &keepCurrentChatStore{})
			s.repos = tc.store

			runSweep(t, s, keepCurrentRepo())

			if got := git.rebasedBranches(); len(got) != 0 {
				t.Fatalf("rebased %v when %s, want none", got, tc.name)
			}
		})
	}
}

// TestLogCommitsBehindBaseAtMerge_OnlyForOptedInRepos verifies the acceptance
// metric is not collected for repos that never opted in. CountBehindBase does a
// network fetch inside the per-repo merge gate, so an opted-out repo must pay
// nothing on merge.
func TestLogCommitsBehindBaseAtMerge_OnlyForOptedInRepos(t *testing.T) {
	for _, tc := range []struct {
		name    string
		optedIn bool
		want    int
	}{
		{name: "opted out", optedIn: false, want: 0},
		{name: "opted in", optedIn: true, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			git := &keepCurrentGit{behind: 3}
			s := keepCurrentServer(keepCurrentSession(), git, &keepCurrentChatStore{})

			repo := keepCurrentRepo()
			repo.ShouldKeepBranchesCurrent = tc.optedIn
			s.logCommitsBehindBaseAtMerge(context.Background(), repo, keepCurrentSession(), "main")

			if got := git.behindCalls(); got != tc.want {
				t.Fatalf("CountBehindBase called %d times for a %s repo, want %d", got, tc.name, tc.want)
			}
		})
	}
}

// TestEnqueueKeepCurrentSweep_OptOutStartsNoGoroutine verifies the opt-in gate
// is checked before any work is scheduled, so a repo that never opted in pays
// nothing on every merge.
func TestEnqueueKeepCurrentSweep_OptOutStartsNoGoroutine(t *testing.T) {
	git := &keepCurrentGit{behind: 3}
	s := keepCurrentServer(keepCurrentSession(), git, &keepCurrentChatStore{})

	var started bool
	s.keepCurrentSweepHook = func(_ <-chan struct{}) { started = true }

	repo := keepCurrentRepo()
	repo.ShouldKeepBranchesCurrent = false
	s.enqueueKeepCurrentSweep(repo, "main", "")

	if started {
		t.Fatal("sweep goroutine started for a repo that did not opt in")
	}
}

// TestEnqueueKeepCurrentSweep_RunsAsync verifies the opted-in path schedules a
// real background sweep that completes on its own context (the caller's ctx is
// cancelled the moment the merge handler returns).
func TestEnqueueKeepCurrentSweep_RunsAsync(t *testing.T) {
	git := &keepCurrentGit{behind: 3}
	s := keepCurrentServer(keepCurrentSession(), git, &keepCurrentChatStore{})

	var done <-chan struct{}
	s.keepCurrentSweepHook = func(d <-chan struct{}) { done = d }

	s.enqueueKeepCurrentSweep(keepCurrentRepo(), "main", "")

	if done == nil {
		t.Fatal("no sweep goroutine started for an opted-in repo")
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("sweep did not finish")
	}

	if got := git.rebasedBranches(); len(got) != 1 {
		t.Fatalf("rebased %v, want the eligible branch rebased once", got)
	}
}
