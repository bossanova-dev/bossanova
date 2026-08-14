package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/safego"
	"github.com/recurser/bossalib/sessionreason"

	"github.com/recurser/bossd/internal/db"
)

// backgroundDraftPRTimeout bounds a single background draft-PR create. The
// measured cost of the create is ~12s of intrinsic work (push + fetch + `gh pr
// create`), but a degraded GitHub window has stretched it to 64s, so the bound
// is generous: it exists only so a wedged network call cannot pin a goroutine
// (and the session's in-flight marker) forever.
const backgroundDraftPRTimeout = 10 * time.Minute

// backgroundDraftPRStateWriteTimeout bounds the small blocked_reason writes that
// record how the background step ENDED. They are deliberately short: they are
// single-row SQLite updates, and the only reason they exist is to guarantee the
// session never keeps advertising a PR that is not coming.
const backgroundDraftPRStateWriteTimeout = 15 * time.Second

// stateWriteContext returns the context the terminal blocked_reason writes must
// use. It is detached from the create's own context on purpose: the two ways a
// create ends badly — the backgroundDraftPRTimeout firing, and
// CancelBackgroundDraftPR aborting it — both leave that context Done, so
// reusing it would make the very update that clears the in-flight marker fail.
// The session would then advertise "draft PR creation in progress" forever,
// with no PR, no failure, and nothing to retry: the one state the marker must
// never get stuck in.
func stateWriteContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), backgroundDraftPRStateWriteTimeout)
}

// backgroundDraftPRHandle is one registry entry: the channel that closes when
// the step finishes, plus the cancel that aborts its in-flight git/GitHub calls.
// Holding the cancel is what lets cleanupFailedCreateSession stop a create it is
// about to invalidate instead of waiting out a push it does not want.
type backgroundDraftPRHandle struct {
	done   chan struct{}
	cancel context.CancelFunc
}

// startBackgroundDraftPR moves the default-path draft-PR create off
// StartSession's synchronous return path (BOS-540). The session is attachable
// and the agent is already running by the time this is called; nothing about the
// agent's work needs the PR to exist, so making the caller wait ~64s for GitHub
// was pure latency.
//
// Context: the ctx handed to StartSession belongs to the CreateSession request
// stream and is cancelled the moment that RPC returns — which is now BEFORE the
// create finishes. Inheriting it would cancel every background create mid-push
// and leave exactly the half-created PR this must avoid. So the background step
// runs on context.WithoutCancel (keeping request-scoped values for logging /
// tracing) under its own timeout. The cost of detaching is that daemon shutdown
// no longer cancels the create implicitly, which is why the step is *tracked*:
// WaitForBackgroundDraftPRs lets shutdown join in-flight creates and, on
// timeout, name the sessions it is abandoning instead of stranding them
// silently.
//
// The returned channel closes when the background step has finished (including
// its failure handling). StartSession ignores it — that is the point — but it is
// the same channel WaitForBackgroundDraftPR hands to callers that do need to
// join, so the step is never fire-and-forget: its completion is always
// observable through the registry.
func (l *Lifecycle) startBackgroundDraftPR(ctx context.Context, sessionID, worktreePath, branchName string) <-chan struct{} {
	// Publish "a PR is coming" before the goroutine starts, so a client that
	// re-reads the session the instant CreateSession returns already sees the
	// in-flight marker rather than a bare missing PR. The write is detached from
	// the request ctx: on the client-disconnect path that ctx is already
	// cancelled, and that is exactly the case where the marker matters most.
	markerCtx, markerCancel := stateWriteContext(ctx)
	l.setDraftPRInFlightReason(markerCtx, sessionID)
	markerCancel()

	bgCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), backgroundDraftPRTimeout)
	done, release := l.trackBackgroundDraftPR(sessionID, cancel)
	safego.Go(l.logger, func() {
		// Defers run LIFO, so these are registered in reverse of the order they
		// run: the panic guard first, then cancel, then release last — a waiter
		// that wakes on the released channel is entitled to observe every write
		// this step made, including the guard's.
		defer release()
		defer cancel()

		// safego.Go RECOVERS panics rather than crashing the process, so without
		// this a panic anywhere under runBackgroundDraftPR (the provider, the
		// worktree manager) would unwind straight past every failure handler and
		// leave the in-flight marker set forever — a session advertising a PR
		// that is not coming, with no failure and nothing to retry, surviving
		// until the next daemon boot. Recording a failure is the honest end
		// state: the branch is preserved and the PR is retryable.
		completed := false
		defer func() {
			if !completed {
				l.failInFlightDraftPR(bgCtx, sessionID, errors.New("draft PR creation panicked"))
			}
		}()

		l.runBackgroundDraftPR(bgCtx, sessionID, worktreePath, branchName)
		completed = true
	})
	return done
}

// runBackgroundDraftPR is the body of the tracked background step. It re-reads
// the session and repo rather than reusing StartSession's copies: the row is the
// source of truth, it may have changed while the agent was starting (a rename
// must reach the PR title), and re-reading keeps the caller's *models.Session
// out of this goroutine entirely, so nothing here can race a reader that still
// holds it.
func (l *Lifecycle) runBackgroundDraftPR(ctx context.Context, sessionID, worktreePath, branchName string) {
	session, err := l.sessions.Get(ctx, sessionID)
	if err != nil {
		// Two shapes, and they must not be confused. Either the step was
		// cancelled/timed out before it began (a teardown calling
		// StopBackgroundDraftPR, or a wedged predecessor), or the session really
		// was deleted while we were starting up. Only the latter is "gone";
		// report the former as the abort it is, so a log reader is not sent
		// hunting for a missing row.
		//
		// The abort path still has to clear the marker. Two of the four
		// teardowns that cancel — ArchiveSession and hardDeleteSession's archive
		// — PRESERVE the row, and ClearStaleDraftPRInFlightReasons scans active
		// sessions only, so an archived session would otherwise carry "a PR is
		// coming" forever and resurrect still claiming it. The clear runs on its
		// own detached context precisely because this one is Done.
		if ctxErr := ctx.Err(); ctxErr != nil {
			l.logger.Info().Err(ctxErr).
				Str("session", sessionID).
				Str("branch", branchName).
				Msg("background draft PR aborted before it started")
			writeCtx, cancel := stateWriteContext(ctx)
			defer cancel()
			l.clearInFlightMarker(writeCtx, sessionID)
			return
		}
		l.logger.Warn().Err(err).
			Str("session", sessionID).
			Str("branch", branchName).
			Msg("skipping background draft PR: session is gone")
		return
	}

	// Re-check under the fresh read: finalize's EnsurePR, SubmitPR, or the
	// reconciler may have opened or attached a PR while this step was queued.
	// This is the cheap half of the interlock; the expensive half is
	// openDraftPRForBranch's ErrPRAlreadyExists attach, which covers the
	// remaining window where the other writer lands *during* our create.
	if session.PRNumber != nil {
		writeCtx, cancel := stateWriteContext(ctx)
		l.clearInFlightDraftPRReason(writeCtx, sessionID, session)
		cancel()
		l.logger.Info().
			Str("session", sessionID).
			Int("prNumber", *session.PRNumber).
			Msg("background draft PR skipped: session already has a PR")
		return
	}

	repo, err := l.repos.Get(ctx, session.RepoID)
	if err != nil {
		wrapped := fmt.Errorf("get repo for background draft PR: %w", err)
		l.failInFlightDraftPR(ctx, sessionID, wrapped)
		l.logger.Warn().Err(wrapped).
			Str("session", sessionID).
			Str("branch", branchName).
			Msg("draft PR creation failed during session start; branch is preserved and retryable")
		return
	}

	if err := l.createDraftPR(ctx, sessionID, worktreePath, branchName, session, repo); err != nil {
		// Same failure handling as the old synchronous call site, just later:
		// the blocked reason replaces the in-flight marker so the session reads
		// as "failed" rather than "still coming", and createDraftPR has already
		// logged the branch debug snapshot for the PR-open failures.
		l.failInFlightDraftPR(ctx, sessionID, err)
		l.logger.Warn().Err(err).
			Str("session", sessionID).
			Str("branch", branchName).
			Msg("draft PR creation failed during session start; branch is preserved and retryable")
		return
	}

	prNumber := 0
	if session.PRNumber != nil {
		prNumber = *session.PRNumber
	}
	l.logger.Info().
		Str("session", sessionID).
		Str("branch", branchName).
		Int("prNumber", prNumber).
		Msg("background draft PR created")
}

// failInFlightDraftPR replaces the in-flight marker with the failure reason for
// err. Every terminal failure path in runBackgroundDraftPR goes through here
// rather than calling setDraftPRBlockedReason directly, for two reasons.
//
// First, the context: the write must not go through the create's own, which is
// already Done on exactly the paths that need it (the timeout, a cancel). See
// stateWriteContext.
//
// Second, ownership. setDraftPRBlockedReason is an unconditional write, and this
// step's failure is decided up to a full push + fetch + `gh pr create` after it
// started — so by the time it lands, the reason may no longer be ours to set.
// Two ways that happens, both silent and both wrong:
//
//   - A PR arrived meanwhile (finalize's EnsurePR raced us and won). Our failure
//     is moot, and writing it would render "? PR failed" forever on a session
//     that has its PR — that label sits near the top of the display cascade.
//   - The session's own run failed during the window and recorded a real block.
//     Overwriting it swaps the true diagnostic for a PR-creation one.
//
// This is the failure-path twin of the re-read in clearDraftPRBlockedReason; the
// two exits of this step have to be guarded the same way.
func (l *Lifecycle) failInFlightDraftPR(ctx context.Context, sessionID string, err error) {
	writeCtx, cancel := stateWriteContext(ctx)
	defer cancel()

	// A cancel is a deliberate teardown (StopBackgroundDraftPR), not a failure.
	// Recording one would leave an archived session reading "draft PR creation
	// failed: … context canceled", which satisfies IsDraftPRCreationFailure and
	// so renders "? PR failed" near the top of the display cascade for a session
	// nobody asked to have a PR. Clear the marker instead.
	//
	// Detect this from the STEP'S CONTEXT, not from err. Cancelling kills the
	// git/gh subprocess (exec.CommandContext), so what surfaces is an
	// *exec.ExitError — "signal: killed" — which does NOT satisfy
	// errors.Is(err, context.Canceled). The context is the only reliable signal,
	// and it also distinguishes the case that must still be recorded: the
	// backgroundDraftPRTimeout leaves DeadlineExceeded, not Canceled, so a
	// wedged create is still reported as the failure it is. Deliberately NOT
	// also matching on err: should a callee ever grow an inner cancellable
	// context, an err-based match would start reading real failures as
	// teardowns and drop their diagnostic.
	if errors.Is(ctx.Err(), context.Canceled) {
		l.logger.Info().Err(err).
			Str("session", sessionID).
			Msg("background draft PR cancelled by teardown; not recording a failure")
		l.clearInFlightMarker(writeCtx, sessionID)
		return
	}

	// A failed read falls through to the write: that is the behaviour this had
	// before the guard, and losing the failure reason entirely is worse than
	// risking an overwrite that is only possible in a race.
	if current, getErr := l.sessions.Get(writeCtx, sessionID); getErr != nil {
		l.logger.Warn().Err(getErr).
			Str("session", sessionID).
			Msg("could not re-read session before recording a draft PR failure; recording it anyway")
	} else if current.PRNumber != nil {
		l.logger.Info().Err(err).
			Str("session", sessionID).
			Int("prNumber", *current.PRNumber).
			Msg("background draft PR failed but the session already has a PR; not recording a failure")
		// Clear rather than bare-return. The attacher may have left the marker
		// behind — the reconciler decides from its own snapshot, which can
		// predate the marker being written — and declining to record a failure
		// must not turn into leaving "a PR is coming" on a session that has one
		// and has no create left to come. This is the only in-process exit that
		// could otherwise strand it.
		l.clearInFlightDraftPRReason(writeCtx, sessionID, current)
		return
	} else if current.BlockedReason != nil && !isClearableDraftPRReason(current.BlockedReason) {
		l.logger.Warn().Err(err).
			Str("session", sessionID).
			Str("blockedReason", *current.BlockedReason).
			Msg("background draft PR failed but the session is blocked for another reason; leaving it in place")
		return
	}

	l.setDraftPRBlockedReason(writeCtx, sessionID, err)
}

// setDraftPRInFlightReason marks the session as "a draft PR is being created
// right now". It is deliberately a different string from the failure reason so
// the two are distinguishable by anything reading blocked_reason: a user who no
// longer sees a PR immediately can tell "one is coming" from "this failed". Only
// the failure form drives the "? PR failed" display label.
func (l *Lifecycle) setDraftPRInFlightReason(ctx context.Context, sessionID string) {
	reason := sessionreason.DraftPRCreationInFlight()
	reasonPtr := &reason
	if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		BlockedReason: &reasonPtr,
	}); err != nil {
		l.logger.Warn().Err(err).
			Str("session", sessionID).
			Msg("failed to persist draft PR in-flight marker")
	}
}

// clearInFlightMarker reads the row and drops the in-flight marker. On a read
// failure it still ATTEMPTS the clear, from the marker this step knows it wrote,
// and says so: a silent no-op here is precisely how the marker gets stranded on
// a row that survives teardown, which the active-sessions-only startup sweep
// cannot reach.
func (l *Lifecycle) clearInFlightMarker(writeCtx context.Context, sessionID string) {
	current, err := l.sessions.Get(writeCtx, sessionID)
	if err != nil {
		l.logger.Warn().Err(err).
			Str("session", sessionID).
			Msg("could not read session to clear the draft PR in-flight marker; clearing from the marker this step wrote")
		inFlight := sessionreason.DraftPRCreationInFlight()
		current = &models.Session{ID: sessionID, BlockedReason: &inFlight}
	}
	l.clearInFlightDraftPRReason(writeCtx, sessionID, current)
}

// clearInFlightDraftPRReason drops the in-flight marker when the background step
// bows out without creating anything: another writer opened the PR first, or a
// teardown cancelled the step. The success path clears it through
// openDraftPRForBranch's clearDraftPRBlockedReason instead.
//
// CONTEXT OWNERSHIP: writeCtx must ALREADY be a stateWriteContext. Detaching is
// the caller's job here and in setDraftPRBlockedReason, so it happens exactly
// once per path — detaching again inside would strip the outer deadline
// (WithoutCancel on an already-detached context) and silently double the write
// budget. Every call site in this file wraps before calling.
//
// Like its sibling clearDraftPRBlockedReason, this re-reads before writing: the
// clear is an unconditional `blocked_reason = NULL`, and the caller's snapshot
// can be a whole create old, so deciding from it alone could erase a block
// another owner recorded during the window.
func (l *Lifecycle) clearInFlightDraftPRReason(writeCtx context.Context, sessionID string, session *models.Session) {
	if !sessionreason.IsDraftPRCreationInFlight(session.BlockedReason) {
		return
	}
	if current, err := l.sessions.Get(writeCtx, sessionID); err != nil {
		l.logger.Warn().Err(err).
			Str("session", sessionID).
			Msg("could not re-read session before clearing the draft PR in-flight marker; clearing from the caller's copy")
	} else if !sessionreason.IsDraftPRCreationInFlight(current.BlockedReason) {
		session.BlockedReason = current.BlockedReason
		return
	}
	var cleared *string
	updated, err := l.sessions.Update(writeCtx, sessionID, db.UpdateSessionParams{
		BlockedReason: &cleared,
	})
	if err != nil {
		l.logger.Warn().Err(err).
			Str("session", sessionID).
			Msg("failed to clear draft PR in-flight marker")
		return
	}
	// Keep the caller's copy honest, matching clearDraftPRBlockedReason. The one
	// caller today returns immediately, but an out-of-date snapshot is exactly
	// the class of bug this file has already had once.
	session.BlockedReason = updated.BlockedReason
}

// ClearStaleDraftPRInFlightReasons clears the background step's in-flight marker
// from every active session that still carries one, and returns the session ids
// it cleared.
//
// Call this ONCE at daemon startup, before any session can be started. That
// timing is the whole correctness argument: the marker only ever means "a
// goroutine in THIS process is opening a PR right now", so a marker present
// before this process has started anything is necessarily a leftover from a
// daemon that died mid-create. Every in-process exit of the step replaces or
// clears the marker itself (see failInFlightDraftPR), so a hard restart is the
// only way one survives — and without this sweep it survives forever, leaving
// the session advertising a PR that nothing will ever create. Calling it later,
// concurrently with live creates, would wrongly clear a marker that is still
// true.
//
// The marker is dropped rather than converted to a failure: the branch and its
// placeholder commit are still on the remote, so the periodic PR-association
// reconcile can still attach a PR if one was in fact opened. A bare "no PR yet"
// session is honest; "creation in progress" is not.
//
// The sweep covers active sessions only. A session archived mid-create keeps its
// marker across a restart and would resurrect still claiming a PR is coming —
// cosmetic, and narrow enough not to justify scanning archived rows on every
// boot.
func (l *Lifecycle) ClearStaleDraftPRInFlightReasons(ctx context.Context) ([]string, error) {
	sessions, err := l.sessions.ListActive(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("list active sessions: %w", err)
	}

	var cleared []string
	for _, sess := range sessions {
		if !sessionreason.IsDraftPRCreationInFlight(sess.BlockedReason) {
			continue
		}
		var empty *string
		if _, err := l.sessions.Update(ctx, sess.ID, db.UpdateSessionParams{
			BlockedReason: &empty,
		}); err != nil {
			l.logger.Warn().Err(err).
				Str("session", sess.ID).
				Msg("failed to clear stale draft PR in-flight marker")
			continue
		}
		cleared = append(cleared, sess.ID)
	}
	return cleared, nil
}

// trackBackgroundDraftPR registers an in-flight background create for sessionID
// and returns the channel that closes when it finishes, plus the release func
// that closes it.
//
// Registration happens BEFORE the goroutine is spawned. safego.Go's own done
// channel cannot serve as the registry handle without a race — the goroutine can
// finish before the returned channel is stored, leaving an entry nothing ever
// removes — so the step owns a channel that is registered first and closed from
// a defer inside the goroutine body (which safego runs before closing its own).
func (l *Lifecycle) trackBackgroundDraftPR(sessionID string, cancel context.CancelFunc) (<-chan struct{}, func()) {
	handle := &backgroundDraftPRHandle{done: make(chan struct{}), cancel: cancel}

	l.backgroundDraftPRMu.Lock()
	if l.backgroundDraftPRs == nil {
		l.backgroundDraftPRs = make(map[string]*backgroundDraftPRHandle)
	}
	l.backgroundDraftPRs[sessionID] = handle
	l.backgroundDraftPRMu.Unlock()

	return handle.done, func() {
		l.backgroundDraftPRMu.Lock()
		// Only deregister our own entry: a second StartSession for the same
		// session id (resurrect/retry) may already have replaced it.
		if current, ok := l.backgroundDraftPRs[sessionID]; ok && current == handle {
			delete(l.backgroundDraftPRs, sessionID)
		}
		l.backgroundDraftPRMu.Unlock()
		close(handle.done)
	}
}

// hasBackgroundDraftPR reports whether a background draft-PR create is
// registered for sessionID right now.
//
// It answers a question WaitForBackgroundDraftPR cannot: that call returns
// immediately both when a create has finished AND when none was ever started,
// so a caller that must not act while one is live gets the same "go ahead" in
// either case. The draft-PR retry sweep needs the distinction — pushing the same
// branch from two places is the failure its interlocks exist to prevent — and
// tests need it to assert the negative case (a DeferPR session starts no step at
// all).
//
// Deliberately on the existing backgroundDraftPRMu rather than a second lock
// over the same map: two mutexes guarding one map is a data race waiting to be
// written, not extra safety.
func (l *Lifecycle) hasBackgroundDraftPR(sessionID string) bool {
	l.backgroundDraftPRMu.Lock()
	defer l.backgroundDraftPRMu.Unlock()
	_, ok := l.backgroundDraftPRs[sessionID]
	return ok
}

// CancelBackgroundDraftPR aborts the in-flight background draft-PR create for
// sessionID, if any. It does NOT wait — pair it with WaitForBackgroundDraftPR
// when the caller needs the step to have stopped touching the branch.
//
// Cancelling is what keeps the join cheap. The step deliberately runs on a
// context detached from the request that started it, so without this a caller
// that must interlock with it (cleanupFailedCreateSession, tearing down the very
// branch the step is pushing) could only wait out a full push + `gh pr create`.
// The step's own terminal blocked_reason writes are immune, because they run on
// stateWriteContext rather than the cancelled one.
func (l *Lifecycle) CancelBackgroundDraftPR(sessionID string) {
	l.backgroundDraftPRMu.Lock()
	handle, ok := l.backgroundDraftPRs[sessionID]
	l.backgroundDraftPRMu.Unlock()
	if ok {
		handle.cancel()
	}
}

// stopBackgroundDraftPRTimeout bounds StopBackgroundDraftPR's join. The step has
// already been cancelled by then, so this only covers the unwind of an in-flight
// git/GitHub call, not its natural duration.
const stopBackgroundDraftPRTimeout = 10 * time.Second

// StopBackgroundDraftPR cancels sessionID's in-flight background draft-PR create
// and waits for it to unwind. Call it from every path that tears down a
// session's branch, worktree, or row.
//
// Before BOS-540 no such path could collide with the create, because
// CreateSession did not return until the PR existed. Now the RPC hands the
// client a session handle mid-create, so a remove, an archive, or a hard-delete
// issued seconds later races a goroutine that is about to `git push` the branch
// and open a PR for it — leaving an orphan PR and a remote branch behind a
// session row that no longer exists, with nothing to reconcile them.
//
// It is a no-op when nothing is in flight, which is the overwhelmingly common
// case (an established session, or a DeferPR one that never started a step), so
// it is safe to call unconditionally.
func (l *Lifecycle) StopBackgroundDraftPR(ctx context.Context, sessionID string) {
	l.CancelBackgroundDraftPR(sessionID)
	joinCtx, cancel := context.WithTimeout(ctx, stopBackgroundDraftPRTimeout)
	defer cancel()
	if err := l.WaitForBackgroundDraftPR(joinCtx, sessionID); err != nil {
		l.logger.Warn().Err(err).
			Str("session", sessionID).
			Msg("tearing down a session whose draft PR is still being opened; a PR or remote branch may be left behind")
	}
}

// CancelBackgroundDraftPRs aborts every in-flight background draft-PR create.
// Daemon shutdown calls it after WaitForBackgroundDraftPRs has given up, so the
// stragglers stop before the process tears the database out from under them
// rather than racing the close.
func (l *Lifecycle) CancelBackgroundDraftPRs() {
	l.backgroundDraftPRMu.Lock()
	handles := make([]*backgroundDraftPRHandle, 0, len(l.backgroundDraftPRs))
	for _, handle := range l.backgroundDraftPRs {
		handles = append(handles, handle)
	}
	l.backgroundDraftPRMu.Unlock()
	for _, handle := range handles {
		handle.cancel()
	}
}

// WaitForBackgroundDraftPR blocks until the background draft-PR create for
// sessionID has finished, or ctx is done. It returns immediately when no create
// is in flight, so it is safe to call for a DeferPR session or one that already
// had a PR.
func (l *Lifecycle) WaitForBackgroundDraftPR(ctx context.Context, sessionID string) error {
	l.backgroundDraftPRMu.Lock()
	handle, ok := l.backgroundDraftPRs[sessionID]
	l.backgroundDraftPRMu.Unlock()
	if !ok {
		return nil
	}
	select {
	case <-handle.done:
		return nil
	case <-ctx.Done():
		// A select with two ready cases picks at random, so re-check before
		// reporting a timeout: a create that finished in the same instant the
		// deadline expired has finished, and callers turn this error into a
		// test failure or a shutdown warning.
		select {
		case <-handle.done:
			return nil
		default:
			return ctx.Err()
		}
	}
}

// WaitForBackgroundDraftPRs blocks until every in-flight background draft-PR
// create has finished, or ctx is done. It returns the session ids still in
// flight when it gave up — empty on a clean drain.
//
// Daemon shutdown calls this so an interrupted create is *named* in the logs
// rather than silently abandoned: the branch and any placeholder commit survive
// on the remote, so the next reconcile pass (or a manual retry) can pick the PR
// up. Shutdown cancels the stragglers immediately afterwards, which routes them
// through the cancel-as-teardown branch and clears their in-flight marker — the
// session is left honestly PR-less rather than still advertising one.
func (l *Lifecycle) WaitForBackgroundDraftPRs(ctx context.Context) []string {
	l.backgroundDraftPRMu.Lock()
	pending := make(map[string]*backgroundDraftPRHandle, len(l.backgroundDraftPRs))
	for id, handle := range l.backgroundDraftPRs {
		pending[id] = handle
	}
	l.backgroundDraftPRMu.Unlock()

	var abandoned []string
	for id, handle := range pending {
		select {
		case <-handle.done:
		case <-ctx.Done():
			// Re-check rather than trust the random pick between two ready
			// cases; see WaitForBackgroundDraftPR.
			select {
			case <-handle.done:
			default:
				abandoned = append(abandoned, id)
			}
		}
	}
	return abandoned
}
