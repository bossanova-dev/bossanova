// Package main manages Claude CLI subprocess lifecycle for coding sessions.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/contract/plugin/agentruntime"
	"github.com/recurser/bossalib/contract/plugin/loginshell"
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
	model                      string
	effort                     string
	cmdFactory                 agentruntime.CommandFactory
}

const defaultClaudeEffort = "high"

// StartWithProjectConfigTrust enforces the repository's configuration policy.
func (r *Runner) StartWithProjectConfigTrust(ctx context.Context, workDir, plan string, resume *string, sessionID, logPath, model, effort string, extraEnv map[string]string, trusted bool) (string, error) {
	if !trusted {
		if err := r.checkProjectIsolationSupport(ctx, extraEnv); err != nil {
			return "", err
		}
	}
	return r.StartWithOptions(ctx, workDir, plan, resume, sessionID, logPath, extraEnv, map[string]string{
		"model": model, "effort": effort,
	}, agentruntime.StartOptions{ShouldTrustProjectConfig: trusted})
}

// checkProjectIsolationSupport fails closed when this installation cannot
// suppress repository-controlled configuration. Probe outside the worktree.
func (r *Runner) checkProjectIsolationSupport(ctx context.Context, extraEnv map[string]string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	argv := loginshell.Wrap(r.loginShell, loginshell.Flags(r.loginShell), []string{"claude", "--help"})
	factory := r.cmdFactory
	if factory == nil {
		factory = exec.CommandContext
	}
	cmd := factory(ctx, argv[0], argv[1:]...)
	cmd.Dir = os.TempDir()
	cmd.Env = probeEnviron(extraEnv)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("cannot verify Claude --strict-mcp-config and --setting-sources support: %w; upgrade Claude or use boss repo update <id> --trusted", err)
	}
	fields := strings.Fields(string(output))
	for _, flag := range []string{"--strict-mcp-config", "--setting-sources"} {
		if !slices.Contains(fields, flag) {
			return fmt.Errorf("claude is missing %s; upgrade Claude or use boss repo update <id> --trusted", flag)
		}
	}
	return nil
}

func projectIsolationArgs(trusted bool) []string {
	if trusted {
		return nil
	}
	return []string{"--strict-mcp-config", "--setting-sources", "user"}
}

// RunnerOption configures a Runner. Kept as RunnerOption (rather than the
// agentruntime-style Option) because main.go's runnerOptsFromEnv returns
// []RunnerOption and the public surface is callable from outside the package.
type RunnerOption func(*Runner)

// WithDangerouslySkipPermissions toggles passing --dangerously-skip-permissions
// to the Claude CLI. Wired in production from the BOSS_PLUGIN_dangerously_skip_permissions
// env var (set by bossd's plugin host from Settings.Plugins[claude].Config) —
// see runnerOptsFromEnv in main.go. Every daemon spawn path reaches argv
// through this plugin: headless via buildArgv, and the tmux/interactive paths
// via BuildInteractiveCommand (services/bossd/internal/session/tmux_chat.go and
// server/spawn_chat_tmux.go call it rather than assembling claude argv
// themselves), so a toggle set here applies to all of them.
func WithDangerouslySkipPermissions(v bool) RunnerOption {
	return func(r *Runner) { r.dangerouslySkipPermissions = v }
}

// WithLoginShell wraps claude argv through the configured login shell.
func WithLoginShell(shell string) RunnerOption {
	return func(r *Runner) { r.loginShell = shell }
}

// WithModel pins the fallback claude --model selection for runs that do not
// carry their own. Empty means "pass no --model", which lets the claude CLI
// resolve the model from its own settings.
func WithModel(model string) RunnerOption {
	return func(r *Runner) { r.model = model }
}

// WithEffort pins the fallback claude --effort selection for runs that do not
// carry their own. Empty means "pass no --effort", which lets the claude CLI
// resolve the reasoning effort from its own settings.
func WithEffort(effort string) RunnerOption {
	return func(r *Runner) { r.effort = effort }
}

// resolveClaudeModel applies per-request-wins/plugin-default-fallback. A run
// that carries an explicit model (session.model / agent_chats.model, set via
// `boss new --model`, a cron job's model, or MCP create_session) always wins;
// otherwise the plugin-level default from BOSS_PLUGIN_model applies. Both empty
// means no --model on argv and the claude CLI picks.
//
// The fallback exists because "no --model" is not deterministic in practice:
// two chats spawned minutes apart in the same worktree were observed resolving
// to different context windows, so a run's usable context depended on state
// outside boss. Pinning the model here makes every boss-spawned claude run
// resolve identically without the daemon having to thread a default through
// each of its spawn sites.
func resolveClaudeModel(reqModel, pluginModel string) string {
	if reqModel != "" {
		return reqModel
	}
	return pluginModel
}

func resolveClaudeEffort(reqEffort, pluginEffort string) string {
	if reqEffort != "" {
		return reqEffort
	}
	return pluginEffort
}

// WithCommandFactory overrides the agent command factory (for testing).
// Delegates to agentruntime.WithCommandFactory; preserved as a claude-side
// RunnerOption so existing tests don't have to import agentruntime.
func WithCommandFactory(f agentruntime.CommandFactory) RunnerOption {
	return func(r *Runner) { r.cmdFactory = f }
}

// NewRunner creates a Claude process runner backed by agentruntime.
func NewRunner(logger zerolog.Logger, opts ...RunnerOption) *Runner {
	r := &Runner{effort: defaultClaudeEffort}
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
	if model := resolveClaudeModel(in.Options["model"], r.model); model != "" {
		args = append(args, "--model", model)
	}
	if effort := resolveClaudeEffort(in.Options["effort"], r.effort); effort != "" {
		args = append(args, "--effort", effort)
	}
	// Trusted repos retain native Claude configuration discovery. Untrusted
	// repos disable MCP configuration and project/local settings together.
	args = append(args, projectIsolationArgs(in.ShouldTrustProjectConfig)...)
	return loginshell.Wrap(r.loginShell, loginshell.Flags(r.loginShell), args)
}
