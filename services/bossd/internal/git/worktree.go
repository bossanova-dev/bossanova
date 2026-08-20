// Package git provides Git worktree management for the Bossanova daemon.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/gitremote"
	"github.com/recurser/bossalib/keyedgate"
	"github.com/recurser/bossalib/setupscript"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// RepoCloneGateTimeout is the clone gate's own ceiling: the window it covers is
// a handful of git invocations, each already capped at GitCommandTimeout, so a
// wait beyond this is a wedged holder rather than a queue that will drain.
//
// One caveat since BOS-876: the remote-touching invocations inside that window
// (Manager.Create's FetchBase, and availableNewBranchName's branch probes) go
// through runGitRemote, so each is capped at 3 x GitCommandTimeout rather than
// one. Attempts that fail fast add ~4s apiece and change nothing here, but a
// holder whose attempts HANG can legitimately outlast this ceiling — so read a
// timeout during a known remote outage as a retrying holder, not a wedged one.
//
// It is deliberately the LOOSER of the two bounds a queued create is subject to.
// keyedgate derives its wait from the caller's context, and every create on the
// bootstrap path carries a 10-minute session.BootstrapTimeout, so in practice
// that deadline is what a queued create fails on — this constant only binds a
// caller with a longer (or no) deadline, such as a repair or registration path
// creating a worktree outside a session bootstrap.
const RepoCloneGateTimeout = 20 * time.Minute

// RepoCloneCleanupGateTimeout is the clone gate's budget for the best-effort
// CLEANUP operations (PurgeWorktree, ReapLocalBranches) rather than for a
// create. It is two orders of magnitude shorter, and the difference is not
// tuning — the two waits mean different things:
//
//   - A create that finds the gate held is QUEUED behind another create for the
//     same clone, and there is no other way for it to get its worktree. Waiting
//     out the holder is the whole point, so its budget is sized to outlast one.
//
//   - A cleanup that finds the gate held has learnt that someone is actively
//     mutating this clone right now, and it is best-effort by construction: its
//     callers have already decided the session is dead and are about to drop or
//     block the row.
//
// Giving up is NOT free, and the earlier version of this comment claimed it was.
// The row naming these artifacts is deleted (the failure cleanups) or moved to
// Blocked and out of the sweep's state set (the stranded reaper) immediately
// after, and a surviving branch pushes the next same-titled create onto a
// `<branch>-2` path — so a skipped cleanup is generally abandoned rather than
// retried, and both callers log it at Error for exactly that reason. The trade
// is therefore one abandoned worktree against a stall, not a retry against a
// stall, and the stall is still much the worse of the two: it is measured on
// every caller below and bounded by nothing else.
//
// The stall is the real hazard, because every cleanup caller runs with NO
// deadline of its own: the failure cleanups run on context.WithoutCancel, and
// the stranded-bootstrap reaper on the daemon's poller context. At the
// create-sized budget one reap could hold the daemon's shared 2-minute ticker
// (which also drives stranded-cron recovery) for twenty minutes per session,
// and a failure cleanup could hold its session target lock for forty across
// purge plus reap — which is the budget session.TargetStartLockTimeout's margin
// is justified against. Sized so purge plus reap together fit inside that
// margin.
const RepoCloneCleanupGateTimeout = 30 * time.Second

// repoCloneGates serializes the MUTATING git window of worktree creation per
// clone (BOS-717).
//
// Two creates in one repo now bootstrap concurrently — that is the point of
// scoping the session start lock per repo, and it is what AC 1 buys. But both
// of them run `git fetch origin +refs/heads/<base>:refs/remotes/origin/<base>`
// against the SAME clone, and a fetch is a ref write: concurrent fetches
// contend on `refs/remotes/origin/<base>.lock` and the loser dies with "cannot
// lock ref". Concurrent creates also both read `availableNewBranchName` before
// either writes, so two same-titled creates can settle on the same branch and
// the loser fails with ErrBranchExists instead of getting the `-2` suffix the
// uniquifier exists to hand it.
//
// The process-global start mutex used to make all of this impossible by
// accident, and nothing wrote that property down. This gate restores exactly it
// and no more: keyed by clone path (so other repos are untouched) and released
// before the setup script — the long step, and the one a parallel epic actually
// needs to overlap.
//
// Package-level rather than per-Manager because the resource being guarded is a
// directory on disk, so two Managers over one clone must share the gate.
var repoCloneGates = &keyedgate.Registry{Name: "repo-clone"}

// ErrBranchExists is returned when a branch with the derived name already
// exists and the caller did not set Force in CreateOpts.
var ErrBranchExists = errors.New("branch already exists")

func isBranchAlreadyExistsGitOutput(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "fatal: a branch named ") &&
		strings.Contains(msg, " already exists")
}

// ErrBaseBranchNotReady is returned by MergeLocalBranch when the local base
// branch cannot be safely fast-forwarded to match origin/<base>. The error
// message always explains the condition (dirty tree, divergence, etc.) so
// callers can surface it to the user verbatim. (The local-only merge path
// genuinely mutates the checkout, so it still enforces this precondition.)
var ErrBaseBranchNotReady = errors.New("base branch not ready for sync")

// ErrLocalSyncDeferred is returned by SyncBaseBranch when the local base
// branch cannot be fast-forwarded right now because the base branch is
// checked out with a dirty working tree. It is NON-FATAL: the GitHub merge
// (or auto-merge) has already completed; the local fast-forward is recorded
// and retried later by RetryDeferredBaseSyncs. refs/remotes/origin/<base> is
// always freshened regardless, so new worktrees still branch from the merged tip.
var ErrLocalSyncDeferred = errors.New("local base sync deferred")

// isNonFastForwardGitOutput reports whether a git error is a refusal to
// move a ref because the update would not be a fast-forward (a diverged
// local base). It matches the fast-forward-specific phrasing git uses across
// `fetch <base>:<base>` ("[rejected] … (non-fast-forward)") and
// `merge --ff-only` ("Not possible to fast-forward, aborting."),
// case-insensitively. It deliberately does NOT match the bare word
// "rejected": both git paths that matter here emit an explicit
// fast-forward phrase, and "rejected" appears in unrelated ref-update
// failures (tag clobbers, shallow-update refusals, hook rejections). A
// caller (SyncBaseBranch) reclassifies a match as a benign diverged-base
// warning and returns nil, so a too-broad needle would silently swallow
// genuine errors.
func isNonFastForwardGitOutput(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"non-fast-forward",
		"not a fast-forward",
		"not possible to fast-forward",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// ErrRefLockContended is returned by a fetch that could not update a
// remote-tracking ref because another git process moved the same ref while this
// one was writing it. It is RETRYABLE and the request was never invalid: the
// only reason the fetch failed is that a concurrent writer won the race, and
// fetchWithRefLockRetry has already spent a bounded ladder losing it. A caller
// that surfaces this should say "try again", not "this failed" — which is why
// createSessionConnectError maps it to connect.CodeUnavailable rather than
// leaving it in the CodeInternal bucket with genuine failures.
var ErrRefLockContended = errors.New("concurrent git updated the same remote-tracking ref")

// isRefLockContentionGitOutput reports whether a git error is the loss of a race
// to write a remote-tracking ref: another git process — a second daemon profile,
// a human shell, an agent worktree, or one of this package's own ungated
// fetches — moved refs/remotes/origin/<base> between this fetch's read and its
// write. git says:
//
//	error: cannot lock ref 'refs/remotes/origin/main': is at <sha> but expected <sha>
//	 ! <old>..<new>  main -> origin/main  (unable to update local ref)
//
// Both halves are matched because which one surfaces depends on the refspec
// form: git prints the first on stderr and the second in the ref-update summary,
// and a fetch can carry either one alone.
//
// The needles are deliberately narrow, for the reason isNonFastForwardGitOutput
// above spells out. A bare "lock" also names index.lock and shallow.lock
// failures, which are a wedged repository rather than a lost race and must
// surface immediately; a bare "rejected" names tag clobbers and hook refusals.
// Matching either would spend two silent retries on a genuine failure and then
// label it retryable at the RPC boundary — telling the caller to re-run
// something that can never succeed.
//
// "cannot lock ref" is narrow but not narrow enough on its own: git reuses that
// same prefix for ref-lock failures that are permanent, and those land in
// exactly the wedged-repository class the paragraph above says must surface
// immediately. refLockTerminalReasons subtracts them back out.
//
// This answers a question about TEXT, and text is not enough on its own: the
// runners append git's raw stderr to a context-attributed error, so a fetch
// killed mid-walk can carry a real contention line for a ref it really was
// racing on. Ask ctx.Err() BEFORE asking this — fetchWithRefLockRetry does, and
// any other caller must too.
func isRefLockContentionGitOutput(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	matched := false
	for _, needle := range []string{
		"cannot lock ref",
		"unable to update local ref",
	} {
		if strings.Contains(msg, needle) {
			matched = true
			break
		}
	}
	if !matched {
		return false
	}
	for _, reason := range refLockTerminalReasons {
		if strings.Contains(msg, reason) {
			return false
		}
	}
	return true
}

// refLockTerminalReasons are the reasons git gives for a ref-lock failure that
// no amount of retrying can clear. They are subtracted from the needle match
// above because the cost of getting this wrong is not the 200ms of retries — it
// is that createSessionConnectError then answers CodeUnavailable, telling the
// caller to re-run a request that will fail identically every time, and burying
// the operator action (delete the stale .lock, rename the conflicting branch,
// fix the permissions) under a retry loop.
//
// Lowercase, because the message is lowercased before matching.
//
//   - "unable to create directory" — the refs/ tree cannot be extended, which is
//     a permissions or disk problem rather than a busy neighbour.
//   - "exists; cannot create" / "there are still refs under" / "non-empty
//     directory" / "at the same time" — a directory/file conflict between ref
//     names (refs/remotes/origin/foo against refs/remotes/origin/foo/bar), from
//     the ref backend or from the transaction that spotted both in one fetch.
//     Permanent until one of the two branches is renamed or pruned, which is why
//     it is worth naming here: slash-bearing branch names make it reachable.
//   - "permission denied" — an ownership or mode problem on the refs tree.
//   - "unable to resolve reference" / "reference broken" — the ref file exists
//     but cannot be read as a ref: corrupt content, or a symref loop. Cleared by
//     `git update-ref -d` or a re-clone, never by a retry.
//
// Deliberately NOT on this list: EEXIST on the ref's own lockfile,
//
//	cannot lock ref 'refs/remotes/origin/main': Unable to create
//	'.../refs/remotes/origin/main.lock': File exists.
//
// It reads like a wedged repository and it was on this list for one round, but
// it is the OPPOSITE case. Losing at lock acquisition is a wider window than
// losing at value verification, so this — not "is at X but expected Y" — is the
// likeliest way two concurrent fetches for the same remote-tracking ref
// actually collide, which is the collision this whole ladder exists to absorb.
// git's own advice under that message is to try again. A lock genuinely left
// behind by a crashed process is the rarer reading, and it costs 200ms of
// retries before surfacing — as CodeUnavailable, which is still what git tells
// the operator to do.
//
// None of these can appear inside a refname, so none can be triggered by a
// branch that happens to be called something unfortunate: every entry contains a
// space, and git refnames forbid spaces.
//
// One property worth knowing: the match runs over the WHOLE message, and `fetch
// --prune origin` (SyncBaseBranch) reports every ref it touched at once. A
// terminal failure on some unrelated branch in the same message therefore
// suppresses the retry for a genuine race on refs/remotes/origin/<base>
// reported alongside it. That direction is safe — it degrades to the
// pre-BOS-747 behaviour of surfacing immediately as CodeInternal, never to a
// wrong success. The four single-refspec sites are far less exposed, though not
// immune: fetch still auto-follows tags, so their summaries can name more than
// the one ref that was asked for.
var refLockTerminalReasons = []string{
	"unable to create directory",
	"exists; cannot create",
	"there are still refs under",
	"non-empty directory",
	"at the same time",
	"permission denied",
	"unable to resolve reference",
	"reference broken",
}

// gitRunFn is the shape shared by runGit and runGitRemote.
// fetchWithRefLockRetry takes one rather than hard-coding either, so every fetch
// site keeps the runner it already had: the remote-transport ladder BOS-876
// applied to FetchBase and CountMergeCommits stays exactly where it was applied,
// and the sites deliberately left on plain runGit are not widened into it as a
// side effect of this change.
type gitRunFn func(ctx context.Context, dir string, args ...string) (string, error)

// refLockRetryBackoff is the wait before each retry, and its LENGTH is the retry
// budget: two entries, so at most two retries and three attempts in all.
//
// The sizing follows from what the failure means. The losing fetch failed
// BECAUSE the winner already succeeded, so a retry is a re-read of a ref that
// has just settled — not a wait for a long operation to finish. Tens of
// milliseconds is the right order, and the whole ladder (200ms) stays far inside
// GitCommandTimeout so it can never be the reason a caller's budget runs out.
//
// A var so a test can zero the waits instead of sleeping through them; keep two
// entries when doing so, or the test quietly changes the retry count too.
var refLockRetryBackoff = []time.Duration{50 * time.Millisecond, 150 * time.Millisecond}

// fetchWithRefLockRetry runs a fetch through run, retrying only the lost-race
// signature isRefLockContentionGitOutput classifies. Every other failure — a
// missing branch, a bad repo, a full disk — is returned from the first attempt
// untouched, so this can never turn a real error into a delay.
//
// # Nesting under runGitRemote is bounded and disjoint
//
// Two call sites pass runGitRemote, so this ladder wraps another one. Neither
// ladder ever retries the other's failures, because their classifiers are
// disjoint. gitremote's transient set is argued entirely about the REMOTE
// (nothing was negotiated, or re-running converges); a ref-lock failure is local
// contention over refs/remotes/origin/<base> with a perfectly healthy remote, so
// gitremote classifies it terminal and returns after ONE attempt on exactly the
// failures this ladder retries. The converse holds too: a transport failure is
// not ref-lock contention, so this ladder returns it as soon as gitremote is
// done, adding no attempts of its own.
//
// Disjointness does NOT bound the product, though, and it is worth being exact
// about that rather than claiming a 3-attempt cap this code does not have. A
// MIXED failure sequence compounds the two ladders today: an inner gitremote
// ladder can spend two transport retries and then fail on ref-lock contention,
// which is the one error the outer helper retries — into a fresh inner ladder.
// The real worst case is 3 outer attempts x 3 inner ones = 9 git invocations.
//
// What makes that acceptable rather than a bug is the WAITS, which are the only
// part these ladders add: ~4s per inner ladder (1s and 3s, +/-20% jitter) and
// 200ms across the outer one, so under a minute of sleeping in the worst case.
// The ATTEMPTS themselves are not bounded by anything this file computes — an
// attempt that hangs costs GitCommandTimeout, so nine of them is a wall clock
// measured in tens of minutes, exactly as runGitRemote's own doc says of its
// three. What bounds the nest is the caller's context, which both ladders treat
// as authoritative; do not read the multiplication above as a time budget.
// Anything that raises either attempt count should re-run it first.
//
// On exhaustion the returned error wraps ErrRefLockContended AND the last git
// failure, so errors.Is finds the sentinel while the raw git text a human needs
// to read survives.
func fetchWithRefLockRetry(ctx context.Context, dir string, run gitRunFn, args ...string) (string, error) {
	for attempt := 0; ; attempt++ {
		out, err := run(ctx, dir, args...)
		if err == nil {
			return out, nil
		}
		// A failure that arrived with the context already ended is NOT a lost
		// race, whatever its stderr says, and asking the classifier first would
		// be wrong twice over. runGitWithTimeout appends git's raw stderr to the
		// context error, and `fetch --prune origin` walks refs one at a time and
		// reports each as it goes — so a fetch killed mid-flight can already
		// have printed "cannot lock ref" for some ref it genuinely was racing
		// on. The needle then matches an error whose actual cause is the
		// deadline. That would spend the whole ladder re-running git against a
		// context that cancels every attempt instantly, and then hand the caller
		// CodeUnavailable: retry a call your own context already ended. Return
		// the runner's error untouched instead — both runners wrap the context
		// error with %w, so errors.Is(err, context.DeadlineExceeded) still holds
		// for whoever is deciding how loudly to log this.
		if ctx.Err() != nil {
			return "", err
		}
		if !isRefLockContentionGitOutput(err) {
			return "", err
		}
		if attempt >= len(refLockRetryBackoff) {
			return "", fmt.Errorf("%w: %w", ErrRefLockContended, err)
		}
		select {
		case <-ctx.Done():
			// Ended mid-backoff, after a genuine contention match. The race was
			// real, but it is no longer the actionable fact: the caller is gone,
			// and labelling this ErrRefLockContended would answer CodeUnavailable
			// — "try again" — to something that ended because the caller stopped
			// asking. Lead with the context error so errors.Is finds
			// Canceled/DeadlineExceeded, and keep git's message behind it so the
			// race is still legible in the log. context.Cause reports the
			// cancellation reason where one was attached, and falls back to
			// ctx.Err() where none was.
			return "", fmt.Errorf("%w: %w", context.Cause(ctx), err)
		case <-time.After(refLockRetryBackoff[attempt]):
		}
	}
}

// ErrMergeConflict is returned by MergeLocalBranch when the local merge
// cannot proceed without conflict resolution. The caller should surface this
// so the user can resolve the conflict by hand; boss never auto-resolves.
var ErrMergeConflict = errors.New("merge conflict")

// ErrRebaseConflict is returned by RebaseOntoBase when replaying the branch
// onto the base stopped on a genuine content conflict. The rebase has already
// been aborted and the branch restored to its pre-rebase tip when this is
// returned, so the worktree is never left half-rebased. boss never
// auto-resolves — the session owns its own conflicts.
var ErrRebaseConflict = errors.New("rebase conflict")

// ErrRebaseFailed is returned by RebaseOntoBase when the rebase failed for a
// reason that is NOT a conflict: a cancelled context, a bad ref, a failing
// hook, a held index.lock. It is deliberately distinct from ErrRebaseConflict
// because a conflict is a benign, expected outcome the caller can log quietly,
// whereas everything else may indicate a wedged or damaged worktree and must be
// surfaced loudly.
var ErrRebaseFailed = errors.New("rebase failed")

// ErrBranchNotPushed is returned by RebaseOntoBaseAndPush when the local branch
// tip does not match refs/remotes/origin/<branch>. The push half of that
// operation anchors its --force-with-lease on the pre-rebase LOCAL tip, but git
// evaluates the lease against the actual remote ref: if the session has commits
// that were committed but never pushed, the lease is stale by construction and
// the push is always rejected. Detecting that up front turns a guaranteed
// rebase-then-discard cycle into a cheap skip.
var ErrBranchNotPushed = errors.New("local branch tip differs from origin")

// SetupScriptTimeout is the maximum time allowed for a setup script to run.
const SetupScriptTimeout = 5 * time.Minute

// GitCommandTimeout bounds a single git invocation (BOS-717). Every git command
// bossd runs — including the network-touching fetch/ls-remote/push — completes
// in seconds once the clone is warm; this is a generous ceiling whose only job
// is to turn "never returns" into a reported error. It matches SetupScriptTimeout
// so the two per-step ceilings stay legible against the overall bootstrap
// budget rather than drifting into arbitrary distinct numbers.
//
// The one honest exception it caps is the FIRST FetchBase against a very large
// repository over a slow link, where minutes of transfer is not a hang. Clone
// already warms the object store (and has its own larger budget below), so that
// fetch transfers a single branch rather than the repo; if it ever does need its
// own ceiling, give it runGitWithTimeout rather than raising this one.
//
// DERIVED from SetupScriptTimeout rather than restating its value, so the
// relationship the paragraph above asserts cannot rot into two independently
// edited numbers.
const GitCommandTimeout = SetupScriptTimeout

// GitCloneTimeout is the one deliberately larger per-invocation budget: a cold
// `git clone` of a large repository is honestly minutes of network transfer, so
// holding it to GitCommandTimeout would break repo registration rather than
// catch a hang. Cloning is not on the session-bootstrap path.
const GitCloneTimeout = 30 * time.Minute

// WorktreeManager manages Git worktrees for coding sessions.
type WorktreeManager interface {
	// Create creates a new worktree branching from baseBranch.
	// It returns the worktree path and branch name.
	Create(ctx context.Context, opts CreateOpts) (*CreateResult, error)

	// CreateFromExistingBranch creates a worktree that checks out an existing
	// remote branch (e.g. a PR head branch). It fetches the branch from origin
	// and creates a worktree tracking it.
	CreateFromExistingBranch(ctx context.Context, opts CreateFromExistingBranchOpts) (*CreateResult, error)

	// Archive removes the worktree directory but keeps the branch alive.
	Archive(ctx context.Context, worktreePath string) error

	// PurgeWorktree removes any worktree (registration + on-disk directory) for
	// branch under worktreeBaseDir without deleting the branch. Best-effort
	// cleanup after a failed session start; safe when nothing exists.
	//
	// A non-nil error means the purge did NOT RUN — callers must then skip the
	// branch reap, which `git branch -D` refuses while the worktree is still
	// registered. Its individual steps stay best-effort and are logged, not
	// returned, so nil does not promise the directory is gone.
	PurgeWorktree(ctx context.Context, repoPath, repoName, worktreeBaseDir, branch string) error

	// Resurrect re-creates a worktree from an existing branch and runs the
	// setup script if present.
	Resurrect(ctx context.Context, opts ResurrectOpts) error

	// ReapLocalBranches force-deletes LOCAL branches for archived sessions and
	// prunes stale worktree refs. It never contacts the remote.
	ReapLocalBranches(ctx context.Context, repoPath string, branches []string) error

	// DeleteLocalBranch force-deletes the LOCAL branch and prunes stale
	// worktree refs. It never touches the remote (no push --delete). Used by
	// archive gating to reclaim a session's branch once BranchSafeToDelete
	// confirms it is safe.
	DeleteLocalBranch(ctx context.Context, repoPath, branch string) error

	// BranchSafeToDelete reports whether branchTip is an ancestor of baseBranch
	// (merged, fast-forwarded, or a zero-commit NO_CHANGE branch), i.e. safe to
	// auto-delete locally on archive. A non-ancestor is (false, nil); only git
	// invocation failures return an error.
	BranchSafeToDelete(ctx context.Context, repoPath, branchTip, baseBranch string) (bool, error)

	// EmptyCommit creates an empty commit in the given worktree. This is
	// used to ensure a branch has at least one commit diverging from the
	// base branch before creating a PR.
	EmptyCommit(ctx context.Context, worktreePath, message string) error
	VerifyCurrentBranch(ctx context.Context, worktreePath, expectedBranch string) error

	// Push pushes the given branch to the "origin" remote.
	Push(ctx context.Context, worktreePath, branch string) error

	// PushWithLease force-updates the remote branch only if it still points at
	// expectedRemoteSHA and the local branch has integrated that SHA. It returns
	// the pushed local HEAD SHA.
	PushWithLease(ctx context.Context, worktreePath, branch, expectedRemoteSHA string) (string, error)

	// InjectPRNumbers rewrites the commit subjects on branch between its
	// merge-base with baseRef and HEAD, inserting "[#<prNumber>]" into any
	// conventional-commit subject that lacks it, then force-pushes the rewrite
	// to origin. Idempotent: already-tagged subjects are left untouched and no
	// push happens when nothing changes. Requires a clean working tree.
	InjectPRNumbers(ctx context.Context, worktreePath, branch string, prNumber int, baseRef string) error

	// Status runs `git status --porcelain` in the given worktree and returns
	// its trimmed stdout. Empty output means the working tree has no changes
	// (no untracked or modified files). Used by the cron-finalize path to
	// decide between the no-changes cleanup branch and the PR branch.
	Status(ctx context.Context, worktreePath string) (string, error)

	// LatestCommitSubject returns the subject line for HEAD in the given
	// worktree. Used by cron finalize to derive a human-facing PR title while
	// keeping the actual commit message conventional.
	LatestCommitSubject(ctx context.Context, worktreePath string) (string, error)

	// CommitSubjects returns the subjects of commits on HEAD ahead of baseRef,
	// oldest first. Used by cron finalize to give a PR-title suggester the full
	// change context instead of only the last commit.
	CommitSubjects(ctx context.Context, worktreePath, baseRef string) ([]string, error)

	// HasDiffAgainstBase reports whether HEAD has a non-empty diff against
	// baseRef, using three-dot (merge-base) semantics so it matches what
	// GitHub shows as the PR's diff. False means the branch changes nothing
	// relative to the base — the shape of a branch carrying only bossd's
	// empty draft-PR bootstrap commit. Finalize uses it as the last line of
	// defence before marking a PR ready for review (BOS-591).
	HasDiffAgainstBase(ctx context.Context, worktreePath, baseRef string) (bool, error)

	// BranchDebugSnapshot captures branch state used to diagnose draft PR
	// creation failures.
	BranchDebugSnapshot(ctx context.Context, worktreePath, branch, baseBranch string) (*BranchDebugSnapshot, error)

	// VerifyPushedBranchAheadOfBase verifies the worktree is on branch, HEAD is
	// ahead of origin/<baseBranch>, and origin/<branch> points at local HEAD.
	// It fetches the base first unless opts.SkipFetch is set.
	VerifyPushedBranchAheadOfBase(ctx context.Context, worktreePath, branch, baseBranch string, opts VerifyPushedBranchAheadOfBaseOpts) (*BranchVerification, error)

	// Clone clones a remote repository to the given local path.
	Clone(ctx context.Context, cloneURL, localPath string) error

	// DetectOriginURL returns the "origin" remote URL for the repo at the
	// given path, or empty string if none is configured.
	DetectOriginURL(ctx context.Context, repoPath string) (string, error)

	// IsGitRepo returns true if the given path is inside a git repository.
	IsGitRepo(ctx context.Context, path string) bool

	// DetectDefaultBranch returns the default branch name for the repo at
	// the given path by inspecting refs/remotes/origin/HEAD. Falls back to
	// "main" if the ref doesn't exist.
	DetectDefaultBranch(ctx context.Context, repoPath string) (string, error)

	// SyncBaseBranch freshens refs/remotes/origin/<base> (always, via
	// `git fetch --prune origin`) and then best-effort fast-forwards the
	// local refs/heads/<base>. It is never a merge blocker:
	//   - base not checked out → direct ref update via
	//     `git fetch origin <base>:<base>` (refuses non-fast-forward, so safe).
	//   - base checked out + clean → `git merge --ff-only`.
	//   - base checked out + dirty and a fast-forward is available → the local
	//     fast-forward is recorded and ErrLocalSyncDeferred is returned; the
	//     remote-tracking ref is already freshened. RetryDeferredBaseSyncs
	//     applies it later once the tree is clean.
	// A diverged local base is never force-moved: sync logs a warning and
	// returns nil, leaving the operator's commits intact.
	SyncBaseBranch(ctx context.Context, localPath, base string) error

	// RetryDeferredBaseSyncs re-attempts any local base fast-forwards that
	// SyncBaseBranch previously deferred (ErrLocalSyncDeferred) because the
	// base was checked out with a dirty working tree. Safety is re-validated
	// at apply time (a base that became diverged or is still dirty is left
	// alone), so it is safe to call on daemon start. Best-effort: it returns
	// no error and logs a one-line summary.
	RetryDeferredBaseSyncs(ctx context.Context)

	// IsAncestor reports whether ref is an ancestor of target in the repo
	// at localPath. Returns (false, nil) when it is not an ancestor;
	// returns an error only on git invocation failures (missing repo, bad
	// refs). Used by post-merge verification to confirm a PR's merge commit
	// landed on the base branch.
	IsAncestor(ctx context.Context, localPath, ref, target string) (bool, error)

	// FetchBase fetches the named base branch from origin so subsequent
	// reads of refs/remotes/origin/<base> reflect the remote's current
	// state. Used by post-merge verification.
	FetchBase(ctx context.Context, localPath, base string) error

	// CountMergeCommits reports how many merge commits exist on head that
	// are not already on origin/<base>. Used to detect PR branches GitHub
	// cannot rebase-merge before requesting that merge strategy.
	CountMergeCommits(ctx context.Context, localPath, base, head string) (int, error)

	// CountBehindBase reports how many commits exist on origin/<base> that
	// are not yet on branch — i.e. how far the branch has fallen behind the
	// base. origin/<base> is freshened first so the count reflects the
	// remote's current tip. Zero means the branch already contains the base.
	CountBehindBase(ctx context.Context, worktreePath, branch, base string) (int, error)

	// RebaseOntoBaseAndPush replays branch (which must be the worktree's
	// checked-out branch, with a clean working tree) on top of a freshly
	// fetched origin/<base>, then force-pushes the result with a lease
	// anchored on the pre-rebase tip. It never creates a merge commit by
	// construction. It refuses up front (ErrBranchNotPushed) when the local
	// tip does not already equal origin/<branch> — or when the branch no
	// longer exists on the remote at all — because the lease anchor would then
	// be stale by construction. Every other failure mode — conflict
	// (ErrRebaseConflict), any other rebase failure (ErrRebaseFailed), or a
	// rejected/failed push — restores the branch to its exact pre-rebase tip,
	// so the worktree is never left half-rebased and never silently diverges
	// from origin/<branch>. The restore is best-effort, not guaranteed: it is
	// skipped when no rebase is in progress and the worktree holds changes a
	// hard reset would destroy (work written after the clean-worktree
	// precondition was checked), and it can also fail outright if the
	// cleanliness probe or the restore command itself errors. Each of those
	// leaves the branch as-is and logs at Error for manual recovery.
	RebaseOntoBaseAndPush(ctx context.Context, worktreePath, branch, base string) (*RebaseResult, error)

	// MergeLocalBranch performs a safe local merge of head into base inside
	// the repo at localPath. It does not push anywhere. Requires a clean
	// working tree on base and returns ErrBaseBranchNotReady / ErrMergeConflict
	// with a human-readable message otherwise. Strategy accepts the same
	// values as MergePR ("merge", "squash", "rebase"; empty → "merge").
	MergeLocalBranch(ctx context.Context, localPath, base, head, strategy string) error
}

// BranchDebugSnapshot contains branch state useful for diagnosing PR creation
// failures against a remote base branch.
type BranchDebugSnapshot struct {
	CurrentBranch string
	HeadSHA       string
	RemoteHeadSHA string
	AheadBehind   string
}

// BranchVerification contains local/remote commit state for a PR branch.
type BranchVerification struct {
	HeadSHA       string
	BaseSHA       string
	RemoteHeadSHA string
	AheadCount    int
}

// VerifyPushedBranchAheadOfBaseOpts tunes the verification. The zero value
// fetches the base branch, which is what any caller that has not just fetched
// it needs. No production caller takes that default today — createDraftPR is
// the only one and it skips — so the fetching branch is kept deliberately, to
// leave the helper correct for a future caller rather than because one relies
// on it now.
type VerifyPushedBranchAheadOfBaseOpts struct {
	// SkipFetch skips freshening refs/remotes/origin/<baseBranch> and reads
	// the remote-tracking ref that is already present. Only set it when the
	// same call path fetched that base moments earlier; the verification then
	// compares against a possibly-stale base SHA.
	SkipFetch bool
}

// CreateOpts holds the parameters for creating a new worktree.
type CreateOpts struct {
	RepoPath          string    // Path to the main repository.
	BaseBranch        string    // Branch to base the worktree on (e.g. "main").
	WorktreeBaseDir   string    // Directory under which worktrees are created.
	RepoName          string    // Display name of the repo, used to derive worktree subdirectory.
	Title             string    // Session title, used to derive branch name.
	BranchName        string    // If set, use this branch name instead of deriving from Title.
	SetupScript       *string   // Optional setup script to run after creation.
	SetupScriptOutput io.Writer // If non-nil, setup script output is written here.
	Force             bool      // If true, remove any existing branch with the same name.

	// OnWorktreeReady, when non-nil, is called once `git worktree add` has
	// succeeded and BEFORE the setup script runs, with the branch name this
	// create settled on and the worktree path.
	//
	// It exists so a caller can durably record the branch it now owns while the
	// slowest, most hang-prone step is still ahead of it (BOS-717). The branch
	// is usually DERIVED here — from the session title, plus a uniquifying
	// suffix — so until Create returns, a caller that inserted a row before
	// calling has no idea what to clean up. A bootstrap killed by its deadline
	// inside the setup script (the 2026-08-06 incident's exact shape) would
	// otherwise leave the worktree and branch orphaned with nothing naming them.
	//
	// Deliberately after the add, not after the name is chosen: before the add,
	// this create does not yet own the branch, and recording a name another
	// create owns would point the failure cleanup at someone else's worktree.
	OnWorktreeReady func(ctx context.Context, branch, worktreePath string)
}

// CreateResult holds the output of a successful worktree creation.
//
// The four phase-duration fields attribute the cost of worktree creation. They
// deliberately do not sum to the caller's aggregate: on both creation paths the
// glue between phases is untimed, and clearStaleWorktree in particular is not
// always cheap — reaping a prior worktree and its Bazel output base can take
// seconds. That difference is the unattributed remainder, not a bug.
type CreateResult struct {
	WorktreePath string
	BranchName   string
	// SetupErr is non-nil when the worktree was created successfully but its
	// configured setup script failed. The worktree is still usable, so callers
	// may proceed (in a degraded/flagged state) rather than abort. nil means
	// the setup script ran cleanly or none was configured.
	SetupErr error

	// FetchDuration is the time spent fetching the base (Create) or existing
	// (CreateFromExistingBranch) branch from origin. Always populated on
	// success.
	FetchDuration time.Duration
	// BranchProbeDuration is the time spent probing for a unique branch name
	// (the local/remote existence checks in availableNewBranchName, including
	// its ls-remote). Zero when the probe is skipped: under CreateOpts.Force,
	// and always on the CreateFromExistingBranch path, which has no
	// name-collision probe to time.
	BranchProbeDuration time.Duration
	// WorktreeAddDuration is the time spent in the `git worktree add`
	// invocation that creates the worktree directory. Always populated on
	// success.
	WorktreeAddDuration time.Duration
	// SetupScriptDuration is the time spent running the repo's configured
	// setup script. Zero when no setup script is configured.
	SetupScriptDuration time.Duration
}

// CreateFromExistingBranchOpts holds the parameters for creating a worktree
// from an existing remote branch (e.g. a PR head branch).
type CreateFromExistingBranchOpts struct {
	RepoPath          string    // Path to the main repository.
	BranchName        string    // Remote branch to check out (e.g. "feature/foo").
	WorktreeBaseDir   string    // Directory under which worktrees are created.
	RepoName          string    // Display name of the repo, used to derive worktree subdirectory.
	SetupScript       *string   // Optional setup script to run after creation.
	SetupScriptOutput io.Writer // If non-nil, setup script output is written here.
}

// ResurrectOpts holds the parameters for resurrecting an archived worktree.
type ResurrectOpts struct {
	RepoPath     string // Path to the main repository.
	WorktreePath string // Target path for the worktree directory.
	BranchName   string // Existing branch to check out.
	// BaseBranch is the session's base branch, used to recreate BranchName when
	// the local branch is missing — e.g. after a BOS-180 safe-delete on archive
	// (merged into base, or a zero-commit NO_CHANGE branch). May be empty for
	// legacy call sites; recreation then fails loudly rather than orphaning.
	BaseBranch        string
	SetupScript       *string   // Optional setup script to run after creation.
	SetupScriptOutput io.Writer // If non-nil, setup script output is written here.
}

var _ WorktreeManager = (*Manager)(nil)

// Manager is the default WorktreeManager implementation backed by real git commands.
type Manager struct {
	logger zerolog.Logger
	// removeAll is injectable so teardown failure paths can be tested without
	// relying on platform-specific directory permission behavior.
	removeAll func(string) error
	// remoteBranches resolves every origin branch sharing a prefix in a single
	// ls-remote (BOS-539); remoteBranchProbe is the per-candidate fallback used
	// when that batched query fails. Both are injectable — same rationale as
	// removeAll — so the batching contract and its fail-open degradation can be
	// asserted without a network remote.
	remoteBranches    func(ctx context.Context, repoPath, prefix string) (map[string]struct{}, error)
	remoteBranchProbe func(ctx context.Context, repoPath, branch string) bool
	// LoginShell is the user's login shell ($SHELL, captured in settings). When
	// set, the repo setup script runs through it so per-project version-manager
	// shims (nodenv/asdf/…) land on PATH — otherwise the daemon's restricted
	// PATH can't find `pnpm` and setup silently skips dep/hook installation.
	// Set once after construction (main.go); zero value preserves direct exec.
	LoginShell string

	mu              sync.Mutex        // guards pendingBaseSync
	pendingBaseSync map[string]string // localPath -> base awaiting a deferred local fast-forward
}

// NewManager creates a new git WorktreeManager.
func NewManager(logger zerolog.Logger) *Manager {
	// Best-effort tidy-up of ssh control sockets left behind by a previous
	// daemon (BOS-878). Errors are logged inside, never returned: failing to
	// sweep debris is not a reason to refuse to manage worktrees.
	sweepStaleSSHControlSockets(logger, time.Now())
	return &Manager{
		logger:            logger,
		removeAll:         os.RemoveAll,
		remoteBranches:    remoteBranchesWithPrefix,
		remoteBranchProbe: remoteBranchExists,
	}
}

// sanitizeBranchName converts a session title into a valid git branch name.
// Example: "Fix the login bug!" → "fix-the-login-bug"
func sanitizeBranchName(title string) string {
	s := strings.ToLower(title)
	// Replace non-alphanumeric characters with hyphens.
	re := regexp.MustCompile(`[^a-z0-9]+`)
	s = re.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	// Truncate to a reasonable length.
	if len(s) > 60 {
		s = s[:60]
		s = strings.TrimRight(s, "-")
	}
	return s
}

// sanitizeDirName converts a name (e.g. repo display name) into a
// filesystem-safe directory component.
func sanitizeDirName(name string) string {
	s := strings.ToLower(name)
	re := regexp.MustCompile(`[^a-z0-9]+`)
	s = re.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "repo"
	}
	return s
}

// bossdManagedExcludePatterns are the gitignore patterns bossd ensures
// are present in every worktree's $GIT_COMMON_DIR/info/exclude so that
// bossd-managed artifacts (Claude session logs, hook config, etc.) don't
// pollute `git status` or get accidentally committed.
//
// .claude/settings.local.json is particularly load-bearing: bossd writes
// the cron Stop-hook config there with a bearer token. Without this
// exclude, `git status` reported it as untracked, which made the cron
// finalize pipeline misclassify "do nothing" runs as having Claude
// changes and route them to pr_failed → Blocked. (services/bossd/
// internal/session/finalize.go has a parallel in-process filter as
// belt-and-suspenders.)
//
// TODO(task20): .claude/settings.local.json is plugin-owned (declared by
// bossd-plugin-claude via ListIgnoredDirtyFiles). The correct fix is to
// query the agent client here and pass the union to ensureGitInfoExclude.
// That requires threading an AgentRunnerClient through Manager.Create, which
// crosses the git→claude package boundary — deferred until a future task
// adds the wiring. The in-process filter in finalize.go (which does call
// ListIgnoredDirtyFiles) is the primary fix; this exclude entry is
// belt-and-suspenders.
//
// .opencode/plugins/bossd-question.js is the opencode question-signal event
// hook (BOS-486). bossd-plugin-opencode drops it into the worktree just before
// `opencode run` and removes it when the run ends, but a crashed run can leave
// it behind — and while it exists it is untracked, which trips the same
// pr_failed → Blocked misclassification as the Stop-hook config.
//
// Unlike .claude/settings.local.json above, this entry is NOT the
// belt-and-suspenders half: it is the PRIMARY filter, because the plugin's own
// ListIgnoredDirtyFiles declaration cannot match the collapsed `?? .opencode/`
// line porcelain emits for a new untracked directory. Measurement and full
// reasoning:
// docs/solutions/logic-errors/spike-opencode-question-signal-events-unreachable.md
// ("Canonical: which dirty-file filter actually keeps the injected asset out of
// finalize").
//
// The pattern is the full path, not a directory, so a user-authored sibling
// under .opencode/plugins/ still shows up in `git status`.
var bossdManagedExcludePatterns = []string{
	".boss/",
	".claude/settings.local.json",
	".opencode/plugins/bossd-question.js",
}

// retiredGitInfoExcludePatterns are patterns bossd used to manage and no longer
// does. ensureGitInfoExclude deletes them, because info/exclude lives in
// $GIT_COMMON_DIR and is never rewritten from scratch: dropping a pattern from
// bossdManagedExcludePatterns only stops NEW repos from getting it, while every
// repo bossd has already touched keeps hiding the path from `git status` — and
// from commits — forever.
//
// The single entry below is the scratch directory of the legacy plugin BOS-815
// removed; it is deliberately the only place in this package that still spells
// the retired name, so scripts/legacy-support-refs.test.mjs can pin it.
//
// Removal is by exact whole-line match against a line bossd itself would have
// written — the pattern alone, no leading or trailing whitespace, no comment —
// and only inside a bossd-managed block (see bossdManagedExcludeBlock), so a
// user-authored line is left untouched whether it merely embeds the same word (a
// differently-named sibling directory, or a commented-out copy) or spells the
// pattern byte-for-byte somewhere the user put it.
var retiredGitInfoExcludePatterns = []string{
	".superpowers/",
}

// bossdExcludeMarker identifies the block of patterns bossd has added
// to info/exclude, so the additions are easy to spot and remove by hand.
const bossdExcludeMarker = "# bossd-managed: ignore worktree-local artifacts"

// bossdManagedExcludeBlock reports the half-open line range [start, end) that
// ensureGitInfoExclude treats as bossd-owned, given that lines[start] is the
// marker. Only lines inside such a range may be purged.
//
// Extent rule: a block is the marker plus the maximal run of immediately
// following lines that are each EXACTLY a pattern bossd writes (the caller's
// patterns) or used to write (retiredGitInfoExcludePatterns). Anything else —
// a blank line, a comment, a second marker, a user pattern, EOF — ends it.
//
// The looser candidates were rejected because each can swallow user lines:
// "until the next marker" and "until EOF" both absorb everything a user appended
// after bossd's last write (bossd only ever appends, so that is the normal place
// for a user to add their own patterns), and "until a blank line" absorbs them
// whenever the user did not leave one. This rule is the only one that cannot
// consume a line bossd could not itself have written.
//
// Failure mode, in both directions:
//
//	False negative (safe): a retired line separated from its marker by a line
//	bossd no longer writes stays put. It keeps hiding a path, which is the status
//	quo, and a later hand-edit fixes it. Today this is unreachable: the sole entry
//	of retiredGitInfoExcludePatterns is the only pattern ever dropped from
//	bossdManagedExcludePatterns, so the two sets below cover every line bossd has
//	ever written to this file.
//
//	False positive (unavoidable): a user who hand-typed the retired pattern on a
//	line directly adjacent to bossd's own entries under the marker loses it. Nothing
//	in the file records provenance, so that line is byte-identical to bossd's and no
//	rule can tell them apart; the marker is the strongest signal available.
//
// The caller must have established that lines[start] == bossdExcludeMarker.
func bossdManagedExcludeBlock(lines []string, start int, patterns []string) int {
	written := make(map[string]bool, len(patterns)+len(retiredGitInfoExcludePatterns))
	for _, p := range patterns {
		written[p] = true
	}
	for _, p := range bossdManagedExcludePatterns {
		written[p] = true
	}
	for _, p := range retiredGitInfoExcludePatterns {
		written[p] = true
	}
	end := start + 1
	for end < len(lines) && written[lines[end]] {
		end++
	}
	return end
}

// ensureGitInfoExclude appends the given patterns to the worktree's
// $GIT_COMMON_DIR/info/exclude, idempotently, and removes any
// retiredGitInfoExcludePatterns a previous bossd version wrote there — only from
// inside a bossd-managed block, so an identical line the user wrote themselves
// keeps hiding what they meant it to hide. Patterns already present (anywhere in
// the file) are skipped. Pre-existing content is otherwise preserved, in its
// original order.
//
// info/exclude lives in $GIT_COMMON_DIR, which for linked worktrees is
// the main repo's .git directory — so applying this once for any
// worktree of a repo benefits every other worktree of that same repo.
func ensureGitInfoExclude(ctx context.Context, worktreePath string, patterns []string) error {
	commonDir, err := runGit(ctx, worktreePath, "rev-parse", "--git-common-dir")
	if err != nil {
		return fmt.Errorf("resolve git common dir: %w", err)
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(worktreePath, commonDir)
	}
	excludePath := filepath.Join(commonDir, "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o750); err != nil {
		return fmt.Errorf("create info dir: %w", err)
	}

	existing, err := os.ReadFile(filepath.Clean(excludePath)) // #nosec G304 -- bossd-owned worktree-internal git info/exclude path derived from git-common-dir (filepath.Clean sanitized); owner=@recurser; review-by=2026-10-18; issue=BOS-423
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read exclude: %w", err)
	}

	retired := make(map[string]bool, len(retiredGitInfoExcludePatterns))
	for _, p := range retiredGitInfoExcludePatterns {
		retired[p] = true
	}

	// Split/join on "\n" is exactly invertible, so dropping retired entries this
	// way preserves both the order of every surviving line and whether the file
	// ended with a newline.
	lines := strings.Split(string(existing), "\n")
	kept := make([]string, 0, len(lines))
	have := make(map[string]bool, len(lines))
	var purged bool
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if line != bossdExcludeMarker {
			// Outside every managed block, so bossd cannot claim to have written
			// it — a byte-identical retired pattern here is the user's own, and
			// deleting it would un-hide a path they chose to ignore and let it
			// into a later commit.
			have[strings.TrimSpace(line)] = true
			kept = append(kept, line)
			continue
		}
		end := bossdManagedExcludeBlock(lines, i, patterns)
		block := make([]string, 0, end-i-1)
		var blockPurged bool
		for _, p := range lines[i+1 : end] {
			if retired[p] {
				blockPurged = true
				continue
			}
			block = append(block, p)
		}
		purged = purged || blockPurged
		// Drop a marker whose entire body we just purged, rather than leaving a
		// dangling header over nothing. A marker that was already empty before
		// this pass is left alone: we did not create it, so we do not tidy it.
		if !blockPurged || len(block) > 0 {
			have[strings.TrimSpace(line)] = true
			kept = append(kept, line)
		}
		for _, p := range block {
			have[strings.TrimSpace(p)] = true
			kept = append(kept, p)
		}
		i = end - 1
	}
	var missing []string
	for _, p := range patterns {
		if !have[p] {
			missing = append(missing, p)
		}
	}
	if len(missing) == 0 && !purged {
		return nil
	}

	var buf bytes.Buffer
	buf.WriteString(strings.Join(kept, "\n"))
	if len(missing) > 0 {
		if buf.Len() > 0 && !bytes.HasSuffix(buf.Bytes(), []byte("\n")) {
			buf.WriteByte('\n')
		}
		buf.WriteString(bossdExcludeMarker)
		buf.WriteByte('\n')
		for _, p := range missing {
			buf.WriteString(p)
			buf.WriteByte('\n')
		}
	}

	if err := os.WriteFile(excludePath, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("write exclude: %w", err)
	}
	// os.WriteFile only applies the mode when it creates the file; git init
	// pre-creates info/exclude (typically 0o644), so on that common path the
	// write above preserves the old, world-readable mode. Explicitly chmod so
	// the G306 tightening is actually delivered on the existing-file path too.
	// The path is bossd-owned and single-user (git reads it as owner), so 0o600
	// does not break any consumer.
	if err := os.Chmod(excludePath, 0o600); err != nil {
		return fmt.Errorf("chmod exclude: %w", err)
	}
	return nil
}

// runGit runs a git command in the given directory and returns stdout, bounded
// by GitCommandTimeout.
//
// BOS-717: exec.CommandContext alone only inherits whatever deadline the caller
// happened to carry, and the session bootstrap carried none — so a single git
// invocation that never returned (a credential helper waiting on a TTY, a
// wedged network read, a held index.lock) hung the whole create path with no
// upper bound. The timeout is per invocation and is layered under the caller's
// own deadline: context.WithTimeout keeps whichever fires first, so a shorter
// bootstrap budget still wins.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	return runGitWithTimeout(ctx, GitCommandTimeout, dir, args...)
}

// gitWaitDelay bounds how long cmd.Run may keep waiting after the process
// itself is gone (BOS-717).
//
// It is what makes the timeout above REAL. cmd.Stdout/cmd.Stderr are
// *bytes.Buffer rather than *os.File, so os/exec hands git a pipe and copies
// from it; cmd.Run does not return until every writer closes that pipe.
// exec.CommandContext kills only the git PID, so a grandchild git spawned and
// left behind — an ssh transport, or the credential helper waiting on a TTY that
// runGit's own doc comment names — keeps the pipe open and keeps cmd.Run blocked
// long past the deadline. Manager.Create holds the per-clone gate across exactly
// that window, so one such invocation wedges every create for the repo.
//
// lib/bossalib/setupscript/setupscript.go deliberately leaves WaitDelay UNSET
// for the opposite trade, and the difference is the workload: a setup script
// legitimately starts background services (a dev server, a package-manager
// daemon) that outlive it holding the pipe, so a WaitDelay there would report a
// SUCCESSFUL setup as failed routinely. git does not: the commands bossd runs
// exit synchronously, and git's own background maintenance (`gc --auto
// --detach`) daemonizes onto /dev/null before returning, so it never holds these
// pipes. A lingering writer here means something is genuinely stuck.
//
// BOS-878 adds exactly ONE known daemonizing grandchild to that picture, and it
// is safe for the same reason: the `ControlPersist=60s` ssh master outlives its
// git parent by up to a minute, but OpenSSH reopens the master's stdio on
// /dev/null before backgrounding it, so it never writes to these pipes. Were
// that not so, every COLD connection to a host would block here for the full
// delay and then surface exec.ErrWaitDelay below — a SUCCESSFUL fetch or push
// reported as a hard failure. Measured while adding this: a first multiplexed
// ls-remote returns in ~3s with a nil error while its master survives, so the
// pipe is closed at git's exit as this comment requires. Anything that changes
// the ssh options gitCommandEnv authors must preserve that property.
//
// Generous, because it starts counting only AFTER the process has been killed:
// it is drain time for output already written, not a second command budget. A
// var so a test can exercise the expiry without sleeping for it.
var gitWaitDelay = 10 * time.Second

// --- git SSH connection multiplexing (BOS-878) ------------------------------
//
// A single session start pays three separate SSH handshakes — the branch probe,
// the base fetch, and the push. OpenSSH connection multiplexing collapses them
// into one: the first invocation opens a master, the rest ride it, and the
// master expires ControlPersist seconds after the last one. That is a latency
// win on every create, and a smaller exposure window during a remote
// degradation like the one BOS-873 was opened for.
//
// It is also the riskiest change in that epic, because a master that is alive
// but WEDGED degrades every git op for that host rather than one. Three
// non-negotiable mitigations bound that: ConnectTimeout caps the connection
// attempt, the startup sweep below removes provably-dead sockets, and
// BOSSD_GIT_SSH_MULTIPLEXING=0 turns the whole thing off without a rebuild or a
// settings edit — an operator hitting this needs out NOW, not after restarting
// into a possibly-broken config.

// gitSSHMultiplexingEnv is that escape hatch. "0" disables multiplexing and the
// git environment is left exactly as bossd inherited it; anything else (including
// unset) leaves it on.
const gitSSHMultiplexingEnv = "BOSSD_GIT_SSH_MULTIPLEXING"

// sshControlPathLimit is the usable length of a Unix domain socket path.
// sun_path is 104 bytes on macOS/BSD and 108 on Linux, in both cases INCLUDING
// the terminating NUL — so 103 is the conservative floor across both. Taking the
// smaller means a path that would be refused on macOS is refused on Linux too:
// a few bytes of forgone multiplexing, never an unusable socket.
//
// This matters more than it sounds. "~/Library/Application Support/bossanova"
// already eats a third of the budget before the ssh dir and the socket name.
const sshControlPathLimit = 103

// sshControlNameBudget is the number of bytes reserved for the socket name once
// ssh has expanded its tokens. The name is a template (see
// sshControlSocketTemplate), so its final length is not knowable here — bossd
// therefore measures the fixed directory prefix plus this reservation rather
// than the template's own much shorter length.
//
// 40 is generous against the identity that actually occurs: "git@github.com-22"
// is 17 bytes. The budget is deliberately far above that realistic worst case
// rather than tuned close to it.
//
// It is a RESERVATION, not an enforced bound, and nothing here can make it one:
// ssh substitutes the tokens at connect time, long after this process has
// handed over the string, so the expanded length is unknowable from inside
// bossd. A remote whose identity exceeds 40 bytes — a long internal GHE
// hostname, say — therefore passes this guard and then exceeds sun_path inside
// ssh, which exits 255 with git reporting a failed connection. That failure mode
// and its workaround (BOSSD_GIT_SSH_MULTIPLEXING=0) are documented in
// docs/automation-troubleshooting.md, since bossd cannot detect it here.
const sshControlNameBudget = 40

// sshControlDirName is the app-data subdirectory the control sockets live in.
const sshControlDirName = "ssh"

// sshControlSocketStaleAfter is the age at which the startup sweep treats a
// socket file as debris.
//
// An hour against a 60s ControlPersist is a wide margin, not a proof: a socket's
// mtime is set when it is bound and is not refreshed by the connections that
// ride it, so a continuously reused master can own a socket far older than this.
// Removing a live master's socket does not break the git command in flight — it
// unlinks a name, and ControlMaster=auto simply opens a fresh connection the
// next time — so the cost of being wrong is one forgone reuse. The margin is
// what keeps that rare rather than routine.
const sshControlSocketStaleAfter = time.Hour

// gitSSHControlDir resolves the directory control sockets live in. A var so a
// test can point it at a temporary path — gitCommandEnv is reached from
// runGitWithTimeout, which has no manager and no configuration to thread one
// through, so there is nowhere to inject it as a parameter.
var gitSSHControlDir = defaultSSHControlDir

// defaultSSHControlDir resolves the control socket directory under the DEFAULT
// app data dir. It deliberately does not consult config.ConfiguredAppDataDir the
// way db and credmaterialize do: reaching the settings would mean threading them
// through runGitWithTimeout, which has neither a manager nor a config. The
// consequence an operator sees is that `app_data_dir` does not move these
// sockets, and that the length guard below therefore measures the default path
// even on an install that moved everything else — documented in
// docs/automation-troubleshooting.md so a short `app_data_dir` is not mistaken
// for a way around the platform's socket path limit.
func defaultSSHControlDir() (string, error) {
	base, err := config.DefaultAppDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, sshControlDirName), nil
}

// These once-guards keep each multiplexing-disabled reason to a single log line
// for the daemon's lifetime. Every git invocation rebuilds this environment, so
// an unguarded warning would bury the log in a fact that never changes.
//
// There is one guard PER REASON rather than one per code path. Sharing a guard
// across two reasons would let whichever fired first permanently silence the
// other, so an operator debugging a missing socket would be told the wrong
// cause — or nothing at all.
var (
	gitSSHControlPathWarnOnce  sync.Once
	gitSSHControlQuoteWarnOnce sync.Once
	gitSSHControlDirWarnOnce   sync.Once
	gitSSHControlMkdirWarnOnce sync.Once
)

// sshControlSocketName renders the control socket's file name for one remote
// identity: <user>@<host>-<port>.
//
// The socket MUST be keyed on the full identity. ssh does not verify that the
// master answering a ControlPath is connected to the host the client asked for,
// so a name shared across identities would silently route one host's git
// commands over another host's authenticated connection.
//
// `host` is the hostname AS WRITTEN in the remote URL, not the one ssh_config
// resolves it to. That distinction is the whole point: a multi-account setup
// gives each account an ssh_config alias with its own IdentityFile and a shared
// HostName, so keying on the resolved host would collapse them onto one master
// and let a push for one account ride the other account's authenticated
// connection. See sshControlSocketTemplate for the token that preserves it.
//
// Production never learns that identity: GIT_SSH_COMMAND is built once per git
// invocation, before git has resolved a remote, and sniffing the remote URL to
// find out would mean an extra git call on every command (and a special case
// for https origins that this change deliberately does not have). So production
// passes OpenSSH's OWN tokens through this same function and lets ssh do the
// substitution at connect time — see sshControlSocketTemplate. Feeding it a
// concrete identity, as the tests do, renders exactly the name ssh will
// materialize.
func sshControlSocketName(user, host, port string) string {
	return user + "@" + host + "-" + port
}

// sshControlSocketTemplate is the name bossd actually hands to ssh: the same
// layout with OpenSSH's remote-user (%r), original-host (%n) and port (%p)
// tokens in place of a concrete identity.
//
// %n, NOT %h. %h is the hostname after ssh_config resolution, so two aliases
// that differ only by IdentityFile — the standard multi-GitHub-account setup —
// both expand to the same name and share one master. ssh does not re-verify
// identity against an existing master, so that sharing is how a push authorized
// as one account travels over the other's connection. %n is the host as given,
// which is exactly the key that selected the ssh_config block in the first
// place, so distinct aliases get distinct sockets. Two spellings of the same
// host now get two masters instead of one: a forgone connection reuse, which is
// the safe direction to err.
//
// Note what is NOT used here. %C is OpenSSH's own hash of the identity and would
// give an exactly knowable length — but it is 64 hex characters, which on macOS
// leaves 103 - 64 = 39 bytes for "~/Library/Application Support/bossanova/ssh".
// That never fits, so %C would disable multiplexing on the platform bossd
// primarily runs on. The token form is ~17 bytes for a real git remote and fits
// with room to spare.
func sshControlSocketTemplate() string {
	return sshControlSocketName("%r", "%n", "%p")
}

// shellSingleQuote wraps s so a POSIX shell reproduces it verbatim, including
// any embedded single quote (a home directory such as /Users/o'brien is why this
// is not a bare "'" + s + "'").
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// sshControlPathOption renders the `-o ControlPath=...` fragment of the authored
// GIT_SSH_COMMAND, quoted for BOTH parsers that read it, and reports whether the
// path can be expressed safely at all.
//
// Getting this wrong breaks every ssh git operation on macOS, where the control
// directory is "~/Library/Application Support/bossanova/ssh" — it contains a
// space. TWO layers split the value, and quoting for only one is still broken:
//
//  1. git executes GIT_SSH_COMMAND through a shell, so an unquoted path is word
//     split; ssh then reads the tail ("Support/bossanova/ssh/%r@%n-%p") as its
//     destination and dies with "Could not resolve hostname".
//  2. ssh's own -o parser re-splits the value it receives, so shell quoting
//     alone yields "keyword controlpath extra arguments at end of line" and
//     exit 255.
//
// So the value is wrapped in double quotes for ssh's parser and the whole
// argument is single quoted for the shell. ssh consumes the double quotes before
// binding the socket — verified with `ssh -G`, which reports the path with the
// space intact and the quotes gone — so they cost nothing against
// sshControlPathLimit and the length accounting above is unchanged.
//
// A path containing a double quote or a backslash cannot be expressed through
// ssh's parser this way. Rather than emit a value that would fail at connect
// time, report it as unusable and let the caller disable multiplexing.
func sshControlPathOption(controlPath string) (string, bool) {
	if strings.ContainsAny(controlPath, "\"\\") {
		return "", false
	}
	return "-o " + shellSingleQuote(`ControlPath="`+controlPath+`"`), true
}

// sshEscapePercent doubles every percent in s so ssh's token expander emits it
// literally.
//
// ssh expands %-tokens anywhere in a ControlPath, not just in the name, so a
// control directory such as /Users/50%off would make ssh exit 255 with
// "vdollar_percent_expand: unknown key %o" on every git operation — the same
// total breakage as an unquoted path, and just as invisible from inside bossd.
// %% is ssh's own escape for a literal percent, verified with `ssh -G`, which
// reports the path with the single percent restored. Escaping rather than
// refusing keeps multiplexing working for those operators instead of silently
// switching it off for them.
//
// Only the directory goes through here. The template's %r/%n/%p are appended
// afterwards and must stay live.
//
// The doubling costs nothing against sshControlPathLimit: that limit applies to
// the path ssh binds, which is post-expansion, and expansion undoes it — which
// is why sshControlPath measures the raw directory and escapes only the value it
// hands on.
func sshEscapePercent(s string) string {
	return strings.ReplaceAll(s, "%", "%%")
}

// sshControlPath assembles the ControlPath under baseDir and reports whether it
// fits within sshControlPathLimit.
//
// Pure — it measures and never touches the filesystem — which is what lets a
// test assert the fallback by handing it a deep directory rather than by
// building one. Measuring at all is the point: ssh refuses an over-long
// ControlPath, which would turn this optimization into an outage.
func sshControlPath(baseDir string) (string, bool) {
	// The template expands at connect time, so the fixed prefix plus the
	// reserved expansion budget is what has to fit, not the template's own
	// length. +1 for the separator filepath.Join will insert.
	if len(baseDir)+1+sshControlNameBudget > sshControlPathLimit {
		return "", false
	}
	return filepath.Join(sshEscapePercent(baseDir), sshControlSocketTemplate()), true
}

// gitCommandEnv returns the environment for one git invocation: os.Environ()
// plus bossd's multiplexing GIT_SSH_COMMAND, or os.Environ() unchanged when
// multiplexing is off or cannot be configured safely.
//
// It is the single place cmd.Env is built, so every git command bossd runs is
// covered by construction rather than by remembering. Setting it unconditionally
// is safe for an https origin: git only consults GIT_SSH_COMMAND for the ssh
// transport, so there is no remote-URL sniffing here and no https special case.
//
// # Why bossd's entry goes FIRST
//
// Exporting GIT_SSH_COMMAND is a supported thing for an operator to do — a jump
// host, a pinned identity file, a wrapper — and bossd must not quietly replace
// it. os/exec deduplicates cmd.Env with the LAST assignment of a duplicate key
// winning, so bossd's value is prepended and the operator's copy, which arrives
// with os.Environ(), is the effective one. Their value is never filtered out;
// bossd's is simply outranked when they have one.
func gitCommandEnv(logger zerolog.Logger) []string {
	env := os.Environ()
	if config.EnvOr(gitSSHMultiplexingEnv, "1") == "0" {
		return env
	}

	dir, err := gitSSHControlDir()
	if err != nil {
		gitSSHControlDirWarnOnce.Do(func() {
			logger.Warn().Err(err).Msg("git ssh multiplexing disabled: cannot resolve the control socket directory")
		})
		return env
	}

	// Both checks run BEFORE the directory is created, so a path that cannot
	// work leaves nothing behind.
	controlPath, ok := sshControlPath(dir)
	if !ok {
		gitSSHControlPathWarnOnce.Do(func() {
			logger.Warn().
				Str("control_dir", dir).
				Int("limit", sshControlPathLimit).
				Int("name_budget", sshControlNameBudget).
				Msg("git ssh multiplexing disabled: the control socket path would exceed the platform limit")
		})
		return env
	}

	controlPathOption, ok := sshControlPathOption(controlPath)
	if !ok {
		gitSSHControlQuoteWarnOnce.Do(func() {
			logger.Warn().
				Str("control_dir", dir).
				Msg("git ssh multiplexing disabled: the control socket path cannot be quoted for ssh")
		})
		return env
	}

	// 0o700: the socket is a live channel into an already-authenticated
	// connection, so a group- or world-traversable parent would widen who can
	// ride it. MkdirAll leaves an EXISTING directory's mode alone, so this
	// establishes 0700 on the directory bossd creates and does not repair one an
	// operator has since widened.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		gitSSHControlMkdirWarnOnce.Do(func() {
			logger.Warn().Err(err).Str("control_dir", dir).
				Msg("git ssh multiplexing disabled: cannot create the control socket directory")
		})
		return env
	}

	// ConnectTimeout bounds the COLD path only — the TCP/handshake phase of
	// opening a new master. A client that attaches to an existing socket never
	// enters that phase, so this cannot bound an alive-but-wedged master; the
	// per-invocation GitCommandTimeout in runGitWithTimeout is the only thing
	// that does. It is still worth setting: a cold connect to an unreachable
	// host is the failure that would otherwise sit until that outer timeout.
	multiplexed := "GIT_SSH_COMMAND=ssh" +
		" -o ControlMaster=auto" +
		" -o ControlPersist=60s" +
		" " + controlPathOption +
		" -o ConnectTimeout=10"
	return append([]string{multiplexed}, env...)
}

// sweepStaleSSHControlSockets removes control socket files older than
// sshControlSocketStaleAfter from the control directory.
//
// ControlMaster=auto already copes with an ORPHANED socket — it opens a fresh
// connection when the socket does not answer — so this is not about those. It is
// about keeping the directory from accumulating debris across daemon restarts,
// and about a master that is alive but wedged. See sshControlSocketStaleAfter
// for why the age test is a margin rather than a proof of death, and why being
// wrong costs a reuse rather than a command.
//
// Best-effort by construction. It runs on the daemon's startup path, where a
// failure to tidy up is never a reason not to start, so every error is logged
// and none is returned. A missing directory is the ordinary first-run case and
// is not logged at all.
func sweepStaleSSHControlSockets(logger zerolog.Logger, now time.Time) {
	dir, err := gitSSHControlDir()
	if err != nil {
		logger.Debug().Err(err).Msg("skipping stale ssh control socket sweep: cannot resolve the control socket directory")
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			logger.Warn().Err(err).Str("control_dir", dir).Msg("stale ssh control socket sweep failed to read the control directory")
		}
		return
	}
	for _, entry := range entries {
		// Directories are not sockets, and os.Remove cannot remove a non-empty
		// one anyway — leave anything that is not a plain file alone.
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) <= sshControlSocketStaleAfter {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			logger.Warn().Err(err).Str("socket", path).Msg("failed to remove a stale ssh control socket")
			continue
		}
		logger.Debug().Str("socket", path).Msg("removed a stale ssh control socket")
	}
}

// runGitWithTimeout is runGit with an explicit per-invocation budget, for the
// handful of commands whose honest worst case exceeds the default (a cold clone
// of a large repository).
func runGitWithTimeout(parent context.Context, timeout time.Duration, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	// Every git command bossd runs gets its environment from the one helper
	// (BOS-878), so ssh multiplexing is covered by construction. This is a free
	// function with no manager, so it logs through the package logger.
	cmd.Env = gitCommandEnv(log.Logger)
	cmd.WaitDelay = gitWaitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Context attribution comes FIRST, and stays truthful with WaitDelay set:
		// a killed-by-deadline git reports the deadline, which is the useful fact,
		// rather than the pipe-drain expiry that followed it. Only a command that
		// finished on its own terms while a grandchild held the pipes open falls
		// through to the ErrWaitDelay branch below.
		if ctxErr := ctx.Err(); ctxErr != nil {
			// Attribute the kill correctly. ctx is derived, so ctx.Err() is
			// non-nil for BOTH "this invocation used its whole budget" and "the
			// caller's bootstrap deadline or cancellation killed it 20 seconds
			// in" — reporting the per-invocation budget in the latter case would
			// send a reader looking for a five-minute git command that never ran.
			if parentErr := parent.Err(); parentErr != nil {
				return "", fmt.Errorf("git %s: %w (caller's context ended): %s", strings.Join(args, " "), parentErr, strings.TrimSpace(stderr.String()))
			}
			return "", fmt.Errorf("git %s: %w (after %s): %s", strings.Join(args, " "), ctxErr, timeout, strings.TrimSpace(stderr.String()))
		}
		if errors.Is(err, exec.ErrWaitDelay) {
			// git itself finished; a process it left behind held the output pipe
			// past gitWaitDelay. Say so, because the git command is not the thing
			// to go looking at — its output was abandoned mid-read, so the result
			// cannot be trusted either way.
			return "", fmt.Errorf("git %s: %w: a child process held the output pipe open for more than %s after git exited: %s",
				strings.Join(args, " "), err, gitWaitDelay, strings.TrimSpace(stderr.String()))
		}
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// gitRemoteRetryPolicy bounds the retry ladder every remote-touching git
// invocation runs under. A var in the same spirit as gitWaitDelay above: a test
// swaps in a zero-sleep policy so the suite exercises the ladder without
// waiting on it.
var gitRemoteRetryPolicy = gitremote.DefaultPolicy()

// runGitRemoteFn is the single git invocation runGitRemote retries. A var so a
// test can script a failure sequence without a real remote — replacing it
// exercises the whole wrapper, and through it every converted call site, with no
// network at all.
var runGitRemoteFn = runGit

// runGitRemote is runGit for the invocations that touch origin, retried through
// the shared gitremote classifier and its bounded backoff (BOS-876).
//
// # Only remote-touching invocations belong here
//
// Local plumbing — rev-parse, branch --show-current, merge-base, reflog show,
// rev-list, worktree add — keeps calling runGit. Those cannot fail for a reason
// a second attempt fixes, so retrying one can only spend a caller's budget
// hiding a real repository problem (a broken ref, a held index.lock) behind a
// delay. The classifier is narrow for the same reason: see the gitremote package
// doc for the safety argument each signature owes, and note that widening it to
// anything that can fire AFTER ref negotiation would make retrying push unsafe.
//
// # Retries compose UNDER the caller's deadline, never over it
//
// runGit already caps each invocation at GitCommandTimeout, and gitremote.Do
// takes the caller's context as authoritative: it refuses to start once that
// context is done and abandons the ladder if it ends during a backoff wait. So a
// bootstrap carrying session.BootstrapTimeout still fails on that deadline, at
// that deadline — the ladder can only make the effective budget SHORTER by
// spending part of it waiting, never longer. That is the load-bearing invariant:
// whatever ceiling the caller brought still holds.
//
// The ladder's OWN ceiling depends on how attempts fail, and the two cases are
// three orders of magnitude apart:
//
//   - Attempts that fail fast — the incident shape, ssh refusing in under a
//     second — cost the backoff waits and nothing else: ~4s per operation under
//     DefaultPolicy's 3 attempts.
//   - Attempts that HANG cost GitCommandTimeout each. runGitWithTimeout reports
//     that as "git <argv>: context deadline exceeded (after 5m0s): <stderr>" with git's
//     stderr appended verbatim, so a wedge that printed a transient signature
//     before stalling classifies transient and is retried: 3 x GitCommandTimeout
//     plus the waits, i.e. ~15m, not ~4s.
//
// Deliberately no separate ladder budget is imposed here. A first fetch of a
// large repository is legitimately slow, and a ceiling tight enough to bound the
// hang case would truncate it; the caller's own context is the right place to
// say how long the operation may take. Callers that cannot afford the hang case
// must carry a deadline — every session-start path already does.
//
// The "(after N attempts over D)" suffix AttemptsError appends is what keeps
// either case reading as a repeated failure rather than a mystery hang.
func runGitRemote(ctx context.Context, dir string, args ...string) (string, error) {
	verb := ""
	if len(args) > 0 {
		verb = args[0]
	}

	var out string
	var lastErr error
	attempt := 0

	err := gitremote.Do(ctx, gitRemoteRetryPolicy, func(ctx context.Context) error {
		attempt++
		if attempt > 1 {
			// Reached only because gitremote.Do classified the previous failure
			// as transient and had an attempt left, so this logs exactly once
			// per RETRY — never on the success-first-try path. Warn rather than
			// Info: the next GitHub degradation should read as retries in
			// bossd.stderr.log rather than as silence.
			//
			// Read the fields as a pair: "attempt" names the try ABOUT TO START,
			// while "error" is the PREVIOUS try's failure — the one that earned
			// the retry. Full argv is carried because verb alone cannot answer
			// which branch or refspec was in flight.
			log.Warn().
				Err(lastErr).
				Str("git", verb).
				Str("args", strings.Join(args, " ")).
				Str("dir", dir).
				Int("attempt", attempt).
				Msg("retrying git remote operation after a transient failure")
		}
		out, lastErr = runGitRemoteFn(ctx, dir, args...)
		return lastErr
	})
	if err != nil {
		if lastErr != nil && !errors.Is(err, lastErr) {
			// gitremote.Do returns a BARE ctx.Err() when the context ends during
			// a backoff wait — git's own error is not reachable through it (see
			// the Do doc). Without this the incident that motivated BOS-876
			// would surface as "context deadline exceeded" alone, losing the
			// "Permission denied (publickey)" that says what actually broke.
			// The retry Warn above cannot cover it: on this path there is no
			// next attempt to log at the start of.
			log.Warn().
				Err(lastErr).
				Str("git", verb).
				Str("dir", dir).
				Int("attempts", attempt).
				Msg("git remote ladder abandoned before its last error could be returned")
			// Log alone is not enough: the caller keeps the returned error, and
			// on this path it is exactly the caller most likely to be facing a
			// broken remote. Re-attach git's message so the text consumers the
			// gitremote package doc names — a wrapped error's message, and the
			// session blocked reason a later child of BOS-873 will classify with
			// IsTransientMessage over its text — can still see the signature.
			// (No such classifier exists in the tree yet; this keeps the text it
			// will read intact rather than answering a present-day caller.)
			//
			// Only ctx.Err() is wrapped with %w, so errors.Is(err,
			// context.Canceled) and its DeadlineExceeded twin keep answering as
			// they did before. lastErr goes in with %v deliberately: it is not a
			// cause of the context ending, it is a separate failure that
			// happened first, and putting it in the cause chain would let
			// errors.Is find it through an error that did not come from it. Both
			// designed consumers read text, so text is all it owes them.
			return "", fmt.Errorf("%w (last git error: %v)", err, lastErr)
		}
		return "", err
	}
	return out, nil
}

// branchExists checks whether a local branch ref exists. It is a thin alias
// over refExists on the refs/heads/ namespace, so both local-ref probes share
// one implementation.
func branchExists(ctx context.Context, repoPath, branch string) bool {
	return refExists(ctx, repoPath, "refs/heads/"+branch)
}

func remoteBranchExists(ctx context.Context, repoPath, branch string) bool {
	_, err := runGitRemote(ctx, repoPath, "ls-remote", "--exit-code", "--heads", "origin", "refs/heads/"+branch)
	return err == nil
}

// remoteBranchesWithPrefix returns the set of origin branch names beginning with
// prefix, resolved in ONE ls-remote (BOS-539). The collision walk in
// availableNewBranchName only ever generates `<prefix>` and `<prefix>-N`, so a
// single `refs/heads/<prefix>*` query covers every candidate it can reach and
// the walk pays one ~4s network round trip instead of one per locally-absent
// candidate. Names outside the candidate shapes (e.g. `<prefix>bar`) also match
// the glob; that is harmless because membership is tested by exact name. Git
// refname rules forbid `*`, `?` and `[` in a branch, so the base cannot inject
// glob metacharacters, and the pattern reaches git as a literal argv element.
//
// `--exit-code` is deliberately NOT passed: a pattern matching nothing must exit
// 0 with empty output so an empty result stays distinguishable from a failed
// query. Callers rely on that to fail open (see availableNewBranchName) —
// only a returned error means the remote could not be consulted.
func remoteBranchesWithPrefix(ctx context.Context, repoPath, prefix string) (map[string]struct{}, error) {
	out, err := runGitRemote(ctx, repoPath, "ls-remote", "--heads", "origin", "refs/heads/"+prefix+"*")
	if err != nil {
		return nil, fmt.Errorf("list remote branches for %q: %w", prefix, err)
	}
	branches := make(map[string]struct{})
	for line := range strings.SplitSeq(out, "\n") {
		// Each line is "<sha>\t<ref>"; blank lines (empty output) are skipped.
		_, ref, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			continue
		}
		name := strings.TrimPrefix(ref, "refs/heads/")
		if name == "" {
			continue
		}
		branches[name] = struct{}{}
	}
	return branches, nil
}

// refExists reports whether ref resolves against the repo's already-known refs.
// It is a purely local probe (`rev-parse --verify --quiet`) — no network fetch —
// so it is safe on the resurrect hot path.
func refExists(ctx context.Context, repoPath, ref string) bool {
	_, err := runGit(ctx, repoPath, "rev-parse", "--verify", "--quiet", ref)
	return err == nil
}

// firstExistingRef returns the first candidate ref that resolves locally, along
// with true. If none resolve (or all are empty), it returns "", false. Empty
// candidates (e.g. an unset base branch) are skipped rather than probed.
func firstExistingRef(ctx context.Context, repoPath string, candidates []string) (string, bool) {
	for _, ref := range candidates {
		// Skip malformed candidates built from an empty branch/base name
		// (e.g. "refs/heads/" with nothing after the slash).
		if ref == "" || strings.HasSuffix(ref, "/") {
			continue
		}
		if refExists(ctx, repoPath, ref) {
			return ref, true
		}
	}
	return "", false
}

// bazelOutputBaseForWorktree resolves the Bazel output base that a built
// worktree keyed, by reading the worktree's `bazel-out` convenience symlink.
// Bazel keys exactly one output base per workspace path under $OUTPUT_USER_ROOT
// (default /var/tmp/_bazel_<user>/<key>), and points the in-worktree `bazel-out`
// symlink at <base>/execroot/_main/bazel-out. Splitting the target on the LAST
// "/execroot/" yields the output base — the prefix up to <key> — regardless of
// where $OUTPUT_USER_ROOT lives. The last occurrence (not the first) is used so
// that a base path which itself embeds "/execroot/" still resolves to the true
// base rather than a parent directory — the Bazel-appended
// "/execroot/_main/bazel-out" suffix is always the final occurrence. This
// deliberately never reconstructs the key via md5(path): Bazel's key is not
// reproducible that way (verified in BOS-447).
//
// It returns "" when the worktree was never built (no symlink), when the
// symlink cannot be read, or when the resolved target fails any of the defensive
// guards below — the caller must never RemoveAll an unexpected path. The symlink
// lives INSIDE the worktree, so callers must capture the base BEFORE removing the
// worktree dir.
//
// SECURITY: `bazel-out` lives inside an agent-writable worktree, so its target is
// untrusted input about to be handed to os.RemoveAll by the daemon. The guards
// below bind a reaped base to this worktree. The base must contain "/execroot/",
// be absolute, and sit directly under a Bazel output-user-root (a dir named
// `_bazel_<user>`); its execroot must also point back to this worktree's marker.
// This confines a malicious or corrupted target — an arbitrary absolute path, a
// relative target, or a `..`-escape (the base is filepath.Clean'd before the
// check) — to "" (a safe leak, never a wrong delete), and bounds any successful
// reap to the single Bazel output base bound to this worktree. The marker may be
// MODULE.bazel for bzlmod workspaces, or WORKSPACE(.bazel) for legacy workspaces.
//
// The `_bazel_<user>` anchor is load-bearing, not cosmetic: the marker check
// alone is FORGEABLE. An agent can point bazel-out at any same-user-writable
// directory and plant `<dir>/execroot/<name>/MODULE.bazel` as a symlink back to
// this worktree's marker, making that directory's marker resolve to ours. Without
// the anchor, teardown would then chmod+RemoveAll an arbitrary directory such as
// $HOME/important. Requiring base's parent dir to be `_bazel_*` confines even a
// fully-bound forgery to a child of a Bazel scratch root. The cost is that bases
// under a non-default `--output_user_root` are not reaped (they leak to "", so
// the orphan persists exactly as before this feature) rather than trusted — a
// deliberate trade of speculative custom-root support for a closed deletion hole.
// We never reconstruct Bazel's non-reproducible md5 key.
func (m *Manager) bazelOutputBaseForWorktree(worktreePath string) string {
	// A symlinked worktree path can point at another live workspace. RemoveAll
	// only removes that symlink, while os.Readlink below would follow its parent
	// and nominate the target workspace's output base for reaping. Reject it
	// before resolving anything under the supplied path.
	info, err := os.Lstat(worktreePath)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return ""
	}
	target, err := os.Readlink(filepath.Join(worktreePath, "bazel-out"))
	if err != nil {
		return ""
	}
	// Split on the LAST "/execroot/": if the base path itself embeds that
	// segment, the first-occurrence split would truncate to a parent directory
	// and reap far too much. idx <= 0 covers both "not found" (-1) and a target
	// that starts with "/execroot/" (0 → empty base), preserving the defensive
	// "" return.
	idx := strings.LastIndex(target, "/execroot/")
	if idx <= 0 {
		return ""
	}
	base := filepath.Clean(target[:idx])
	execrootPath := target[idx+len("/execroot/"):]
	execrootName, bazelOut, ok := strings.Cut(execrootPath, "/")
	if !ok || execrootName == "" || execrootName == "." || execrootName == ".." || strings.ContainsAny(execrootName, `/\\`) || bazelOut != "bazel-out" {
		return ""
	}
	// Defensive anchoring (see SECURITY note above): reject a relative target.
	if !filepath.IsAbs(base) {
		return ""
	}
	// Bind to a Bazel output-user-root: base must be a direct child of a dir
	// named `_bazel_<user>`. This is the non-forgeable half of the guard — the
	// marker check below is defeatable by a planted symlink, so the base's
	// location, which the agent cannot make point at a sensitive directory, is
	// what confines a forged target to a Bazel scratch root. (See SECURITY note.)
	//
	// The parent chain is resolved before the name check: a purely lexical
	// basename check is defeated by a symlinked ancestor (e.g. an agent creates
	// `/tmp/_bazel_fake -> $HOME` and a base of `/tmp/_bazel_fake/important` —
	// lexically the parent is `_bazel_fake`, but RemoveAll follows the ancestor
	// symlink and deletes `$HOME/important`). EvalSymlinks collapses the chain so
	// we test the REAL output-user-root's name. A parent that cannot be resolved
	// (e.g. it does not exist) is rejected.
	realParent, err := filepath.EvalSymlinks(filepath.Dir(base))
	if err != nil || !strings.HasPrefix(filepath.Base(realParent), "_bazel_") {
		return ""
	}
	for _, marker := range []string{"MODULE.bazel", "WORKSPACE", "WORKSPACE.bazel"} {
		worktreeMarker := filepath.Join(worktreePath, marker)
		info, err := os.Lstat(worktreeMarker)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		resolvedWorktreeMarker, err := filepath.EvalSymlinks(worktreeMarker)
		if err != nil {
			continue
		}
		baseMarker := filepath.Join(base, "execroot", execrootName, marker)
		resolvedMarker, err := filepath.EvalSymlinks(baseMarker)
		if err == nil && filepath.Clean(resolvedMarker) == filepath.Clean(resolvedWorktreeMarker) {
			return base
		}
	}
	return ""
}

// reapBazelOutputBase best-effort removes a worktree's Bazel output base after
// the worktree directory itself is gone. Bazel never garbage-collects a base
// when its worktree is deleted, so without this they accumulate (1.5-4 GB each)
// until the disk fills (BOS-447). It NEVER returns an error and NEVER fails
// teardown — every failure is logged at Debug and swallowed, so the worst case
// is an orphan base persisting exactly as before.
//
// base == "" (nothing to reap) and a base that no longer exists are clean
// no-ops. The shared content-addressed --disk_cache is a different directory and
// is never touched here.
func (m *Manager) reapBazelOutputBase(base string) {
	if base == "" {
		return
	}
	base = filepath.Clean(base)
	if _, err := os.Stat(base); err != nil {
		// Not-exist (nothing to reap) or unreadable — either way, best-effort.
		return
	}

	// We deliberately do NOT signal the base's Bazel server before removing it.
	// server.pid.txt records a pid, but that pid cannot be reliably tied back to
	// THIS output base: after an unclean death the OS recycles the pid, and a
	// recycled pid may belong to an unrelated process or — worse — a *different*
	// live worktree's Bazel server, whose active build a stray SIGTERM would
	// interrupt. There is no portable way to confirm the tie: the output base
	// appears in neither the server's `ps` argv (its title is only
	// `bazel(<workspace>)`) nor its `lsof` open files (an idle server holds
	// nothing under the base; its cwd is the worktree, which teardown has already
	// removed by the time we reap). Per that ambiguity we skip signalling
	// entirely. It costs nothing: an idle server isn't writing, so RemoveAll does
	// not race it (it just unlinks the server's already-closed files); an active
	// server is precisely what we must not kill. RemoveAll below fully reaps the
	// tree on its own — the pid file is one more file it deletes.
	//
	// chmod -R u+w is MANDATORY: Bazel marks the output tree read-only
	// (0555 dirs / 0444 files); on macOS os.RemoveAll otherwise fails because a
	// child cannot be unlinked from a non-writable parent. A pure-Go walk keeps
	// this dependency-free and testable. Per-entry chmod errors are ignored.
	// Symlink entries are skipped: WalkDir does not descend into them, they need
	// no write bit to be unlinked from their parent, and chmod'ing one would
	// follow it to its target outside the base — the TOCTOU that G122 warns of.
	_ = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // best-effort: keep walking past unreadable entries.
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil // never chmod through a symlink (see comment above).
		}
		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		// #nosec G122 -- best-effort chmod of a bossd-owned Bazel output base being torn down; `base` came from splitting a real in-worktree `bazel-out` /execroot/ symlink target (never md5-reconstructed) and symlink entries are skipped above, so no traversal escapes the captured base; owner=@recurser; review-by=2027-01-19; issue=BOS-447
		_ = os.Chmod(path, info.Mode()|0o200)
		return nil
	})

	if err := os.RemoveAll(base); err != nil {
		m.logger.Debug().Err(err).Str("base", base).Msg("reap bazel output base (best effort)")
		return
	}
	m.logger.Debug().Str("base", base).Msg("reaped bazel output base on worktree teardown")
}

// clearStaleWorktree removes a leftover worktree at wtPath so a fresh
// `git worktree add` can succeed. A directory can be left behind when a prior
// session was orphaned (daemon crash/restart, or a duplicate daemon stealing
// the socket) — `git worktree prune` alone does not remove an on-disk
// directory, so every subsequent attempt would fail with
// "fatal: '<path>' already exists" and wedge the branch forever (the dependabot
// repair-session loop). This is a no-op when nothing is on disk.
//
// Safe to call unconditionally, but no single guard is the reason, and the
// version of this comment that named only StreamCreateSession's existing-session
// short-circuit covered one caller out of three. Since BOS-717 the ways in are
// Create and CreateFromExistingBranch directly, plus PurgeWorktree from the two
// failed-bootstrap cleanups and the stranded-bootstrap sweep. What each relies
// on:
//
//   - Create with a derived branch (the default): availableNewBranchName has
//     just settled on a name no local or remote branch holds, so wtPath is
//     unused by construction. The short-circuit does NOT apply here — it returns
//     immediately for an empty branch, which is every title-derived create.
//   - Create with an explicit branch, and CreateFromExistingBranch: the
//     short-circuit plus the BOS-236 duplicate check, which answer a create
//     naming a branch some live session holds with that session.
//   - Create with Force: no guard, by request. Force means "remove any existing
//     branch of this name", so the caller has asked for the clobber.
//   - PurgeWorktree: it is only ever handed the branch of a session whose
//     bootstrap has already failed or been declared dead.
//
// All steps are best-effort.
func (m *Manager) clearStaleWorktree(ctx context.Context, repoPath, wtPath string) {
	// Capture the Bazel output base BEFORE the worktree dir (and its in-worktree
	// `bazel-out` symlink) is removed, then reap it AFTER. Typically a no-op here:
	// PurgeWorktree (which delegates to this) runs after a FAILED session start,
	// where the worktree was usually never built, so the symlink is absent.
	base := m.bazelOutputBaseForWorktree(wtPath)

	if _, err := os.Stat(wtPath); err == nil {
		m.logger.Warn().
			Str("path", wtPath).
			Msg("clearing stale worktree directory before create")
	} else if os.IsNotExist(err) {
		m.logger.Debug().
			Str("path", wtPath).
			Msg("pruning stale worktree registration before create")
	} else {
		m.logger.Debug().
			Err(err).
			Str("path", wtPath).
			Msg("stat stale worktree path before create")
	}

	// Drop any git registration for the path (handles a registered worktree),
	// then prune dangling refs, then remove whatever directory remains
	// (handles a directory left with no registration). Prune must run even
	// when the directory is already missing, because Git still treats a stale
	// registration as a branch checkout until $GIT_DIR/worktrees is pruned.
	if _, err := runGit(ctx, repoPath, "worktree", "remove", "--force", wtPath); err != nil {
		m.logger.Debug().Err(err).Msg("worktree remove (best effort)")
	}
	if _, err := runGit(ctx, repoPath, "worktree", "prune"); err != nil {
		m.logger.Debug().Err(err).Msg("worktree prune (best effort)")
	}
	if err := m.removeAll(wtPath); err != nil {
		m.logger.Warn().Err(err).Str("path", wtPath).Msg("remove stale worktree dir (best effort)")
		// Do not tear down this worktree's Bazel server/output base while its
		// directory remains usable. A later cleanup attempt can reap it after
		// successful removal.
		return
	}
	m.reapBazelOutputBase(base)
}

// PurgeWorktree removes any worktree (git registration + on-disk directory) for
// the given branch under worktreeBaseDir, WITHOUT deleting the branch. Used to
// clean up after a failed session start so a leftover directory can't wedge
// future attempts (notably the dependabot repair loop). Best-effort and safe
// when nothing exists. Path derivation matches Create/CreateFromExistingBranch.
//
// It takes the per-clone gate (see repoCloneGates), because `worktree remove`
// and `worktree prune` mutate the SAME shared clone a concurrent create is
// fetching and adding into — and since BOS-717 this really does run
// concurrently with live creates: the stranded-bootstrap reaper calls it from
// the daemon poller by design. The gate is not reentrant, so it is taken HERE
// rather than in clearStaleWorktree, which Create and CreateFromExistingBranch
// call directly while already holding it.
//
// The error reports whether the purge RAN, not whether it succeeded: the steps
// inside are best-effort and log their own failures. Only the refused gate
// returns non-nil, and it is returned rather than swallowed because the caller
// must then skip the branch reap — see the caller contract on
// WorktreeManager.PurgeWorktree.
func (m *Manager) PurgeWorktree(ctx context.Context, repoPath, repoName, worktreeBaseDir, branch string) error {
	release, err := m.acquireCloneGate(ctx, repoPath, "purge worktree")
	if err != nil {
		// Skipping is the only safe choice — stalling a caller that has no
		// deadline of its own is worse — but it is not a free one, and the
		// error is returned rather than swallowed so the caller can say what
		// was actually lost. Do NOT restate the old claim that "the next
		// create for this path clears it anyway": a create whose branch
		// survived settles on `<branch>-2` and clears THAT path instead.
		m.logger.Warn().Err(err).Str("repo", repoPath).Str("branch", branch).
			Msg("purge worktree: could not serialize against the shared clone; skipping (the caller reports what this leaves behind)")
		return err
	}
	defer release()

	wtPath := filepath.Join(worktreeBaseDir, sanitizeDirName(repoName), branch)
	m.clearStaleWorktree(ctx, repoPath, wtPath)
	return nil
}

// acquireCloneGate takes the per-clone gate for a MUTATING operation on the
// shared clone that is not part of worktree creation, logging a contended wait.
// op names the caller in that log line.
//
// It uses RepoCloneCleanupGateTimeout, NOT the create-sized budget — see there
// for why a refused cleanup is cheap and a stalled one is not.
//
// The gate is capacity-1 and NOT reentrant: only exported entry points may call
// this, and only ones no already-gated code path can reach. Create and
// CreateFromExistingBranch hold the gate across windows that call
// clearStaleWorktree and deleteBranchRef, so those stay ungated bodies.
func (m *Manager) acquireCloneGate(ctx context.Context, repoPath, op string) (func(), error) {
	release, waited, err := repoCloneGates.Acquire(ctx, repoPath, RepoCloneCleanupGateTimeout)
	if err != nil {
		return nil, fmt.Errorf("serialize git for %s: %w", repoPath, err)
	}
	if waited > time.Second {
		m.logger.Info().
			Str("repo", repoPath).
			Str("op", op).
			Dur("clone_gate_wait", waited).
			Msg("waited for concurrent git on this repo")
	}
	return release, nil
}

// Create creates a new git worktree with a fresh branch based on baseBranch.
func (m *Manager) Create(ctx context.Context, opts CreateOpts) (*CreateResult, error) {
	branch := opts.BranchName
	if branch == "" {
		branch = sanitizeBranchName(opts.Title)
	}

	if opts.BaseBranch == "" {
		return nil, fmt.Errorf("base branch is required")
	}

	// Serialize the mutating git window against this clone (see repoCloneGates).
	// Released explicitly below once `worktree add` has landed, so the setup
	// script — the long step — runs outside it. The deferred call is the
	// all-other-exits safety net; release is idempotent, so both firing is fine.
	releaseClone, cloneWait, err := repoCloneGates.Acquire(ctx, opts.RepoPath, RepoCloneGateTimeout)
	if err != nil {
		return nil, fmt.Errorf("serialize git for %s: %w", opts.RepoPath, err)
	}
	defer releaseClone()
	if cloneWait > time.Second {
		m.logger.Info().
			Str("repo", opts.RepoPath).
			Dur("clone_gate_wait", cloneWait).
			Msg("waited for a concurrent worktree create on this repo")
	}

	fetchStarted := time.Now()
	if err := m.FetchBase(ctx, opts.RepoPath, opts.BaseBranch); err != nil {
		return nil, err
	}
	fetchDuration := time.Since(fetchStarted)
	if !hasRef(ctx, opts.RepoPath, "refs/remotes/origin/"+opts.BaseBranch) {
		return nil, fmt.Errorf("origin/%s does not exist", opts.BaseBranch)
	}
	var branchProbeDuration time.Duration
	if !opts.Force {
		// For tracker-linked creates the CreateSession dedup guard (BOS-236)
		// fires before reaching here, so availableNewBranchName's allowSuffix
		// rename never masks a tracker duplicate. Behavior for non-tracker /
		// explicit-branch creates is unchanged.
		branchProbeStarted := time.Now()
		uniqueBranch, err := m.availableNewBranchName(ctx, opts.RepoPath, branch, opts.BranchName == "")
		if err != nil {
			return nil, err
		}
		branchProbeDuration = time.Since(branchProbeStarted)
		branch = uniqueBranch
	}

	wtPath := filepath.Join(opts.WorktreeBaseDir, sanitizeDirName(opts.RepoName), branch)

	// Ensure the worktree base directory exists.
	if err := os.MkdirAll(opts.WorktreeBaseDir, 0o750); err != nil {
		return nil, fmt.Errorf("create worktree base dir: %w", err)
	}

	// Check for an existing branch with the same name.
	if branchExists(ctx, opts.RepoPath, branch) {
		if !opts.Force {
			return nil, ErrBranchExists
		}

		m.logger.Warn().
			Str("branch", branch).
			Msg("force-removing existing branch")

		// Capture the Bazel output base BEFORE the worktree dir (and its
		// in-worktree `bazel-out` symlink) is removed, so force-recreating a
		// previously-built worktree does not leak the base it replaces — the exact
		// orphan BOS-447 targets. base=="" (never built) is a clean no-op.
		staleBase := m.bazelOutputBaseForWorktree(wtPath)

		// Remove any worktree that references this branch.
		if _, err := runGit(ctx, opts.RepoPath, "worktree", "remove", "--force", wtPath); err != nil {
			// Worktree may not exist — that's fine.
			m.logger.Debug().Err(err).Msg("worktree remove (may not exist)")
		}

		// Prune stale worktree refs so the branch is no longer locked.
		if _, err := runGit(ctx, opts.RepoPath, "worktree", "prune"); err != nil {
			m.logger.Debug().Err(err).Msg("worktree prune")
		}

		// Delete the local branch.
		if _, err := runGit(ctx, opts.RepoPath, "branch", "-D", branch); err != nil {
			return nil, fmt.Errorf("delete existing branch: %w", err)
		}

		// Reap only after git removed the worktree directory. If removal failed
		// for an unregistered stale directory, clearStaleWorktree below owns the
		// direct removal and reaps only after that succeeds.
		if _, err := os.Lstat(wtPath); os.IsNotExist(err) {
			m.reapBazelOutputBase(staleBase)
		}
	}

	m.logger.Info().
		Str("repo", opts.RepoPath).
		Str("branch", branch).
		Str("path", wtPath).
		Msg("creating worktree")

	// Clear any stale worktree left at this path by an orphaned prior session
	// so the add below doesn't fail with "already exists".
	m.clearStaleWorktree(ctx, opts.RepoPath, wtPath)

	// git worktree add -b <branch> <path> origin/<baseBranch>
	worktreeAddStarted := time.Now()
	if _, err := runGit(ctx, opts.RepoPath,
		"worktree", "add", "-b", branch, wtPath, "origin/"+opts.BaseBranch,
	); err != nil {
		if isBranchAlreadyExistsGitOutput(err) {
			return nil, ErrBranchExists
		}
		return nil, fmt.Errorf("worktree add: %w", err)
	}
	worktreeAddDuration := time.Since(worktreeAddStarted)

	// Tell the caller what it now owns, while the hang-prone setup script is
	// still ahead of us (BOS-717). See CreateOpts.OnWorktreeReady.
	if opts.OnWorktreeReady != nil {
		opts.OnWorktreeReady(ctx, branch, wtPath)
	}

	// Ensure bossd-managed paths (e.g. .boss/) are git-ignored before any
	// downstream step writes into them.
	if err := ensureGitInfoExclude(ctx, wtPath, bossdManagedExcludePatterns); err != nil {
		return nil, fmt.Errorf("ensure info/exclude: %w", err)
	}

	// Everything that touches the shared clone is now done — including
	// ensureGitInfoExclude, which read-modify-writes $GIT_COMMON_DIR/info/exclude,
	// a MAIN-CLONE file two overlapping creates would otherwise race on. Release
	// before the setup script: that step is minutes long, is scoped to the new
	// worktree, and is exactly what a parallel epic needs to overlap.
	releaseClone()

	// Run setup script if provided. A setup-script failure is non-fatal: the
	// worktree itself is valid (the git add above succeeded), so we surface the
	// failure on the result and let the caller decide rather than tearing the
	// worktree down and blocking the whole session.
	var setupErr error
	setupScriptDuration, err := m.runAndLogSetup(ctx, setupRunOpts{
		Op:       "create",
		RepoPath: opts.RepoPath,
		Worktree: wtPath,
		Branch:   branch,
		Script:   opts.SetupScript,
		Output:   opts.SetupScriptOutput,
	})
	if err != nil {
		setupErr = fmt.Errorf("setup script: %w", err)
	}

	if err := m.verifyCurrentBranch(ctx, wtPath, branch); err != nil {
		return nil, fmt.Errorf("verify created worktree branch: %w", err)
	}

	return &CreateResult{
		WorktreePath:        wtPath,
		BranchName:          branch,
		SetupErr:            setupErr,
		FetchDuration:       fetchDuration,
		BranchProbeDuration: branchProbeDuration,
		WorktreeAddDuration: worktreeAddDuration,
		SetupScriptDuration: setupScriptDuration,
	}, nil
}

func (m *Manager) availableNewBranchName(ctx context.Context, repoPath, branch string, allowSuffix bool) (string, error) {
	if branch == "" {
		return "", fmt.Errorf("branch name is required")
	}

	// One batched ls-remote covers every candidate this walk can generate, so a
	// colliding create pays a single network round trip rather than one per
	// locally-absent candidate (BOS-539). sync.OnceValues makes the three
	// properties that matter STRUCTURAL rather than flag-maintained: the query
	// runs at most ONCE (a failure is never retried per candidate), it runs
	// LAZILY — the closure is reached only past the free branchExists
	// short-circuit below, so a walk whose candidates all exist locally stays
	// entirely offline — and the cached error makes the degradation sticky.
	// Note the prefix is the BASE, never the candidate: `refs/heads/<base>*`
	// covers `<base>` and every `<base>-N`, whereas a candidate-keyed pattern
	// would not.
	resolveRemote := sync.OnceValues(func() (map[string]struct{}, error) {
		found, err := m.remoteBranches(ctx, repoPath, branch)
		if err != nil {
			// Log here so the lost batching is observable exactly once.
			m.logger.Warn().Err(err).Str("branch", branch).
				Msg("batched remote branch probe failed; falling back to per-candidate probes")
			return nil, err
		}
		return found, nil
	})

	// remoteTaken reports whether origin already holds candidate.
	//
	// Fail OPEN on a batch error: a failed query must never be read as "no
	// remote branches exist", which would hand back a name already taken on
	// origin and make the later push fail confusingly. Degrade to the
	// per-candidate probe instead.
	//
	// That restores exactly the pre-batching behaviour, no more:
	// remoteBranchProbe itself reports "free" on any error, so a PERSISTENT
	// outage (network down, auth broken) still ends in "branch free" as it
	// always did. What the fallback genuinely recovers is a batch-SPECIFIC
	// failure — e.g. a transient blip — where the per-candidate query answers.
	remoteTaken := func(candidate string) bool {
		found, err := resolveRemote()
		if err != nil {
			return m.remoteBranchProbe(ctx, repoPath, candidate)
		}
		_, ok := found[candidate]
		return ok
	}

	for i := 0; ; i++ {
		candidate := branch
		if i > 0 {
			if !allowSuffix {
				return "", ErrBranchExists
			}
			candidate = fmt.Sprintf("%s-%d", branch, i+1)
		}

		if branchExists(ctx, repoPath, candidate) || remoteTaken(candidate) {
			if i >= 99 {
				return "", fmt.Errorf("find unique branch name for %q: %w", branch, ErrBranchExists)
			}
			continue
		}
		return candidate, nil
	}
}

// Archive removes the worktree directory but keeps the git branch alive.
func (m *Manager) Archive(ctx context.Context, worktreePath string) error {
	m.logger.Info().Str("path", worktreePath).Msg("archiving worktree")

	// Capture the Bazel output base from the in-worktree `bazel-out` symlink
	// BEFORE any removal path runs, and reap it only AFTER a removal path
	// succeeds. base=="" (worktree never built) is a clean no-op.
	base := m.bazelOutputBaseForWorktree(worktreePath)
	removeAndReap := func() error {
		if err := removeWorktreeDir(worktreePath); err != nil {
			return err
		}
		m.reapBazelOutputBase(base)
		return nil
	}

	// Use the worktree path itself to find its parent repo.
	// git worktree remove needs to be run from the main repo, but we can
	// find it via the .git file in the worktree.
	repoPath, err := runGit(ctx, worktreePath, "rev-parse", "--git-common-dir")
	if err != nil {
		// Worktree is corrupted or not a valid git repo — fall back to
		// removing the directory directly. Stale worktree refs will be
		// cleaned up by `git worktree prune` during ReapLocalBranches.
		m.logger.Warn().Err(err).Str("path", worktreePath).
			Msg("worktree is not a valid git repo, removing directory directly")
		return removeAndReap()
	}
	// --git-common-dir returns the .git dir; we want the repo root.
	repoPath = filepath.Dir(repoPath)

	if _, err := runGit(ctx, repoPath, "worktree", "remove", "--force", worktreePath); err != nil {
		// git worktree remove failed — fall back to direct removal.
		m.logger.Warn().Err(err).Str("path", worktreePath).
			Msg("git worktree remove failed, removing directory directly")
		return removeAndReap()
	}
	m.reapBazelOutputBase(base)
	return nil
}

// removeWorktreeDir removes a worktree directory directly via os.RemoveAll.
func removeWorktreeDir(path string) error {
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove worktree dir: %w", err)
	}
	return nil
}

// Resurrect re-creates a worktree from an existing branch.
func (m *Manager) Resurrect(ctx context.Context, opts ResurrectOpts) error {
	m.logger.Info().
		Str("branch", opts.BranchName).
		Str("path", opts.WorktreePath).
		Msg("resurrecting worktree")

	// Ensure parent directory exists.
	if err := os.MkdirAll(filepath.Dir(opts.WorktreePath), 0o750); err != nil {
		return fmt.Errorf("create parent dir: %w", err)
	}

	// Add the worktree. If the local branch still exists, check it out directly
	// (unchanged path). If it was safe-deleted on archive (BOS-180), recreate it
	// from a start point resolved locally — restoring the exact tip when a ref is
	// still known, otherwise the base branch (equivalent for a merged/empty
	// branch). See BOS-421.
	if branchExists(ctx, opts.RepoPath, opts.BranchName) {
		if _, err := runGit(ctx, opts.RepoPath,
			"worktree", "add", opts.WorktreePath, opts.BranchName,
		); err != nil {
			return fmt.Errorf("worktree add: %w", err)
		}
	} else {
		// Resolve a start point from already-known refs only (no network fetch on
		// the resurrect hot path): prefer the exact remote head, then the LOCAL
		// base, then the remote base.
		//
		// Local base before remote base is deliberate: BOS-180's safe-delete is
		// judged against the local base (BranchSafeToDelete → merge-base
		// --is-ancestor <branch> <base>, where git resolves the bare <base> to
		// refs/heads/<base>). So refs/heads/<base> is the ONLY ref guaranteed to
		// contain the reaped branch's merged work; refs/remotes/origin/<base> can
		// lag it (e.g. a local merge not yet pushed, or a stale tracking ref) and
		// would silently omit that work. origin/<base> stays as a last resort for
		// the rare case where the local base branch is absent entirely.
		candidates := []string{
			"refs/remotes/origin/" + opts.BranchName,
			"refs/heads/" + opts.BaseBranch,
			"refs/remotes/origin/" + opts.BaseBranch,
		}
		startPoint, ok := firstExistingRef(ctx, opts.RepoPath, candidates)
		if !ok {
			return fmt.Errorf(
				"resurrect: branch %q missing and no start point resolved (base %q)",
				opts.BranchName, opts.BaseBranch,
			)
		}
		m.logger.Info().
			Str("branch", opts.BranchName).
			Str("start_point", startPoint).
			Msg("resurrect: local branch missing, recreating from start point")
		if _, err := runGit(ctx, opts.RepoPath,
			"worktree", "add", "-b", opts.BranchName, opts.WorktreePath, startPoint,
		); err != nil {
			return fmt.Errorf("worktree add (recreate branch): %w", err)
		}
	}

	// Ensure bossd-managed paths (e.g. .boss/) are git-ignored — covers
	// worktrees that predate this feature or had info/exclude cleared.
	if err := ensureGitInfoExclude(ctx, opts.WorktreePath, bossdManagedExcludePatterns); err != nil {
		return fmt.Errorf("ensure info/exclude: %w", err)
	}

	// Run setup script if provided. Non-fatal: this is the reattach path for an
	// existing worktree, where a failed setup step must not block resurrection.
	// Log it and carry on.
	// The error is intentionally dropped: runAndLogSetup has already logged it,
	// and resurrection continues regardless.
	_, _ = m.runAndLogSetup(ctx, setupRunOpts{
		Op:       "resurrect",
		RepoPath: opts.RepoPath,
		Worktree: opts.WorktreePath,
		Branch:   opts.BranchName,
		Script:   opts.SetupScript,
		Output:   opts.SetupScriptOutput,
	})

	if err := m.verifyCurrentBranch(ctx, opts.WorktreePath, opts.BranchName); err != nil {
		return fmt.Errorf("verify resurrected worktree branch: %w", err)
	}

	return nil
}

// ReapLocalBranches force-deletes local branches and prunes stale worktree
// refs. It never contacts or deletes remote branches.
//
// Like PurgeWorktree it takes the per-clone gate: `worktree prune` and
// `branch -D` write refs in the shared clone that a concurrent create is
// fetching into. The gate is not reentrant, so the ungated body lives in
// reapLocalBranchesLocked and no already-gated path calls this method.
func (m *Manager) ReapLocalBranches(ctx context.Context, repoPath string, branches []string) error {
	release, err := m.acquireCloneGate(ctx, repoPath, "reap local branches")
	if err != nil {
		// Returned rather than swallowed: unlike the purge, a refused branch
		// delete is how an orphaned branch goes unnoticed, and every caller
		// already logs this error.
		return err
	}
	defer release()
	return m.reapLocalBranchesLocked(ctx, repoPath, branches)
}

// reapLocalBranchesLocked is ReapLocalBranches' body, without the clone gate.
// Callers must already hold the gate for repoPath.
func (m *Manager) reapLocalBranchesLocked(ctx context.Context, repoPath string, branches []string) error {
	m.logger.Info().
		Int("count", len(branches)).
		Msg("reaping local branches")

	var errs []error
	// Prune before deleting: a stale worktree registration makes git branch -D
	// refuse the branch as checked out. Keep reaping if pruning fails because
	// the individual branch deletes may still succeed.
	if _, err := runGit(ctx, repoPath, "worktree", "prune"); err != nil {
		errs = append(errs, fmt.Errorf("prune worktrees before reaping local branches: %w", err))
		m.logger.Warn().Err(err).Msg("failed to prune worktrees")
	}

	seen := make(map[string]struct{}, len(branches))
	for _, branch := range branches {
		if _, ok := seen[branch]; ok {
			continue
		}
		seen[branch] = struct{}{}
		if err := m.deleteBranchRef(ctx, repoPath, branch); err != nil {
			errs = append(errs, err)
			m.logger.Warn().Err(err).Str("branch", branch).Msg("failed to delete local branch")
		}
	}
	return errors.Join(errs...)
}

// DeleteLocalBranch force-deletes a LOCAL branch and prunes stale worktree
// refs. It never touches the remote. The `-D` (force) form is deliberate: the caller
// gates deletion on BranchSafeToDelete, and the safe form (`-d`) would wrongly
// refuse squash/rebase-merged branches whose commits are not literal ancestors
// of the base.
func (m *Manager) DeleteLocalBranch(ctx context.Context, repoPath, branch string) error {
	// Prune BEFORE deleting. Archive's common path (`git worktree remove
	// --force`) already unregisters the worktree, but its os.RemoveAll fallback
	// deletes the directory without unregistering it — leaving a stale worktree
	// ref that makes `git branch -D` fail with "branch is checked out at
	// <missing path>". Pruning first clears any such stale registration so the
	// delete succeeds regardless of which archive path ran.
	if _, err := runGit(ctx, repoPath, "worktree", "prune"); err != nil {
		return fmt.Errorf("prune worktrees before deleting %q: %w", branch, err)
	}
	return m.deleteBranchRef(ctx, repoPath, branch)
}

// deleteBranchRef force-deletes one local branch ref. It intentionally does
// not prune worktrees or contact a remote, so callers can control pruning
// frequency. An already-absent ref is successful: trash cleanup is retryable.
func (m *Manager) deleteBranchRef(ctx context.Context, repoPath, branch string) error {
	branches, err := runGit(ctx, repoPath, "branch", "--list", "--format=%(refname:short)", branch)
	if err != nil {
		return fmt.Errorf("check local branch %q: %w", branch, err)
	}
	if strings.TrimSpace(branches) == "" {
		return nil
	}

	tipSHA, err := runGit(ctx, repoPath, "rev-parse", "--verify", "refs/heads/"+branch)
	if err != nil {
		return fmt.Errorf("read tip for local branch %q: %w", branch, err)
	}
	m.logger.Info().Str("branch", branch).Str("tip_sha", tipSHA).Msg("deleting local branch")
	if _, err := runGit(ctx, repoPath, "branch", "-D", branch); err != nil {
		return fmt.Errorf("delete local branch %q: %w", branch, err)
	}
	return nil
}

// BranchSafeToDelete reports whether the session's local branch is safe to
// auto-delete on archive. It implements SIGNAL 1 only: branchTip is an ancestor
// of baseBranch (`git merge-base --is-ancestor`, reused via IsAncestor). That is
// true for a merged/fast-forwarded branch AND for a zero-commit NO_CHANGE branch
// (its tip is trivially an ancestor of the base). A non-ancestor is a normal
// (false, nil); only real git invocation failures propagate as errors.
//
// branchTip may be a branch name or a tip SHA — the caller passes the session's
// BranchName directly and lets git resolve it.
//
// It is a named domain predicate on the WorktreeManager interface (rather than
// inlining the IsAncestor call at the caller) so the "safe to delete" policy has
// one home: the BOS-180 fast-follow below adds a second signal (PR-merged) here
// without touching archive gating. IsAncestor already provides interface-level
// testability, so this method is a policy seam, not a testability workaround.
//
// TODO(BOS-180 fast-follow): squash/rebase merges aren't caught by
// --is-ancestor; wire the PR-merged signal when cheaply available. Remote
// squash-merged branches are auto-deleted by GitHub.
func (m *Manager) BranchSafeToDelete(ctx context.Context, repoPath, branchTip, baseBranch string) (bool, error) {
	return m.IsAncestor(ctx, repoPath, branchTip, baseBranch)
}

// Push pushes the given branch to the "origin" remote.
func (m *Manager) EmptyCommit(ctx context.Context, worktreePath, message string) error {
	if _, err := runGit(ctx, worktreePath, "commit", "--allow-empty", "--no-verify", "-m", message); err != nil {
		return fmt.Errorf("empty commit: %w", err)
	}
	return nil
}

// Status runs `git status --porcelain` in the worktree. runGit already trims
// trailing whitespace, so empty output indicates a clean tree.
func (m *Manager) Status(ctx context.Context, worktreePath string) (string, error) {
	out, err := runGit(ctx, worktreePath, "status", "--porcelain")
	if err != nil {
		return "", fmt.Errorf("git status: %w", err)
	}
	return out, nil
}

func (m *Manager) LatestCommitSubject(ctx context.Context, worktreePath string) (string, error) {
	out, err := runGit(ctx, worktreePath, "log", "-1", "--pretty=%s")
	if err != nil {
		return "", fmt.Errorf("latest commit subject: %w", err)
	}
	return out, nil
}

// CommitSubjects returns the subjects of commits on HEAD that are ahead of
// baseRef, oldest first. It gives a PR-title suggester the full change context
// (not just the last commit). baseRef is typically the PR base branch (e.g.
// "dev"); an empty or unresolvable base returns an error the caller treats as
// "no history available" and falls back accordingly.
func (m *Manager) CommitSubjects(ctx context.Context, worktreePath, baseRef string) ([]string, error) {
	if strings.TrimSpace(baseRef) == "" {
		return nil, fmt.Errorf("commit subjects: empty base ref")
	}
	out, err := runGit(ctx, worktreePath, "log", baseRef+"..HEAD", "--pretty=%s", "--reverse")
	if err != nil {
		return nil, fmt.Errorf("commit subjects: %w", err)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// HasDiffAgainstBase reports whether HEAD has a non-empty diff against
// baseRef. It uses three-dot (`<baseRef>...HEAD`) semantics so the comparison
// is against the merge base — exactly the diff GitHub renders for a PR — and
// so a base that has independently advanced never makes an otherwise-empty
// branch look changed.
//
// `git diff --name-only` (not `--quiet`) is deliberate: --quiet signals
// "differences found" with exit status 1, which runGit cannot distinguish
// from a genuine git failure. Non-empty stdout is the unambiguous signal.
//
// `-z` is what makes "non-empty stdout" actually unambiguous. runGit
// TrimSpace's every command's stdout, so with newline-separated output a diff
// whose only changed path is made entirely of spaces would trim away to "" and
// be misread as "no diff" — refusing to mark a legitimate PR ready. The NUL
// terminator `-z` appends is not whitespace, so it survives the trim and any
// non-empty result stays non-empty.
func (m *Manager) HasDiffAgainstBase(ctx context.Context, worktreePath, baseRef string) (bool, error) {
	if strings.TrimSpace(baseRef) == "" {
		return false, fmt.Errorf("diff against base: empty base ref")
	}
	out, err := runGit(ctx, worktreePath, "diff", "--name-only", "-z", baseRef+"...HEAD")
	if err != nil {
		return false, fmt.Errorf("diff against base %s: %w", baseRef, err)
	}
	return strings.TrimSpace(out) != "", nil
}

// DraftPRPlaceholderCommitSubject is the subject of the empty commit bossd
// uses to give a branch a diff so GitHub will open a PR for it (package
// session creates this commit for session-start draft PRs and dirty-only
// cron finalize; see draftPRPlaceholderCommitSubject in lifecycle.go, which
// aliases this constant, and IsDraftPRPlaceholderSubject, which both packages
// use to recognise it). InjectPRNumbers' rebase --exec must never rewrite
// this subject: doing so used to insert a PR tag into the placeholder,
// making an otherwise-empty branch look like it carried real work and
// defeating finalize's empty-run guard (BOS-591).
const DraftPRPlaceholderCommitSubject = "chore: [skip ci] create pull request"

// draftPRPlaceholderPrefix is the conventional-commit "type: " prefix of
// DraftPRPlaceholderCommitSubject ("chore: "), derived from the constant
// rather than retyped so injectPRTagExec's placeholder-detection shell
// snippet can't drift from it.
var draftPRPlaceholderPrefix = mustConventionalPrefix(DraftPRPlaceholderCommitSubject)

// mustConventionalPrefix returns subject's leading conventional-commit
// "type: " (or "type(scope): ") prefix, including the trailing space, and
// panics at package init if there is none. Slicing on a bare strings.Index
// would silently yield a one-character prefix ("c") should the constant ever
// lose its ": " separator, quietly corrupting injectPRTagExec's shell snippet;
// failing loudly at init makes that impossible to miss.
func mustConventionalPrefix(subject string) string {
	i := strings.Index(subject, ": ")
	if i < 0 {
		panic(fmt.Sprintf("draft-PR placeholder subject %q has no conventional-commit %q prefix", subject, ": "))
	}
	return subject[:i+len(": ")]
}

var (
	draftPRPlaceholderConventionalPrefixRE = regexp.MustCompile(`^[[:alpha:]][[:alnum:]-]*(\([^)]*\))?!?:[[:space:]]+`)
	// One or more tags, each a single literal space — deliberately NOT
	// [[:space:]]+: this must match stripPlaceholderTagSed's
	// `(\[#[0-9]+\] )+` byte for byte. If it were looser, a placeholder tagged
	// with (say) a tab would be classified as a placeholder here while the
	// rebase --exec re-tagged it. Only this package's own injector emits these
	// tags and it always emits exactly one space.
	//
	// The `+` matters: tags STACK. The boss-finalize skill's add-pr-numbers.sh
	// (plugins/bossd-plugin-claude/skilldata/skills/boss-finalize/, and its
	// services/boss mirror) still tags the placeholder unconditionally, and its
	// idempotence check is `grep -q "\[#$PR_NUM\]"` — it only skips when THIS
	// run's number is already present. Two runs for different PR numbers
	// therefore leave `chore: [#7] [#42] [skip ci] create pull request`. A
	// single-tag strip would classify that as real work and reopen the very
	// guard BOS-591 closes, so the strip must be repeat-capable.
	// `[skip ci]` can never be eaten by it: the tag shape requires `#`+digits.
	// (The prefix RE above may stay loose: its looseness is neutralised by the
	// whole-subject equality check that follows it.)
	draftPRPlaceholderTagRE = regexp.MustCompile(`^(\[#[0-9]+\] )+`)
)

// IsDraftPRPlaceholderSubject reports whether subject is the draft-PR
// bootstrap placeholder commit (DraftPRPlaceholderCommitSubject), tolerating
// any run of "[#NNN] " PR tags injected immediately after the
// conventional-commit "type: "/"type(scope): " prefix — a rebase run before
// this fix, a retry against a branch already tagged for a different PR number,
// or the boss-finalize skill's still-unconditional add-pr-numbers.sh (whose
// idempotence check only recognises the current run's number, so tags stack)
// can all leave tags in place.
//
// It is exported because package session needs the same classification to
// decide whether a branch carries real work, and the dependency only runs one
// way (session imports git), so this package owns both the literal and the
// logic that interprets it.
//
// The normalization stays narrow despite the repeat: it strips only tags in the
// exact recognized shape, only at the front of the subject, only to decide
// membership — and the decision is still whole-subject equality against the
// constant, so no amount of stripping can turn genuine work into a placeholder
// ("feat(boss): [#1] [#2] add X" becomes "feat(boss): add X", which is not the
// constant). Callers must keep using the original, unmodified subject.
func IsDraftPRPlaceholderSubject(subject string) bool {
	t := strings.TrimSpace(subject)
	if t == DraftPRPlaceholderCommitSubject {
		return true
	}
	prefix := draftPRPlaceholderConventionalPrefixRE.FindString(t)
	if prefix == "" {
		return false
	}
	rest := t[len(prefix):]
	tag := draftPRPlaceholderTagRE.FindString(rest)
	if tag == "" {
		return false
	}
	return prefix+rest[len(tag):] == DraftPRPlaceholderCommitSubject
}

// InjectPRNumbers ports the boss-finalize skill's add-pr-numbers.sh into the
// daemon so cron runs satisfy the commit-message PR-tag policy without spawning
// a separate finalize chat. It rebases the branch onto its merge-base with
// baseRef and, via a per-commit --exec, inserts "[#<prNumber>]" into each
// conventional-commit subject missing it, then force-pushes the rewrite.
//
// All git hooks are disabled for the rewrite — the husky commit-msg hook is
// absent or broken in session worktrees — and GPG signing is forced off so an
// unconfigured key can't abort the amend. The working tree must be clean;
// callers commit or strip pending changes first. A partial rebase is aborted
// before returning an error so the worktree stays usable.
func (m *Manager) InjectPRNumbers(ctx context.Context, worktreePath, branch string, prNumber int, baseRef string) error {
	if prNumber <= 0 {
		return fmt.Errorf("invalid PR number %d", prNumber)
	}
	if err := m.verifyCurrentBranch(ctx, worktreePath, branch); err != nil {
		return fmt.Errorf("verify branch before PR-number injection: %w", err)
	}

	remoteHead, err := remoteBranchHead(ctx, worktreePath, branch)
	if err != nil {
		return err
	}

	baseSHA, err := runGit(ctx, worktreePath, "merge-base", "HEAD", baseRef)
	if err != nil {
		return fmt.Errorf("merge-base HEAD %s: %w", baseRef, err)
	}

	tag := "[#" + strconv.Itoa(prNumber) + "]"

	// Pre-scan the subjects: skip the rebase entirely when every commit already
	// carries the tag. A no-op `--exec` rebase still rewrites committer dates
	// (new SHAs), so this guard keeps a re-run a true no-op.
	subjects, err := runGit(ctx, worktreePath, "log", "--format=%s", baseSHA+"..HEAD")
	if err != nil {
		return fmt.Errorf("list commit subjects since base: %w", err)
	}
	needsTag := false
	for s := range strings.SplitSeq(subjects, "\n") {
		trimmed := strings.TrimSpace(s)
		// bossd never tags the placeholder (see injectPRTagExec), so it must
		// never make needsTag true either: a branch whose only commit is the
		// placeholder must be a true no-op here. It is skipped by
		// classification rather than by exact match precisely because it CAN
		// arrive already tagged — the boss-finalize skill's add-pr-numbers.sh
		// still tags it, and its tags stack (see draftPRPlaceholderTagRE).
		// Skipping it keeps the
		// rebase itself from running — git does preserve the SHA of a commit
		// its --exec leaves untouched, so this is not the only thing standing
		// between a placeholder-only branch and a new SHA, but it is what
		// keeps the run free of any rewrite, force-push, or lease check at
		// all. TestInjectPRNumbers_OnlyPlaceholderCommitSkipsRebase pins that.
		if trimmed == "" || IsDraftPRPlaceholderSubject(trimmed) {
			continue
		}
		if !strings.Contains(s, tag) {
			needsTag = true
			break
		}
	}
	if !needsTag {
		if _, err := runGit(ctx, worktreePath, "push", "-u", "origin", "HEAD:refs/heads/"+branch); err != nil {
			return fmt.Errorf("push already-tagged commits: %w", err)
		}
		return nil
	}

	if _, err := runGit(ctx, worktreePath, "merge-base", "--is-ancestor", remoteHead, "HEAD"); err != nil {
		return fmt.Errorf("remote branch head %s is not integrated in local branch before PR-number injection", remoteHead)
	}

	if _, err := runGit(ctx, worktreePath,
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.editor=true",
		"-c", "sequence.editor=true",
		"-c", "commit.gpgsign=false",
		"rebase", baseSHA, "--exec", injectPRTagExec(tag),
	); err != nil {
		// Leave the worktree usable: abort any partial rebase before returning.
		_, _ = runGit(ctx, worktreePath, "rebase", "--abort")
		return fmt.Errorf("rebase to inject PR numbers: %w", err)
	}

	// Force-push the rewrite. The explicit lease requires the
	// remote to still point at the remote head resolved before the rewrite, so
	// local commits on an existing PR branch can be tagged and pushed without
	// depending on stale remote-tracking refs.
	lease := "--force-with-lease=refs/heads/" + branch + ":" + remoteHead
	if _, err := runGit(ctx, worktreePath, "push", lease, "origin", "HEAD:refs/heads/"+branch); err != nil {
		return fmt.Errorf("force-push rewritten commits: %w", err)
	}
	return nil
}

func remoteBranchHead(ctx context.Context, worktreePath, branch string) (string, error) {
	out, err := runGit(ctx, worktreePath, "ls-remote", "--heads", "origin", "refs/heads/"+branch)
	if err != nil {
		return "", fmt.Errorf("resolve remote branch origin/%s: %w", branch, err)
	}
	fields := strings.Fields(out)
	if len(fields) == 0 || strings.TrimSpace(fields[0]) == "" {
		return "", fmt.Errorf("remote branch origin/%s not found", branch)
	}
	return strings.TrimSpace(fields[0]), nil
}

// injectPRTagExec builds the shell command git runs after each replayed commit
// during InjectPRNumbers' rebase. It mirrors add-pr-numbers.sh: skip if the
// subject already has the tag, otherwise insert it after the conventional
// "type(scope): " (or "type: ") prefix, falling back to appending it. The
// message is amended via stdin (-F -) so no subject quoting is needed, and
// --no-verify skips the worktree's commit-msg hook.
//
// Before any of that, it leaves the draft-PR placeholder commit's subject
// byte-identical (BOS-591): it strips any RUN of "[#NNN] " tags immediately
// after the placeholder's "chore: " prefix and compares the result to
// DraftPRPlaceholderCommitSubject, exiting 0 on a match. That covers an
// untagged placeholder, one already tagged by an earlier PR number, and one
// carrying several stacked tags — none may be retagged with this run's number.
// "at most one" would be the wrong belief here: see draftPRPlaceholderTagRE's
// comment for why tags stack. draftPRPlaceholderPrefix and
// DraftPRPlaceholderCommitSubject supply the literal text so it is never
// retyped here.
func injectPRTagExec(tag string) string {
	// `(\[#[0-9]+\] )+` strips a whole RUN of stacked tags in one substitution.
	// A `:a … ta` loop would also work but is a BSD-sed portability trap (BSD
	// consumes the rest of the line as the label name, so `:a; s/…/…/; ta`
	// silently defines a label called "a; s/…/…/"). A repeat group needs no
	// label and behaves identically under GNU and BSD `sed -E`. Kept in
	// lockstep with draftPRPlaceholderTagRE — see the comment there for why
	// tags stack in the first place.
	stripPlaceholderTagSed := "s/^" + draftPRPlaceholderPrefix + "(\\[#[0-9]+\\] )+/" + draftPRPlaceholderPrefix + "/"
	return "s=$(git log -1 --format=%s); " +
		"p=$(printf '%s' \"$s\" | sed -E '" + stripPlaceholderTagSed + "'); " +
		"if [ \"$p\" = '" + DraftPRPlaceholderCommitSubject + "' ]; then exit 0; fi; " +
		"if printf '%s' \"$s\" | grep -qF '" + tag + "'; then exit 0; fi; " +
		"n=$(printf '%s' \"$s\" | sed 's/): /): " + tag + " /'); " +
		"if [ \"$n\" = \"$s\" ]; then n=$(printf '%s' \"$s\" | sed 's/^\\([a-z][a-z]*\\): /\\1: " + tag + " /'); fi; " +
		"if [ \"$n\" = \"$s\" ]; then n=\"$s " + tag + "\"; fi; " +
		"b=$(git log -1 --format=%b); " +
		// --allow-empty so amending a commit that carries no diff can't abort
		// the whole rebase. The draft-PR placeholder no longer reaches this
		// line (it exits 0 above, BOS-591), so this now covers any *other*
		// empty commit a branch happens to carry.
		"if [ -n \"$b\" ]; then printf '%s\\n\\n%s' \"$n\" \"$b\"; else printf '%s' \"$n\"; fi | git commit --amend --no-verify --allow-empty -F -"
}

// branchDebugUnavailable is the sentinel recorded in a BranchDebugSnapshot
// field when the underlying git command fails (e.g. the remote ref does not
// exist yet). An explicit sentinel keeps failure logs self-documenting rather
// than leaving an ambiguous empty string.
const branchDebugUnavailable = "<none>"

func (m *Manager) BranchDebugSnapshot(ctx context.Context, worktreePath, branch, baseBranch string) (*BranchDebugSnapshot, error) {
	// A detached HEAD is exactly the kind of state worth reporting, so don't
	// abort the snapshot when there's no current branch — record it instead.
	current, err := m.currentBranch(ctx, worktreePath)
	if err != nil {
		current = "(detached)"
	}
	head, err := runGit(ctx, worktreePath, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("rev-parse HEAD: %w", err)
	}

	remoteHead, remoteErr := runGit(ctx, worktreePath, "rev-parse", "origin/"+branch)
	if remoteErr != nil {
		remoteHead = branchDebugUnavailable
	}

	aheadBehind, aheadErr := runGit(ctx, worktreePath, "rev-list", "--left-right", "--count", "origin/"+baseBranch+"...HEAD")
	if aheadErr != nil {
		aheadBehind = branchDebugUnavailable
	}

	return &BranchDebugSnapshot{
		CurrentBranch: current,
		HeadSHA:       strings.TrimSpace(head),
		RemoteHeadSHA: strings.TrimSpace(remoteHead),
		AheadBehind:   strings.TrimSpace(aheadBehind),
	}, nil
}

func (m *Manager) currentBranch(ctx context.Context, worktreePath string) (string, error) {
	out, err := runGit(ctx, worktreePath, "branch", "--show-current")
	if err != nil {
		return "", fmt.Errorf("current branch: %w", err)
	}
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("current branch: detached HEAD in %s", worktreePath)
	}
	return strings.TrimSpace(out), nil
}

func (m *Manager) verifyCurrentBranch(ctx context.Context, worktreePath, expectedBranch string) error {
	current, err := m.currentBranch(ctx, worktreePath)
	if err != nil {
		return err
	}
	if current != expectedBranch {
		return fmt.Errorf("worktree is on branch %q, expected %q", current, expectedBranch)
	}
	return nil
}

func (m *Manager) VerifyCurrentBranch(ctx context.Context, worktreePath, expectedBranch string) error {
	return m.verifyCurrentBranch(ctx, worktreePath, expectedBranch)
}

// CurrentBranch returns the branch currently checked out in worktreePath.
// It errors on a detached HEAD or any git failure.
func (m *Manager) CurrentBranch(ctx context.Context, worktreePath string) (string, error) {
	return m.currentBranch(ctx, worktreePath)
}

func (m *Manager) Push(ctx context.Context, worktreePath, branch string) error {
	m.logger.Info().
		Str("path", worktreePath).
		Str("branch", branch).
		Msg("pushing branch")

	if err := m.verifyCurrentBranch(ctx, worktreePath, branch); err != nil {
		return fmt.Errorf("verify branch before push: %w", err)
	}

	if _, err := runGitRemote(ctx, worktreePath, "push", "-u", "origin", "HEAD:refs/heads/"+branch); err != nil {
		return fmt.Errorf("push: %w", err)
	}
	return nil
}

// PushWithLease force-pushes branch, refusing if origin has moved off
// expectedRemoteSHA. It returns the pushed HEAD SHA.
//
// # The retried push re-sends an IDENTICAL lease, which can report a false failure
//
// This push runs through runGitRemote, so a transient failure is retried
// (BOS-876) — and the lease is NOT re-derived between attempts. That is right
// for the classifier's fail-before-negotiation signatures (a refused ssh
// handshake changes nothing on the remote), and it is what keeps the lease
// meaningful: re-reading origin between attempts would launder away the very
// concurrent update the lease exists to catch. It is deliberately pinned by
// TestPushWithLeaseRetriesTransientFailureAndStillRejectsStaleLease.
//
// The cost is on the other side. The classifier also accepts signatures that can
// fire AFTER receive-pack applied the update ("remote end hung up", "RPC
// failed", "early EOF" — see the gitremote package doc, which names
// --force-with-lease as the case to watch). If one of those lands on a push that
// really did take effect, attempt 2 sends the same lease against a remote now at
// headSHA, git answers "stale info", and this returns an error for a push that
// SUCCEEDED with exactly the intended content. Plain Push has no such hazard: it
// converges on "Everything up-to-date".
//
// Know what that costs on the live path before deciding it is cheap. The
// production caller is RebaseOntoBaseAndPush, reached from the keep-current
// sweep, and it treats a failed push as a failed rebase: it calls
// forceRestoreTip to put the local branch back on res.PriorHead. If the push had
// in fact landed, origin now holds the rebased tip while the local branch holds
// the old one, and every later sweep is refused by this function's own
// verifyBranchMatchesOrigin precondition — the session drops out of keep-current
// silently. No work is lost (a rebase only replays commits), but nothing
// self-heals either.
//
// Left as-is anyway, because the retry does not CREATE that hazard: a one-shot
// limb-2 failure on a landed push already returns an error today and unwinds
// identically, so all a retry changes is the second attempt's rejection text.
// The honest fixes — a limb-1-only classification seam in gitremote, or an
// ls-remote reconciliation that treats "remote is already at headSHA" as success
// on a late stale-info rejection — are a change to the shared classifier or to
// this function's contract, and belong to whoever takes on that divergence
// properly. Name the divergence plainly, because a reader of gitremote alone
// will assume the opposite: that package's doc says a retried lease "must be
// re-derived between attempts by whatever wires the retry, never assumed to
// survive a first try that may have partly landed". This function is the
// wiring, and it deliberately does not re-derive — for the reason two
// paragraphs up. The package doc states the safe default; this states the
// exception and what it costs.
//
// A false "push failed" is still the safe direction to leave open;
// silently clobbering a concurrent update would not be. Do not let it surprise
// you, and do not widen the classifier without revisiting this.
func (m *Manager) PushWithLease(ctx context.Context, worktreePath, branch, expectedRemoteSHA string) (string, error) {
	m.logger.Info().
		Str("path", worktreePath).
		Str("branch", branch).
		Str("expectedRemoteSHA", expectedRemoteSHA).
		Msg("pushing branch with lease")

	if strings.TrimSpace(expectedRemoteSHA) == "" {
		return "", errors.New("expected remote SHA is required for push with lease")
	}
	if err := m.verifyCurrentBranch(ctx, worktreePath, branch); err != nil {
		return "", fmt.Errorf("verify branch before push with lease: %w", err)
	}

	headSHA, err := runGit(ctx, worktreePath, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolve local HEAD before push with lease: %w", err)
	}
	headSHA = strings.TrimSpace(headSHA)

	expectedRemoteSHA = strings.TrimSpace(expectedRemoteSHA)
	if err := verifyLeaseSHAIntegrated(ctx, worktreePath, branch, expectedRemoteSHA, headSHA); err != nil {
		return "", err
	}

	lease := "refs/heads/" + branch + ":" + expectedRemoteSHA
	if _, err := runGitRemote(ctx, worktreePath, "push", "--force-with-lease="+lease, "origin", headSHA+":refs/heads/"+branch); err != nil {
		return "", fmt.Errorf("push with lease: %w", err)
	}
	return headSHA, nil
}

func verifyLeaseSHAIntegrated(ctx context.Context, worktreePath, branch, expectedRemoteSHA, headSHA string) error {
	if _, err := runGit(ctx, worktreePath, "merge-base", "--is-ancestor", expectedRemoteSHA, headSHA); err == nil {
		return nil
	}

	reflog, err := runGit(ctx, worktreePath, "reflog", "show", "--format=%H%x00%gs", "refs/heads/"+branch)
	if err != nil {
		return fmt.Errorf("verify lease SHA in local branch reflog before push with lease: %w", err)
	}
	if leaseSHAHasRebaseEvidence(reflog, expectedRemoteSHA, headSHA) {
		return nil
	}

	return fmt.Errorf("lease SHA %s is not integrated or rebased in local branch before push with lease", expectedRemoteSHA)
}

func leaseSHAHasRebaseEvidence(reflog, expectedRemoteSHA, headSHA string) bool {
	type entry struct {
		sha     string
		subject string
	}
	var entries []entry
	for _, line := range strings.Split(reflog, "\n") {
		sha, subject, ok := strings.Cut(line, "\x00")
		if !ok {
			continue
		}
		entries = append(entries, entry{sha: strings.TrimSpace(sha), subject: strings.TrimSpace(subject)})
	}

	for i := 1; i < len(entries); i++ {
		if entries[i].sha != expectedRemoteSHA {
			continue
		}
		previous := entries[i-1]
		if previous.sha == headSHA && strings.HasPrefix(previous.subject, "rebase (finish):") {
			return true
		}
	}
	return false
}

// VerifyPushedBranchAheadOfBase verifies the worktree is on branch, HEAD is
// ahead of origin/<baseBranch>, and origin/<branch> points at local HEAD.
// Remote-tracking refs live in the shared common git dir and `git push`
// maintains refs/remotes/origin/<branch> itself, so opts.SkipFetch reads both
// refs without a network round-trip when the caller already fetched the base.
func (m *Manager) VerifyPushedBranchAheadOfBase(ctx context.Context, worktreePath, branch, baseBranch string, opts VerifyPushedBranchAheadOfBaseOpts) (*BranchVerification, error) {
	if err := m.verifyCurrentBranch(ctx, worktreePath, branch); err != nil {
		return nil, err
	}
	if !opts.SkipFetch {
		if err := m.FetchBase(ctx, worktreePath, baseBranch); err != nil {
			return nil, err
		}
	}

	headSHA, err := runGit(ctx, worktreePath, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("resolve HEAD: %w", err)
	}
	baseSHA, err := runGit(ctx, worktreePath, "rev-parse", "origin/"+baseBranch)
	if err != nil {
		return nil, fmt.Errorf("resolve origin/%s: %w", baseBranch, err)
	}
	remoteHeadSHA, err := runGit(ctx, worktreePath, "rev-parse", "origin/"+branch)
	if err != nil {
		return nil, fmt.Errorf("resolve origin/%s: %w", branch, err)
	}

	verification := &BranchVerification{
		HeadSHA:       strings.TrimSpace(headSHA),
		BaseSHA:       strings.TrimSpace(baseSHA),
		RemoteHeadSHA: strings.TrimSpace(remoteHeadSHA),
	}

	if verification.RemoteHeadSHA != verification.HeadSHA {
		return verification, fmt.Errorf("remote head mismatch for %s: local HEAD %s, origin/%s %s", branch, verification.HeadSHA, branch, verification.RemoteHeadSHA)
	}

	aheadRaw, err := runGit(ctx, worktreePath, "rev-list", "--count", "origin/"+baseBranch+"..HEAD")
	if err != nil {
		return verification, fmt.Errorf("count commits over origin/%s: %w", baseBranch, err)
	}
	if _, err := fmt.Sscanf(strings.TrimSpace(aheadRaw), "%d", &verification.AheadCount); err != nil {
		return verification, fmt.Errorf("parse commits over origin/%s %q: %w", baseBranch, strings.TrimSpace(aheadRaw), err)
	}
	if verification.AheadCount == 0 {
		return verification, fmt.Errorf("head branch %s has no commits over base origin/%s (base SHA %s, head SHA %s)", branch, baseBranch, verification.BaseSHA, verification.HeadSHA)
	}

	return verification, nil
}

// Clone clones a remote repository to the given local path.
func (m *Manager) Clone(ctx context.Context, cloneURL, localPath string) error {
	m.logger.Info().
		Str("url", cloneURL).
		Str("path", localPath).
		Msg("cloning repository")

	if _, err := runGitWithTimeout(ctx, GitCloneTimeout, ".", "clone", cloneURL, localPath); err != nil {
		return fmt.Errorf("clone: %w", err)
	}
	return nil
}

// DetectOriginURL returns the "origin" remote URL for the given repo path.
func (m *Manager) DetectOriginURL(ctx context.Context, repoPath string) (string, error) {
	url, err := runGit(ctx, repoPath, "remote", "get-url", "origin")
	if err != nil {
		// No origin remote configured — not an error for our purposes.
		return "", nil
	}
	return url, nil
}

// IsGitRepo returns true if the given path is inside a git repository.
func (m *Manager) IsGitRepo(ctx context.Context, path string) bool {
	_, err := runGit(ctx, path, "rev-parse", "--git-dir")
	return err == nil
}

// SyncBaseBranch freshens refs/remotes/origin/<base> and best-effort
// fast-forwards the local refs/heads/<base>. It is never a merge blocker:
// a dirty checked-out base yields ErrLocalSyncDeferred (recorded for a later
// retry) and a diverged base is left untouched with a warning. See
// WorktreeManager.SyncBaseBranch for the full contract.
func (m *Manager) SyncBaseBranch(ctx context.Context, localPath, base string) error {
	if base == "" {
		return fmt.Errorf("base branch is required")
	}

	// Safe half — always runs. Fetch with --prune so merged-and-deleted
	// remote branches (e.g. the session branch `gh pr merge --delete-branch`
	// just removed) are dropped, and refs/remotes/origin/<base> reflects the
	// merged tip so new worktrees branch from it. Never touches the working tree.
	// Routed through fetchWithRefLockRetry because this runs on the post-merge
	// path — concurrently, by design, with a Create for a new session on the same
	// clone — and both write refs/remotes/origin/<base>.
	if _, err := fetchWithRefLockRetry(ctx, localPath, runGit, "fetch", "--prune", "origin"); err != nil {
		return fmt.Errorf("fetch --prune origin: %w", err)
	}

	current, isDetached, err := currentBranch(ctx, localPath)
	if err != nil {
		return fmt.Errorf("resolve HEAD: %w", err)
	}

	baseCheckedOut := !isDetached && current == base

	if !baseCheckedOut {
		// Base is not checked out — update the local ref directly without
		// touching the working tree. `fetch origin <base>:<base>` refuses any
		// non-fast-forward, so a diverged local base is rejected rather than
		// rewritten.
		//
		// The ONE fetch in this file deliberately NOT routed through
		// fetchWithRefLockRetry. It writes refs/heads/<base>, not the contended
		// refs/remotes/origin/<base>, and its failure is already classified
		// below by isNonFastForwardGitOutput into the deferred-sync state
		// machine (warn, clear the pending sync, return nil). Adding a second
		// classify-and-retry ahead of that would decide the branch's fate twice
		// and could hold a fetch open across the deferral bookkeeping.
		if _, ferr := runGit(ctx, localPath, "fetch", "origin", base+":"+base); ferr != nil {
			if isNonFastForwardGitOutput(ferr) {
				m.warnDivergedBase(localPath, base)
				m.clearPendingBaseSync(localPath)
				return nil
			}
			return fmt.Errorf("fast-forward local %s: %w", base, ferr)
		}
		m.clearPendingBaseSync(localPath)
		return nil
	}

	// Base is checked out — the fast-forward would move the working tree, so
	// it must be clean.
	dirty, err := runGit(ctx, localPath, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("git status: %w", err)
	}

	if dirty == "" {
		// Clean — fast-forward via merge so the working tree tracks HEAD.
		if _, merr := runGit(ctx, localPath, "merge", "--ff-only", "refs/remotes/origin/"+base); merr != nil {
			if isNonFastForwardGitOutput(merr) {
				m.warnDivergedBase(localPath, base)
				m.clearPendingBaseSync(localPath)
				return nil
			}
			return fmt.Errorf("ff-only merge origin/%s: %w", base, merr)
		}
		m.clearPendingBaseSync(localPath)
		return nil
	}

	// Dirty and checked out — decide whether a fast-forward is even needed.
	// If origin/<base> is already an ancestor of the local base, the local
	// ref is at or ahead of origin: nothing to do.
	originIsAncestor, err := m.IsAncestor(ctx, localPath, "refs/remotes/origin/"+base, "refs/heads/"+base)
	if err != nil {
		return fmt.Errorf("check origin ancestry of local %s: %w", base, err)
	}
	if originIsAncestor {
		m.clearPendingBaseSync(localPath)
		return nil
	}

	// A clean fast-forward is available (local base is an ancestor of origin)
	// but the tree is dirty — defer it and record for a later retry.
	localIsAncestor, err := m.IsAncestor(ctx, localPath, "refs/heads/"+base, "refs/remotes/origin/"+base)
	if err != nil {
		return fmt.Errorf("check local %s ancestry of origin: %w", base, err)
	}
	if localIsAncestor {
		m.recordPendingBaseSync(localPath, base)
		return ErrLocalSyncDeferred
	}

	// Neither is an ancestor — the base has diverged. Never force-move.
	m.warnDivergedBase(localPath, base)
	m.clearPendingBaseSync(localPath)
	return nil
}

// warnDivergedBase logs the one-line divergence warning shared by the sync
// paths: the operator has local-only commits on <base>, so it is left as-is.
func (m *Manager) warnDivergedBase(localPath, base string) {
	m.logger.Warn().
		Str("local_path", localPath).
		Str("base", base).
		Msgf("local %s has diverged from origin/%s; leaving local ref untouched", base, base)
}

func (m *Manager) recordPendingBaseSync(localPath, base string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pendingBaseSync == nil {
		m.pendingBaseSync = make(map[string]string)
	}
	m.pendingBaseSync[localPath] = base
}

func (m *Manager) clearPendingBaseSync(localPath string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.pendingBaseSync, localPath)
}

// RetryDeferredBaseSyncs re-runs SyncBaseBranch for each pending deferred local
// fast-forward. See WorktreeManager.RetryDeferredBaseSyncs.
func (m *Manager) RetryDeferredBaseSyncs(ctx context.Context) {
	type pending struct{ localPath, base string }

	m.mu.Lock()
	snapshot := make([]pending, 0, len(m.pendingBaseSync))
	for localPath, base := range m.pendingBaseSync {
		snapshot = append(snapshot, pending{localPath: localPath, base: base})
	}
	m.mu.Unlock()

	if len(snapshot) == 0 {
		return
	}

	stillDeferred := 0
	for _, p := range snapshot {
		// SyncBaseBranch re-validates safety at apply time and self-updates the
		// pending map (clears on success/divergence, re-records if still dirty).
		if err := m.SyncBaseBranch(ctx, p.localPath, p.base); err != nil {
			if errors.Is(err, ErrLocalSyncDeferred) {
				stillDeferred++
				continue
			}
			// Log-and-continue: a single bad repo must not block the rest.
			m.logger.Warn().Err(err).
				Str("local_path", p.localPath).
				Str("base", p.base).
				Msg("retry of deferred local base sync failed")
		}
	}

	m.logger.Info().
		Int("retried", len(snapshot)).
		Int("still_deferred", stillDeferred).
		Msg("retried deferred local base syncs")
}

// FetchBase fetches origin/<base> so refs/remotes/origin/<base> reflects
// the remote's current tip. Narrower than the `--prune origin` fetch in
// SyncBaseBranch so it is safe to call on the verification hot path.
func (m *Manager) FetchBase(ctx context.Context, localPath, base string) error {
	if base == "" {
		return fmt.Errorf("base branch is required")
	}
	// A fetch is a ref WRITE, and runGit sets cmd.Dir from localPath
	// unconditionally — so an empty path would not error, it would silently
	// fetch into whatever repo the daemon's process cwd happens to be. Guard it
	// here rather than at any one caller: this covers every call site at once
	// and cannot drift out of sync with them (BOS-591).
	if localPath == "" {
		return fmt.Errorf("local path is required")
	}
	refspec := "+refs/heads/" + base + ":refs/remotes/origin/" + base
	if _, err := fetchWithRefLockRetry(ctx, localPath, runGitRemote, "fetch", "origin", refspec); err != nil {
		return fmt.Errorf("fetch origin/%s: %w", base, err)
	}
	return nil
}

// CountMergeCommits reports how many merge commits exist on head that are
// not already on origin/<base>. GitHub structurally refuses to rebase-merge
// any branch containing a merge commit ("GraphQL: This branch can't be
// rebased"), so callers use this to detect that case before requesting a
// rebase merge.
//
// The remote head branch (refs/remotes/origin/<head>, freshly fetched) is
// authoritative since that is what GitHub will actually rebase. If head
// hasn't been pushed (or the fetch fails for another reason) this falls back
// to the local branch when it exists.
func (m *Manager) CountMergeCommits(ctx context.Context, localPath, base, head string) (int, error) {
	if base == "" {
		return 0, fmt.Errorf("base branch is required")
	}
	if head == "" {
		return 0, fmt.Errorf("head branch is required")
	}

	if err := m.FetchBase(ctx, localPath, base); err != nil {
		return 0, err
	}

	headRef := "refs/remotes/origin/" + head
	headRefspec := "+refs/heads/" + head + ":" + headRef
	if _, err := fetchWithRefLockRetry(ctx, localPath, runGitRemote, "fetch", "origin", headRefspec); err != nil {
		if !branchExists(ctx, localPath, head) {
			return 0, fmt.Errorf("head branch %q not found locally or on origin: %w", head, err)
		}
		headRef = "refs/heads/" + head
	}

	baseRef := "refs/remotes/origin/" + base
	out, err := runGit(ctx, localPath, "rev-list", "--merges", "--count", baseRef+".."+headRef)
	if err != nil {
		return 0, fmt.Errorf("rev-list --merges --count %s..%s: %w", baseRef, headRef, err)
	}
	count, err := strconv.Atoi(out)
	if err != nil {
		return 0, fmt.Errorf("parse merge commit count %q: %w", out, err)
	}
	return count, nil
}

// IsAncestor reports whether ref is an ancestor of target. A non-ancestor is
// a normal outcome (returns false, nil); only true invocation failures
// propagate as errors. Use e.g. ref="<sha>" and target="refs/remotes/origin/main"
// to verify a post-merge commit actually landed on the remote base.
func (m *Manager) IsAncestor(ctx context.Context, localPath, ref, target string) (bool, error) {
	// Bounded like every other git invocation (BOS-717). This one cannot go
	// through runGitWithTimeout — it reads the exit code to distinguish "not an
	// ancestor" (1) from a broken ref (>=128) — so it applies the same budget
	// itself. Callers include the stranded-bootstrap reaper, which runs on the
	// daemon's poller context and carries no deadline of its own.
	ctx, cancel := context.WithTimeout(ctx, GitCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "merge-base", "--is-ancestor", ref, target)
	cmd.Dir = localPath
	// The same shared environment runGitWithTimeout uses (BOS-878). This call
	// site builds its own exec.Cmd only to read the exit code, so it has to opt
	// in explicitly — and it is the one place a future git command could miss.
	cmd.Env = gitCommandEnv(m.logger)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	// And bounded the same way runGitWithTimeout is, for the same reason: stderr
	// is a *bytes.Buffer, so os/exec hands git a PIPE and cmd.Run blocks until
	// every inheritor of it closes. exec.CommandContext kills only the git PID,
	// so a grandchild left holding that pipe keeps this call blocked long past
	// the deadline above — which would make that deadline decorative. See
	// gitWaitDelay for why git (unlike a setup script) never legitimately leaves
	// such a process behind.
	cmd.WaitDelay = gitWaitDelay
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	// The wait-delay expiry is checked BEFORE the exit code, and reported as an
	// error rather than as an answer. Wait substitutes ErrWaitDelay only for a
	// nil error, so this is the "git said ancestor, but its output was abandoned
	// mid-drain" case — and a `true` here is what the stranded-bootstrap reaper
	// force-deletes a branch on, so a guess is not available.
	if errors.Is(err, exec.ErrWaitDelay) {
		return false, fmt.Errorf("merge-base --is-ancestor %s %s: %w: a child process held the output pipe open for more than %s after git exited: %s",
			ref, target, err, gitWaitDelay, strings.TrimSpace(stderr.String()))
	}
	// merge-base --is-ancestor exits 1 when not an ancestor (no error),
	// and exits ≥128 when refs are bad or the repo is broken.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("merge-base --is-ancestor %s %s: %w: %s",
		ref, target, err, strings.TrimSpace(stderr.String()))
}

// RebaseResult reports the branch tips either side of a successful
// RebaseOntoBase. PriorHead is the pre-rebase tip — the value a follow-up
// `push --force-with-lease` must use as its anchor, since the rebase leaves
// PriorHead unreachable from the new tip.
type RebaseResult struct {
	PriorHead string
	NewHead   string
}

// CountBehindBase reports how many commits are on origin/<base> but not on
// branch. It is the `git rev-list --count <branch>..origin/<base>` reading,
// taken after freshening origin/<base>, so the answer reflects the remote's
// current tip rather than a stale remote-tracking ref.
func (m *Manager) CountBehindBase(ctx context.Context, worktreePath, branch, base string) (int, error) {
	if branch == "" {
		return 0, errors.New("branch is required")
	}
	if base == "" {
		return 0, errors.New("base branch is required")
	}
	if err := m.FetchBase(ctx, worktreePath, base); err != nil {
		return 0, err
	}
	baseRef := "refs/remotes/origin/" + base
	out, err := runGit(ctx, worktreePath, "rev-list", "--count", branch+".."+baseRef)
	if err != nil {
		return 0, fmt.Errorf("rev-list --count %s..%s: %w", branch, baseRef, err)
	}
	count, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("parse behind count %q: %w", out, err)
	}
	return count, nil
}

// RebaseOntoBase replays the checked-out branch on top of a freshly fetched
// origin/<base>. Preconditions are verified before any write: branch must be
// the worktree's current branch and the working tree must be clean, because a
// rebase over uncommitted work would either refuse midway or silently move the
// operator's changes.
//
// On conflict the rebase is aborted and — belt and braces, in case the abort
// itself fails or leaves state behind — the branch is force-restored to its
// exact pre-rebase tip. A half-rebased worktree is the one outcome this
// function must never produce; a skipped session is always the safer failure.
func (m *Manager) RebaseOntoBase(ctx context.Context, worktreePath, branch, base string) (*RebaseResult, error) {
	if branch == "" {
		return nil, errors.New("branch is required")
	}
	if base == "" {
		return nil, errors.New("base branch is required")
	}
	if err := m.verifyCurrentBranch(ctx, worktreePath, branch); err != nil {
		return nil, fmt.Errorf("verify branch before rebase: %w", err)
	}
	dirty, err := runGit(ctx, worktreePath, "status", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("git status before rebase: %w", err)
	}
	if strings.TrimSpace(dirty) != "" {
		return nil, fmt.Errorf("worktree %s has uncommitted changes; refusing to rebase", worktreePath)
	}
	if err := m.FetchBase(ctx, worktreePath, base); err != nil {
		return nil, err
	}

	priorHead, err := runGit(ctx, worktreePath, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("resolve HEAD before rebase: %w", err)
	}
	priorHead = strings.TrimSpace(priorHead)

	baseRef := "refs/remotes/origin/" + base
	if _, rebaseErr := runGit(ctx, worktreePath, "rebase", baseRef); rebaseErr != nil {
		// Classify BEFORE unwinding: the unwind erases the on-disk rebase state
		// the detector reads.
		conflicted := rebaseStoppedOnConflict(ctx, worktreePath)
		m.restoreAfterFailedRebase(ctx, worktreePath, branch, priorHead)
		sentinel := ErrRebaseFailed
		if conflicted {
			sentinel = ErrRebaseConflict
		}
		return nil, fmt.Errorf("%w: rebase %s onto %s: %v", sentinel, branch, baseRef, rebaseErr)
	}

	newHead, err := runGit(ctx, worktreePath, "rev-parse", "HEAD")
	if err != nil {
		// The rebase landed but we cannot name the new tip, so we cannot hand a
		// lease anchor to the push half. Leaving the branch here would leave it
		// rebased-but-unpushed — local ahead of origin on a rewritten history,
		// the permanent wedge this whole path exists to avoid. Unwind.
		m.forceRestoreTip(ctx, worktreePath, branch, priorHead, "resolving HEAD after rebase failed")
		return nil, fmt.Errorf("resolve HEAD after rebase: %w", err)
	}
	return &RebaseResult{PriorHead: priorHead, NewHead: strings.TrimSpace(newHead)}, nil
}

// rebaseStoppedOnConflict conservatively decides whether a failed `git rebase`
// stopped because it needs conflict resolution, as opposed to failing for some
// other reason (cancelled context, bad ref, failing hook, held index.lock).
//
// The positive signal is git's own on-disk rebase state: a rebase that stops
// for conflict resolution leaves rebase-merge/rebase-apply behind so the
// operator can continue or abort. Anything we cannot positively identify as a
// conflict is reported as NOT a conflict, so ambiguity is logged loudly rather
// than filed away as benign.
func rebaseStoppedOnConflict(ctx context.Context, worktreePath string) bool {
	if ctx.Err() != nil {
		// The rebase was killed by cancellation/timeout, not by a conflict.
		return false
	}
	// Probe on an uncancellable context so the answer reflects the worktree
	// rather than a ctx that expired between the rebase and this check.
	probeCtx, cancel := unwindContext(ctx)
	defer cancel()
	return rebaseInProgress(probeCtx, worktreePath)
}

// RebaseOntoBaseAndPush is RebaseOntoBase followed by a force-with-lease push
// of the replayed branch, as one all-or-nothing step.
//
// The two halves are coupled deliberately. A rebase that lands locally but
// fails to push leaves the worktree ahead of origin/<branch> on a history the
// remote can no longer fast-forward, and the *next* sweep would anchor its
// lease on the new local tip — a lease the remote can never satisfy, wedging
// the branch permanently. So a failed push is unwound the same way a conflict
// is: the branch goes back to its exact pre-rebase tip, which still holds all
// of the session's work (a rebase only replays commits, it never invents or
// drops them).
func (m *Manager) RebaseOntoBaseAndPush(ctx context.Context, worktreePath, branch, base string) (*RebaseResult, error) {
	// Precondition, checked before any write: the local tip must already equal
	// origin/<branch>. The push below anchors its lease on the PRE-rebase local
	// tip, but git evaluates --force-with-lease against the real remote ref, so
	// an unpushed local commit makes the lease stale by construction and the
	// push is certain to be rejected ("stale info"). Without this check the
	// sweep would rebase and then throw the rebase away on every single merge,
	// forever, for exactly those sessions.
	if err := m.verifyBranchMatchesOrigin(ctx, worktreePath, branch); err != nil {
		return nil, err
	}

	res, err := m.RebaseOntoBase(ctx, worktreePath, branch, base)
	if err != nil {
		return nil, err
	}
	// The pre-rebase tip is the lease anchor: it is what origin/<branch> still
	// points at, and PushWithLease accepts it because the reflog carries the
	// `rebase (finish)` evidence that the old tip was integrated, not lost.
	if _, err := m.PushWithLease(ctx, worktreePath, branch, res.PriorHead); err != nil {
		m.forceRestoreTip(ctx, worktreePath, branch, res.PriorHead, "push of rebased branch failed")
		return nil, fmt.Errorf("push rebased branch %s: %w", branch, err)
	}
	return res, nil
}

// unwindTimeout bounds the recovery git commands run by the unwind paths. Short
// and independent of the caller's budget: an unwind is a handful of local git
// invocations with no network in them.
const unwindTimeout = 2 * time.Minute

// unwindContext derives the context every recovery git command runs on.
//
// It deliberately DROPS the caller's cancellation (context.WithoutCancel). The
// unwind is what makes a failed rebase safe, and the most likely reason a
// rebase failed in the first place is that the caller's context expired — on
// the caller's ctx every recovery command would then fail instantly with
// exec.CommandContext's kill, leaving exactly the half-rebased worktree the
// unwind exists to prevent. Its own short timeout keeps it bounded.
func unwindContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), unwindTimeout)
}

// restoreAfterFailedRebase returns the worktree to priorHead, checked out on
// branch, with no rebase state on disk. `git rebase --abort` normally does the
// whole job; forceRestoreTip is the fallback for the cases where it cannot
// (abort failed, or git left a rebase-apply/rebase-merge directory behind).
func (m *Manager) restoreAfterFailedRebase(ctx context.Context, worktreePath, branch, priorHead string) {
	ctx, cancel := unwindContext(ctx)
	defer cancel()

	_, abortErr := runGit(ctx, worktreePath, "rebase", "--abort")

	head, headErr := runGit(ctx, worktreePath, "rev-parse", "HEAD")
	restored := headErr == nil && strings.TrimSpace(head) == priorHead
	// Only take the early return when the probe positively says no rebase
	// state remains; an unreadable probe is treated as residue so the forced
	// path (which restores unconditionally) still runs.
	if restored && headAttachedTo(ctx, worktreePath, branch) && !rebaseInProgressOrUnknown(ctx, worktreePath) {
		return
	}

	m.logger.Warn().AnErr("abort_err", abortErr).Msg("rebase abort left residual state")
	m.forceRestoreTip(ctx, worktreePath, branch, priorHead, "rebase abort left residual state")
}

// forceRestoreTip drops any in-progress rebase state and puts branch back on
// priorHead with HEAD attached to it. It is the last-resort unwind used
// whenever a keep-current step fails after the branch tip may already have
// moved.
//
// The restore is skipped in exactly one case: no rebase is in progress AND the
// worktree holds changes a `reset --hard priorHead` would destroy. The
// clean-worktree precondition was checked before the rebase began, so such
// changes were written since — most plausibly the session's own agent — and
// destroying them is irrecoverable, whereas refusing leaves a branch an
// operator can fix by hand. When a rebase IS in progress (or the rebase-state
// probe cannot say) the dirt is git's own conflict debris, says nothing about
// user work, and must not stop the unwind: refusing there would leave git
// mid-operation on a detached HEAD, the exact wedge this function prevents.
func (m *Manager) forceRestoreTip(ctx context.Context, worktreePath, branch, priorHead, reason string) {
	ctx, cancel := unwindContext(ctx)
	defer cancel()

	m.logger.Warn().
		Str("path", worktreePath).
		Str("branch", branch).
		Str("prior_head", priorHead).
		Str("reason", reason).
		Msg("force-restoring pre-rebase tip")

	if rebaseInProgressOrUnknown(ctx, worktreePath) {
		// Prefer `--abort`, which restores the branch, HEAD and the tree in
		// one step; fall back to quit + a forced re-checkout when it cannot.
		if _, err := runGit(ctx, worktreePath, "rebase", "--abort"); err == nil {
			head, headErr := runGit(ctx, worktreePath, "rev-parse", "HEAD")
			// Every clause matters. A successful abort that left HEAD
			// detached, off priorHead, or with rebase state still on disk is
			// not a restore, so anything short of all three falls through to
			// the forced path rather than returning a wedged worktree.
			if headErr == nil && strings.TrimSpace(head) == priorHead &&
				headAttachedTo(ctx, worktreePath, branch) &&
				!rebaseInProgressOrUnknown(ctx, worktreePath) {
				return
			}
		}
		_, _ = runGit(ctx, worktreePath, "rebase", "--quit")
		m.hardResetTo(ctx, worktreePath, branch, priorHead)
		return
	}

	// No rebase in progress: the rebase either finished or never started, and
	// git leaves a clean tree in both cases. Anything modified here was written
	// since the clean-worktree precondition was checked — most plausibly the
	// session's own agent — and the restore would destroy it irrecoverably.
	// Refusing leaves a branch an operator can fix by hand.
	changes, err := destroyableChanges(ctx, worktreePath, priorHead)
	if err != nil {
		m.logger.Error().Err(err).
			Str("path", worktreePath).
			Str("branch", branch).
			Str("prior_head", priorHead).
			Msg("cannot verify worktree cleanliness; refusing to hard-reset, manual recovery required")
		return
	}
	if changes != "" {
		m.logger.Error().
			Str("path", worktreePath).
			Str("branch", branch).
			Str("prior_head", priorHead).
			Str("changes", excerpt(changes, 10, 1024)).
			Msg("worktree has uncommitted changes; refusing to hard-reset, manual recovery required")
		return
	}
	m.hardResetTo(ctx, worktreePath, branch, priorHead)
}

// hardResetTo moves branch and the working tree to priorHead, logging (rather
// than returning) a failure: every caller is already on an unwind path where
// there is nothing left to fall back to.
//
// It re-checks out the branch rather than resetting in place because its
// callers reach it from `git rebase --quit`, which deliberately does NOT
// re-attach HEAD — HEAD stays DETACHED at the partially replayed commit. A
// plain `reset --hard` there would move the detached HEAD and never touch
// refs/heads/<branch>, so the branch would keep pointing at the pre-unwind tip
// and the session agent's next commit would never reach it. `checkout --force
// -B <branch> <priorHead>` re-attaches HEAD to the branch AND points the
// branch at priorHead in one step, discarding index and worktree debris.
func (m *Manager) hardResetTo(ctx context.Context, worktreePath, branch, priorHead string) {
	args := []string{"reset", "--hard", priorHead}
	if branch != "" {
		args = []string{"checkout", "--force", "-B", branch, priorHead}
	}
	if _, err := runGit(ctx, worktreePath, args...); err != nil {
		m.logger.Error().Err(err).
			Str("path", worktreePath).
			Str("branch", branch).
			Str("prior_head", priorHead).
			Msg("failed to restore worktree tip; manual recovery required")
	}
}

// headAttachedTo reports whether HEAD is a symbolic ref pointing at
// refs/heads/<branch>. A detached HEAD — what `git rebase --quit` and a
// stopped rebase both leave behind — yields false, as does an unreadable
// HEAD. An empty branch means the caller has no attachment expectation.
func headAttachedTo(ctx context.Context, worktreePath, branch string) bool {
	if branch == "" {
		return true
	}
	out, err := runGit(ctx, worktreePath, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == "refs/heads/"+branch
}

// destroyableChanges returns the `git status --porcelain` lines describing
// changes a `reset --hard priorHead` (or the equivalent forced checkout) would
// destroy.
//
// That is every staged or unstaged modification to a tracked file, PLUS any
// untracked file whose path also exists in priorHead's tree: unlike
// `git checkout`, `git reset --hard` does not refuse over such a collision —
// it silently overwrites the untracked file with the target tree's content and
// exits 0. That case is reachable whenever the base branch deleted a file the
// session's agent then recreated untracked.
//
// Untracked files that do NOT collide, and ignored (`!!`) entries, stay
// excluded: nothing in the restore removes them (that would take `git clean`),
// so counting them as "dirty" would make the unwind refuse over files it was
// never going to touch, leaving the branch wedged for no gain.
func destroyableChanges(ctx context.Context, worktreePath, priorHead string) (string, error) {
	out, err := runGit(ctx, worktreePath, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	var kept []string
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(line, "??") || strings.HasPrefix(line, "!!") {
			continue
		}
		kept = append(kept, line)
	}
	colliding, err := untrackedPathsInTree(ctx, worktreePath, priorHead)
	if err != nil {
		return "", err
	}
	for _, p := range colliding {
		kept = append(kept, "?? "+p)
	}
	return strings.Join(kept, "\n"), nil
}

// untrackedPathsInTree lists the worktree's untracked, non-ignored files whose
// path also exists in ref's tree — the untracked files a `reset --hard ref`
// would silently overwrite. Both sides are read NUL-separated so paths with
// spaces or quotes compare literally, and `ls-files --others` is used rather
// than the porcelain's `??` lines because the porcelain collapses a wholly
// untracked directory into a single entry.
func untrackedPathsInTree(ctx context.Context, worktreePath, ref string) ([]string, error) {
	if ref == "" {
		return nil, nil
	}
	others, err := runGit(ctx, worktreePath, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, fmt.Errorf("list untracked files: %w", err)
	}
	untracked := splitNul(others)
	if len(untracked) == 0 {
		return nil, nil
	}
	tree, err := runGit(ctx, worktreePath, "ls-tree", "-r", "--name-only", "-z", ref)
	if err != nil {
		return nil, fmt.Errorf("list tree of %s: %w", ref, err)
	}
	inTree := make(map[string]struct{}, len(tree)/16+1)
	for _, p := range splitNul(tree) {
		inTree[p] = struct{}{}
	}
	var colliding []string
	for _, p := range untracked {
		if _, ok := inTree[p]; ok {
			colliding = append(colliding, p)
		}
	}
	return colliding, nil
}

// splitNul splits git's -z output into entries, dropping the empty tail left by
// the trailing separator.
func splitNul(out string) []string {
	var entries []string
	for _, e := range strings.Split(out, "\x00") {
		if e != "" {
			entries = append(entries, e)
		}
	}
	return entries
}

// excerpt bounds a multi-line blob for a log field: at most maxLines lines and
// maxBytes bytes, with an elision marker when anything was dropped.
func excerpt(s string, maxLines, maxBytes int) string {
	truncated := false
	lines := strings.Split(s, "\n")
	if len(lines) > maxLines {
		lines = lines[:maxLines]
		truncated = true
	}
	out := strings.Join(lines, "\n")
	if len(out) > maxBytes {
		out = out[:maxBytes]
		truncated = true
	}
	if truncated {
		out += "\n…"
	}
	return out
}

// verifyBranchMatchesOrigin reports whether the local branch tip is exactly
// refs/remotes/origin/<branch>, freshening the remote-tracking ref first so the
// comparison is against the remote's current tip rather than a stale one. Any
// divergence — unpushed local commits, a remote that moved, or a branch that
// was never pushed at all — yields ErrBranchNotPushed.
//
// A failed fetch is classified rather than assumed. FetchBase asks for one
// branch by name (+refs/heads/<b>:refs/remotes/origin/<b>), so its commonest
// failure is `couldn't find remote ref` for a branch that was deleted on the
// remote or never pushed — the benign skip, not breakage. `git ls-remote
// --heads origin <branch>` settles it: an empty listing from a working
// ls-remote means the branch genuinely is not on the remote
// (ErrBranchNotPushed); anything else — a listing, or an ls-remote that itself
// fails — means auth failure, a dead remote or a network outage, which is real
// breakage and is returned unwrapped so it is logged loudly.
func (m *Manager) verifyBranchMatchesOrigin(ctx context.Context, worktreePath, branch string) error {
	if branch == "" {
		return errors.New("branch is required")
	}
	// FetchBase's refspec is generic (+refs/heads/X:refs/remotes/origin/X), so
	// it freshens any branch, not just a base branch.
	if err := m.FetchBase(ctx, worktreePath, branch); err != nil {
		out, lsErr := runGit(ctx, worktreePath, "ls-remote", "--heads", "origin", "refs/heads/"+branch)
		if lsErr == nil && strings.TrimSpace(out) == "" {
			return fmt.Errorf("%w: origin/%s does not exist: %v", ErrBranchNotPushed, branch, err)
		}
		return err
	}
	local, err := runGit(ctx, worktreePath, "rev-parse", "refs/heads/"+branch)
	if err != nil {
		return fmt.Errorf("resolve local %s: %w", branch, err)
	}
	remote, err := runGit(ctx, worktreePath, "rev-parse", "refs/remotes/origin/"+branch)
	if err != nil {
		return fmt.Errorf("%w: resolve origin/%s: %w", ErrBranchNotPushed, branch, err)
	}
	if local != remote {
		return fmt.Errorf("%w: %s is at %s but origin/%s is at %s",
			ErrBranchNotPushed, branch, local, branch, remote)
	}
	return nil
}

// rebaseState reports whether git still has rebase state on disk for the given
// worktree. The paths are resolved through `git rev-parse --git-path` so this
// works for linked worktrees, where .git is a file rather than a dir.
//
// A probe that cannot answer — a cancelled or expired context, a git that
// refuses to run, an unreadable git dir — returns an error rather than a
// silent false, because "no rebase in progress" and "cannot tell" lead the
// unwind paths to opposite decisions.
func rebaseState(ctx context.Context, worktreePath string) (bool, error) {
	for _, name := range []string{"rebase-merge", "rebase-apply"} {
		out, err := runGit(ctx, worktreePath, "rev-parse", "--git-path", name)
		if err != nil {
			return false, fmt.Errorf("resolve --git-path %s: %w", name, err)
		}
		p := strings.TrimSpace(out)
		if p == "" {
			return false, fmt.Errorf("empty --git-path %s", name)
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(worktreePath, p)
		}
		switch _, err := os.Stat(p); {
		case err == nil:
			return true, nil
		case errors.Is(err, fs.ErrNotExist):
			// Definitively absent; keep looking at the other state dir.
		default:
			return false, fmt.Errorf("stat %s: %w", p, err)
		}
	}
	return false, nil
}

// rebaseInProgress reports whether git POSITIVELY has rebase state on disk. An
// unanswerable probe reads as false, which is what rebaseStoppedOnConflict
// wants: anything it cannot identify as a conflict must be reported as not a
// conflict, so ambiguity is logged loudly rather than filed away as benign.
func rebaseInProgress(ctx context.Context, worktreePath string) bool {
	inProgress, err := rebaseState(ctx, worktreePath)
	return err == nil && inProgress
}

// rebaseInProgressOrUnknown reports whether rebase state exists OR the probe
// could not tell. It is the reading the unwind paths use: the rebase-in-progress
// branch restores unconditionally, so treating an unknown state as "in progress"
// costs nothing beyond an extra `rebase --abort`, whereas the alternative routes
// a genuinely mid-rebase worktree into the refuse path and leaves it mid-rebase
// on a detached HEAD. The probe is unanswerable exactly when things are already
// going wrong — most plausibly because the unwind's own budget has expired —
// which is when being conservative matters most.
func rebaseInProgressOrUnknown(ctx context.Context, worktreePath string) bool {
	inProgress, err := rebaseState(ctx, worktreePath)
	return inProgress || err != nil
}

// MergeLocalBranch merges head into base inside the repo at localPath. It
// does NOT push. Invariants enforced before any write:
//   - base must exist locally
//   - working tree must be clean on base
//   - head must exist locally (refs/heads/<head>)
//   - if origin/<base> exists, local base must be an ancestor (no divergence)
//
// On conflict the merge is aborted and ErrMergeConflict is returned so the
// repo is left in the pre-merge state.
func (m *Manager) MergeLocalBranch(ctx context.Context, localPath, base, head, strategy string) error {
	if base == "" {
		return fmt.Errorf("base branch is required")
	}
	if head == "" {
		return fmt.Errorf("head branch is required")
	}
	if strategy == "" {
		strategy = "merge"
	}
	switch strategy {
	case "merge", "squash", "rebase":
	default:
		return fmt.Errorf("unknown merge strategy %q", strategy)
	}

	if !branchExists(ctx, localPath, base) {
		return fmt.Errorf("base branch %q does not exist in %s", base, localPath)
	}
	if !branchExists(ctx, localPath, head) {
		return fmt.Errorf("head branch %q does not exist in %s", head, localPath)
	}

	// If origin exists, refresh remote state and reject if local base has
	// diverged. Unlike the GitHub-backed merge path (where the local
	// fast-forward is deferrable), a local-only merge genuinely mutates this
	// checkout, so a clean, non-diverged base is a hard precondition. Missing
	// origin is fine (local-only repo).
	originURL, _ := m.DetectOriginURL(ctx, localPath)
	if originURL != "" {
		if _, err := fetchWithRefLockRetry(ctx, localPath, runGit, "fetch", "origin", base); err != nil {
			return fmt.Errorf("fetch origin/%s: %w", base, err)
		}
		if hasRef(ctx, localPath, "refs/remotes/origin/"+base) {
			ok, err := m.IsAncestor(ctx, localPath, "refs/heads/"+base, "refs/remotes/origin/"+base)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf(
					"%w: local %s has diverged from origin/%s; rebase or reset before merging",
					ErrBaseBranchNotReady, base, base,
				)
			}
		}
	}

	// Checkout base to run the merge there. Reject if the working tree is
	// dirty — stashing silently would hide user changes.
	dirty, err := runGit(ctx, localPath, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("git status: %w", err)
	}
	if dirty != "" {
		return fmt.Errorf(
			"%w: repo at %s has uncommitted changes; commit or stash before merging",
			ErrBaseBranchNotReady, localPath,
		)
	}
	if _, err := runGit(ctx, localPath, "checkout", base); err != nil {
		return fmt.Errorf("checkout %s: %w", base, err)
	}

	// Fast-forward base from origin if possible, so the merge starts from the
	// latest base tip.
	if originURL != "" && hasRef(ctx, localPath, "refs/remotes/origin/"+base) {
		if _, err := runGit(ctx, localPath, "merge", "--ff-only", "refs/remotes/origin/"+base); err != nil {
			return fmt.Errorf("ff-only pull of origin/%s: %w", base, err)
		}
	}

	switch strategy {
	case "merge":
		if _, err := runGit(ctx, localPath,
			"merge", "--no-ff", "--no-edit",
			"-m", fmt.Sprintf("Merge branch '%s' into %s", head, base),
			head,
		); err != nil {
			_, _ = runGit(ctx, localPath, "merge", "--abort")
			return fmt.Errorf("%w: merge --no-ff %s into %s: %v", ErrMergeConflict, head, base, err)
		}
	case "squash":
		if _, err := runGit(ctx, localPath, "merge", "--squash", head); err != nil {
			_, _ = runGit(ctx, localPath, "reset", "--merge")
			return fmt.Errorf("%w: merge --squash %s into %s: %v", ErrMergeConflict, head, base, err)
		}
		if _, err := runGit(ctx, localPath,
			"commit", "-m", fmt.Sprintf("Squash-merge branch '%s' into %s", head, base),
		); err != nil {
			_, _ = runGit(ctx, localPath, "reset", "--merge")
			return fmt.Errorf("squash commit: %w", err)
		}
	case "rebase":
		// Rebase the head branch on top of base in a detached worktree-safe
		// way: rebase head onto base, then ff-merge base to head.
		if _, err := runGit(ctx, localPath, "rebase", base, head); err != nil {
			_, _ = runGit(ctx, localPath, "rebase", "--abort")
			// Get back to base so the repo isn't left on a half-rebased head.
			_, _ = runGit(ctx, localPath, "checkout", base)
			return fmt.Errorf("%w: rebase %s onto %s: %v", ErrMergeConflict, head, base, err)
		}
		if _, err := runGit(ctx, localPath, "checkout", base); err != nil {
			return fmt.Errorf("checkout %s after rebase: %w", base, err)
		}
		if _, err := runGit(ctx, localPath, "merge", "--ff-only", head); err != nil {
			return fmt.Errorf("ff-only merge of rebased %s into %s: %w", head, base, err)
		}
	}

	// Delete the head branch. For merge and rebase, `-d` safely refuses if
	// not merged — the invariant holds because both strategies record the
	// merge relationship in the DAG. Squash records no such relationship,
	// so `-d` would always refuse; use `-D` since the squash commit above
	// confirmed the content landed on base.
	deleteFlag := "-d"
	if strategy == "squash" {
		deleteFlag = "-D"
	}
	if _, err := runGit(ctx, localPath, "branch", deleteFlag, head); err != nil {
		m.logger.Warn().Err(err).
			Str("head", head).
			Msg("failed to delete merged head branch; continuing")
	}

	return nil
}

// hasRef reports whether the given ref resolves in the repo.
func hasRef(ctx context.Context, repoPath, ref string) bool {
	_, err := runGit(ctx, repoPath, "rev-parse", "--verify", "--quiet", ref)
	return err == nil
}

// currentBranch returns the name of the checked-out branch, or ("", true, nil)
// when HEAD is detached. Errors from `git symbolic-ref` other than the
// detached-HEAD case are propagated.
func currentBranch(ctx context.Context, repoPath string) (name string, detached bool, err error) {
	out, gitErr := runGit(ctx, repoPath, "symbolic-ref", "--quiet", "--short", "HEAD")
	if gitErr == nil {
		return out, false, nil
	}
	// symbolic-ref --quiet exits non-zero (exit 1) on detached HEAD without
	// writing stderr; distinguish that from a genuine failure.
	if _, statErr := runGit(ctx, repoPath, "rev-parse", "--verify", "HEAD"); statErr == nil {
		return "", true, nil
	}
	return "", false, gitErr
}

// DetectDefaultBranch returns the default branch name for a repo by
// inspecting refs/remotes/origin/HEAD. Falls back to "main".
func (m *Manager) DetectDefaultBranch(ctx context.Context, repoPath string) (string, error) {
	ref, err := runGit(ctx, repoPath, "symbolic-ref", "refs/remotes/origin/HEAD")
	if err != nil {
		// Ref doesn't exist — fall back to "main".
		return "main", nil
	}
	// ref is e.g. "refs/remotes/origin/main" → extract "main".
	parts := strings.SplitN(ref, "refs/remotes/origin/", 2)
	if len(parts) == 2 && parts[1] != "" {
		return parts[1], nil
	}
	return "main", nil
}

// CreateFromExistingBranch creates a worktree from an existing remote branch.
// It fetches the branch from origin and creates a worktree tracking it.
func (m *Manager) CreateFromExistingBranch(ctx context.Context, opts CreateFromExistingBranchOpts) (*CreateResult, error) {
	wtPath := filepath.Join(opts.WorktreeBaseDir, sanitizeDirName(opts.RepoName), opts.BranchName)

	// Ensure the worktree base directory exists.
	if err := os.MkdirAll(opts.WorktreeBaseDir, 0o750); err != nil {
		return nil, fmt.Errorf("create worktree base dir: %w", err)
	}

	m.logger.Info().
		Str("repo", opts.RepoPath).
		Str("branch", opts.BranchName).
		Str("path", wtPath).
		Msg("creating worktree from existing branch")

	// Same clone gate as Create, for the same reason: the fetch and the two
	// `git branch` writes below mutate refs in the shared clone, so a concurrent
	// create in this repo must not run them at the same time (see
	// repoCloneGates). Released once `worktree add` lands, before the setup
	// script.
	releaseClone, _, err := repoCloneGates.Acquire(ctx, opts.RepoPath, RepoCloneGateTimeout)
	if err != nil {
		return nil, fmt.Errorf("serialize git for %s: %w", opts.RepoPath, err)
	}
	defer releaseClone()

	// Fetch the branch from origin into its remote-tracking ref first. If the
	// remote branch is missing, callers may fall back to creating from a local
	// branch, so do not clear any existing path until this succeeds.
	fetchStarted := time.Now()
	if _, err := fetchWithRefLockRetry(ctx, opts.RepoPath, runGit,
		"fetch", "origin",
		"+refs/heads/"+opts.BranchName+":refs/remotes/origin/"+opts.BranchName,
	); err != nil {
		return nil, fmt.Errorf("fetch existing branch: %w", err)
	}
	fetchDuration := time.Since(fetchStarted)

	// Clear any stale worktree left at this path by an orphaned prior session
	// before updating the local branch ref. Git refuses to move a branch that is
	// still checked out in a registered stale worktree.
	m.clearStaleWorktree(ctx, opts.RepoPath, wtPath)

	if _, err := runGit(ctx, opts.RepoPath,
		"branch", "-f", opts.BranchName, "refs/remotes/origin/"+opts.BranchName,
	); err != nil {
		return nil, fmt.Errorf("update existing branch: %w", err)
	}

	if _, err := runGit(ctx, opts.RepoPath,
		"branch", "--set-upstream-to=origin/"+opts.BranchName, opts.BranchName,
	); err != nil {
		return nil, fmt.Errorf("set existing branch upstream: %w", err)
	}

	// Create worktree from the fetched branch.
	// git worktree add <path> <branch> — checks out existing branch.
	worktreeAddStarted := time.Now()
	if _, err := runGit(ctx, opts.RepoPath,
		"worktree", "add", wtPath, opts.BranchName,
	); err != nil {
		return nil, fmt.Errorf("create worktree from existing branch: %w", err)
	}
	worktreeAddDuration := time.Since(worktreeAddStarted)

	// Ensure bossd-managed paths (e.g. .boss/) are git-ignored before any
	// downstream step writes into them.
	if err := ensureGitInfoExclude(ctx, wtPath, bossdManagedExcludePatterns); err != nil {
		return nil, fmt.Errorf("ensure git info exclude: %w", err)
	}

	// Shared-clone work is done (ensureGitInfoExclude included — see Create);
	// let a queued create in this repo proceed while the setup script runs.
	releaseClone()

	// Run setup script if provided. Non-fatal — see Create for rationale.
	var setupErr error
	setupScriptDuration, err := m.runAndLogSetup(ctx, setupRunOpts{
		Op:       "create_from_existing_branch",
		RepoPath: opts.RepoPath,
		Worktree: wtPath,
		Branch:   opts.BranchName,
		Script:   opts.SetupScript,
		Output:   opts.SetupScriptOutput,
	})
	if err != nil {
		setupErr = fmt.Errorf("setup script: %w", err)
	}

	if err := m.verifyCurrentBranch(ctx, wtPath, opts.BranchName); err != nil {
		return nil, fmt.Errorf("verify existing-branch worktree branch: %w", err)
	}

	// BranchProbeDuration is left zero here — there is no branch-name collision
	// probe on this path — so the log shape stays identical across both
	// creation paths. See CreateResult for the per-field semantics.
	return &CreateResult{
		WorktreePath:        wtPath,
		BranchName:          opts.BranchName,
		SetupErr:            setupErr,
		FetchDuration:       fetchDuration,
		WorktreeAddDuration: worktreeAddDuration,
		SetupScriptDuration: setupScriptDuration,
	}, nil
}

// runSetupScript parses the stored setup_script value into a structured
// setupscript.Spec and executes it in the worktree with a 5-minute timeout.
//
// The following environment variables are set for the process:
//   - REPO_DIR:     path to the main git repository (the original clone)
//   - WORKTREE_DIR: path to the worktree being set up
//
// If output is non-nil, stdout and stderr are written there; otherwise they
// go to os.Stderr (daemon logs). logOutput is an additional, diagnostics-only
// sink that receives the bounded output tail on completion — it exists because
// the create-session RPC stream claims output, which left that (interactive)
// path with no record in the rotated daemon log at all. Both writers may be
// nil.
//
// bossd passes logOutput unconditionally, which is deliberate: on the other two
// shapes it is a bounded (≤4 KiB) duplicate, not a wasted write.
//   - nil output: the raw stream goes to os.Stderr, which under launchd is the
//     unrotated bossd.stderr.log — never the rotated bossd.log that tooling and
//     bug reports read. Only the structured event reaches that.
//   - task-orchestrator output: that path routes its own per-line writer to the
//     session logger, so the tail repeats the last few KB it already logged.
//
// Do not "fix" either duplicate by dropping the sink; the rotated-log record
// is the point of the ticket.
//
// The returned duration is the script's own measured run time; it is reported
// on the error path too. Legacy bare-string values are rewritten to
// <worktree>/.boss/setup.sh on first use — the user is nudged via `warn` to
// migrate to a structured {"type":...} value.
func runSetupScript(ctx context.Context, repoPath, dir, script, loginShell string, output, logOutput io.Writer) (time.Duration, error) {
	spec, err := setupscript.Parse(script)
	if err != nil {
		return 0, err
	}
	return spec.Execute(ctx, setupscript.ExecuteOpts{
		RepoPath:     repoPath,
		WorktreePath: dir,
		LoginShell:   loginShell,
		Output:       output,
		LogOutput:    logOutput,
		Timeout:      SetupScriptTimeout,
		Warn: func(msg string) {
			fmt.Fprintln(os.Stderr, "bossd: "+msg)
		},
	})
}

// setupTailLogWriter adapts the manager's logger to the io.Writer that
// setupscript.Execute uses for its bounded output tail, so the tail is emitted
// as a structured event instead of a raw stderr blob. Named for the tail to
// keep it distinct from taskorchestrator's same-purpose but per-line
// setupLogWriter, which is wired to Output rather than LogOutput.
type setupTailLogWriter struct {
	logger   zerolog.Logger
	worktree string
}

func (w setupTailLogWriter) Write(p []byte) (int, error) {
	w.logger.Info().
		Str("worktree", w.worktree).
		Str("output", strings.TrimRight(string(p), "\n")).
		Msg("setup script output")
	return len(p), nil
}

// setupRunOpts names the per-call-site inputs of runAndLogSetup.
type setupRunOpts struct {
	Op       string    // "create" | "resurrect" | "create_from_existing_branch"; tags the log event
	RepoPath string    // main repo path
	Worktree string    // worktree the script runs in
	Branch   string    // branch checked out there; log context only
	Script   *string   // the stored setup_script value; nil or blank means "no setup step"
	Output   io.Writer // caller's live stream; nil falls back to os.Stderr inside setupscript
}

// runAndLogSetup runs the repo's configured setup script (when there is one)
// and records the run in the daemon log. All three setup call sites go through
// it, so the "is there a script" guard, the log sink, the timing and the log
// event cannot drift apart between create, resurrect, and
// create-from-existing-branch.
//
// The returned duration is the caller's wall-clock (parse + exec) — what
// CreateResult.SetupScriptDuration reports — and is zero when no script ran.
// The error is the setup failure, which every call site treats as non-fatal;
// it is already logged here.
func (m *Manager) runAndLogSetup(ctx context.Context, opts setupRunOpts) (time.Duration, error) {
	if opts.Script == nil || strings.TrimSpace(*opts.Script) == "" {
		return 0, nil
	}
	started := time.Now()
	runDuration, err := runSetupScript(ctx, opts.RepoPath, opts.Worktree, *opts.Script, m.LoginShell,
		opts.Output, setupTailLogWriter{logger: m.logger, worktree: opts.Worktree})
	m.logSetupScriptRun(opts.Op, opts.Worktree, opts.Branch, runDuration, err)
	return time.Since(started), err
}

// logSetupScriptRun records the setup script's own measured run time in the
// daemon log, tagged with the op that ran it so a create and a resurrect of
// the same worktree stay distinguishable.
//
// This is the finer-grained cmd.Run measurement, and it is complementary to —
// not a replacement for — CreateResult.SetupScriptDuration, the caller's
// wall-clock (parse + exec) that travels to the client and is already logged
// as setup_script_ms by session.Lifecycle on the create paths. On the
// resurrect path, which returns no duration to any caller, this event is the
// only duration record there is.
//
// A setup failure is non-fatal at every call site, so it is logged as a
// warning here rather than returned; on the create paths the session lifecycle
// logs the same failure again when it records SetupErr on the session.
func (m *Manager) logSetupScriptRun(op, worktreePath, branch string, d time.Duration, err error) {
	if err != nil {
		m.logger.Warn().Err(err).
			Str("op", op).
			Str("worktree", worktreePath).
			Str("branch", branch).
			Dur("setup_script_run_ms", d).
			Msg("setup script failed; continuing")
		return
	}
	m.logger.Info().
		Str("op", op).
		Str("worktree", worktreePath).
		Str("branch", branch).
		Dur("setup_script_run_ms", d).
		Msg("setup script finished")
}
