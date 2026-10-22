package session

import (
	"context"
	"fmt"
	"time"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sessionreason"
	"github.com/recurser/bossd/internal/db"
)

// bootstrapReapMargin is the slack added to the bootstrap deadline before the
// periodic sweep will touch a pre-agent session.
//
// The deadline itself is what makes reaping these states safe at all: a live
// create path fails ITSELF once its bootstrap exceeds BootstrapTimeout, so a row
// still sitting in CreatingWorktree/StartingAgent past that point has no owner
// left to race. The margin covers the gap between the bootstrap erroring and
// StreamCreateSession's cleanup deleting the row, so the sweep never fights that
// cleanup for the same session. This is what distinguishes the sweep from the
// BOS-426 hazard, where a periodic reap of pre-agent states deleted rows out
// from under in-flight creates and failed them with
// `update worktree path: sql: no rows in result set`.
const bootstrapReapMargin = 2 * time.Minute

// bootstrapReapThreshold is the age a pre-agent session must exceed before the
// periodic sweep reclaims it. Derived from the effective bootstrap deadline (not
// a separate hard-coded number) so shortening one automatically shortens the
// other and the two can never disagree about when a bootstrap is dead.
func (l *Lifecycle) bootstrapReapThreshold() time.Duration {
	return l.bootstrapDeadline() + bootstrapReapMargin
}

// preAgentReapStates is the set the stranded-bootstrap reaper acts on, DERIVED
// from strandedReapStates so it cannot drift from the shared definition of "a
// pre-agent transition".
func preAgentReapStates() []machine.State {
	out := make([]machine.State, 0, len(strandedReapStates))
	for _, s := range strandedReapStates {
		if isPreAgentState(s) {
			out = append(out, s)
		}
	}
	return out
}

func preAgentReapStateInts() []int {
	states := preAgentReapStates()
	out := make([]int, len(states))
	for i, s := range states {
		out[i] = int(s)
	}
	return out
}

// ReapStrandedBootstrapSessionsAtStartup runs the one-shot post-restart pass of
// the stranded-bootstrap reaper. A daemon restart is the one thing that can
// strand a young pre-agent row, so no age threshold is applied here; instead the
// row must predate THIS process (l.startedAt), which is a stronger guarantee
// than an age window: a create started by the running daemon can never satisfy
// it, so this pass cannot race the live create path however fast it runs.
func (l *Lifecycle) ReapStrandedBootstrapSessionsAtStartup(ctx context.Context) (int, error) {
	return l.reapStrandedBootstrapSessions(ctx, reapStartup)
}

// ReapStrandedBootstrapSessionsPeriodic runs the running-daemon sweep. Here a
// live create path genuinely can own a pre-agent row, so eligibility is gated on
// age exceeding bootstrapReapThreshold — past which the owner would already have
// failed itself against the bootstrap deadline.
func (l *Lifecycle) ReapStrandedBootstrapSessionsPeriodic(ctx context.Context) (int, error) {
	return l.reapStrandedBootstrapSessions(ctx, reapPeriodic)
}

// reapStrandedBootstrapSessions reclaims sessions stranded in an early bootstrap
// state (CreatingWorktree / StartingAgent), which nothing else reclaims: the
// stranded-cron sweep only covers UNATTENDED runs, and taskorchestrator's
// liveness ladder only runs for orchestrator-tracked tasks — so a plain
// `boss new` that died mid-bootstrap sat in `initializing` forever.
//
// A stranded row is marked Blocked with an explanatory reason (rather than
// deleted) so the failure is visible rather than silent, and its worktree — plus
// the branch when this session owned a fresh one — is cleaned up. The state
// transition is conditional on the row still being in a pre-agent state, so a
// create that revives between the list and the write is a no-op rather than a
// corruption.
//
// Returns the number of sessions reclaimed. A per-session failure is logged and
// the sweep continues; only the initial list error aborts.
func (l *Lifecycle) reapStrandedBootstrapSessions(ctx context.Context, phase reapPhase) (int, error) {
	reapInts := preAgentReapStateInts()
	stranded, err := l.sessions.ListByStates(ctx, reapInts)
	if err != nil {
		return 0, fmt.Errorf("list stranded bootstrap sessions: %w", err)
	}

	now := time.Now()
	reaped := 0
	for _, sess := range stranded {
		if !l.bootstrapStrandIsDead(sess, phase, now) {
			continue
		}

		// Serialize against the create/cleanup/retry lifecycle for this target
		// before touching anything. See acquireTargetForReap for why elapsed age
		// is not sufficient on its own.
		release, ok := acquireTargetForReap(ctx, sess.RepoID, sess.BranchName, sess.PRNumber)
		if !ok {
			l.logger.Debug().Str("session", sess.ID).Str("branch", sess.BranchName).
				Msg("stranded-bootstrap reap: target busy; leaving this row for a later sweep")
			continue
		}
		if l.reapOneStrandedBootstrap(ctx, sess, phase, reapInts) {
			reaped++
		}
		release()
	}

	return reaped, nil
}

// reapOneStrandedBootstrap reclaims a single stranded row. Split out from the
// loop so the target lock acquired around it is released on EVERY exit path,
// including the early returns below, without a defer inside a loop body.
//
// The caller holds this session's target lock for the whole call.
func (l *Lifecycle) reapOneStrandedBootstrap(ctx context.Context, sess *models.Session, phase reapPhase, reapInts []int) bool {
	// Re-read under the lock. The listing is a snapshot taken before the lock,
	// so between the two the owning create may have finished cleaning up and
	// deleted the row, or revived and reached an agent. UpdateStateConditionalFrom
	// below catches the state half; this catches the rest — an agent id or tmux
	// pane written while the row was still in a pre-agent state, which is exactly
	// the window StartSession sits in between spawning an agent and moving to
	// ImplementingPlan.
	fresh, err := l.sessions.Get(ctx, sess.ID)
	if err != nil || fresh == nil {
		// A row that vanished was cleaned up by its owner — the good outcome.
		l.logger.Debug().Err(err).Str("session", sess.ID).
			Msg("stranded-bootstrap reap: row gone or unreadable under the target lock; nothing to reclaim")
		return false
	}
	if !l.bootstrapStrandIsDead(fresh, phase, time.Now()) {
		l.logger.Debug().Str("session", sess.ID).
			Msg("stranded-bootstrap reap: row revived under the target lock; leaving it alone")
		return false
	}
	sess = fresh

	l.logger.Warn().
		Str("session", sess.ID).
		Str("state", sess.State.String()).
		Str("branch", sess.BranchName).
		Dur("age", time.Since(sess.CreatedAt)).
		Msg("reclaiming session stranded in early bootstrap")

	moved, updErr := l.sessions.UpdateStateConditionalFrom(ctx, sess.ID, int(machine.Blocked), reapInts)
	if updErr != nil {
		l.logger.Warn().Err(updErr).Str("session", sess.ID).Msg("stranded-bootstrap reap: mark blocked failed")
		return false
	}
	if !moved {
		// Someone else advanced the row between the re-read and the write —
		// a late-returning bootstrap, or a concurrent sweep. Leave it alone.
		return false
	}

	reason := sessionreason.BootstrapStranded()
	reasonPtr := &reason
	if _, updErr := l.sessions.Update(ctx, sess.ID, db.UpdateSessionParams{BlockedReason: &reasonPtr}); updErr != nil {
		l.logger.Warn().Err(updErr).Str("session", sess.ID).Msg("stranded-bootstrap reap: persist blocked reason failed")
	}

	abandoned := l.cleanUpStrandedBootstrapArtifacts(ctx, sess)
	// A cron run has nothing left to open or resume and its schedule fires a
	// fresh run, so leaving it Blocked only parks an alert nobody can act on.
	// A run whose worktree purge did not run stays visible: its artifacts need
	// reclaiming by hand.
	if isCronSession(sess) && !abandoned {
		if err := l.sessions.MarkArchived(ctx, sess.ID); err != nil {
			l.logger.Warn().Err(err).Str("session", sess.ID).Msg("stranded-bootstrap reap: archive cron run failed; leaving it Blocked")
		}
	}
	return true
}

// bootstrapStrandIsDead reports whether a pre-agent row is genuinely ownerless.
// Fail-safe by construction: every ambiguity leaves the session for a later
// sweep rather than risking a live create.
func (l *Lifecycle) bootstrapStrandIsDead(sess *models.Session, phase reapPhase, now time.Time) bool {
	if sess == nil || !isPreAgentState(sess.State) {
		return false
	}
	// ArchiveSession leaves the row in place; there is nothing to reclaim.
	if sess.ArchivedAt != nil {
		return false
	}
	// A row that already has an agent id or a tmux pane got PAST bootstrap; its
	// recovery belongs to the stranded-cron sweep, which understands agent
	// liveness. This reaper only owns rows that never reached an agent.
	if sess.AgentSessionID != nil && *sess.AgentSessionID != "" {
		return false
	}
	if sess.TmuxSessionName != nil && *sess.TmuxSessionName != "" {
		return false
	}

	if phase == reapStartup {
		// Only rows that predate this process: a create started by the running
		// daemon can never satisfy this, so the startup pass cannot race it.
		return sess.CreatedAt.Before(l.startedAt)
	}
	return now.Sub(sess.CreatedAt) >= l.bootstrapReapThreshold()
}

// cleanUpStrandedBootstrapArtifacts removes the worktree a stranded bootstrap
// may have left on disk, and the branch when this session owned a fresh one.
//
// Both operations are best-effort and safe when the artifact was never created —
// PurgeWorktree is a documented no-op on a missing worktree, and the branch reap
// is gated by strandedBranchIsOurs below.
//
// Best-effort is not the same as unordered, though: a purge that did not RUN
// (a contended clone) suppresses the reap entirely, because a branch still
// checked out in a registered worktree cannot be deleted and trying leaks it.
//
// That suppression is TERMINAL for this row, not a deferral, and the caller is
// why: reapStrandedBootstrapSessions has already moved the session to Blocked by
// the time this runs, and the sweep lists only pre-agent states — so no later
// tick will ever see this row again. Marking it Blocked first is deliberate (it
// is the atomic claim that stops a late-returning bootstrap being cleaned up
// underneath itself), so the honest response to a refused purge is to say the
// artifacts are being abandoned and name them, not to promise a retry that
// cannot happen.
//
// It reports whether it abandoned a worktree on disk, which happens only when
// the purge did not run.
func (l *Lifecycle) cleanUpStrandedBootstrapArtifacts(ctx context.Context, sess *models.Session) (abandoned bool) {
	if sess.RepoID == "" || l.worktrees == nil || l.repos == nil {
		return false
	}
	repo, err := l.repos.Get(ctx, sess.RepoID)
	if err != nil {
		l.logger.Warn().Err(err).Str("session", sess.ID).Msg("stranded-bootstrap reap: get repo failed")
		return false
	}
	// Decide ownership BEFORE the purge (the ancestry check is read-only in the
	// main clone, so the purge does not disturb it) but delete the branch AFTER:
	// `git branch -D` refuses a branch still checked out in a registered
	// worktree, and `worktree prune` only unregisters worktrees whose directory
	// is already gone — which a stranded bootstrap's is not.
	if sess.WorktreeCreatedAt == nil || sess.WorktreePath == "" {
		l.logger.Warn().Str("worktree", sess.WorktreePath).Msg("worktree ownership unknown; not purged")
		return false
	}
	ours := l.strandedBranchIsOurs(ctx, sess, repo)
	if err := l.worktrees.PurgeWorktree(ctx, repo.LocalPath, repo.DisplayName, repo.WorktreeBaseDir, sess.WorktreePath); err != nil {
		// A purge that did not run leaves the worktree registered, and
		// `git branch -D` refuses a branch checked out in one, so reaping anyway
		// would log a generic delete failure and leak the branch behind it.
		//
		// Nothing comes back for these: the row is already Blocked, so it has
		// left the pre-agent state set this sweep lists on. At Error, and naming
		// what was abandoned, because reclaiming it is now a manual act — see the
		// ordering note on this function.
		//
		// Only a branch `ours` already cleared is named as reclaimable. When it
		// is false the branch is a PR head, or one carrying work not on the base,
		// and strandedBranchIsOurs has just decided this reaper must not delete
		// it — an operator told to reclaim it by hand would undo that judgement
		// with the one command it exists to prevent.
		event := l.logger.Error().Err(err).
			Str("session", sess.ID).
			Str("repo", repo.LocalPath)
		if ours {
			event.Str("branch", sess.BranchName).
				Msg("stranded-bootstrap reap: worktree purge did not run; abandoning this session's worktree and branch — no later sweep revisits a Blocked row, so reclaim them by hand")
		} else {
			// Deliberately does not restate WHY. strandedBranchIsOurs declines on
			// four distinct grounds — a PR head, a branch equal to (or without) a
			// base, an ancestry check that could not be resolved, and a branch
			// carrying work not on the base — and only the last two log a reason,
			// so this line can neither name the cause nor point reliably at one
			// that did. Asserting a specific one here would be the same class of
			// error this round has been correcting.
			event.Str("worktree_dir_branch", sess.BranchName).
				Msg("stranded-bootstrap reap: worktree purge did not run; abandoning this session's worktree directory — no later sweep revisits a Blocked row. The branch is left alone deliberately: this reaper judged it not its own to delete")
		}
		return true
	}
	if ours {
		if err := l.worktrees.ReapLocalBranches(ctx, repo.LocalPath, []string{sess.BranchName}); err != nil {
			l.logger.Warn().Err(err).Str("session", sess.ID).Str("branch", sess.BranchName).
				Msg("stranded-bootstrap reap: delete branch failed")
		}
	}
	return false
}

// strandedBranchIsOurs requires recorded fresh-branch ownership and no PR.
// Ancestry is checked against both local and remote base refs so a stale local
// base cannot strand a branch, while a branch with unique work is preserved.
func (l *Lifecycle) strandedBranchIsOurs(ctx context.Context, sess *models.Session, repo *models.Repo) bool {
	if sess.PRNumber != nil {
		return false
	}
	if sess.WorktreePath == "" || !sess.IsBranchCreated {
		// No `git worktree add` ever landed for this row, so the branch it names
		// is a request, not something this bootstrap made. Debug, not Warn: for
		// the row shape this fires on there is nothing on disk to leak, so it is
		// the reaper working as intended rather than a condition to chase.
		l.logger.Debug().Str("session", sess.ID).Str("branch", sess.BranchName).
			Msg("stranded-bootstrap reap: no recorded worktree, so this branch was never ours to delete; leaving it in place")
		return false
	}
	base := sess.BaseBranch
	if base == "" {
		base = repo.DefaultBaseBranch
	}
	if base == "" || sess.BranchName == base {
		return false
	}

	checked := false
	for _, ref := range []string{base, "refs/remotes/origin/" + base} {
		safe, err := l.worktrees.BranchSafeToDelete(ctx, repo.LocalPath, sess.BranchName, ref)
		if err != nil {
			// A missing ref is expected for one of the two (a repo with no
			// origin, or a base never fetched); only both failing is unknown.
			l.logger.Debug().Err(err).Str("session", sess.ID).Str("base_ref", ref).
				Msg("stranded-bootstrap reap: ancestry check failed for this base ref")
			continue
		}
		checked = true
		if safe {
			return true
		}
	}

	if !checked {
		l.logger.Warn().Str("session", sess.ID).Str("branch", sess.BranchName).
			Msg("stranded-bootstrap reap: branch safety could not be determined; leaving the branch in place")
		return false
	}
	l.logger.Warn().Str("session", sess.ID).Str("branch", sess.BranchName).
		Msg("stranded-bootstrap reap: branch carries work not on the base; leaving it in place")
	return false
}
