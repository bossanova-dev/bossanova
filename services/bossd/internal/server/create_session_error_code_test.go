package server

import (
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect"

	gitpkg "github.com/recurser/bossd/internal/git"
)

// A create that died on a lost race for refs/remotes/origin/<base> must reach
// the caller as Unavailable, not Internal: an unattended caller reads
// Unavailable as retry-the-same-call, which is the whole point of classifying
// the failure. Everything else stays in the Internal bucket.
func TestCreateSessionConnectError_RefLockContentionIsUnavailable(t *testing.T) {
	// Wrapped the way the git manager actually returns it — several layers down,
	// with the raw git text still attached.
	err := fmt.Errorf("create worktree: %w", fmt.Errorf(
		"fetch --prune origin: %w: git fetch --prune origin: exit status 1: error: cannot lock ref 'refs/remotes/origin/main'",
		gitpkg.ErrRefLockContended,
	))

	got := createSessionConnectError(err)
	if connect.CodeOf(got) != connect.CodeUnavailable {
		t.Errorf("code = %v, want %v", connect.CodeOf(got), connect.CodeUnavailable)
	}
	if !errors.Is(got, gitpkg.ErrRefLockContended) {
		t.Error("the sentinel did not survive the connect wrapping")
	}
}

func TestCreateSessionConnectError_UnrelatedErrorStaysInternal(t *testing.T) {
	got := createSessionConnectError(errors.New("fatal: not a git repository"))
	if connect.CodeOf(got) != connect.CodeInternal {
		t.Errorf("code = %v, want %v", connect.CodeOf(got), connect.CodeInternal)
	}
}

// A branch git rejects that surfaces from deep in the create (a PR head, or a
// title that sanitizes to nothing) is the caller's bad input, not a daemon fault.
func TestCreateSessionConnectError_InvalidBranchNameIsInvalidArgument(t *testing.T) {
	err := fmt.Errorf("create worktree: %w", fmt.Errorf("%w: %q", gitpkg.ErrInvalidBranchName, "a..b"))
	if got := connect.CodeOf(createSessionConnectError(err)); got != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want %v", got, connect.CodeInvalidArgument)
	}
}
