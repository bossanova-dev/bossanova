package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// reusePreparedWorktree runs under the clone gate, including the durable claim.
// The marker survives adoption; only a successful agent start consumes it.
func (m *Manager) reusePreparedWorktree(ctx context.Context, opts CreateOpts, branch string) (*CreateResult, error) {
	repoBase := worktreeRepoBase(opts.WorktreeBaseDir, opts.RepoName)
	path := filepath.Join(repoBase, branch)
	if err := ensureWithin(repoBase, path); err != nil {
		return nil, err
	}
	marker, err := ReadPreparedWorktreeMarker(ctx, path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	reason := ""
	switch {
	case err != nil:
		reason = "marker unreadable or invalid"
	case opts.Force:
		reason = "force requested"
	case opts.ExistingBranch:
		reason = "existing branch checkout"
	case opts.RepoID == "" || marker.RepoID != opts.RepoID:
		reason = "repository mismatch"
	case marker.Branch != branch:
		reason = "branch mismatch"
	case marker.Claimed:
		reason = "already adopted"
	case !time.Now().Before(marker.KeptUntil):
		reason = "marker expired"
	case !m.worktreeReadyAt(ctx, opts.RepoPath, path, branch):
		reason = "worktree unhealthy"
	default:
		head, headErr := PreparedWorktreeHead(ctx, path)
		status, statusErr := preparedGitOutput(ctx, path, "status", "--porcelain")
		if headErr != nil || head != marker.HeadSHA {
			reason = "HEAD moved or unreadable"
		} else if statusErr != nil || len(status) != 0 {
			reason = "worktree dirty or unreadable"
		}
	}
	if reason != "" {
		// A missing path is an ordinary fresh create, not a rejected kept worktree.
		if _, statErr := os.Stat(path); statErr == nil {
			m.logger.Info().Str("path", path).Str("reason", reason).Msg("prepared worktree not adopted")
		}
		return nil, nil
	}
	marker.Claimed = true
	if err := WritePreparedWorktreeMarker(ctx, path, *marker); err != nil {
		return nil, fmt.Errorf("claim prepared worktree: %w", err)
	}
	if opts.OnWorktreeAdded != nil {
		if err := opts.OnWorktreeAdded(path); err != nil {
			marker.Claimed = false
			return nil, errors.Join(fmt.Errorf("record adopted worktree: %w", err), WritePreparedWorktreeMarker(ctx, path, *marker))
		}
	}
	if opts.OnWorktreeReady != nil {
		opts.OnWorktreeReady(ctx, branch, path, marker.IsBranchCreated)
	}
	moved, countErr := preparedGitOutput(ctx, path, "rev-list", "--count", marker.HeadSHA+"..origin/"+opts.BaseBranch)
	if countErr != nil {
		m.logger.Info().Err(countErr).Str("path", path).Msg("prepared worktree base movement unavailable")
	} else {
		count, _ := strconv.Atoi(strings.TrimSpace(string(moved)))
		m.logger.Info().Str("path", path).Int("base_commits_moved", count).Msg("reusing prepared worktree")
	}
	return &CreateResult{WorktreePath: path, BranchName: branch, Reused: true, IsBranchCreated: marker.IsBranchCreated}, nil
}
