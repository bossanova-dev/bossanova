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

	// BranchDebugSnapshot captures branch state used to diagnose draft PR
	// creation failures.
	BranchDebugSnapshot(ctx context.Context, worktreePath, branch, baseBranch string) (*BranchDebugSnapshot, error)

	// VerifyPushedBranchAheadOfBase verifies the worktree is on branch, HEAD is
	// ahead of origin/<baseBranch>, and origin/<branch> points at local HEAD.
	VerifyPushedBranchAheadOfBase(ctx context.Context, worktreePath, branch, baseBranch string) (*BranchVerification, error)

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
type CreateResult struct {
	WorktreePath string
	BranchName   string
	// SetupErr is non-nil when the worktree was created successfully but its
	// configured setup script failed. The worktree is still usable, so callers
	// may proceed (in a degraded/flagged state) rather than abort. nil means
	// the setup script ran cleanly or none was configured.
	SetupErr error
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
	return &Manager{logger: logger, removeAll: os.RemoveAll}
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
	if err := m.FetchBase(ctx, opts.RepoPath, opts.BaseBranch); err != nil {
		return nil, err
	}
	if !hasRef(ctx, opts.RepoPath, "refs/remotes/origin/"+opts.BaseBranch) {
		return nil, fmt.Errorf("origin/%s does not exist", opts.BaseBranch)
	}
	if !opts.Force {
		// For tracker-linked creates the CreateSession dedup guard (BOS-236)
		// fires before reaching here, so availableNewBranchName's allowSuffix
		// rename never masks a tracker duplicate. Behavior for non-tracker /
		// explicit-branch creates is unchanged.
		uniqueBranch, err := m.availableNewBranchName(ctx, opts.RepoPath, branch, opts.BranchName == "")
		if err != nil {
			return nil, err
		}
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
	if _, err := runGit(ctx, opts.RepoPath,
		"worktree", "add", "-b", branch, wtPath, "origin/"+opts.BaseBranch,
	); err != nil {
		if isBranchAlreadyExistsGitOutput(err) {
			return nil, ErrBranchExists
		}
		return nil, fmt.Errorf("worktree add: %w", err)
	}

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
	if opts.SetupScript != nil && *opts.SetupScript != "" {
		if err := runSetupScript(ctx, opts.RepoPath, wtPath, *opts.SetupScript, m.LoginShell, opts.SetupScriptOutput); err != nil {
			setupErr = fmt.Errorf("setup script: %w", err)
		}
	}

	if err := m.verifyCurrentBranch(ctx, wtPath, branch); err != nil {
		return nil, fmt.Errorf("verify created worktree branch: %w", err)
	}

	return &CreateResult{
		WorktreePath: wtPath,
		BranchName:   branch,
		SetupErr:     setupErr,
	}, nil
}

func (m *Manager) availableNewBranchName(ctx context.Context, repoPath, branch string, allowSuffix bool) (string, error) {
	if branch == "" {
		return "", fmt.Errorf("branch name is required")
	}

	for i := 0; ; i++ {
		candidate := branch
		if i > 0 {
			if !allowSuffix {
				return "", ErrBranchExists
			}
			candidate = fmt.Sprintf("%s-%d", branch, i+1)
		}

		if branchExists(ctx, repoPath, candidate) || remoteBranchExists(ctx, repoPath, candidate) {
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
	if opts.SetupScript != nil && *opts.SetupScript != "" {
		if err := runSetupScript(ctx, opts.RepoPath, opts.WorktreePath, *opts.SetupScript, m.LoginShell, opts.SetupScriptOutput); err != nil {
			m.logger.Warn().Err(err).
				Str("worktree", opts.WorktreePath).
				Str("branch", opts.BranchName).
				Msg("setup script failed during resurrect; continuing")
		}
	}

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
		if strings.TrimSpace(s) != "" && !strings.Contains(s, tag) {
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
func injectPRTagExec(tag string) string {
	return "s=$(git log -1 --format=%s); " +
		"if printf '%s' \"$s\" | grep -qF '" + tag + "'; then exit 0; fi; " +
		"n=$(printf '%s' \"$s\" | sed 's/): /): " + tag + " /'); " +
		"if [ \"$n\" = \"$s\" ]; then n=$(printf '%s' \"$s\" | sed 's/^\\([a-z][a-z]*\\): /\\1: " + tag + " /'); fi; " +
		"if [ \"$n\" = \"$s\" ]; then n=\"$s " + tag + "\"; fi; " +
		"b=$(git log -1 --format=%b); " +
		// --allow-empty so an empty placeholder commit (chore: [skip ci] create
		// pull request) can still be reworded without aborting the whole rebase.
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

func (m *Manager) VerifyPushedBranchAheadOfBase(ctx context.Context, worktreePath, branch, baseBranch string) (*BranchVerification, error) {
	if err := m.verifyCurrentBranch(ctx, worktreePath, branch); err != nil {
		return nil, err
	}
	if err := m.FetchBase(ctx, worktreePath, baseBranch); err != nil {
		return nil, err
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
	refspec := "+refs/heads/" + base + ":refs/remotes/origin/" + base
	if _, err := runGit(ctx, localPath, "fetch", "origin", refspec); err != nil {
		return fmt.Errorf("fetch origin/%s: %w", base, err)
	}
	return nil
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
	if _, err := runGit(ctx, opts.RepoPath,
		"fetch", "origin",
		"+refs/heads/"+opts.BranchName+":refs/remotes/origin/"+opts.BranchName,
	); err != nil {
		return nil, fmt.Errorf("fetch existing branch: %w", err)
	}

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
	if _, err := runGit(ctx, opts.RepoPath,
		"worktree", "add", wtPath, opts.BranchName,
	); err != nil {
		return nil, fmt.Errorf("create worktree from existing branch: %w", err)
	}

	// Ensure bossd-managed paths (e.g. .boss/) are git-ignored before any
	// downstream step writes into them.
	if err := ensureGitInfoExclude(ctx, wtPath, bossdManagedExcludePatterns); err != nil {
		return nil, fmt.Errorf("ensure git info exclude: %w", err)
	}

	// Run setup script if provided. Non-fatal — see Create for rationale.
	var setupErr error
	if opts.SetupScript != nil && strings.TrimSpace(*opts.SetupScript) != "" {
		if err := runSetupScript(ctx, opts.RepoPath, wtPath, *opts.SetupScript, m.LoginShell, opts.SetupScriptOutput); err != nil {
			setupErr = fmt.Errorf("setup script: %w", err)
		}
	}

	if err := m.verifyCurrentBranch(ctx, wtPath, opts.BranchName); err != nil {
		return nil, fmt.Errorf("verify existing-branch worktree branch: %w", err)
	}

	return &CreateResult{
		WorktreePath: wtPath,
		BranchName:   opts.BranchName,
		SetupErr:     setupErr,
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
// go to os.Stderr (daemon logs). Legacy bare-string values are rewritten to
// <worktree>/.boss/setup.sh on first use — the user is nudged via `warn` to
// migrate to a structured {"type":...} value.
func runSetupScript(ctx context.Context, repoPath, dir, script, loginShell string, output io.Writer) error {
	spec, err := setupscript.Parse(script)
	if err != nil {
		return err
	}
	return spec.Execute(ctx, setupscript.ExecuteOpts{
		RepoPath:     repoPath,
		WorktreePath: dir,
		LoginShell:   loginShell,
		Output:       output,
		Timeout:      SetupScriptTimeout,
		Warn: func(msg string) {
			fmt.Fprintln(os.Stderr, "bossd: "+msg)
		},
	})
}
