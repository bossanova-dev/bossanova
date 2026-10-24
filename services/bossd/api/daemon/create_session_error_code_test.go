package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	gitpkg "github.com/recurser/bossd/internal/adapter/git"
	"github.com/recurser/bossd/internal/session"
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

func TestCreateSessionConnectError_AgentStartFailures(t *testing.T) {
	cause := context.DeadlineExceeded
	until := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		err     error
		code    connect.Code
		message string
	}{
		{"kept", &session.PreparedWorktreeKeptError{Cause: cause, WorktreePath: "/work/prepared", Branch: "feature/keep", Attempts: 3, KeptUntil: until}, connect.CodeUnavailable, "agent start failed under host load after 3 attempts; prepared worktree kept at /work/prepared until 2026-10-24T12:00:00Z; re-run `boss new` for branch feature/keep to reuse it"},
		{"transient without keep", &session.AgentStartFailedError{Cause: cause, Attempts: 3}, connect.CodeUnavailable, "agent start failed under host load after 3 attempts; nothing was kept"},
		{"permanent", &session.AgentStartFailedError{Cause: errors.New("invalid agent configuration"), Attempts: 1}, connect.CodeInternal, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := createSessionConnectError(fmt.Errorf("start session: %w", tt.err))
			if connect.CodeOf(got) != tt.code {
				t.Fatalf("code = %v, want %v: %v", connect.CodeOf(got), tt.code, got)
			}
			if tt.message != "" && !strings.Contains(got.Error(), tt.message) {
				t.Errorf("error = %q, want message %q", got, tt.message)
			}
			if !errors.Is(got, tt.err) {
				t.Error("typed start error missing from unwrap chain")
			}
		})
	}
}
