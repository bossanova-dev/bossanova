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
	sessions           db.SessionStore
	repos              db.RepoStore
	provider           vcs.Provider
	tracker            *status.DisplayTracker
	snapshots          db.CheckSnapshotStore // optional; nil disables persistence
	completionNotifier SessionCompletionNotifier
	health             WebhookHealth          // optional; nil polls every repo at interval
	archiver           SessionArchiver        // optional; nil disables archive-after-merge
	archiveTracker     ArchiveWorkerTracker   // optional; nil leaves archives outside shutdown coordination
	receiptPoster      vcs.CommitStatusPoster // optional; nil (or nil receiptProbe) disables receipt carrying
	receiptProbe       ReceiptPushProbe       // optional; see SetReceiptCarrier
	interval           time.Duration
	logger             zerolog.Logger
	done               chan struct{}

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

// ReceiptPushProbe reports whether a session's own worktree pushed newSHA on
// top of fromSHA. *git.Manager satisfies it with SessionPushedHead; it is
// declared here so the poller needs no dependency on the large worktree
// interface.
type ReceiptPushProbe interface {
	SessionPushedHead(ctx context.Context, worktreePath, branch, fromSHA, newSHA string) (bool, error)
}

// SetReceiptCarrier wires boss/build receipt carrying (BOS-1452): when a
// session's own worktree pushes a head descending from the last receipted
// head, the poller posts a carried receipt on the new head so the hand-off
// status survives the session's follow-up commits. nil-safe: either argument
// nil disables carrying.
func (p *DisplayPoller) SetReceiptCarrier(poster vcs.CommitStatusPoster, probe ReceiptPushProbe) {
	p.receiptPoster = poster
	p.receiptProbe = probe
}

// SetWebhookHealth wires the webhook-delivery tracker. Sessions in a repo whose
// webhooks reach the daemon are polled at webhookSafetyNetInterval instead of
// interval. nil-safe: leaving it unset polls every repo at interval.
func (p *DisplayPoller) SetWebhookHealth(h WebhookHealth) {
	p.health = h
}

// SetCompletionNotifier wires the task-orchestrator completion hook for
// terminal-state reconciles recovered by the display poller.
func (p *DisplayPoller) SetCompletionNotifier(n SessionCompletionNotifier) {
	p.completionNotifier = n
}

// SetArchiver wires the archive-after-merge automation onto the poller's
// terminal reconcile, so a session this poller lands on Merged is archived when
// its repo has ShouldArchiveSessionsAfterMerge on — the same hook the PR-merged
// webhook runs (BOS-697). Before this, the reconcile was the *only* path to
// Merged for a daemon that missed the webhook, and it never archived.
// nil-safe: leaving it unset disables the automation.
//
// track joins the archive goroutine this path launches to daemon shutdown
// (BOS-923); nil leaves it untracked. It is a parameter of this setter, not a
// separate method, so an archiver cannot be wired without a tracker decision at
// the same call site.
func (p *DisplayPoller) SetArchiver(a SessionArchiver, track ArchiveWorkerTracker) {
	p.archiver = a
	p.archiveTracker = track
}

// HasArchiveTracker reports whether an archive worker tracker is wired, for the
// startup wiring assertion. See Dispatcher.HasArchiveTracker.
func (p *DisplayPoller) HasArchiveTracker() bool { return p.archiveTracker != nil }

func (p *DisplayPoller) notifyCompletion(ctx context.Context, sessionID string, outcome models.TaskMappingStatus) {
	if p.completionNotifier != nil {
		p.completionNotifier.HandleSessionCompleted(ctx, sessionID, outcome)
	}
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

// intervalFor is the poll interval for one session. Webhook evidence only ever
// LENGTHENS it: a session a webhook refreshed recently backs off to at least
// webhookHealthyInterval, and a session in a repo whose webhooks reach the
// daemon backs off to at least webhookSafetyNetInterval. Both take the max with
// the configured interval, so a deliberately long DisplayPollInterval is never
// shortened by good news.
func (p *DisplayPoller) intervalFor(repoOriginURL, sessionID string, now time.Time) time.Duration {
	interval := p.interval
	p.refreshMu.Lock()
	last, ok := p.latestWebhookRefresh[sessionID]
	if ok && now.Sub(last) > webhookHealthyWindow {
		delete(p.latestWebhookRefresh, sessionID)
		ok = false
	}
	p.refreshMu.Unlock()
	if ok && now.Sub(last) <= webhookHealthyWindow {
		interval = max(interval, webhookHealthyInterval)
	}
	if p.health != nil && p.health.WebhookDeliveryHealthy(repoOriginURL) {
		interval = max(interval, webhookSafetyNetInterval)
	}
	return interval
}

func (p *DisplayPoller) markPolled(sessionID string, ts time.Time) {
	p.lastPollMu.Lock()
	defer p.lastPollMu.Unlock()
	if p.lastPoll == nil {
		p.lastPoll = make(map[string]time.Time)
	}
	p.lastPoll[sessionID] = ts
}

func (p *DisplayPoller) shouldPollSession(repoOriginURL, sessionID string, now time.Time) bool {
	interval := p.intervalFor(repoOriginURL, sessionID, now)

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

// RefreshPR re-polls one PR's display status on demand and credits the affected
// sessions with webhook health, backing their scheduled poll interval off to
// webhookHealthyInterval. Use it only for refreshes actually triggered by an
// inbound webhook — the backoff is only sound as evidence that the webhook
// pipeline is delivering for that session. Locally-initiated refreshes must use
// RefreshPRWithoutWebhookCredit.
func (p *DisplayPoller) RefreshPR(ctx context.Context, repoOriginURL string, prNumber int) error {
	return p.refreshPR(ctx, repoOriginURL, prNumber, true)
}

// RefreshPRWithoutWebhookCredit re-polls one PR's display status on demand
// without treating the refresh as evidence of webhook health. Callers that
// refresh because the daemon itself just acted on the PR (MergeSession) have no
// webhook to credit: crediting one would stretch the session's scheduled poll
// interval to webhookHealthyInterval, so at any DisplayPollInterval below that
// (including the 2-minute default) a still-active session's status would
// refresh LESS often after a blocked or failed merge than before it — the
// caller's cadence should not hinge on who last refreshed. The immediate-poll
// suppression (markPolled) still applies to both variants — that part is just
// "this session was polled a moment ago".
func (p *DisplayPoller) RefreshPRWithoutWebhookCredit(ctx context.Context, repoOriginURL string, prNumber int) error {
	return p.refreshPR(ctx, repoOriginURL, prNumber, false)
}

func (p *DisplayPoller) refreshPR(ctx context.Context, repoOriginURL string, prNumber int, creditWebhookHealth bool) error {
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
		if p.pollSession(ctx, repo, sess, *sess.PRNumber) {
			refreshed++
			refreshedSessions = append(refreshedSessions, sess.ID)
		}
	}
	if refreshed > 0 {
		for _, sessionID := range refreshedSessions {
			p.markPolled(sessionID, now)
			if creditWebhookHealth {
				p.recordRefresh(sessionID, now)
			}
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
//
// The scheduled sweep reads through the provider's short read cache
// (vcs.WithCachedReads), so it shares one GitHub read per PR with the
// state-machine poller. RefreshPR does not: it runs after a webhook or the
// daemon's own action on the PR, when only a fresh read is useful.
func (p *DisplayPoller) poll(ctx context.Context) {
	ctx = vcs.WithCachedReads(ctx)
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
			if !p.shouldPollSession(repo.OriginURL, sess.ID, now) {
				continue
			}
			p.pollSession(ctx, repo, sess, *sess.PRNumber)
		}
	}
	p.pruneLastPoll(activeSessions)
}

// pollSession fetches PR status, checks, and reviews for a single session
// and updates the tracker with the computed display status.
func (p *DisplayPoller) pollSession(ctx context.Context, repo *models.Repo, sess *models.Session, prNumber int) bool {
	sessionID := sess.ID
	repoPath := repo.OriginURL
	prStatus, err := p.provider.GetPRStatus(ctx, repoPath, prNumber)
	if err != nil {
		p.logger.Warn().Err(err).Str("session", sessionID).Msg("display poller: get PR status")
		return false
	}

	p.logger.Info().
		Str("session_id", sessionID).
		Str("repo_origin_url", repoPath).
		Int("pr_number", prNumber).
		Str("pr_state", prStateString(prStatus.State)).
		Bool("pr_draft", prStatus.Draft).
		Msg("display poller: fetched PR status")

	if prStatus.State == vcs.PRStateMerged || prStatus.State == vcs.PRStateClosed {
		// Reconcile persisted lifecycle state before recording the terminal
		// tracker entry. Once the entry is terminal, poll() and RefreshPR() skip
		// the session, so a transient store error inside the reconcile would
		// otherwise never retry until a daemon restart.
		if err := p.reconcileTerminalPRForSession(ctx, sessionID, prStatus); err != nil {
			p.logger.Warn().Err(err).Str("session", sessionID).Msg("display poller: terminal-PR reconcile failed; will retry next poll")
			return false
		}
		info := vcs.ComputeDisplayStatus(prStatus, nil, nil)
		info.HeadSHA = prStatus.HeadSHA
		p.tracker.Set(sessionID, info)
		p.persistSnapshot(ctx, sessionID, prStatus, nil, info)
		return true
	}

	// Skip checks and reviews for draft PRs — they aren't ready for review
	// so CI results and review comments are not actionable. This saves 2 API
	// calls per draft PR per poll cycle.
	if prStatus.Draft {
		info := vcs.ComputeDisplayStatus(prStatus, nil, nil)
		info.HeadSHA = prStatus.HeadSHA
		p.tracker.Set(sessionID, info)
		p.persistSnapshot(ctx, sessionID, prStatus, nil, info)
		return true
	}

	// On any inputs error, skip the update rather than recomputing with empty
	// results. A transient GitHub API blip would otherwise collapse a
	// "Failing" or "Rejected" row to "Idle" / "Passing" — silently
	// disabling the repair plugin (which only triggers on
	// FAILING/CONFLICT/REJECTED). The previous tracker entry sticks; the
	// next poll cycle retries.
	checkSet, err := vcs.ReadCheckSet(ctx, p.provider, repoPath, prNumber)
	if err != nil {
		p.logger.Warn().Err(err).Str("session", sessionID).Msg("display poller: get check results; preserving previous status")
		return false
	}

	reviews, err := p.provider.GetReviewComments(ctx, repoPath, prNumber)
	if err != nil {
		p.logger.Warn().Err(err).Str("session", sessionID).Msg("display poller: get review comments; preserving previous status")
		return false
	}

	checks := checkSet.Checks
	info := vcs.ComputeDisplayStatus(prStatus, checks, reviews)
	info.HasBuildReceipt = checkSet.HasBuildReceipt
	info.BuildReceiptSeen = checkSet.BuildReceiptSeen
	// Carry the receipt BEFORE Set, so Ready shows on this same tick and the
	// tracker latch moves to the carried head.
	if p.maybeCarryReceipt(ctx, repo, sess, prNumber, prStatus.HeadSHA, checkSet) {
		info.HasBuildReceipt = true
	}
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
	return true
}

// carriedReceiptDescriptionPrefix marks a boss/build receipt bossd carried
// forward rather than one a skill posted. Documented in
// docs/skills/commit-status-receipts.md.
const carriedReceiptDescriptionPrefix = "carried by bossd from "

// maybeCarryReceipt posts a carried boss/build receipt on head when the
// session's own worktree pushed it on top of the last receipted head
// (BOS-1452), and reports whether a receipt is now on head. Conditions are
// checked cheapest first; the git probe runs only for a session whose head
// moved off a latched receipt with no boss/build status of its own. A failed
// probe or post is never fatal: it claims no receipt and the next poll
// retries.
func (p *DisplayPoller) maybeCarryReceipt(ctx context.Context, repo *models.Repo, sess *models.Session, prNumber int, head string, checkSet vcs.CheckSet) bool {
	if p.receiptPoster == nil || p.receiptProbe == nil {
		return false
	}
	prev := p.tracker.Get(sess.ID)
	if prev == nil || prev.ReceiptHeadSHA == "" {
		return false
	}
	from := prev.ReceiptHeadSHA
	if head == "" || head == from {
		return false
	}
	if checkSet.BuildReceiptSeen {
		return false
	}
	if sess.WorktreePath == "" || sess.BranchName == "" {
		return false
	}
	pushed, err := p.receiptProbe.SessionPushedHead(ctx, sess.WorktreePath, sess.BranchName, from, head)
	if err != nil || !pushed {
		// Evaluated every tick for a session in this state: Debug only.
		p.logger.Debug().Err(err).
			Str("session_id", sess.ID).
			Int("pr_number", prNumber).
			Str("from", from).
			Str("to", head).
			Msg("display poller: head not carried; not this session's own push on the receipted head")
		return false
	}
	err = p.receiptPoster.PostCommitStatus(ctx, repo.OriginURL, head, vcs.CommitStatus{
		Context:     vcs.BuildReceiptContext,
		State:       "success",
		Description: carriedReceiptDescriptionPrefix + shortSHA(from),
	})
	if err != nil {
		p.logger.Warn().Err(err).
			Str("session_id", sess.ID).
			Int("pr_number", prNumber).
			Str("from", from).
			Str("to", head).
			Msg("display poller: post carried boss/build receipt failed; will retry next poll")
		return false
	}
	if inv, ok := p.provider.(vcs.ReadInvalidator); ok {
		inv.InvalidatePR(repo.OriginURL, prNumber)
	}
	p.logger.Info().
		Str("session_id", sess.ID).
		Int("pr_number", prNumber).
		Str("from", from).
		Str("to", head).
		Msg("carried boss/build receipt")
	return true
}

// shortSHA is the first 12 characters of sha (all of it when shorter).
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
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

// reconcileTerminalPRForSession reconciles persisted state for a session whose
// PR the provider now reports resolved (merged or closed). It returns an error
// only when a reconcile it attempted could not be persisted, so the caller can
// decline to record a terminal tracker entry and retry on the next poll (a nil
// return means the row is already clean or was reconciled here):
//
//   - Active non-terminal rows: fire the terminal transition and persist it.
//     This is the fallback for missed realtime webhooks.
//   - Wedged in Blocked: same terminal transition, plus the machine's
//     OnExit(actionClearBlocked) resets the block reason + attempt count.
//   - Already terminal (Merged/Closed) but still carrying a stale block reason —
//     e.g. a pre-fix PRClosed webhook advanced a Blocked session to Closed while
//     the old handler wrote only State. Nothing else revisits such a row (poll /
//     RefreshPR skip terminal tracker entries and pollableState excludes terminal
//     states) and web sessionWarningHints surfaces a bare blockedReason, so the
//     stale hint would linger forever. Clear the residual metadata in place, with
//     no transition (the state is already correct).
//
// The in-place metadata clear emits no completion notification, but the
// non-terminal transition branch does: reconcileNonTerminalToResolved calls
// notifyCompletion, which is what lets MergeSession's synchronous post-merge
// refresh settle the task mapping before its RPC returns.
//
// It archives too, on the merged branch only: reconcileNonTerminalToResolved
// calls the shared archiveSessionAfterMergeIfEnabled, the same hook the
// dispatcher's PRMerged handler runs. A daemon that never receives the
// PR-merged webhook therefore still auto-archives, instead of depending on the
// state poller reaching the row first (once this reconcile lands Merged,
// pollableState excludes it). A closed PR never archives, matching the
// dispatcher's deliberate merge-only rule.
func (p *DisplayPoller) reconcileTerminalPRForSession(ctx context.Context, sessionID string, prStatus *vcs.PRStatus) error {
	if prStatus.State != vcs.PRStateMerged && prStatus.State != vcs.PRStateClosed {
		return nil
	}

	sess, err := p.sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get session for terminal-PR reconcile: %w", err)
	}

	switch {
	case !isResolvedTerminalState(sess.State):
		return p.reconcileNonTerminalToResolved(ctx, sessionID, sess, prStatus)
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

// reconcileNonTerminalToResolved fires the terminal transition on a session whose
// PR has resolved, persisting the state plus any block metadata that
// OnExit(actionClearBlocked) reset in the machine.
func (p *DisplayPoller) reconcileNonTerminalToResolved(ctx context.Context, sessionID string, sess *models.Session, prStatus *vcs.PRStatus) error {
	reason := ""
	if sess.BlockedReason != nil {
		reason = *sess.BlockedReason
	}
	sm := machine.NewWithContext(sess.State, &machine.SessionContext{
		AttemptCount:  sess.AttemptCount,
		MaxAttempts:   machine.MaxAttempts,
		BlockedReason: reason,
	})

	event := machine.PRMerged
	if prStatus.State == vcs.PRStateClosed {
		event = machine.PRClosed
	}
	if !sm.CanFire(event) {
		if sess.State == 0 {
			return nil
		}
		return fmt.Errorf("terminal PR event %s cannot fire from state %s", event.String(), sess.State.String())
	}
	if err := sm.FireCtx(ctx, event); err != nil {
		return fmt.Errorf("fire terminal transition for resolved PR: %w", err)
	}

	newState := int(sm.State())
	params := db.UpdateSessionParams{State: &newState}
	if sess.State == machine.Blocked || sess.BlockedReason != nil {
		clearBlockMetadata(&params)
	}
	if _, err := p.sessions.Update(ctx, sessionID, params); err != nil {
		return fmt.Errorf("persist terminal-PR reconcile: %w", err)
	}

	outcome := models.TaskMappingStatusCompleted
	if prStatus.State == vcs.PRStateClosed {
		outcome = models.TaskMappingStatusFailed
	}
	p.notifyCompletion(ctx, sessionID, outcome)
	// Merge-only, mirroring the dispatcher: a PR *close* must never archive.
	// Tested positively (== merged) rather than as "not closed": the caller has
	// already filtered to merged-or-closed, but a future second caller must not
	// silently turn an open PR into an archive.
	if prStatus.State == vcs.PRStateMerged {
		archiveSessionAfterMergeIfEnabled(ctx, p.repos, p.archiver, p.archiveTracker, p.logger, sess)
	}

	p.logger.Info().
		Str("session", sessionID).
		Str("new_state", sm.State().String()).
		Msg("display poller: session reconciled to terminal state on resolved PR")
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
