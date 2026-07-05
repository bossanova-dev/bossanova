package session

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/safego"
	"github.com/recurser/bossalib/sessionreason"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/status"
)

const (
	webhookHealthyWindow   = 5 * time.Minute
	webhookHealthyInterval = 5 * time.Minute
)

// DisplayPoller periodically polls PR status, checks, and reviews for all
// active sessions with PRs and updates the DisplayTracker with computed display statuses.
type DisplayPoller struct {
	sessions  db.SessionStore
	repos     db.RepoStore
	provider  vcs.Provider
	tracker   *status.DisplayTracker
	snapshots db.CheckSnapshotStore // optional; nil disables persistence
	interval  time.Duration
	logger    zerolog.Logger
	done      chan struct{}

	refreshMu sync.Mutex
	// latestWebhookRefresh maps session ID -> the last time a webhook refreshed
	// that session. A session only backs off its poll interval when it received
	// a webhook recently; sibling sessions in the same repo are unaffected.
	latestWebhookRefresh map[string]time.Time
	lastPollMu           sync.Mutex
	lastPoll             map[string]time.Time
}

// NewDisplayPoller creates a new display status poller.
func NewDisplayPoller(
	sessions db.SessionStore,
	repos db.RepoStore,
	provider vcs.Provider,
	tracker *status.DisplayTracker,
	interval time.Duration,
	logger zerolog.Logger,
) *DisplayPoller {
	return &DisplayPoller{
		sessions: sessions,
		repos:    repos,
		provider: provider,
		tracker:  tracker,
		interval: interval,
		logger:   logger,
		done:     make(chan struct{}),
	}
}

// SetSnapshotStore wires an optional CheckSnapshotStore. When set, every
// successful pollSession persists what the daemon saw + the DisplayStatus
// it computed, so `boss session checks <id>` can show the timeline.
// nil-safe — leaving the store unset disables persistence (handy for
// tests that don't want SQLite writes on every tick).
func (p *DisplayPoller) SetSnapshotStore(s db.CheckSnapshotStore) {
	p.snapshots = s
}

// Run starts the polling loop in a background goroutine. It stops when the
// context is cancelled.
func (p *DisplayPoller) Run(ctx context.Context) {
	safego.Go(p.logger, func() {
		defer close(p.done)

		ticker := time.NewTicker(p.interval)
		defer ticker.Stop()

		// Poll immediately on start for initial state.
		p.poll(ctx)

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.poll(ctx)
			}
		}
	})
}

// Done returns a channel closed when Run's goroutine exits.
func (p *DisplayPoller) Done() <-chan struct{} { return p.done }

func (p *DisplayPoller) recordRefresh(sessionID string, ts time.Time) {
	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()
	if p.latestWebhookRefresh == nil {
		p.latestWebhookRefresh = make(map[string]time.Time)
	}
	if prev, ok := p.latestWebhookRefresh[sessionID]; !ok || ts.After(prev) {
		p.latestWebhookRefresh[sessionID] = ts
	}
}

func (p *DisplayPoller) intervalFor(sessionID string, now time.Time) time.Duration {
	p.refreshMu.Lock()
	last, ok := p.latestWebhookRefresh[sessionID]
	if ok && now.Sub(last) > webhookHealthyWindow {
		delete(p.latestWebhookRefresh, sessionID)
		ok = false
	}
	p.refreshMu.Unlock()
	if ok && now.Sub(last) <= webhookHealthyWindow {
		return webhookHealthyInterval
	}
	return p.interval
}

func (p *DisplayPoller) markPolled(sessionID string, ts time.Time) {
	p.lastPollMu.Lock()
	defer p.lastPollMu.Unlock()
	if p.lastPoll == nil {
		p.lastPoll = make(map[string]time.Time)
	}
	p.lastPoll[sessionID] = ts
}

func (p *DisplayPoller) shouldPollSession(sessionID string, now time.Time) bool {
	interval := p.intervalFor(sessionID, now)

	p.lastPollMu.Lock()
	defer p.lastPollMu.Unlock()
	if last, ok := p.lastPoll[sessionID]; ok && now.Sub(last) < interval {
		return false
	}
	if p.lastPoll == nil {
		p.lastPoll = make(map[string]time.Time)
	}
	p.lastPoll[sessionID] = now
	return true
}

func (p *DisplayPoller) pruneWebhookRefreshes(now time.Time) {
	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()
	for sessionID, last := range p.latestWebhookRefresh {
		if now.Sub(last) > webhookHealthyWindow {
			delete(p.latestWebhookRefresh, sessionID)
		}
	}
}

func (p *DisplayPoller) pruneLastPoll(active map[string]struct{}) {
	p.lastPollMu.Lock()
	defer p.lastPollMu.Unlock()
	for sessionID := range p.lastPoll {
		if _, ok := active[sessionID]; !ok {
			delete(p.lastPoll, sessionID)
		}
	}
}

func (p *DisplayPoller) RefreshPR(ctx context.Context, repoOriginURL string, prNumber int) error {
	now := time.Now()
	repo, err := p.repos.GetByOrigin(ctx, repoOriginURL)
	if err != nil {
		return fmt.Errorf("display poller: get repo by origin %q: %w", repoOriginURL, err)
	}

	sessions, err := p.sessions.ListActive(ctx, repo.ID)
	if err != nil {
		return fmt.Errorf("display poller: list active sessions for repo %q: %w", repo.ID, err)
	}

	refreshed := 0
	refreshedSessions := make([]string, 0, len(sessions))
	for _, sess := range sessions {
		if sess.PRNumber == nil || *sess.PRNumber != prNumber {
			continue
		}
		if entry := p.tracker.Get(sess.ID); entry != nil && isTerminalDisplayStatus(entry.Status) {
			continue
		}
		p.pollSession(ctx, repo, sess.ID, *sess.PRNumber)
		refreshed++
		refreshedSessions = append(refreshedSessions, sess.ID)
	}
	if refreshed > 0 {
		for _, sessionID := range refreshedSessions {
			p.markPolled(sessionID, now)
			p.recordRefresh(sessionID, now)
		}
	}

	p.logger.Info().
		Str("repo_origin_url", repoOriginURL).
		Int("pr_number", prNumber).
		Strs("session_ids", refreshedSessions).
		Int("sessions_refreshed", refreshed).
		Msg("display poller: refresh pr")
	return nil
}

// poll iterates all active sessions with PRs and updates display statuses.
func (p *DisplayPoller) poll(ctx context.Context) {
	now := time.Now()
	p.pruneWebhookRefreshes(now)
	activeSessions := make(map[string]struct{})
	repos, err := p.repos.List(ctx)
	if err != nil {
		p.logger.Error().Err(err).Msg("display poller: list repos")
		return
	}

	for _, repo := range repos {
		sessions, err := p.sessions.ListActive(ctx, repo.ID)
		if err != nil {
			p.logger.Error().Err(err).Str("repo", repo.ID).Msg("display poller: list sessions")
			continue
		}

		for _, sess := range sessions {
			if sess.PRNumber == nil {
				continue
			}
			// Skip terminal PR states — no further polling needed.
			if entry := p.tracker.Get(sess.ID); entry != nil && isTerminalDisplayStatus(entry.Status) {
				continue
			}
			activeSessions[sess.ID] = struct{}{}
			if !p.shouldPollSession(sess.ID, now) {
				continue
			}
			p.pollSession(ctx, repo, sess.ID, *sess.PRNumber)
		}
	}
	p.pruneLastPoll(activeSessions)
}

// pollSession fetches PR status, checks, and reviews for a single session
// and updates the tracker with the computed display status.
func (p *DisplayPoller) pollSession(ctx context.Context, repo *models.Repo, sessionID string, prNumber int) {
	repoPath := repo.OriginURL
	prStatus, err := p.provider.GetPRStatus(ctx, repoPath, prNumber)
	if err != nil {
		p.logger.Warn().Err(err).Str("session", sessionID).Msg("display poller: get PR status")
		return
	}

	p.logger.Info().
		Str("session_id", sessionID).
		Str("repo_origin_url", repoPath).
		Int("pr_number", prNumber).
		Str("pr_state", prStateString(prStatus.State)).
		Bool("pr_draft", prStatus.Draft).
		Msg("display poller: fetched PR status")

	if prStatus.State == vcs.PRStateMerged || prStatus.State == vcs.PRStateClosed {
		// Reconcile a session wedged in Blocked (pollableState excludes Blocked, so
		// the state-machine poller never re-examines it) — or a terminal row still
		// carrying a stale block reason — BEFORE recording the terminal tracker
		// entry. Once the entry is terminal, poll() and RefreshPR() skip the
		// session, so a transient store error inside the reconcile would otherwise
		// never retry until a daemon restart. On error, leave the tracker
		// non-terminal so the next poll cycle retries; the stale "finalize failed"
		// hint would otherwise linger forever (BOS-246).
		if err := p.reconcileTerminalPRForBlockedSession(ctx, sessionID, prStatus); err != nil {
			p.logger.Warn().Err(err).Str("session", sessionID).Msg("display poller: terminal-PR reconcile failed; will retry next poll")
			return
		}
		info := vcs.ComputeDisplayStatus(prStatus, nil, nil)
		info.HeadSHA = prStatus.HeadSHA
		p.tracker.Set(sessionID, info)
		p.persistSnapshot(ctx, sessionID, prStatus, nil, info)
		return
	}

	// Skip checks and reviews for draft PRs — they aren't ready for review
	// so CI results and review comments are not actionable. This saves 2 API
	// calls per draft PR per poll cycle.
	if prStatus.Draft {
		info := vcs.ComputeDisplayStatus(prStatus, nil, nil)
		info.HeadSHA = prStatus.HeadSHA
		p.tracker.Set(sessionID, info)
		p.persistSnapshot(ctx, sessionID, prStatus, nil, info)
		return
	}

	// On any inputs error, skip the update rather than recomputing with empty
	// results. A transient GitHub API blip would otherwise collapse a
	// "Failing" or "Rejected" row to "Idle" / "Passing" — silently
	// disabling the repair plugin (which only triggers on
	// FAILING/CONFLICT/REJECTED). The previous tracker entry sticks; the
	// next poll cycle retries.
	checks, err := p.provider.GetCheckResults(ctx, repoPath, prNumber)
	if err != nil {
		p.logger.Warn().Err(err).Str("session", sessionID).Msg("display poller: get check results; preserving previous status")
		return
	}

	reviews, err := p.provider.GetReviewComments(ctx, repoPath, prNumber)
	if err != nil {
		p.logger.Warn().Err(err).Str("session", sessionID).Msg("display poller: get review comments; preserving previous status")
		return
	}

	info := vcs.ComputeDisplayStatus(prStatus, checks, reviews)
	if repairableConflictBlock(ctx, p.provider, repo, prStatus, p.logger, "display poller") {
		info.Status = vcs.DisplayStatusConflict
	}
	info.HeadSHA = prStatus.HeadSHA
	// Surface mergeability so a conflict-after-green is readable from
	// get_session without a merge attempt (BOS-234). nil stays unknown.
	info.Mergeable = prStatus.Mergeable
	p.tracker.Set(sessionID, info)
	p.persistSnapshot(ctx, sessionID, prStatus, checks, info)

	// A session Blocked by a stale fix-loop exhaustion is never re-examined by
	// the state-machine poller (pollableState excludes Blocked), so once the PR
	// is genuinely clean+green+mergeable nothing clears the block and
	// merge_session stays wedged. The display poller already has live PR state
	// here, so downgrade it (BOS-235 Bug 1, direction 2).
	p.maybeClearStaleFixLoopBlock(ctx, sessionID, prStatus, checks, info)
}

// maybeClearStaleFixLoopBlock auto-unblocks a session sitting in Blocked with
// the FixLoopExhausted reason when the live PR is observed clean + green +
// mergeable. It is deliberately narrow: it only ever touches the
// FixLoopExhausted reason (genuine human-required blocks are left alone), and
// only when the PR is Open, mergeable is a concrete true, no checks are still
// running, and the computed status is a green terminal-ready state
// (Passing or Approved).
func (p *DisplayPoller) maybeClearStaleFixLoopBlock(ctx context.Context, sessionID string, prStatus *vcs.PRStatus, checks []vcs.CheckResult, info vcs.DisplayInfo) {
	if info.Status != vcs.DisplayStatusPassing && info.Status != vcs.DisplayStatusApproved {
		return
	}
	if prStatus.State != vcs.PRStateOpen {
		return
	}
	if prStatus.Mergeable == nil || !*prStatus.Mergeable {
		return
	}
	if anyCheckRunning(checks) {
		return
	}

	sess, err := p.sessions.Get(ctx, sessionID)
	if err != nil {
		p.logger.Warn().Err(err).Str("session", sessionID).Msg("display poller: get session for stale-block check")
		return
	}
	if sess.State != machine.Blocked {
		return
	}
	if sess.BlockedReason == nil || *sess.BlockedReason != sessionreason.FixLoopExhausted() {
		return
	}

	// Fire Unblock on a machine restored to Blocked; actionClearBlocked resets
	// BlockedReason + AttemptCount in the machine, and we persist the same.
	sm := machine.NewWithContext(machine.Blocked, &machine.SessionContext{
		AttemptCount:  sess.AttemptCount,
		MaxAttempts:   machine.MaxAttempts,
		BlockedReason: *sess.BlockedReason,
	})
	if err := sm.FireCtx(ctx, machine.Unblock); err != nil {
		p.logger.Warn().Err(err).Str("session", sessionID).Msg("display poller: fire unblock for stale fix_loop_exhausted block")
		return
	}

	newState := int(sm.State())
	zeroAttempts := 0
	var clearReason *string  // *nil → set blocked_reason NULL
	var clearHeadSHA *string // *nil → set last_attempt_head_sha NULL
	if _, err := p.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		State:              &newState,
		AttemptCount:       &zeroAttempts,
		BlockedReason:      &clearReason,
		LastAttemptHeadSHA: &clearHeadSHA,
	}); err != nil {
		p.logger.Warn().Err(err).Str("session", sessionID).Msg("display poller: persist stale fix_loop_exhausted unblock")
		return
	}

	p.logger.Info().
		Str("session", sessionID).
		Str("new_state", sm.State().String()).
		Msg("display poller: fix_loop_exhausted cleared, unblock fired on clean green PR")
}

// reconcileTerminalPRForBlockedSession reconciles the persisted block metadata of
// a session whose PR the provider now reports resolved (merged or closed). It
// handles two cases and returns an error only when a reconcile it attempted could
// not be persisted, so the caller can decline to record a terminal tracker entry
// and retry on the next poll (a nil return means the row is already clean or was
// reconciled here):
//
//   - Wedged in Blocked: pollableState excludes Blocked, so the state-machine
//     poller never re-examines it. Fire the terminal transition, whose
//     OnExit(actionClearBlocked) resets the block reason + attempt count, and
//     persist the cleared fields alongside the new terminal state.
//   - Already terminal (Merged/Closed) but still carrying a stale block reason —
//     e.g. a pre-fix PRClosed webhook advanced a Blocked session to Closed while
//     the old handler wrote only State. Nothing else revisits such a row (poll /
//     RefreshPR skip terminal tracker entries and pollableState excludes terminal
//     states) and web sessionWarningHints surfaces a bare blockedReason, so the
//     stale hint would linger forever. Clear the residual metadata in place, with
//     no transition (the state is already correct).
//
// It is the merged/closed counterpart of maybeClearStaleFixLoopBlock but is
// deliberately NOT gated on a specific block reason: a resolved PR is terminal
// truth for the work, so whatever reason set the block (typically a non-gating
// "finalize failed" hint) is moot. A non-Blocked, non-terminal session (e.g.
// ReadyForReview) is left untouched — the dispatcher / webhook path
// (handlePRMerged) still owns advancing it. Like maybeClearStaleFixLoopBlock, it
// emits no completion notification.
func (p *DisplayPoller) reconcileTerminalPRForBlockedSession(ctx context.Context, sessionID string, prStatus *vcs.PRStatus) error {
	if prStatus.State != vcs.PRStateMerged && prStatus.State != vcs.PRStateClosed {
		return nil
	}

	sess, err := p.sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get session for terminal-PR reconcile: %w", err)
	}

	switch {
	case sess.State == machine.Blocked:
		return p.reconcileWedgedBlockToTerminal(ctx, sessionID, sess, prStatus)
	case isResolvedTerminalState(sess.State) && sess.BlockedReason != nil:
		// Terminal row with residual metadata: clear in place, no transition.
		params := db.UpdateSessionParams{}
		clearBlockMetadata(&params)
		if _, err := p.sessions.Update(ctx, sessionID, params); err != nil {
			return fmt.Errorf("clear residual block metadata on terminal session: %w", err)
		}
		p.logger.Info().
			Str("session", sessionID).
			Str("state", sess.State.String()).
			Msg("display poller: cleared stale block reason on already-terminal session")
		return nil
	default:
		return nil
	}
}

// reconcileWedgedBlockToTerminal fires the terminal transition on a session still
// in Blocked whose PR has resolved, persisting the state plus the block metadata
// that OnExit(actionClearBlocked) reset in the machine.
func (p *DisplayPoller) reconcileWedgedBlockToTerminal(ctx context.Context, sessionID string, sess *models.Session, prStatus *vcs.PRStatus) error {
	reason := ""
	if sess.BlockedReason != nil {
		reason = *sess.BlockedReason
	}
	sm := machine.NewWithContext(machine.Blocked, &machine.SessionContext{
		AttemptCount:  sess.AttemptCount,
		MaxAttempts:   machine.MaxAttempts,
		BlockedReason: reason,
	})

	event := machine.PRMerged
	if prStatus.State == vcs.PRStateClosed {
		event = machine.PRClosed
	}
	if err := sm.FireCtx(ctx, event); err != nil {
		return fmt.Errorf("fire terminal transition for wedged block: %w", err)
	}

	newState := int(sm.State())
	params := db.UpdateSessionParams{State: &newState}
	clearBlockMetadata(&params)
	if _, err := p.sessions.Update(ctx, sessionID, params); err != nil {
		return fmt.Errorf("persist terminal-PR reconcile: %w", err)
	}

	p.logger.Info().
		Str("session", sessionID).
		Str("new_state", sm.State().String()).
		Msg("display poller: wedged block reconciled to terminal state on resolved PR")
	return nil
}

// isResolvedTerminalState reports whether a machine state is one of the two
// PR-resolved terminals (Merged or Closed) — the states an already-resolved row
// can sit in while still carrying stale block metadata.
func isResolvedTerminalState(s machine.State) bool {
	return s == machine.Merged || s == machine.Closed
}

// anyCheckRunning reports whether any check has not yet completed. Used to hold
// off the stale-block downgrade while CI is still settling.
func anyCheckRunning(checks []vcs.CheckResult) bool {
	for _, c := range checks {
		if c.Status != vcs.CheckStatusCompleted {
			return true
		}
	}
	return false
}

func prStateString(state vcs.PRState) string {
	switch state {
	case vcs.PRStateOpen:
		return "open"
	case vcs.PRStateClosed:
		return "closed"
	case vcs.PRStateMerged:
		return "merged"
	default:
		return "unknown"
	}
}

func isTerminalDisplayStatus(status vcs.DisplayStatus) bool {
	return status == vcs.DisplayStatusMerged || status == vcs.DisplayStatusClosed
}

func (p *DisplayPoller) persistSnapshot(ctx context.Context, sessionID string, prStatus *vcs.PRStatus, checks []vcs.CheckResult, info vcs.DisplayInfo) {
	if p.snapshots != nil {
		raw, err := json.Marshal(checks)
		if err != nil {
			p.logger.Warn().Err(err).Str("session", sessionID).Msg("display poller: marshal checks for snapshot")
			return
		}
		if err := p.snapshots.Insert(ctx, db.CheckSnapshot{
			SessionID:      sessionID,
			HeadSHA:        prStatus.HeadSHA,
			RawJSON:        string(raw),
			ComputedStatus: int(info.Status),
		}); err != nil {
			p.logger.Warn().Err(err).Str("session", sessionID).Msg("display poller: persist check snapshot")
		}
	}
}
