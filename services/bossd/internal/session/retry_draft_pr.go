package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sessionreason"
)

// Bounds on the draft-PR retry sweep. Together they cap the worst case at
// draftPRRetryMaxPerSweep pushes per tick across the WHOLE daemon, and a single
// genuinely-terminal session at draftPRRetryMaxAttempts attempts over roughly
// draftPRRetryCooldown × draftPRRetryMaxAttempts before it goes quiet. Raising
// either without a reason trades a bounded repair for unbounded GitHub rate
// pressure during exactly the degradation window that produced the failures.
const (
	// draftPRRetryCooldown is the minimum gap between two retries of the same
	// session by this process.
	draftPRRetryCooldown = 5 * time.Minute
	// draftPRRetryMaxAttempts is how many times this process will retry one
	// session before leaving it alone. The blocked reason stays in place either
	// way, so a session that exhausts its budget is quiet, not hidden.
	draftPRRetryMaxAttempts = 5
	// draftPRRetryMaxPerSweep caps the work one tick may do. A degradation
	// window produces a BURST of failed sessions, so the remainder is picked up
	// by the next tick rather than fired at GitHub all at once.
	draftPRRetryMaxPerSweep = 3
)

// draftPRRetryState is one session's rate-limiter entry. It lives in memory on
// the Lifecycle (see the draftPRRetries field) and is intentionally lost on
// daemon restart.
type draftPRRetryState struct {
	Attempts      int
	LastAttemptAt time.Time
}

// RetryFailedDraftPRsPeriodic re-attempts PR creation for active sessions whose
// draft PR failed to open. background_draft_pr.go logs that such a branch "is
// preserved and retryable" — this sweep is what makes that true. Without it a
// session carries its failure for the whole run, showing as PR-less to
// boss-epic/boss-build, until finalize eventually calls the idempotent EnsurePR.
//
// The name and (int, error) shape match the sibling sweeps
// RecoverStrandedCronSessionsPeriodic and ReapStrandedBootstrapSessionsPeriodic
// so main.go's tick bodies stay uniform.
//
// Selection requires BOTH that the session has no PR number and that its blocked
// reason is a draft-PR creation FAILURE. Each conjunct rules out a different
// writer: a set PRNumber means someone already attached one, and the in-flight
// marker is a distinct string (sessionreason.DraftPRCreationInFlight), so a
// create that is still running never satisfies the predicate.
//
// SINGLE CALLER by design (main.go's 60s tick). Eligibility is read under
// draftPRRetryMu and the attempt is stamped under a second acquisition, with a
// git subprocess in between, so two concurrent sweeps could both clear the same
// session's cooldown and double-push it. Serialise here — do not add a second
// caller without an outer lock.
//
// Returns the number of sessions retried — attempts, not successes, since a
// failed attempt still consumed the tick's budget. A per-session EnsurePR error
// is logged and the loop continues (one bad repo must not block the rest); only
// the initial list error aborts the sweep.
func (l *Lifecycle) RetryFailedDraftPRsPeriodic(ctx context.Context) (int, error) {
	// Going quiet on shutdown starts here, not at the loop: ListActive would
	// otherwise fail on the cancelled context and main.go would log the sweep
	// as FAILED at exactly the moment there is nothing wrong with it.
	if ctx.Err() != nil {
		return 0, nil
	}
	sessions, err := l.sessions.ListActive(ctx, "")
	if err != nil {
		// Same reasoning for the cancellation that lands mid-query.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return 0, nil
		}
		return 0, fmt.Errorf("list active sessions: %w", err)
	}

	ensure := l.draftPREnsurer
	if ensure == nil {
		ensure = l.EnsurePR
	}

	now := l.now()
	retried := 0
	for _, sess := range sessions {
		// Stop on shutdown. Everything below this line either spawns a git
		// subprocess or calls out to GitHub, and both fail instantly once
		// pollerCtx is cancelled — so without this the last sweep of the
		// daemon's life turns into one warning per eligible session, logged at
		// exactly the moment the shutdown output matters. The per-sweep cap
		// does not bound that burst: a session skipped by the guard below never
		// increments retried.
		if ctx.Err() != nil {
			break
		}
		if sess == nil {
			continue
		}
		// Bookkeeping first, and for EVERY session rather than only until the
		// per-sweep cap is reached: a session that has since acquired a PR (from
		// this sweep, from finalize, or from the association reconcile that runs
		// just before us on the same tick) must drop its entry so a LATER,
		// unrelated failure on the same id starts from a full budget. This costs
		// a map lookup and no I/O, so doing it past the cap is free.
		if sess.PRNumber != nil {
			l.clearDraftPRRetryState(sess.ID)
			continue
		}
		if retried >= draftPRRetryMaxPerSweep {
			continue
		}
		if !l.shouldRetryDraftPR(sess, now) {
			continue
		}

		// A retry can only succeed against a branch that carries a commit.
		// EnsurePR deliberately makes no placeholder commit of its own, and
		// GitHub rejects a PR whose head is not ahead of base ("No commits
		// between"). Both pre-existing EnsurePR callers guard for exactly this
		// (finalize.go runs cleanBranchHasCommittedWork before each of its two
		// calls), and createDraftPR reaches its own EmptyCommit only AFTER the
		// origin lookup and the branch verification — so every failure raised
		// before that point parks a draft-PR-failure reason on a session whose
		// branch is still empty. Those are precisely the sessions this sweep
		// selects, which is why the guard belongs here too.
		//
		// Skip rather than call out, and skip WITHOUT spending an attempt. An
		// unwinnable call does not merely waste budget: EnsurePR's failure path
		// rewrites BlockedReason, replacing the diagnostic naming the real cause
		// ("origin lookup failed", say) with a GitHub "No commits between"
		// message that misdirects whoever debugs the session. Nor is skipping a
		// lost repair — the branch gains its commit the moment the agent commits
		// real work, and the next sweep then succeeds; a run that ends with
		// nothing committed is finalize's placeholder-commit path, not ours.
		//
		// fetchBase is false on purpose. A stale local base can only make a
		// branch look FURTHER ahead, never falsely empty, so the local ref
		// answers this question correctly without a network fetch per tick. The
		// check is a local git call per eligible session and is NOT charged
		// against draftPRRetryMaxPerSweep — that cap bounds pushes to GitHub,
		// and starving a session that does have work behind ones that do not
		// would defeat the sweep.
		// Two ways the check itself cannot be answered, and both spend an
		// attempt even though no PR call is made. The budget bounds how long a
		// session may keep the sweep busy, not how many times EnsurePR ran; a
		// row we cannot evaluate is a structural problem that will fail
		// identically on every tick, so leaving it free would trade the plan's
		// "burns its attempts and then goes quiet" for a warning once a minute
		// for the rest of the daemon's life. A branch that is merely not ready
		// yet is the opposite case and stays free, below.
		//
		// The missing-field cases are checked here rather than left to the
		// helper. An empty worktree path matters most: git.Manager.IsAncestor
		// assigns it straight to cmd.Dir, so it would run `git merge-base
		// --is-ancestor` in BOSSD'S OWN working directory and answer
		// confidently about the wrong repository. An empty branch name is
		// checked here too because cleanBranchHasCommittedWork reports it as
		// (false, nil) — indistinguishable from a real branch that simply has
		// no commit yet, which would take the free-skip path below and never
		// go quiet.
		if sess.WorktreePath == "" || sess.BranchName == "" {
			l.recordDraftPRRetryAttempt(sess.ID, now)
			l.logger.Warn().
				Str("session", sess.ID).
				Str("branch", sess.BranchName).
				Str("worktree", sess.WorktreePath).
				Msg("draft PR retry: session is missing the branch or worktree needed to check for committed work")
			continue
		}
		hasCommittedWork, err := l.cleanBranchHasCommittedWork(ctx, sess, false)
		if err != nil {
			l.recordDraftPRRetryAttempt(sess.ID, now)
			l.logger.Warn().Err(err).
				Str("session", sess.ID).
				Str("branch", sess.BranchName).
				Msg("draft PR retry: committed-work lookup failed; leaving the session alone")
			continue
		}
		if !hasCommittedWork {
			l.logger.Debug().
				Str("session", sess.ID).
				Str("branch", sess.BranchName).
				Msg("draft PR retry: branch carries no commit yet; skipping without spending an attempt")
			continue
		}

		// Stamp the attempt BEFORE calling out. The cooldown must start from
		// when we tried, not from whether it worked — otherwise a long-failing
		// EnsurePR could be re-entered by the next tick with no gap at all.
		l.recordDraftPRRetryAttempt(sess.ID, now)
		retried++

		l.logger.Info().
			Str("session", sess.ID).
			Str("branch", sess.BranchName).
			Msg("retrying draft PR creation for a session whose create failed")

		if err := ensure(ctx, sess.ID); err != nil {
			// Log and continue. EnsurePR has already refreshed the blocked
			// reason, so the session still reads as failed and the next sweep
			// past the cooldown will pick it up again until the budget runs out.
			l.logger.Warn().Err(err).
				Str("session", sess.ID).
				Str("branch", sess.BranchName).
				Msg("draft PR retry failed; leaving the session for a later sweep")
			continue
		}

		l.logger.Info().
			Str("session", sess.ID).
			Str("branch", sess.BranchName).
			Msg("draft PR retry succeeded")
		l.clearDraftPRRetryState(sess.ID)
	}

	return retried, nil
}

// shouldRetryDraftPR reports whether the sweep may re-attempt PR creation for
// sess at now. Split out from the loop so tests can drive the eligibility rules
// directly rather than inferring them from a sweep's return count.
//
// Fail-safe by construction: every ambiguity leaves the session alone. Pushing
// the same branch from two places is the failure this guards against, so the
// in-flight registry check is a SECOND interlock beside the PRNumber test —
// selection reads a list snapshot, and a background create can be registered
// between that read and this call. Do not drop either as redundant; they cover
// different windows.
func (l *Lifecycle) shouldRetryDraftPR(sess *models.Session, now time.Time) bool {
	if sess == nil || sess.PRNumber != nil {
		return false
	}
	if !sessionreason.IsDraftPRCreationFailure(sess.BlockedReason) {
		return false
	}
	if l.hasBackgroundDraftPR(sess.ID) {
		return false
	}

	l.draftPRRetryMu.Lock()
	defer l.draftPRRetryMu.Unlock()
	state, tried := l.draftPRRetries[sess.ID]
	if !tried {
		return true
	}
	if state.Attempts >= draftPRRetryMaxAttempts {
		return false
	}
	return now.Sub(state.LastAttemptAt) >= draftPRRetryCooldown
}

// recordDraftPRRetryAttempt increments sessionID's attempt count and stamps the
// attempt time. Lazily creates the registry so the zero Lifecycle is usable.
func (l *Lifecycle) recordDraftPRRetryAttempt(sessionID string, now time.Time) {
	l.draftPRRetryMu.Lock()
	defer l.draftPRRetryMu.Unlock()
	if l.draftPRRetries == nil {
		l.draftPRRetries = make(map[string]draftPRRetryState)
	}
	state := l.draftPRRetries[sessionID]
	state.Attempts++
	state.LastAttemptAt = now
	l.draftPRRetries[sessionID] = state
}

// clearDraftPRRetryState forgets sessionID's rate-limiter entry, so a later
// failure on the same session starts from a full budget.
func (l *Lifecycle) clearDraftPRRetryState(sessionID string) {
	l.draftPRRetryMu.Lock()
	defer l.draftPRRetryMu.Unlock()
	delete(l.draftPRRetries, sessionID)
}
