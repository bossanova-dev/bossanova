package git

import (
	"context"
	"errors"
	"os/exec"
)

// RemoteBranchExists reports whether origin has the exact branch. A missing ref
// is false, nil; an unreachable origin remains an error.
func RemoteBranchExists(ctx context.Context, repoPath, branch string) (bool, error) {
	_, err := runGitRemote(ctx, repoPath, "ls-remote", "--exit-code", "--heads", "origin", "refs/heads/"+branch)
	if remoteRefAbsent(err) {
		return false, nil
	}
	return err == nil, err
}

func remoteRefAbsent(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == 2
}

func remoteBranchExists(ctx context.Context, repoPath, branch string) bool {
	_, err := runGitRemote(ctx, repoPath, "ls-remote", "--exit-code", "--heads", "origin", "refs/heads/"+branch)
	return err == nil
}
