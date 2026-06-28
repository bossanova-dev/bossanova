// Package gatecmd executes a "gate command" before a cron job fires.
// Exit 0 means the job may proceed; any other outcome (non-zero exit,
// launch failure, timeout) blocks the job.
package gatecmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// DefaultTimeout is the gate timeout when Options.Timeout <= 0.
const DefaultTimeout = 60 * time.Second

// Options configures a gate-command run.
type Options struct {
	Command      string // the gate command (trimmed defensively)
	RepoPath     string // cwd for the command; also REPO_DIR / WORKTREE_DIR / BOSS_WORKTREE_DIR
	LinearAPIKey string
	SentryAPIKey string
	SentryOrg    string
	Timeout      time.Duration // <=0 → use DefaultTimeout
	Output       io.Writer     // combined stdout+stderr sink; nil → io.Discard
}

// Run executes the gate command and reports whether the job may proceed.
// passed == true iff the process exited 0.
// passed == false for: empty command, non-zero exit, launch failure, timeout.
// err is non-nil and descriptive on launch failure, timeout, or non-zero exit,
// and nil on a clean exit-0.
func Run(ctx context.Context, o Options) (passed bool, err error) {
	cmd := strings.TrimSpace(o.Command)
	if cmd == "" {
		return false, errors.New("empty gate command")
	}

	timeout := o.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	output := o.Output
	if output == nil {
		output = io.Discard
	}

	c := buildCommand(ctx, cmd)
	c.Dir = o.RepoPath
	c.Env = append(os.Environ(),
		"REPO_DIR="+o.RepoPath,
		"WORKTREE_DIR="+o.RepoPath,
		"BOSS_WORKTREE_DIR="+o.RepoPath,
		"LINEAR_API_KEY="+o.LinearAPIKey,
		"SENTRY_API_KEY="+o.SentryAPIKey,
		"SENTRY_ORG="+o.SentryOrg,
	)
	c.Stdout = output
	c.Stderr = output

	if runErr := c.Run(); runErr != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return false, fmt.Errorf("gate command timed out after %v", timeout)
		}
		return false, fmt.Errorf("gate command failed: %w", runErr)
	}
	return true, nil
}

// buildCommand constructs the *exec.Cmd for the given (pre-trimmed) command.
// Commands starting with '/', './', or '../' are split on whitespace and run
// directly without a shell. Everything else is run via `sh -c`.
func buildCommand(ctx context.Context, cmd string) *exec.Cmd {
	if strings.HasPrefix(cmd, "/") || strings.HasPrefix(cmd, "./") || strings.HasPrefix(cmd, "../") {
		argv := strings.Fields(cmd)
		return exec.CommandContext(ctx, argv[0], argv[1:]...)
	}
	return exec.CommandContext(ctx, "sh", "-c", cmd)
}
