package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/safego"
	gitpkg "github.com/recurser/bossd/internal/git"
)

// keepCurrentSweepTimeout bounds one whole sweep. A sweep does network git work
// (fetch + force-push) once per eligible session, so it is generous — but it is
// never unbounded, because the sweep holds the per-repo merge gate and a wedged
// sweep would block every subsequent merge on that repo.
const keepCurrentSweepTimeout = 15 * time.Minute

// keepCurrentSweepHook observes a sweep goroutine's completion. Test-only: zero
// in production, where the sweep is deliberately fire-and-forget (see
// enqueueKeepCurrentSweep for why the merge handler cannot wait on it).
type keepCurrentSweepHook func(done <-chan struct{})

// SetKeepCurrentSweepHookForTest installs the sweep-completion observer.
// Test-only, and only exported because the end-to-end tests live in a
// different package (internal/testharness): without a join point they would
// have to poll for an asynchronous side effect. Production never calls it, so
// the hook stays nil and the sweep stays fire-and-forget.
func (s *Server) SetKeepCurrentSweepHookForTest(hook func(done <-chan struct{})) {
	s.keepCurrentSweepHook = hook
}

// enqueueKeepCurrentSweep starts the post-merge keep-current sweep for repo
// (BOS-521), if the repo opted in.
//
// The sweep runs in the background on its own context: the merge handler's ctx
// is cancelled the moment it returns, and the sweep must outlive it. The
// handler must NOT wait for the returned goroutine either — the sweep's first
// act is to acquire the per-repo merge gate, which the handler still holds
// until its own deferred release runs, so waiting here would deadlock. The done
// channel is therefore handed to the test hook rather than dropped, giving
// tests a deterministic join point without changing production sequencing.
func (s *Server) enqueueKeepCurrentSweep(repo *models.Repo, baseBranch, mergedSessionID string) {
	if repo == nil || !repo.ShouldKeepBranchesCurrent || baseBranch == "" {
		return
	}
	if s.sessions == nil || s.worktrees == nil {
		return
	}
	done := safego.Go(s.logger, func() {
		ctx, cancel := context.WithTimeout(context.Background(), keepCurrentSweepTimeout)
		defer cancel()
		s.runKeepCurrentSweep(ctx, repo, baseBranch, mergedSessionID)
	})
	if s.keepCurrentSweepHook != nil {
		s.keepCurrentSweepHook(done)
	}
}

// NotifyBaseAdvanced implements session.BaseAdvanceNotifier: it is the webhook
// path into the keep-current sweep, covering PRs that merge without boss doing
// the merging (GitHub UI, merge queue, another tool).
func (s *Server) NotifyBaseAdvanced(ctx context.Context, mergedSessionID string) {
	if s.sessions == nil || s.repos == nil {
		return
	}
	sess, err := s.sessions.Get(ctx, mergedSessionID)
	if err != nil {
		s.logger.Debug().Err(err).Str("session_id", mergedSessionID).
			Msg("keep-current: merged session lookup failed")
		return
	}
	repo, err := s.repos.Get(ctx, sess.RepoID)
	if err != nil {
		s.logger.Debug().Err(err).Str("repo_id", sess.RepoID).
			Msg("keep-current: repo lookup failed")
		return
	}
	s.enqueueKeepCurrentSweep(repo, sessionBaseBranch(sess, repo.DefaultBaseBranch), mergedSessionID)
}

// runKeepCurrentSweep rebases every eligible in-flight branch in the repo onto
// the freshly advanced base and force-pushes it with a lease.
//
// It runs behind the same per-repo merge gate user-initiated merges use, so the
// ordering is deterministic (merge → sweep → next merge) and a sweep can never
// rewrite a branch out from under a merge that is verifying it.
func (s *Server) runKeepCurrentSweep(ctx context.Context, repo *models.Repo, baseBranch, mergedSessionID string) {
	if repo == nil || !repo.ShouldKeepBranchesCurrent {
		return
	}
	log := s.logger.With().Str("repo_id", repo.ID).Str("base", baseBranch).Logger()

	release, err := s.acquireRepoMerge(ctx, repo.ID)
	if err != nil {
		log.Warn().Err(err).Msg("keep-current sweep abandoned while queued")
		return
	}
	defer release()

	// The *models.Repo above is the snapshot taken when the merge enqueued this
	// sweep, and the sweep may have sat behind other merges on the gate for
	// minutes. Re-read it so an operator who opted out (or removed the repo) in
	// that window is honoured rather than swept against their wishes.
	if s.repos != nil {
		fresh, freshErr := s.repos.Get(ctx, repo.ID)
		if freshErr != nil {
			log.Warn().Err(freshErr).Msg("keep-current sweep abandoned: repo re-read failed")
			return
		}
		if fresh == nil || !fresh.ShouldKeepBranchesCurrent {
			log.Info().Msg("keep-current sweep abandoned: repo opted out while queued")
			return
		}
	}

	sessions, err := s.sessions.ListActive(ctx, repo.ID)
	if err != nil {
		log.Warn().Err(err).Msg("keep-current sweep: list sessions failed")
		return
	}

	var rebased, skipped int
	for _, sess := range sessions {
		if ctx.Err() != nil {
			log.Warn().Err(ctx.Err()).Msg("keep-current sweep cancelled")
			return
		}
		if sess == nil || sess.ID == mergedSessionID {
			continue
		}
		if reason := s.keepCurrentSkipReason(ctx, sess, baseBranch); reason != "" {
			skipped++
			log.Debug().
				Str("session_id", sess.ID).
				Str("branch", sess.BranchName).
				Str("reason", reason).
				Msg("keep-current skipped")
			continue
		}
		if s.keepSessionCurrent(ctx, sess, baseBranch) {
			rebased++
		} else {
			skipped++
		}
	}
	log.Info().
		Int("rebased", rebased).
		Int("skipped", skipped).
		Int("candidates", len(sessions)).
		Msg("keep-current sweep complete")
}

// keepSessionCurrent rebases one session's branch onto origin/<base> and
// force-pushes it. It reports whether the branch was actually moved.
//
// Every failure is a skip, never an escalation: the branch simply stays behind
// and the existing repair lane and merge-time reconciliation own it from there.
func (s *Server) keepSessionCurrent(ctx context.Context, sess *models.Session, baseBranch string) bool {
	log := s.logger.With().
		Str("session_id", sess.ID).
		Str("branch", sess.BranchName).
		Str("base", baseBranch).
		Logger()

	behind, err := s.worktrees.CountBehindBase(ctx, sess.WorktreePath, sess.BranchName, baseBranch)
	if err != nil {
		log.Warn().Err(err).Msg("keep-current skipped: behind-count failed")
		return false
	}
	if behind == 0 {
		log.Debug().Msg("keep-current skipped: branch already current")
		return false
	}

	res, err := s.worktrees.RebaseOntoBaseAndPush(ctx, sess.WorktreePath, sess.BranchName, baseBranch)
	if err != nil {
		// Two outcomes are expected and benign, and are logged quietly:
		//   - a conflict: boss never auto-resolves, so the branch is left
		//     exactly as it was and stays behind;
		//   - unpushed local commits: the force-with-lease anchor would be
		//     stale by construction, so the git layer refuses before rebasing.
		// Everything else — a cancelled rebase, a failing hook, a held
		// index.lock — may mean a damaged worktree and is logged loudly, WITH
		// the error attached, so it cannot hide behind a benign-looking line.
		switch {
		case errors.Is(err, gitpkg.ErrRebaseConflict):
			log.Info().Int("commits_behind", behind).Msg("keep-current skipped: conflict")
		case errors.Is(err, gitpkg.ErrBranchNotPushed):
			log.Info().Int("commits_behind", behind).Msg("keep-current skipped: local commits not pushed")
		default:
			log.Warn().Err(err).Int("commits_behind", behind).Msg("keep-current skipped: rebase failed")
		}
		return false
	}

	log.Info().
		Int("commits_behind", behind).
		Str("prior_head", res.PriorHead).
		Str("new_head", res.NewHead).
		Msg("keep-current: rebased in-flight branch onto advanced base")
	return true
}

// keepCurrentSkipReason returns the reason this session must NOT be rebased, or
// "" when every safety predicate holds.
//
// The predicates exist because the sweep mutates a worktree the session owns.
// Each one protects work the daemon cannot reconstruct: an agent mid-turn, an
// operator's uncommitted edits, or a repair run already rewriting the branch.
// The cheap in-memory checks run before the git subprocess, so a busy repo does
// not pay for `git status` on every session.
func (s *Server) keepCurrentSkipReason(ctx context.Context, sess *models.Session, baseBranch string) string {
	switch {
	case sess.PRNumber == nil:
		return "no open pr"
	case sess.State == machine.Merged, sess.State == machine.Closed, sess.State == machine.Orphaned:
		return "session not in flight"
	case sess.WorktreePath == "":
		return "no worktree"
	case sess.BranchName == "":
		return "no branch"
	case sessionBaseBranch(sess, baseBranch) != baseBranch:
		// A session targeting a different base is unaffected by this advance.
		return "different base branch"
	}
	if s.repairLease != nil && s.repairLease.Active(sess.ID) {
		return "repair lease held"
	}
	if busy := s.busyChatStatus(ctx, sess.ID); busy != "" {
		return busy
	}
	dirty, err := s.worktrees.Status(ctx, sess.WorktreePath)
	if err != nil {
		return fmt.Sprintf("git status failed: %v", err)
	}
	if dirty != "" {
		return "worktree dirty"
	}
	return ""
}

// sessionBaseBranch resolves the base a session actually targets, defaulting to
// the branch that just advanced when the session records none.
func sessionBaseBranch(sess *models.Session, fallback string) string {
	if sess.BaseBranch == "" {
		return fallback
	}
	return sess.BaseBranch
}

// busyChatStatus returns a skip reason when any of the session's chats is
// mid-turn (WORKING) or waiting on the operator (QUESTION), or "" when they are
// all settled. Rebasing under a live agent would move the ground beneath an
// in-flight edit; rebasing under a question would silently change what the
// operator is being asked about.
//
// Only a failed chat *lookup* fails closed. A chat with no live tracker entry —
// never seen, or last heard from more than status.StaleThreshold ago — reads as
// NOT busy, matching the repo-wide convention that a missing heartbeat means
// "not running" (Tracker.GetBatch reports a stale entry as CHAT_STATUS_STOPPED,
// and chatStatusFromSessionChats/displaystatus rely on that). Inverting it would
// make the sweep inert for exactly the idle sessions it exists to serve.
func (s *Server) busyChatStatus(ctx context.Context, sessionID string) string {
	if s.chatStatus == nil || s.agentChats == nil {
		return ""
	}
	chats, err := s.agentChats.ListBySession(ctx, sessionID)
	if err != nil {
		// Unknown chat state is treated as busy: skipping costs one deferred
		// rebase, rebasing under a working agent costs the agent's work.
		return fmt.Sprintf("chat lookup failed: %v", err)
	}
	for _, chat := range chats {
		if chat == nil {
			continue
		}
		entry := s.chatStatus.Get(chat.AgentSessionID)
		if entry == nil {
			// No heartbeat at all, or one older than status.StaleThreshold —
			// Tracker.Get collapses both to nil. Neither means "busy": a chat
			// that has not checked in within the threshold is treated as
			// stopped everywhere else in the daemon, so it is not a reason to
			// hold off a rebase.
			continue
		}
		switch entry.Status {
		case pb.ChatStatus_CHAT_STATUS_WORKING:
			return "chat working"
		case pb.ChatStatus_CHAT_STATUS_QUESTION:
			return "chat awaiting answer"
		default:
			// IDLE, STOPPED, LIMITED and UNSPECIFIED all mean "not mid-turn":
			// nothing is writing to the worktree, so a rebase is safe.
		}
	}
	return ""
}

// logCommitsBehindBaseAtMerge emits the BOS-521 acceptance metric: how far the
// merging branch had drifted from its base at the moment it merged. With the
// keep-current sweep enabled this number trends to ~0, which is the measurable
// the source report asked for.
//
// It is only collected for repos that opted in. CountBehindBase does a network
// `git fetch` and runs inside the per-repo merge gate, so charging every merge
// in every repo for a number nobody enabled would slow down merges that get no
// benefit from it.
func (s *Server) logCommitsBehindBaseAtMerge(ctx context.Context, repo *models.Repo, sess *models.Session, baseBranch string) {
	if repo == nil || !repo.ShouldKeepBranchesCurrent {
		return
	}
	if s.worktrees == nil || sess.WorktreePath == "" || sess.BranchName == "" || baseBranch == "" {
		return
	}
	behind, err := s.worktrees.CountBehindBase(ctx, sess.WorktreePath, sess.BranchName, baseBranch)
	if err != nil {
		s.logger.Debug().Err(err).
			Str("session_id", sess.ID).
			Msg("merge: behind-count unavailable")
		return
	}
	s.logger.Info().
		Str("session_id", sess.ID).
		Str("branch", sess.BranchName).
		Str("base", baseBranch).
		Int("commits_behind_base_at_merge", behind).
		Msg("merge: branch drift at merge time")
}
