package server

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"

	"github.com/recurser/bossalib/config"
)

// validateRepoWorktreeBaseDir enforces the per-repo worktree base invariant:
// non-empty and absolute. It returns the cleaned path. It has no filesystem
// side effect — worktree creation makes the directory on demand — so repo
// registration and update stay pure metadata writes. A `~/...` value is
// rejected as relative because the daemon cannot expand it.
func validateRepoWorktreeBaseDir(dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", connect.NewError(connect.CodeInvalidArgument,
			errors.New("worktree_base_dir must not be empty"))
	}
	if !filepath.IsAbs(dir) {
		return "", connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("worktree_base_dir must be an absolute path, got %q", dir))
	}
	return filepath.Clean(dir), nil
}

// resolveRepoWorktreeBaseDir returns the worktree base to store for a new repo
// row. A non-blank requested value is validated and kept; a blank one falls
// back to the global settings.worktree_base_dir obtained from load. It never
// returns an empty base: a load failure is Internal and a blank global value is
// FailedPrecondition naming both remedies. A global value that is not absolute
// is also FailedPrecondition: the caller sent no argument to blame.
func resolveRepoWorktreeBaseDir(requested string, load func() (config.Settings, error)) (string, error) {
	if strings.TrimSpace(requested) != "" {
		return validateRepoWorktreeBaseDir(requested)
	}
	settings, err := load()
	if err != nil {
		return "", connect.NewError(connect.CodeInternal,
			fmt.Errorf("load settings for default worktree_base_dir: %w", err))
	}
	global := strings.TrimSpace(settings.WorktreeBaseDir)
	if global == "" {
		return "", connect.NewError(connect.CodeFailedPrecondition,
			errors.New("no worktree_base_dir given and the global settings.worktree_base_dir is empty; pass worktree_base_dir or set the global default"))
	}
	if !filepath.IsAbs(global) {
		return "", connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("no worktree_base_dir given and the global settings.worktree_base_dir %q is not absolute; pass worktree_base_dir or set an absolute global default", global))
	}
	return filepath.Clean(global), nil
}
