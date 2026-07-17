// Package main manages Claude CLI subprocess lifecycle for coding sessions.
package main

import (
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/agentruntime"
	"github.com/recurser/bossalib/loginshell"
)

// Runner wraps agentruntime.Runner with claude-specific argv building. The
// embedded *agentruntime.Runner exposes the generic process lifecycle
// methods (Start, Stop, IsRunning, ExitError, Subscribe, History) used by
// server.go; this wrapper only adds the claude-specific argv construction
// and the dangerouslySkipPermissions toggle.
type Runner struct {
	*agentruntime.Runner
	dangerouslySkipPermissions bool
	loginShell                 string
	cmdFactory                 agentruntime.CommandFactory
}

// RunnerOption configures a Runner. Kept as RunnerOption (rather than the
// agentruntime-style Option) because main.go's runnerOptsFromEnv returns
// []RunnerOption and the public surface is callable from outside the package.
type RunnerOption func(*Runner)

// WithDangerouslySkipPermissions toggles passing --dangerously-skip-permissions
// to the Claude CLI. Wired in production from the BOSS_PLUGIN_dangerously_skip_permissions
// env var (set by bossd's plugin host from Settings.Plugins[claude].Config) —
// see runnerOptsFromEnv in main.go. The daemon-side tmux paths
// (server.EnsureChat, session.startCronTmuxChat) build the claude argv
// directly, so they read the config entry there.
func WithDangerouslySkipPermissions(v bool) RunnerOption {
	return func(r *Runner) { r.dangerouslySkipPermissions = v }
}

// WithLoginShell wraps claude argv through the configured login shell.
func WithLoginShell(shell string) RunnerOption {
	return func(r *Runner) { r.loginShell = shell }
}

// WithCommandFactory overrides the agent command factory (for testing).
// Delegates to agentruntime.WithCommandFactory; preserved as a claude-side
// RunnerOption so existing tests don't have to import agentruntime.
func WithCommandFactory(f agentruntime.CommandFactory) RunnerOption {
	return func(r *Runner) { r.cmdFactory = f }
}

// NewRunner creates a Claude process runner backed by agentruntime.
func NewRunner(logger zerolog.Logger, opts ...RunnerOption) *Runner {
	r := &Runner{}
	for _, opt := range opts {
		opt(r)
	}
	var extra []agentruntime.Option
	if r.cmdFactory != nil {
		extra = append(extra, agentruntime.WithCommandFactory(r.cmdFactory))
	}
	r.Runner = agentruntime.NewRunner(logger, agentruntime.Options{
		BinaryName: "claude",
		BuildArgv:  r.buildArgv,
		// On a non-nil exit, classify the log tail for a provider usage cap
		// or rate limit and upgrade to ErrUsageLimited / ErrRateLimited so the
		// daemon can detect the capped state via ExitStatus. Auth is out of
		// scope for claude (no sentinel); classifyUsageCap returns nil for any
		// non-usage/non-rate classification, so a benign or auth tail leaves
		// the exit error unchanged.
		PostExit: func(orig error, tail []byte) error {
			if orig == nil {
				return nil
			}
			return classifyUsageCap(logger, tail, time.Now())
		},
	}, extra...)
	return r
}

// buildArgv constructs the claude CLI argv for headless runs. Mirrors the
// historical wiring: --print --verbose --output-format stream-json, optional
// --resume <id>, optional --session-id <id> when explicitly provided, and
// optional --dangerously-skip-permissions when the runner was configured for
// it.
func (r *Runner) buildArgv(in agentruntime.BuildArgvInput) []string {
	args := []string{"claude", "--print", "--verbose", "--output-format", "stream-json"}
	if in.Resume != nil {
		args = append(args, "--resume", *in.Resume)
	}
	if in.ProvidedSessionID {
		args = append(args, "--session-id", in.SessionID)
	}
	if r.dangerouslySkipPermissions {
		args = append(args, "--dangerously-skip-permissions")
	}
	if model := in.Options["model"]; model != "" {
		args = append(args, "--model", model)
	}
	return loginshell.Wrap(r.loginShell, loginshell.Flags(r.loginShell), args)
}
