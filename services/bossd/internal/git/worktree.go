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
	"strings"
	"time"

	"github.com/recurser/bossalib/setupscript"
	"github.com/rs/zerolog"
)

// ErrBranchExists is returned when a branch with the derived name already
// exists and the caller did not set Force in CreateOpts.
var ErrBranchExists = errors.New("branch already exists")

// ErrBaseBranchNotReady is returned by EnsureBaseBranchReadyForSync when the
// main repo cannot be safely fast-forwarded to match origin/<base>. The error
// message always explains the condition (dirty tree, divergence, etc.) so
// callers can surface it to the user verbatim.
var ErrBaseBranchNotReady = errors.New("base branch not ready for sync")

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

	// Push pushes the given branch to the "origin" remote.
	Push(ctx context.Context, worktreePath, branch string) error

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

// Create creates a new git worktree with a fresh branch based on baseBranch.
func (m *Manager) Create(ctx context.Context, opts CreateOpts) (*CreateResult, error) {
	branch := opts.BranchName
	if branch == "" {
		branch = sanitizeBranchName(opts.Title)
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

	// Fetch the latest base branch from origin so the worktree starts from
	// the most recent remote state, not a potentially stale local ref.
	if _, err := runGit(ctx, opts.RepoPath,
		"fetch", "origin", opts.BaseBranch,
	); err != nil {
		return nil, fmt.Errorf("fetch base branch: %w", err)
	}

	// git worktree add -b <branch> <path> origin/<baseBranch>
	if _, err := runGit(ctx, opts.RepoPath,
		"worktree", "add", "-b", branch, wtPath, "origin/"+opts.BaseBranch,
	); err != nil {
		return nil, fmt.Errorf("worktree add: %w", err)
	}

	// Run setup script if provided.
	if opts.SetupScript != nil && *opts.SetupScript != "" {
		if err := runSetupScript(ctx, opts.RepoPath, wtPath, *opts.SetupScript, opts.SetupScriptOutput); err != nil {
			return nil, fmt.Errorf("setup script: %w", err)
		}
	}

	return &CreateResult{
		WorktreePath: wtPath,
		BranchName:   branch,
	}, nil
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

	// Run setup script if provided.
	if opts.SetupScript != nil && *opts.SetupScript != "" {
		if err := runSetupScript(ctx, opts.RepoPath, opts.WorktreePath, *opts.SetupScript, opts.SetupScriptOutput); err != nil {
			return fmt.Errorf("setup script: %w", err)
		}
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
	if _, err := runGit(ctx, worktreePath, "commit", "--allow-empty", "-m", message); err != nil {
		return fmt.Errorf("empty commit: %w", err)
	}
	return nil
}

func (m *Manager) Push(ctx context.Context, worktreePath, branch string) error {
	m.logger.Info().
		Str("path", worktreePath).
		Str("branch", branch).
		Msg("pushing branch")

	if _, err := runGit(ctx, worktreePath, "push", "-u", "origin", branch); err != nil {
		return fmt.Errorf("push: %w", err)
	}
	return nil
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

	// Fetch the branch from origin.
	if _, err := runGit(ctx, opts.RepoPath,
		"fetch", "origin", opts.BranchName,
	); err != nil {
		return nil, fmt.Errorf("fetch branch: %w", err)
	}

	// Create worktree from the fetched branch.
	// git worktree add <path> <branch> — checks out existing branch.
	if _, err := runGit(ctx, opts.RepoPath,
		"worktree", "add", wtPath, opts.BranchName,
	); err != nil {
		return nil, fmt.Errorf("worktree add: %w", err)
	}

	// Run setup script if provided.
	if opts.SetupScript != nil && *opts.SetupScript != "" {
		if err := runSetupScript(ctx, opts.RepoPath, wtPath, *opts.SetupScript, opts.SetupScriptOutput); err != nil {
			return nil, fmt.Errorf("setup script: %w", err)
		}
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
//   - BOSS_REPO_DIR:     path to the main git repository (the original clone)
//   - BOSS_WORKTREE_DIR: path to the worktree being set up
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
