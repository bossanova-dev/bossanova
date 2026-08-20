package session

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
)

const defaultPRAssociationCacheTTL = 60 * time.Second

// LiveBranchResolver resolves the branch currently checked out in a worktree.
// Implemented by *git.Manager (see Manager.CurrentBranch). Optional: when nil,
// the reconciler matches PRs by the stored branch name only (legacy behavior).
type LiveBranchResolver interface {
	CurrentBranch(ctx context.Context, worktreePath string) (string, error)
}

// ReconcilePRAssociations scans active sessions that are missing a PR number
// and attempts to match them to existing PRs by branch name. This handles
// sessions created before a PR existed or where PR creation happened
// out-of-band (e.g. manually via the GitHub UI).
//
// It returns the number of sessions that were updated.
func ReconcilePRAssociations(
	ctx context.Context,
	sessions db.SessionStore,
	repos db.RepoStore,
	provider vcs.Provider,
	logger zerolog.Logger,
) (int64, error) {
	return NewPRAssociationResolver(sessions, repos, provider, logger).Reconcile(ctx)
}

type prCacheEntry struct {
	expiresAt time.Time
	prs       []vcs.PRSummary
}

type prAssociationMatch struct {
	pr            vcs.PRSummary
	originURL     string
	matchedBranch string
}

// PRAssociationResolver attaches active sessions to existing PRs by exact head
// branch while caching PR listings per repo.
type PRAssociationResolver struct {
	sessions db.SessionStore
	repos    db.RepoStore
	provider vcs.Provider
	logger   zerolog.Logger
	branches LiveBranchResolver
	cronJobs db.CronJobStore
	archiver SessionArchiver
	// archiveTracker joins each archive this sweep launches to daemon shutdown.
	// Optional; nil leaves them untracked.
	archiveTracker ArchiveWorkerTracker
	notify         func(context.Context, *models.Session)

	mu      sync.Mutex
	ttl     time.Duration
	now     func() time.Time
	prCache map[string]prCacheEntry
}

// NewPRAssociationResolver creates a PR association resolver with the default
// PR cache settings.
func NewPRAssociationResolver(
	sessions db.SessionStore,
	repos db.RepoStore,
	provider vcs.Provider,
	logger zerolog.Logger,
) *PRAssociationResolver {
	return &PRAssociationResolver{
		sessions: sessions,
		repos:    repos,
		provider: provider,
		logger:   logger,
		ttl:      defaultPRAssociationCacheTTL,
		now:      time.Now,
		prCache:  make(map[string]prCacheEntry),
	}
}

// WithBranchResolver attaches a live-branch resolver so the reconciler can
// match PRs against the worktree's current branch in addition to the stored
// branch name. Returns the resolver for chaining.
func (r *PRAssociationResolver) WithBranchResolver(branches LiveBranchResolver) *PRAssociationResolver {
	r.branches = branches
	return r
}

// WithCronJobs attaches a cron-job store so the reconciler can repair cron
// sessions whose title is still the cron job's name even though they already
// have a PR (the placeholder / pr_failed finalize branches never sync the
// agent's PR title onto the session row). Without it the stale-title repair
// pass is skipped. Returns the resolver for chaining.
func (r *PRAssociationResolver) WithCronJobs(cronJobs db.CronJobStore) *PRAssociationResolver {
	r.cronJobs = cronJobs
	return r
}

// WithArchiver attaches the archive-after-merge automation so the reconciler
// can heal rows that reached Merged while the archive hook was unreachable —
// the merged-but-unarchived shape from BOS-697. Nothing else revisits a
// terminal row (the display poller skips terminal tracker entries and
// pollableState excludes terminal states), so without this pass those sessions
// stay unarchived forever. Without an archiver the pass is a no-op. Returns the
// resolver for chaining.
//
// Like the other options it writes the field unguarded, so it must be called
// before anything that can invoke the sweep concurrently. Its only reader is
// Reconcile, whose only concurrent caller is the periodic reconcile goroutine
// (the server's ListSessions path goes through ReconcileSessions, which does
// not run the sweep). main.go calls this during single-threaded startup wiring,
// after the Server exists but well before that goroutine starts.
//
// track joins each archive the sweep launches to daemon shutdown (BOS-923); nil
// leaves them untracked. It rides along on this option rather than getting its
// own so an archiver cannot be attached without a tracker decision being made at
// the same call site.
func (r *PRAssociationResolver) WithArchiver(archiver SessionArchiver, track ArchiveWorkerTracker) *PRAssociationResolver {
	r.archiver = archiver
	r.archiveTracker = track
	return r
}

// HasArchiveTracker reports whether an archive worker tracker is wired, for the
// startup wiring assertion. See Dispatcher.HasArchiveTracker.
func (r *PRAssociationResolver) HasArchiveTracker() bool { return r.archiveTracker != nil }

// WithUpdateNotifier attaches a callback invoked once per session the
// reconciler updates (PR association + rename to the PR title). It exists so
// the PR-title rename reaches the cloud/web: reconcile writes directly through
// the store, bypassing the Server's UpdateSession RPC, so without this hook no
// SessionDelta_KIND_UPDATED event is published and bosso shows a stale title
// until the next full daemon snapshot. Returns the resolver for chaining.
func (r *PRAssociationResolver) WithUpdateNotifier(notify func(context.Context, *models.Session)) *PRAssociationResolver {
	r.notify = notify
	return r
}

func (r *PRAssociationResolver) SetTTLForTest(ttl time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.ttl = ttl
}

func (r *PRAssociationResolver) SetNowForTest(now func() time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if now == nil {
		now = time.Now
	}
	r.now = now
}

// Reconcile scans active sessions missing a PR number and attaches exact
// matching PRs.
func (r *PRAssociationResolver) Reconcile(ctx context.Context) (int64, error) {
	active, err := r.sessions.ListActive(ctx, "")
	if err != nil {
		return 0, fmt.Errorf("list active sessions: %w", err)
	}

	updated, err := r.ReconcileSessions(ctx, active)

	// Runs whatever the association pass returned: the sweep heals an unrelated
	// shape, so a PR-association failure must not silently disable it.
	//
	// The merged-but-unarchived sweep hangs off THIS entry point rather than
	// ReconcileSessions, because only this one sees the whole active set. The
	// other caller (Server.ListSessions, via reconcileListSessionPRAssociations)
	// passes rows pre-filtered by NeedsPRAssociation, which requires
	// PRNumber == nil — a shape a merged row can never have — so running the
	// sweep there would be structurally dead work on a hot path, and would make
	// the "bounded by ListActive" reasoning in its doc comment untrue.
	r.archiveMergedButUnarchived(ctx, active)

	return updated, err
}

// ReconcileSessions reconciles only the supplied active sessions. It exists for
// caller-scoped repair paths, such as list-time dynamic discovery for already
// visible rows. The full Reconcile method remains the startup/periodic scanner.
func (r *PRAssociationResolver) ReconcileSessions(ctx context.Context, sessions []*models.Session) (int64, error) {
	var updated int64
	for _, sess := range sessions {
		if !NeedsPRAssociation(sess) {
			continue
		}

		match, err := r.findPRMatchForSession(ctx, sess)
		if err != nil {
			r.logger.Warn().Err(err).
				Str("session", sess.ID).
				Str("repo_id", sess.RepoID).
				Str("branch", sess.BranchName).
				Msg("reconcile: find PR for session")
			continue
		}
		if match == nil {
			continue
		}

		prNum := match.pr.Number
		prNumPtr := &prNum
		prURL := constructPRURL(match.originURL, match.pr.Number)
		prURLPtr := &prURL

		updateParams := db.UpdateSessionParams{
			PRNumber: &prNumPtr,
			PRURL:    &prURLPtr,
		}

		// Correct a drifted branch name. The agent may have created its own
		// branch (e.g. cron sessions whose agent runs `git checkout -b`); the
		// matched PR proves the live branch is the real one, so persist it. This
		// also repairs the finalize/push path, which verifies the worktree is on
		// session.BranchName before pushing.
		if match.matchedBranch != "" && match.matchedBranch != sess.BranchName {
			branch := match.matchedBranch
			updateParams.BranchName = &branch
		}

		// Rename the session to the PR title now that we've identified the PR.
		// One-shot: once PRNumber is set, NeedsPRAssociation returns false,
		// so this never repeatedly clobbers a later user-edited title.
		if title := strings.TrimSpace(match.pr.Title); title != "" {
			updateParams.Title = &title
		}

		// Un-wedge a session whose PR the agent opened itself, so bossd's
		// BranchPushed / PROpened never fired (BOS-697). Rides the same write as
		// the PR association — no extra round-trip.
		if advanced, ok := adoptedPRState(sess.State); ok {
			state := int(advanced)
			updateParams.State = &state
		}

		clearDraftPRBlockedReasonUpdate(sess.BlockedReason, &updateParams)

		updatedSess, err := r.sessions.Update(ctx, sess.ID, updateParams)
		if err != nil {
			r.logger.Warn().Err(err).
				Str("session", sess.ID).
				Int("pr", match.pr.Number).
				Msg("reconcile: update session")
			continue
		}

		updated++
		r.logger.Info().
			Str("session", sess.ID).
			Str("branch", sess.BranchName).
			Int("pr", match.pr.Number).
			Msg("reconciled session with existing PR")

		// Publish the update (PR association + rename to the PR title) so the
		// cloud/web stays in sync; this store write bypasses the Server RPC
		// that would otherwise emit the event.
		if r.notify != nil && updatedSess != nil {
			r.notify(ctx, updatedSess)
		}
	}

	updated += r.repairStaleCronTitles(ctx, sessions)

	return updated, nil
}

// archiveMergedButUnarchived archives sessions already sitting at
// machine.Merged with no ArchivedAt, on repos with the
// ShouldArchiveSessionsAfterMerge flag on.
//
// It heals rows that reached Merged while the archive hook was unreachable —
// e.g. a session wedged in PushingBranch whose merge webhook the state machine
// rejected, or one the display poller reconciled on a daemon build that had no
// archiver on that path (BOS-697). Nothing else revisits a terminal row, so
// without this pass they stay unarchived forever.
//
// Deliberately best-effort and never fails the reconcile: it returns nothing,
// and the underlying archive is detached and idempotent. Every archive that
// SUCCEEDS converges to zero work immediately — the sessions it sees are
// bounded by ListActive (which excludes archived rows) and the Merged filter,
// so a row it archives is gone from the next pass. A row whose archive keeps
// failing before Lifecycle.ArchiveSession reaches its sessions.Archive write
// (an unreadable repo, a worktree that will not archive) stays in the set and
// is retried every tick, with a Warn line per attempt — deliberate: the shape
// this heals is durable, so giving up on it silently would be worse than the
// retry. It is a documented no-op without an archiver (see WithArchiver).
//
// Known limits, deliberately not papered over:
//
//   - The predicate cannot distinguish a row this pass exists to heal from a
//     session someone deliberately resurrected on a flag-on repo, because a
//     resurrect is not durably recorded anywhere — clearing archived_at is all
//     it writes. ResurrectSession writes archived_at and the live state in ONE
//     conditional statement (BOS-924), so the {archived_at NULL, state Merged}
//     shape this predicate matches is never observable mid-resurrect, and a
//     failed resurrect rolls itself back rather than sitting in this set
//     forever. That closes the write-side window; it does NOT close the
//     read-side one, which is why the loop below re-reads each candidate — the
//     snapshot it is handed was taken by ListActive an unbounded time ago. It
//     does not make the resurrect a durable override: once the display poller sees
//     the still-merged PR on a cold tracker it reconciles the row back to Merged
//     and archives it right there (reconcileNonTerminalToResolved), so that path
//     — not this sweep — is what re-archives a resurrected session. On a repo
//     that opted into archive-after-merge that is the flag being applied, but
//     making a resurrect win would need a persisted resurrect marker, which is
//     out of scope for BOS-697.
//   - Turning the repo flag ON backfills. Merged rows accumulated while it was
//     off are all archived within one tick (worktrees removed, and local
//     branches reaped when CanAutoDeleteBranches is also on), because the flag
//     is read per-pass rather than at merge time.
func (r *PRAssociationResolver) archiveMergedButUnarchived(ctx context.Context, sessions []*models.Session) {
	if r.archiver == nil {
		return
	}
	for _, sess := range sessions {
		if sess == nil || sess.ArchivedAt != nil || sess.State != machine.Merged {
			continue
		}
		// Re-read immediately before dispatching. `sessions` is a snapshot from
		// the top of the tick and the archive it launches is detached and slow,
		// so a row that matched the predicate when the list was built may have
		// been archived by another path, or resurrected out from under this
		// sweep, in between. Archiving on the stale copy would delete the
		// worktree a live session is using. A read error is treated as "cannot
		// prove this row still needs healing" and skipped: the shape is durable,
		// so the next tick retries it.
		//
		// This narrows the window to scheduling latency; it does not close it.
		// The dispatch below hands the archive to a detached goroutine, and
		// ArchiveSession re-reads the row but does not re-check archived_at or
		// state, so a resurrect landing between this Get and that goroutine's
		// first statement is still archived. Closing it outright means guarding
		// ArchiveSession itself, which BOS-924 weighed as OQ-2 and declined: it
		// costs a Get on every archive and changes behaviour for the callers
		// that legitimately archive a non-Merged session.
		current, err := r.sessions.Get(ctx, sess.ID)
		if err != nil || current == nil {
			r.logger.Warn().
				Err(err).
				Str("session", sess.ID).
				Msg("archive-merged sweep: skipping candidate whose row could not be re-read")
			continue
		}
		if current.ArchivedAt != nil || current.State != machine.Merged {
			continue
		}
		// One tracked handle per merged-but-unarchived session, not one per
		// tick: this is the only archive launch point with multiplicity, so the
		// tracker must tolerate many handles from a single call.
		archiveSessionAfterMergeIfEnabled(ctx, r.repos, r.archiver, r.archiveTracker, r.logger, current)
	}
}

// adoptedPRState returns the state a session should advance to when reconcile
// adopts an existing PR onto it, and whether an advance applies at all.
//
// Only the two "the PR should already exist by now" states are advanced. A
// session left in PushingBranch or OpeningDraftPR — the shape produced when an
// agent pushes its own branch and opens the PR itself, so bossd's BranchPushed
// / PROpened never fire — permits no PR lifecycle event, so it silently drops
// every check, review and merge event from then on (BOS-697). Replaying the
// real edges (BranchPushed → PROpened, or PROpened alone) lands it on
// AwaitingChecks: the same destination planCompleteDestination picks when
// HasPR is true, and a member of pollableState, so the session resumes
// receiving events.
//
// Every other state is deliberately left alone. ImplementingPlan in particular
// must stay put: the agent is still working, and PlanComplete routes it to
// AwaitingChecks via HasPR on its own.
//
// The advance also makes the row eligible for the repair plugin, whose
// lookupSession accepts AwaitingChecks (plugins/bossd-plugin-repair) — the
// intended consequence of putting a session back on the normal lifecycle, but a
// behaviour change on CanAutoRepair repos worth naming.
//
// Known trade-off: PushingBranch and OpeningDraftPR are also members of
// strandedReapStates (reconcile_cron.go), so advancing removes the row from the
// stranded-cron sweep's reap set — AwaitingChecks is out of that set by design
// (BOS-332). For a dead unattended run the sweep's finalize would also have
// committed leftover worktree changes and recorded cron_job.last_run_outcome;
// after this advance it does not. The wedge itself is the bigger loss (the row
// silently drops every PR event for as long as it sits there), and un-wedging
// on adopt is what BOS-697 asks for, so the advance is unconditional.
// A replay that cannot fire returns (current, false) and un-wedges nothing,
// silently. That is the safe direction — never persist a state the machine
// would reject — and it is not left unguarded:
// TestReconcilePRAssociations_AdvancesWedgedStateOnAdopt is the tripwire, so an
// FSM edit that removes BranchPushed or PROpened goes red there rather than
// quietly turning this pass off in production.
func adoptedPRState(current machine.State) (machine.State, bool) {
	var replay []machine.Event
	switch current {
	case machine.PushingBranch:
		replay = []machine.Event{machine.BranchPushed, machine.PROpened}
	case machine.OpeningDraftPR:
		replay = []machine.Event{machine.PROpened}
	default:
		return current, false
	}

	sm := machine.New(current)
	for _, event := range replay {
		if err := sm.Fire(event); err != nil {
			return current, false
		}
	}
	return sm.State(), true
}

// repairStaleCronTitles renames cron sessions that already have a PR but whose
// title is still the cron job's name. The PR-association loop above only ever
// touches sessions whose PRNumber is nil, so a cron session that finalized via
// the placeholder / pr_failed branch keeps the cron job name forever even
// though the agent set a real title on the GitHub PR. This pass closes that gap
// (and auto-heals any already-stuck rows). It is a no-op without a cron-job
// store (see WithCronJobs).
func (r *PRAssociationResolver) repairStaleCronTitles(ctx context.Context, sessions []*models.Session) int64 {
	if r.cronJobs == nil {
		return 0
	}
	var repaired int64
	for _, sess := range sessions {
		if sess == nil || sess.ArchivedAt != nil || sess.PRNumber == nil ||
			sess.CronJobID == nil || *sess.CronJobID == "" {
			continue
		}
		updatedSess, changed, err := adoptPRTitleWhenCronTitleStale(
			ctx, r.sessions, r.repos, r.cronJobs, r.provider, r.logger, sess)
		if err != nil {
			r.logger.Warn().Err(err).
				Str("session", sess.ID).
				Msg("reconcile: repair stale cron title")
			continue
		}
		if !changed {
			continue
		}
		repaired++
		if r.notify != nil && updatedSess != nil {
			r.notify(ctx, updatedSess)
		}
	}
	return repaired
}

// NeedsPRAssociation reports whether a session is ready for PR discovery.
// Startup-owned rows are excluded until worktree and agent initialization finish.
func NeedsPRAssociation(sess *models.Session) bool {
	return sess != nil &&
		sess.ArchivedAt == nil &&
		sess.PRNumber == nil &&
		sess.BranchName != "" &&
		sess.State != machine.CreatingWorktree &&
		sess.State != machine.StartingAgent
}

// findPRMatchForSession returns the first open PR whose head branch exactly
// matches the session branch, or nil when none match. Closed and merged PRs are
// intentionally ignored (see prsForRepo) so a dead PR is never auto-attached to
// a live session.
func (r *PRAssociationResolver) findPRMatchForSession(ctx context.Context, s *models.Session) (*prAssociationMatch, error) {
	if s == nil || s.BranchName == "" || s.PRNumber != nil {
		return nil, nil
	}

	repo, err := r.repos.Get(ctx, s.RepoID)
	if err != nil {
		return nil, fmt.Errorf("get repo %q for session %q: %w", s.RepoID, s.ID, err)
	}

	prs, err := r.prsForRepo(ctx, repo.ID, repo.OriginURL)
	if err != nil {
		return nil, err
	}

	// Pass 1: the stored branch name (cheap: no git shell-out). This is the
	// common case for ordinary sessions whose branch never drifts.
	if m := matchPRByBranch(prs, s.BranchName, repo.OriginURL); m != nil {
		return m, nil
	}

	// Pass 2: the worktree's live branch. Cron/auto-implement agents create
	// their own branch (e.g. `git checkout -b dave/won-...`) and open the PR
	// there, so the stored cron branch never matches. Resolve lazily so we only
	// shell out to git when the stored branch missed.
	if live := r.liveBranchForSession(ctx, s); live != "" && live != s.BranchName {
		if m := matchPRByBranch(prs, live, repo.OriginURL); m != nil {
			return m, nil
		}
	}

	return nil, nil
}

// matchPRByBranch returns the first open PR whose head branch equals branch.
func matchPRByBranch(prs []vcs.PRSummary, branch, originURL string) *prAssociationMatch {
	if branch == "" {
		return nil
	}
	for _, pr := range prs {
		if pr.HeadBranch == branch {
			return &prAssociationMatch{pr: pr, originURL: originURL, matchedBranch: branch}
		}
	}
	return nil
}

// liveBranchForSession resolves the worktree's current branch, or "" when it
// can't be determined (no resolver, no worktree path, detached HEAD, missing or
// archived worktree). Failures are logged at debug and swallowed so PR
// association degrades to stored-branch matching rather than erroring.
func (r *PRAssociationResolver) liveBranchForSession(ctx context.Context, s *models.Session) string {
	if r.branches == nil || s.WorktreePath == "" {
		return ""
	}
	branch, err := r.branches.CurrentBranch(ctx, s.WorktreePath)
	if err != nil {
		r.logger.Debug().Err(err).
			Str("session", s.ID).
			Str("worktree", s.WorktreePath).
			Msg("reconcile: resolve live worktree branch")
		return ""
	}
	return strings.TrimSpace(branch)
}

func (r *PRAssociationResolver) prsForRepo(ctx context.Context, repoID, originURL string) ([]vcs.PRSummary, error) {
	cacheKey := repoID
	if cacheKey == "" {
		cacheKey = originURL
	}

	now := r.now()
	r.mu.Lock()
	if cached, ok := r.prCache[cacheKey]; ok && now.Before(cached.expiresAt) {
		r.mu.Unlock()
		return clonePRSummaries(cached.prs), nil
	}
	r.mu.Unlock()

	openPRs, err := r.provider.ListOpenPRs(ctx, originURL)
	if err != nil {
		return nil, fmt.Errorf("list open PRs for repo %q: %w", repoID, err)
	}

	// Only open PRs are candidates. A closed or merged PR must never be
	// auto-attached to a live session, and skipping the closed-PR listing also
	// avoids a second GitHub API call per repo. See
	// docs/plans/2026-06-03-dynamic-pr-discovery.md.
	candidates := make([]vcs.PRSummary, 0, len(openPRs))
	for _, pr := range openPRs {
		if pr.State == vcs.PRStateOpen {
			candidates = append(candidates, pr)
		}
	}

	r.mu.Lock()
	r.prCache[cacheKey] = prCacheEntry{
		expiresAt: r.now().Add(r.ttl),
		prs:       clonePRSummaries(candidates),
	}
	r.mu.Unlock()
	return candidates, nil
}

func clonePRSummaries(prs []vcs.PRSummary) []vcs.PRSummary {
	if len(prs) == 0 {
		return nil
	}
	cloned := make([]vcs.PRSummary, len(prs))
	copy(cloned, prs)
	return cloned
}

func clearDraftPRBlockedReasonUpdate(reason *string, params *db.UpdateSessionParams) {
	// Also clears the background step's in-flight marker (BOS-540): if the
	// reconciler is attaching a PR to this session, the create the marker was
	// advertising is moot — including the case where a daemon restart abandoned
	// the background step and left the marker behind.
	if !isClearableDraftPRReason(reason) {
		return
	}

	var cleared *string
	params.BlockedReason = &cleared
}

// constructPRURL is a package-local alias for vcs.ConstructPRURL.
func constructPRURL(originURL string, prNumber int) string {
	return vcs.ConstructPRURL(originURL, prNumber)
}
