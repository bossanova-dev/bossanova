package session

import (
	"context"
	"strconv"
	"time"

	"github.com/recurser/bossalib/keyedgate"
)

// The session create path takes two locks, always in this order:
//
//  1. the TARGET lock — per (repo, PR-or-branch), held across the whole
//     bootstrap and its failure cleanup, so two creates racing for the same
//     PR/branch cannot both end up with a session, and so the loser waits for
//     the winner's outcome instead of being refused as a duplicate of a
//     half-started row that is about to be deleted;
//  2. the START-PATH lock — per repo, held only across the duplicate check plus
//     the session-row insert, so that check-then-insert is atomic (BOS-236).
//
// Both are bounded, so a wedged holder surfaces as a clear error rather than an
// untimed hang, and both live here rather than on any one entry point because
// StreamCreateSession and the task orchestrator must serialize against EACH
// OTHER, not just against themselves.
//
// Before BOS-717 a single process-global, untimed mutex did both jobs, held
// across the entire multi-minute bootstrap. One bootstrap that never returned
// therefore blocked every subsequent CreateSession in the daemon, across every
// repo, with no log line and no session row — a daemon-wide outage only a
// restart cleared. Splitting the two jobs is what bounds the blast radius to the
// one target that is actually stuck.
//
// NOTE these locks serialize the SESSION-ROW claim on a target, not git. Two
// creates in one repo for different targets now bootstrap concurrently, which
// the old global mutex prevented. The shared-clone git work THE CREATE PATH
// performs is serialized separately, by the per-repo gate in internal/git (see
// Manager.Create) — that gate covers worktree creation only. Git run from other
// paths against the same clone (finalize's FetchBase, merge policy) is ungated,
// as it was before this change; the old mutex never covered it either.
//
// Always acquire the target lock before the start-path lock. A consistent order
// is what makes the pair deadlock-free.
const (
	// StartPathLockTimeout bounds the wait for a repo's start-path lock. That
	// lock covers only local database work measured in milliseconds, so a wait
	// this long is a wedged holder, not a queue that will drain.
	StartPathLockTimeout = 60 * time.Second

	// TargetStartLockTimeout bounds the wait for a target lock. This one is held
	// across a real bootstrap, so it is DERIVED from BootstrapTimeout rather than
	// picked independently: a create legitimately queued behind another create
	// for the same target must be able to outwait the holder.
	//
	// The two-minute margin is not slack, it is the rest of the hold. The
	// bootstrap deadline bounds StartSession alone; the holder also spends up to
	// StartPathLockTimeout waiting for the second lock, plus its duplicate check
	// and — on the failure path — StopBackgroundDraftPR and the artifact cleanup.
	//
	// The margin is sized against the WAITS in that list, because those are the
	// parts that can be spent doing nothing: StartPathLockTimeout (60s), and the
	// cleanup's two clone-gate acquisitions, bounded by
	// gitpkg.RepoCloneCleanupGateTimeout (30s each) — 2 minutes exactly. That
	// second budget is deliberately NOT the create-sized gitpkg.RepoCloneGateTimeout,
	// which for purge plus reap alone would be twenty times this whole margin and
	// would leave the number below meaning nothing.
	//
	// The cleanup's git itself is not in that sum. Each invocation is capped by
	// GitCommandTimeout, but that ceiling exists to turn "never returns" into a
	// reported error rather than because a `worktree remove` takes minutes.
	// Overrunning the margin anyway is still not a hang: the queued create gets a
	// clean Unavailable.
	TargetStartLockTimeout = BootstrapTimeout + 2*time.Minute
)

// TargetReapLockTimeout bounds the stranded-bootstrap reaper's wait for a target
// lock. Deliberately short, and nothing like the two above: the reaper is a
// background sweep with no caller waiting on it, so a busy target is a reason to
// come back later rather than to queue. Long enough to absorb the momentary hold
// of a cleanup finishing, short enough that one wedged target cannot slow a sweep
// across every other stranded row.
//
// A var, for the same reason SlowStartLockWaitThreshold is one: a test that
// proves the reaper DECLINES a busy target has to wait this timeout out, and at
// five seconds per case that cost lands on every run of the suite.
var TargetReapLockTimeout = 5 * time.Second

// SlowStartLockWaitThreshold is the wait above which a SUCCESSFUL session-start
// lock acquisition is logged at Warn (BOS-717). Both locks are meant to be taken
// essentially uncontended: seconds of waiting means creates are queueing behind
// each other, which is the shape that preceded the daemon-wide wedge.
//
// It lives here, beside the two timeouts it is the early warning for, because
// BOTH entry points onto these locks must warn at the same wait — the
// interactive StreamCreateSession and the task orchestrator (cron, dependabot,
// /boss-epic) contend with EACH OTHER, so two independently-chosen thresholds
// would make one path's contention invisible. A var so tests can exercise the
// warning without sleeping for seconds.
var SlowStartLockWaitThreshold = 5 * time.Second

var (
	startPathGates = &keyedgate.Registry{Name: "start-path"}
	targetGates    = &keyedgate.Registry{Name: "session-target"}
)

// AcquireStartPath takes the per-repo start-path lock. Callers must release it
// as soon as the session row is inserted, and must never hold it across the
// bootstrap — no worktree creation, no setup script, no agent start. That is the
// hold that caused the outage.
//
// It is not a "no I/O at all" lock, and the code does not pretend otherwise:
// StreamCreateSession's existing-session short-circuit runs under it and does a
// stat plus a stream write, so a stalled client can hold a repo's lock. Bounded
// acquisition is what keeps that a clean Unavailable for that one repo rather
// than a daemon-wide wedge.
//
// An empty repoID returns a no-op release. waited is returned on both the
// success and failure paths so callers can log it either way.
func AcquireStartPath(ctx context.Context, repoID string) (release func(), waited time.Duration, err error) {
	if repoID == "" {
		return func() {}, 0, nil
	}
	return startPathGates.Acquire(ctx, repoID, StartPathLockTimeout)
}

// AcquireTargetStart takes the per-target lock for the PR or branch a create is
// claiming. It is held across the bootstrap AND its failure cleanup: that hold
// is what makes an overlapping create for the same target wait for the first
// create's outcome, succeeding on its own terms once a failed create has deleted
// its half-started row rather than being refused as a duplicate of it.
//
// A create claiming neither a branch nor a PR gets a no-op release; the per-repo
// start-path lock still makes its insert atomic.
func AcquireTargetStart(ctx context.Context, repoID, branch string, prNumber *int) (release func(), waited time.Duration, err error) {
	key := targetStartKey(repoID, branch, prNumber)
	if key == "" {
		return func() {}, 0, nil
	}
	return targetGates.Acquire(ctx, key, TargetStartLockTimeout)
}

// acquireTargetForReap takes the per-target lock on behalf of the
// stranded-bootstrap reaper, so a reap serializes with the create/cleanup/retry
// lifecycle for that target instead of relying on elapsed age alone.
//
// Age alone was not enough. A create that fails on the bootstrap deadline holds
// this lock across its failure cleanup, and that cleanup's git is NOT inside the
// margin bootstrapReapThreshold adds (see TargetStartLockTimeout above, and
// gitpkg.GitCommandTimeout, which caps a single purge command at five minutes on
// its own). A cleanup that overruns the margin therefore left the reaper free to
// claim a row its original owner was still cleaning up — and once the owner
// finished, deleted the row and released this lock, a retry could recreate the
// same branch and worktree and have them purged out from under it by the
// reaper's delayed cleanup. Holding the lock across the reap closes that window:
// the retry cannot start until the reap is done, and the reap cannot start while
// the cleanup runs.
//
// ok=false means the target is busy and this row must be left for a later sweep.
// A row whose target key is empty — no branch and no PR — takes no lock and
// proceeds: the create path takes none for it either, so there is nothing to
// serialize against. That is not a gap, because a create with no explicit branch
// derives a UNIQUELY SUFFIXED one inside Create, so its retry cannot collide
// with the branch this reap is cleaning up.
func acquireTargetForReap(ctx context.Context, repoID, branch string, prNumber *int) (release func(), ok bool) {
	key := targetStartKey(repoID, branch, prNumber)
	if key == "" {
		return func() {}, true
	}
	release, _, err := targetGates.Acquire(ctx, key, TargetReapLockTimeout)
	if err != nil {
		return nil, false
	}
	return release, true
}

// targetStartKey names the session target a create is claiming.
//
// The PR number wins when there is one, and the branch is only the key for a
// PR-less create. That order is what makes the two entry points agree: the PR
// number is data BOTH callers hold verbatim, whereas the head branch is DERIVED
// — StreamCreateSession resolves it from a GitHub GetPRStatus call and silently
// leaves it empty when that call fails or the repo has no origin URL, while the
// task orchestrator takes it from the emitted task. Keying on the branch first
// therefore made two creates for the same PR take DIFFERENT gates precisely when
// GitHub was flaky, dropping the mutual serialization this lock exists for at
// the moment it is most likely to be needed.
//
// This is a TRADE, not a strict improvement. A PR-numbered create and a PR-less
// create naming the same physical branch now take DIFFERENT gates, where a
// branch-first key gave them one. That pair still cannot double-create — the
// duplicate check under the per-repo start-path lock catches it — but the loser
// is refused rather than made to wait for the winner's outcome. It is the rarer
// divergence, and unlike the other one it does not appear precisely when GitHub
// is down.
//
// An empty result means "no target claimed".
func targetStartKey(repoID, branch string, prNumber *int) string {
	if repoID == "" {
		return ""
	}
	switch {
	case prNumber != nil:
		return repoID + "\x00p\x00" + strconv.Itoa(*prNumber)
	case branch != "":
		return repoID + "\x00b\x00" + branch
	default:
		return ""
	}
}
