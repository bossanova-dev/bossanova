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

	"github.com/recurser/bossalib/setupscript"
	"github.com/rs/zerolog"
)

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
	PurgeWorktree(ctx context.Context, repoPath, repoName, worktreeBaseDir, branch string)

	// Resurrect re-creates a worktree from an existing branch and runs the
	// setup script if present.
	Resurrect(ctx context.Context, opts ResurrectOpts) error

	// EmptyTrash deletes remote branches for archived sessions and prunes
	// stale worktree refs.
	EmptyTrash(ctx context.Context, repoPath string, branches []string) error

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
// .superpowers/ is the superpowers-framework scratch directory. Skills create
// it while they run — including during the finalize/stop skills themselves — so
// the agent cannot reliably clean it before bossd finalizes. Left untracked it
// trips the same pr_failed → Blocked misclassification as the Stop-hook config,
// so it is excluded here for every managed repo.
var bossdManagedExcludePatterns = []string{
	".boss/",
	".claude/settings.local.json",
	".superpowers/",
}

// bossdExcludeMarker identifies the block of patterns bossd has added
// to info/exclude, so the additions are easy to spot and remove by hand.
const bossdExcludeMarker = "# bossd-managed: ignore worktree-local artifacts"

// ensureGitInfoExclude appends the given patterns to the worktree's
// $GIT_COMMON_DIR/info/exclude, idempotently. Patterns already present
// (anywhere in the file) are skipped. Pre-existing content is preserved.
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

	have := make(map[string]bool)
	for line := range strings.SplitSeq(string(existing), "\n") {
		have[strings.TrimSpace(line)] = true
	}
	var missing []string
	for _, p := range patterns {
		if !have[p] {
			missing = append(missing, p)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	var buf bytes.Buffer
	buf.Write(existing)
	if len(existing) > 0 && !bytes.HasSuffix(existing, []byte("\n")) {
		buf.WriteByte('\n')
	}
	buf.WriteString(bossdExcludeMarker)
	buf.WriteByte('\n')
	for _, p := range missing {
		buf.WriteString(p)
		buf.WriteByte('\n')
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

// runGit runs a git command in the given directory and returns stdout.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// branchExists checks whether a local branch ref exists. It is a thin alias
// over refExists on the refs/heads/ namespace, so both local-ref probes share
// one implementation.
func branchExists(ctx context.Context, repoPath, branch string) bool {
	return refExists(ctx, repoPath, "refs/heads/"+branch)
}

func remoteBranchExists(ctx context.Context, repoPath, branch string) bool {
	_, err := runGit(ctx, repoPath, "ls-remote", "--exit-code", "--heads", "origin", "refs/heads/"+branch)
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
	out, err := runGit(ctx, repoPath, "ls-remote", "--heads", "origin", "refs/heads/"+prefix+"*")
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
// Safe to call unconditionally: the server short-circuits to the existing
// session for a branch before reaching worktree creation, so reaching here
// means no live session owns wtPath. All steps are best-effort.
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
func (m *Manager) PurgeWorktree(ctx context.Context, repoPath, repoName, worktreeBaseDir, branch string) {
	wtPath := filepath.Join(worktreeBaseDir, sanitizeDirName(repoName), branch)
	m.clearStaleWorktree(ctx, repoPath, wtPath)
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

	// Ensure bossd-managed paths (e.g. .boss/) are git-ignored before any
	// downstream step writes into them.
	if err := ensureGitInfoExclude(ctx, wtPath, bossdManagedExcludePatterns); err != nil {
		return nil, fmt.Errorf("ensure info/exclude: %w", err)
	}

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
		// cleaned up by `git worktree prune` during EmptyTrash.
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

// EmptyTrash deletes the remote tracking branches and prunes worktree refs.
func (m *Manager) EmptyTrash(ctx context.Context, repoPath string, branches []string) error {
	m.logger.Info().
		Int("count", len(branches)).
		Msg("emptying trash")

	for _, branch := range branches {
		// Delete remote branch. Ignore errors (branch may not exist on remote).
		if _, err := runGit(ctx, repoPath, "push", "origin", "--delete", branch); err != nil {
			m.logger.Warn().Err(err).Str("branch", branch).Msg("failed to delete remote branch")
		}

		// Delete local branch.
		if _, err := runGit(ctx, repoPath, "branch", "-D", branch); err != nil {
			m.logger.Warn().Err(err).Str("branch", branch).Msg("failed to delete local branch")
		}
	}

	// Prune stale worktree references.
	if _, err := runGit(ctx, repoPath, "worktree", "prune"); err != nil {
		m.logger.Warn().Err(err).Msg("failed to prune worktrees")
	}

	return nil
}

// DeleteLocalBranch force-deletes a LOCAL branch and prunes stale worktree
// refs. Unlike EmptyTrash it never touches the remote — there is no
// `git push origin --delete`. The `-D` (force) form is deliberate: the caller
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

	if _, err := runGit(ctx, worktreePath, "push", "-u", "origin", "HEAD:refs/heads/"+branch); err != nil {
		return fmt.Errorf("push: %w", err)
	}
	return nil
}

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
	if _, err := runGit(ctx, worktreePath, "push", "--force-with-lease="+lease, "origin", headSHA+":refs/heads/"+branch); err != nil {
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

	if _, err := runGit(ctx, ".", "clone", cloneURL, localPath); err != nil {
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
	if _, err := runGit(ctx, localPath, "fetch", "--prune", "origin"); err != nil {
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
	if _, err := runGit(ctx, localPath, "fetch", "origin", refspec); err != nil {
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
	if _, err := runGit(ctx, localPath, "fetch", "origin", headRefspec); err != nil {
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
	cmd := exec.CommandContext(ctx, "git", "merge-base", "--is-ancestor", ref, target)
	cmd.Dir = localPath
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return true, nil
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
		if _, err := runGit(ctx, localPath, "fetch", "origin", base); err != nil {
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

	// Fetch the branch from origin into its remote-tracking ref first. If the
	// remote branch is missing, callers may fall back to creating from a local
	// branch, so do not clear any existing path until this succeeds.
	fetchStarted := time.Now()
	if _, err := runGit(ctx, opts.RepoPath,
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
