// Package git provides Git worktree management for the Bossanova daemon.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
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

// ErrBaseBranchNotReady is returned by EnsureBaseBranchReadyForSync when the
// main repo cannot be safely fast-forwarded to match origin/<base>. The error
// message always explains the condition (dirty tree, divergence, etc.) so
// callers can surface it to the user verbatim.
var ErrBaseBranchNotReady = errors.New("base branch not ready for sync")

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

	// EnsureBaseBranchReadyForSync verifies that the main repo at localPath
	// is in a state where SyncBaseBranch can safely fast-forward the local
	// base branch to match origin/<base>. It fetches origin/<base> as a
	// side effect so divergence can be detected against the latest remote.
	// Returns an error wrapping ErrBaseBranchNotReady when the working tree
	// is dirty on the base branch or the local base has diverged from
	// origin; the error message is safe to show to the user.
	EnsureBaseBranchReadyForSync(ctx context.Context, localPath, base string) error

	// SyncBaseBranch fetches origin and fast-forwards the local base branch
	// to match origin/<base>. If <base> is the currently checked-out branch
	// it uses `git merge --ff-only`; otherwise it performs a direct ref
	// update via `git fetch origin <base>:<base>` which leaves the working
	// tree untouched. Also prunes stale remote-tracking refs.
	SyncBaseBranch(ctx context.Context, localPath, base string) error

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
	RepoPath          string    // Path to the main repository.
	WorktreePath      string    // Target path for the worktree directory.
	BranchName        string    // Existing branch to check out.
	SetupScript       *string   // Optional setup script to run after creation.
	SetupScriptOutput io.Writer // If non-nil, setup script output is written here.
}

var _ WorktreeManager = (*Manager)(nil)

// Manager is the default WorktreeManager implementation backed by real git commands.
type Manager struct {
	logger zerolog.Logger
}

// NewManager creates a new git WorktreeManager.
func NewManager(logger zerolog.Logger) *Manager {
	return &Manager{logger: logger}
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
var bossdManagedExcludePatterns = []string{
	".boss/",
	".claude/settings.local.json",
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
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		return fmt.Errorf("create info dir: %w", err)
	}

	existing, err := os.ReadFile(excludePath)
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

	return os.WriteFile(excludePath, buf.Bytes(), 0o644)
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

// branchExists checks whether a local branch ref exists.
func branchExists(ctx context.Context, repoPath, branch string) bool {
	_, err := runGit(ctx, repoPath, "rev-parse", "--verify", "refs/heads/"+branch)
	return err == nil
}

func remoteBranchExists(ctx context.Context, repoPath, branch string) bool {
	_, err := runGit(ctx, repoPath, "ls-remote", "--exit-code", "--heads", "origin", "refs/heads/"+branch)
	return err == nil
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
	if err := os.RemoveAll(wtPath); err != nil {
		m.logger.Warn().Err(err).Str("path", wtPath).Msg("remove stale worktree dir (best effort)")
	}
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
		uniqueBranch, err := m.availableNewBranchName(ctx, opts.RepoPath, branch, opts.BranchName == "")
		if err != nil {
			return nil, err
		}
		branch = uniqueBranch
	}

	wtPath := filepath.Join(opts.WorktreeBaseDir, sanitizeDirName(opts.RepoName), branch)

	// Ensure the worktree base directory exists.
	if err := os.MkdirAll(opts.WorktreeBaseDir, 0o755); err != nil {
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

	// Run setup script if provided.
	if opts.SetupScript != nil && *opts.SetupScript != "" {
		if err := runSetupScript(ctx, opts.RepoPath, wtPath, *opts.SetupScript, opts.SetupScriptOutput); err != nil {
			return nil, fmt.Errorf("setup script: %w", err)
		}
	}

	if err := m.verifyCurrentBranch(ctx, wtPath, branch); err != nil {
		return nil, fmt.Errorf("verify created worktree branch: %w", err)
	}

	return &CreateResult{
		WorktreePath: wtPath,
		BranchName:   branch,
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
		return removeWorktreeDir(worktreePath)
	}
	// --git-common-dir returns the .git dir; we want the repo root.
	repoPath = filepath.Dir(repoPath)

	if _, err := runGit(ctx, repoPath, "worktree", "remove", "--force", worktreePath); err != nil {
		// git worktree remove failed — fall back to direct removal.
		m.logger.Warn().Err(err).Str("path", worktreePath).
			Msg("git worktree remove failed, removing directory directly")
		return removeWorktreeDir(worktreePath)
	}
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
	if err := os.MkdirAll(filepath.Dir(opts.WorktreePath), 0o755); err != nil {
		return fmt.Errorf("create parent dir: %w", err)
	}

	// git worktree add <path> <existing-branch>
	if _, err := runGit(ctx, opts.RepoPath,
		"worktree", "add", opts.WorktreePath, opts.BranchName,
	); err != nil {
		return fmt.Errorf("worktree add: %w", err)
	}

	// Ensure bossd-managed paths (e.g. .boss/) are git-ignored — covers
	// worktrees that predate this feature or had info/exclude cleared.
	if err := ensureGitInfoExclude(ctx, opts.WorktreePath, bossdManagedExcludePatterns); err != nil {
		return fmt.Errorf("ensure info/exclude: %w", err)
	}

	// Run setup script if provided.
	if opts.SetupScript != nil && *opts.SetupScript != "" {
		if err := runSetupScript(ctx, opts.RepoPath, opts.WorktreePath, *opts.SetupScript, opts.SetupScriptOutput); err != nil {
			return fmt.Errorf("setup script: %w", err)
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

// EnsureBaseBranchReadyForSync checks that the main repo at localPath can
// be fast-forwarded to match origin/<base>. It does not modify any branch
// refs — it only fetches origin/<base> so the divergence check sees current
// remote state. See WorktreeManager.EnsureBaseBranchReadyForSync.
func (m *Manager) EnsureBaseBranchReadyForSync(ctx context.Context, localPath, base string) error {
	if base == "" {
		return fmt.Errorf("base branch is required")
	}

	// Refresh the remote-tracking ref so divergence is measured against
	// the latest remote state, not a stale cache.
	if _, err := runGit(ctx, localPath, "fetch", "origin", base); err != nil {
		return fmt.Errorf("fetch origin/%s: %w", base, err)
	}

	current, isDetached, err := currentBranch(ctx, localPath)
	if err != nil {
		return fmt.Errorf("resolve HEAD: %w", err)
	}

	// If the base branch has no local ref yet, there is nothing to
	// fast-forward — SyncBaseBranch will create it.
	if !branchExists(ctx, localPath, base) {
		return nil
	}

	if !isDetached && current == base {
		// Working tree must be clean so `merge --ff-only` won't trip on
		// uncommitted changes.
		dirty, err := runGit(ctx, localPath, "status", "--porcelain")
		if err != nil {
			return fmt.Errorf("git status: %w", err)
		}
		if dirty != "" {
			return fmt.Errorf(
				"%w: main repo at %s has uncommitted changes on %s; commit/stash or switch branches before merging",
				ErrBaseBranchNotReady, localPath, base,
			)
		}
	}

	// Require `refs/heads/<base>` to be an ancestor of `origin/<base>` so the
	// sync step can fast-forward. Divergence needs manual resolution.
	if _, err := runGit(ctx, localPath,
		"merge-base", "--is-ancestor", "refs/heads/"+base, "refs/remotes/origin/"+base,
	); err != nil {
		return fmt.Errorf(
			"%w: local %s has diverged from origin/%s in %s; rebase or reset before merging",
			ErrBaseBranchNotReady, base, base, localPath,
		)
	}

	return nil
}

// SyncBaseBranch fetches origin and fast-forwards the local base branch to
// match origin/<base>. See WorktreeManager.SyncBaseBranch.
func (m *Manager) SyncBaseBranch(ctx context.Context, localPath, base string) error {
	if base == "" {
		return fmt.Errorf("base branch is required")
	}

	// Fetch with --prune so merged-and-deleted remote branches (e.g. the
	// session branch that `gh pr merge --delete-branch` just removed) are
	// dropped from the local remote-tracking refs.
	if _, err := runGit(ctx, localPath, "fetch", "--prune", "origin"); err != nil {
		return fmt.Errorf("fetch --prune origin: %w", err)
	}

	current, isDetached, err := currentBranch(ctx, localPath)
	if err != nil {
		return fmt.Errorf("resolve HEAD: %w", err)
	}

	if !isDetached && current == base {
		// Base is checked out — fast-forward via merge so the working tree
		// stays in sync with HEAD.
		if _, err := runGit(ctx, localPath,
			"merge", "--ff-only", "refs/remotes/origin/"+base,
		); err != nil {
			return fmt.Errorf("ff-only merge origin/%s: %w", base, err)
		}
		return nil
	}

	// Base is not checked out — update the local ref directly without
	// touching the working tree. `fetch origin <base>:<base>` refuses any
	// non-fast-forward, so this remains safe.
	if _, err := runGit(ctx, localPath,
		"fetch", "origin", base+":"+base,
	); err != nil {
		return fmt.Errorf("fast-forward local %s: %w", base, err)
	}
	return nil
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
	// diverged — the same invariant EnsureBaseBranchReadyForSync enforces
	// for GitHub-backed merges. Missing origin is fine (local-only repo).
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
	if err := os.MkdirAll(opts.WorktreeBaseDir, 0o755); err != nil {
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

	// Run setup script if provided.
	if opts.SetupScript != nil && strings.TrimSpace(*opts.SetupScript) != "" {
		if err := runSetupScript(ctx, opts.RepoPath, wtPath, *opts.SetupScript, opts.SetupScriptOutput); err != nil {
			return nil, fmt.Errorf("setup script: %w", err)
		}
	}

	if err := m.verifyCurrentBranch(ctx, wtPath, opts.BranchName); err != nil {
		return nil, fmt.Errorf("verify existing-branch worktree branch: %w", err)
	}

	return &CreateResult{
		WorktreePath: wtPath,
		BranchName:   opts.BranchName,
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
func runSetupScript(ctx context.Context, repoPath, dir, script string, output io.Writer) error {
	spec, err := setupscript.Parse(script)
	if err != nil {
		return err
	}
	return spec.Execute(ctx, setupscript.ExecuteOpts{
		RepoPath:     repoPath,
		WorktreePath: dir,
		Output:       output,
		Timeout:      SetupScriptTimeout,
		Warn: func(msg string) {
			fmt.Fprintln(os.Stderr, "bossd: "+msg)
		},
	})
}
