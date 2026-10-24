package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/contract/daemon/trackerref"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/detach"
)

// TrackerIssueResolver confirms a candidate ticket id against a repo's own
// tracker and returns the tracker's canonical id and URL for it. The
// implementation lives in the server package, next to the task-source plugin
// routing; the interface lives here, with its consumer.
type TrackerIssueResolver interface {
	ResolveTrackerIssue(ctx context.Context, repo *models.Repo, id string) (canonicalID, url string, err error)
}

var (
	// ErrTrackerNotConfigured means the repo has no usable tracker: no Linear
	// key, no plugin host, or no "linear" task source loaded.
	ErrTrackerNotConfigured = errors.New("tracker not configured for this repo")
	// ErrTrackerIssueNotFound means the tracker returned no issue whose id is
	// exactly the candidate.
	ErrTrackerIssueNotFound = errors.New("tracker issue not found")
	// ErrTrackerLinkPassTimedOut means a LinkActive pass spent its own time
	// budget (the caller's context was still live); the rest of the work is
	// picked up by the next pass.
	ErrTrackerLinkPassTimedOut = errors.New("tracker link pass ran out of time")
	// errTrackerResolveTimedOut marks a resolver call that ended after its own
	// per-call bound expired. The linker sets it from the context it owns
	// because the resolver's error cannot be relied on to say so: over the
	// plugin gRPC path a deadline arrives as a status error that does not
	// unwrap to context.DeadlineExceeded.
	errTrackerResolveTimedOut = errors.New("tracker resolve timed out")
)

const (
	// maxTrackerResolvesPerPass caps resolver calls per LinkActive pass. Each
	// is a Linear API lookup, so a first backfill spreads over several ticks
	// instead of bursting.
	maxTrackerResolvesPerPass = 5
	// trackerMissTTL is how long a (session, candidate) pair that did not
	// resolve is left alone before it is tried again.
	trackerMissTTL = 15 * time.Minute
	// trackerLinkPassTimeout bounds one LinkActive pass. The pass shares the
	// daemon's 60s PR-reconcile tick, and each resolve is a plugin round-trip
	// to the Linear API (the resolver has the plugin skip its open-PR
	// listing), so without a bound a slow tracker would delay the rest of that
	// tick by minutes. The bound covers the pass's own work; a link committed
	// at the deadline adds at most one detach.CleanupBudget of post-commit
	// re-read and notify (see LinkActive).
	trackerLinkPassTimeout = 30 * time.Second
	// trackerResolveTimeout bounds one resolver call. It sits well under the
	// pass budget — maxTrackerResolvesPerPass calls fit inside one pass — so a
	// hung tracker call ends as a recorded miss while the pass is still live,
	// instead of always being cut off by the pass deadline (the plugin RPC and
	// Linear HTTP bounds are as long as the pass), never recorded, and retried
	// at the head of every tick ahead of the sessions behind it.
	trackerResolveTimeout = 5 * time.Second
)

// TrackerLinker links active, unlinked sessions to their tracker issue. It
// extracts candidate ids from facts the session already carries (see
// trackerref) and keeps only a candidate the repo's own tracker confirms, so
// a link never comes from a format guess. An existing link is never touched.
type TrackerLinker struct {
	sessions db.SessionStore
	repos    db.RepoStore
	resolver TrackerIssueResolver
	openPRs  func(ctx context.Context, repoID, originURL string) ([]vcs.PRSummary, error)
	notify   func(context.Context, *models.Session)
	logger   zerolog.Logger
	now      func() time.Time
	// passTimeout bounds one LinkActive pass (trackerLinkPassTimeout).
	passTimeout time.Duration
	// resolveTimeout bounds one resolver call (trackerResolveTimeout).
	resolveTimeout time.Duration

	mu sync.Mutex
	// misses maps sessionID+"\x00"+candidate to the time it may be retried.
	misses map[string]time.Time
}

// NewTrackerLinker builds a linker. openPRs supplies the open-PR listing the
// PR title is read from (the PR-association resolver's cached listing); notify
// is called once per linked session so bosso and the web see the new fields.
// Either may be nil.
func NewTrackerLinker(
	sessions db.SessionStore,
	repos db.RepoStore,
	resolver TrackerIssueResolver,
	openPRs func(ctx context.Context, repoID, originURL string) ([]vcs.PRSummary, error),
	notify func(context.Context, *models.Session),
	logger zerolog.Logger,
) *TrackerLinker {
	return &TrackerLinker{
		sessions:       sessions,
		repos:          repos,
		resolver:       resolver,
		openPRs:        openPRs,
		notify:         notify,
		logger:         logger,
		now:            time.Now,
		passTimeout:    trackerLinkPassTimeout,
		resolveTimeout: trackerResolveTimeout,
		misses:         make(map[string]time.Time),
	}
}

// SetPassTimeoutForTest overrides the per-pass time budget.
func (l *TrackerLinker) SetPassTimeoutForTest(d time.Duration) {
	l.passTimeout = d
}

// SetResolveTimeoutForTest overrides the per-resolver-call time budget.
func (l *TrackerLinker) SetResolveTimeoutForTest(d time.Duration) {
	l.resolveTimeout = d
}

// SetNowForTest overrides the linker's clock.
func (l *TrackerLinker) SetNowForTest(now func() time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now == nil {
		now = time.Now
	}
	l.now = now
}

// needsTrackerLink reports whether the pass may link sess: active, not yet
// linked, and neither a quick chat nor a /boss-plan session (the dedup map
// ignores planning sessions; linking one would make create_session refuse the
// build session for the ticket being planned).
func needsTrackerLink(sess *models.Session) bool {
	return sess != nil &&
		sess.ArchivedAt == nil &&
		(sess.TrackerURL == nil || *sess.TrackerURL == "") &&
		!sess.IsQuickChat &&
		!IsPlanningSessionPlan(sess.Plan)
}

// LinkActive runs one tracker-link pass over every active session and returns
// how many it linked. It makes at most maxTrackerResolvesPerPass resolver
// calls and skips (session, candidate) pairs still inside their miss window.
// Its work runs under the pass timeout, but a link's post-commit re-read and
// notify run synchronously on a detached context (see writeLink), so a link
// committed just before the deadline can overrun it by at most one
// detach.CleanupBudget (for a notify that honours its context): once the pass
// context has ended, the loop returns at its next session check, so no second
// link starts. If ctx ends mid-pass it returns the partial
// count with the context error; if the pass's own budget ends first, the
// error wraps ErrTrackerLinkPassTimedOut.
func (l *TrackerLinker) LinkActive(parent context.Context) (int64, error) {
	if l == nil || l.resolver == nil {
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(parent, l.passTimeout)
	defer cancel()
	linked, err := l.linkActive(ctx)
	// Only the pass's own context expiring is a pass timeout. A dependency's
	// shorter deadline also wraps context.DeadlineExceeded, but the pass still
	// had budget left, so that error is reported as itself.
	if err != nil && parent.Err() == nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("%w after %s: %w", ErrTrackerLinkPassTimedOut, l.passTimeout, err)
	}
	return linked, err
}

func (l *TrackerLinker) linkActive(ctx context.Context) (int64, error) {
	active, err := l.sessions.ListActive(ctx, "")
	if err != nil {
		return 0, fmt.Errorf("list active sessions: %w", err)
	}

	l.mu.Lock()
	now := l.now()
	for key, retryAt := range l.misses {
		if !now.Before(retryAt) {
			delete(l.misses, key)
		}
	}
	l.mu.Unlock()

	var linked int64
	budget := maxTrackerResolvesPerPass
	repos := make(map[string]*models.Repo)
	unconfigured := make(map[string]bool)

	for _, sess := range active {
		if err := ctx.Err(); err != nil {
			return linked, err
		}
		if budget <= 0 {
			break
		}
		if !needsTrackerLink(sess) || unconfigured[sess.RepoID] {
			continue
		}

		repo, ok := repos[sess.RepoID]
		if !ok {
			repo, err = l.repos.Get(ctx, sess.RepoID)
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return linked, ctxErr
				}
				l.logger.Debug().Err(err).Str("session", sess.ID).Str("repo_id", sess.RepoID).
					Msg("tracker link: get repo")
				continue
			}
			repos[sess.RepoID] = repo
		}
		if repo.LinearAPIKey == "" {
			unconfigured[sess.RepoID] = true
			continue
		}

		candidates := trackerref.Candidates(l.factsFor(ctx, sess, repo))
		for _, candidate := range candidates {
			if budget <= 0 {
				break
			}
			if l.inMissWindow(sess.ID, candidate) {
				continue
			}
			budget--
			canonicalID, url, err := l.resolve(ctx, repo, candidate)
			if err != nil {
				// The pass ending is not a miss: the candidate was never
				// answered, so the next pass retries it. A resolve that hit
				// its own per-call bound while the pass was live is a miss.
				if ctxErr := ctx.Err(); ctxErr != nil {
					return linked, ctxErr
				}
				if errors.Is(err, ErrTrackerNotConfigured) {
					unconfigured[sess.RepoID] = true
					break
				}
				l.recordMiss(sess.ID, candidate)
				switch {
				case errors.Is(err, ErrTrackerIssueNotFound):
					// The expected outcome for most candidates; not logged.
				case errors.Is(err, errTrackerResolveTimedOut):
					// The pass is live, so this is the per-call bound. Warn:
					// a tracker that is always this slow otherwise stops
					// linking with nothing above Debug to show it.
					l.logger.Warn().Err(err).Str("session", sess.ID).Str("candidate", candidate).
						Dur("timeout", l.resolveTimeout).
						Msg("tracker link: resolve candidate timed out; retrying after the miss window")
				default:
					l.logger.Debug().Err(err).Str("session", sess.ID).Str("candidate", candidate).
						Msg("tracker link: resolve candidate")
				}
				continue
			}
			if l.writeLink(ctx, sess, canonicalID, url) {
				linked++
			}
			break
		}
	}
	return linked, nil
}

// resolve makes one resolver call under its own per-call bound. A failed call
// whose bound had expired wraps errTrackerResolveTimedOut; the caller checks
// the pass context first, so a pass that ended is never read as this.
func (l *TrackerLinker) resolve(ctx context.Context, repo *models.Repo, candidate string) (string, string, error) {
	callCtx, cancel := context.WithTimeout(ctx, l.resolveTimeout)
	defer cancel()
	canonicalID, url, err := l.resolver.ResolveTrackerIssue(callCtx, repo, candidate)
	if err != nil && errors.Is(callCtx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("%w after %s: %w", errTrackerResolveTimedOut, l.resolveTimeout, err)
	}
	return canonicalID, url, err
}

// factsFor gathers the session facts candidates are extracted from. The PR
// title is read from the open-PR listing only when the session has a PR and
// that PR is still open.
func (l *TrackerLinker) factsFor(ctx context.Context, sess *models.Session, repo *models.Repo) trackerref.Facts {
	facts := trackerref.Facts{
		BranchName:   sess.BranchName,
		SessionTitle: sess.Title,
	}
	if sess.TrackerID != nil {
		facts.TrackerID = *sess.TrackerID
	}
	if facts.TrackerID == "" && sess.PRNumber != nil && l.openPRs != nil {
		prs, err := l.openPRs(ctx, repo.ID, repo.OriginURL)
		if err != nil {
			l.logger.Debug().Err(err).Str("session", sess.ID).Msg("tracker link: list open PRs")
		}
		for _, pr := range prs {
			if pr.Number == *sess.PRNumber {
				facts.PRTitle = pr.Title
				break
			}
		}
	}
	return facts
}

// writeLink persists the resolved link and notifies; it reports success.
//
// The write is a compare-and-set (db.SessionStore.SetTrackerIfUnlinked): it
// lands only while the row is still active, has no tracker URL, and carries
// the tracker id this pass read. A manual SetSessionTracker that commits at any
// point after the pass listed the session — during the resolver round-trip or
// just before this write — therefore wins, and the pass skips the session.
func (l *TrackerLinker) writeLink(ctx context.Context, sess *models.Session, canonicalID, url string) bool {
	canonicalID = strings.TrimSpace(canonicalID)
	if canonicalID == "" || url == "" {
		return false
	}
	applied, err := l.sessions.SetTrackerIfUnlinked(ctx, sess.ID, trackerIDOf(sess), canonicalID, url)
	if err != nil {
		l.logger.Warn().Err(err).Str("session", sess.ID).Str("tracker_id", canonicalID).
			Msg("tracker link: write link")
		return false
	}
	if !applied {
		l.logger.Debug().Str("session", sess.ID).Msg("tracker link: session changed during resolve; skipping")
		return false
	}
	l.logger.Info().Str("session", sess.ID).Str("tracker_id", canonicalID).Msg("tracker link: linked session")
	if l.notify != nil {
		// The link has committed, so the re-read and the update event are
		// post-commit work: run them detached from the pass's context, under
		// their own bound, or a pass deadline landing here drops the event.
		detach.Cleanup(ctx, detach.CleanupBudget, func(postCtx context.Context) {
			updated, err := l.sessions.Get(postCtx, sess.ID)
			if err != nil {
				l.logger.Warn().Err(err).Str("session", sess.ID).Msg("tracker link: re-read linked session")
				return
			}
			l.notify(postCtx, updated)
		})
	}
	return true
}

func trackerIDOf(sess *models.Session) string {
	if sess.TrackerID == nil {
		return ""
	}
	return *sess.TrackerID
}

func trackerMissKey(sessionID, candidate string) string {
	return sessionID + "\x00" + candidate
}

func (l *TrackerLinker) inMissWindow(sessionID, candidate string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	retryAt, ok := l.misses[trackerMissKey(sessionID, candidate)]
	return ok && l.now().Before(retryAt)
}

func (l *TrackerLinker) recordMiss(sessionID, candidate string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.misses[trackerMissKey(sessionID, candidate)] = l.now().Add(trackerMissTTL)
}
