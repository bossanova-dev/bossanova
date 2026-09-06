// Package main manages codex CLI subprocess lifecycle for coding sessions.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/agentruntime"
	"github.com/recurser/bossalib/loginshell"
)

// Runner wraps agentruntime.Runner with codex-specific argv building. The
// embedded *agentruntime.Runner exposes the generic process lifecycle
// methods (Start, Stop, IsRunning, ExitError, Subscribe, History) used by
// server.go; this wrapper only adds the codex-specific argv construction
// and the sandbox/approval/model toggles wired from BOSS_PLUGIN_* env vars.
type Runner struct {
	*agentruntime.Runner
	sandbox           string
	approval          string
	model             string
	effort            string
	dangerouslyBypass bool
	loginShell        string
	cmdFactory        agentruntime.CommandFactory
}

const defaultCodexEffort = "medium"

// Option configures a Runner.
type Option func(*Runner)

// WithSandbox sets the codex --sandbox mode. Valid values per Lane 0 spike:
// workspace-write, read-only, danger-full-access. Empty means "use codex
// default" (we do not pass --sandbox).
func WithSandbox(mode string) Option { return func(r *Runner) { r.sandbox = mode } }

// WithApproval sets the codex --ask-for-approval policy. Empty means "use
// codex default". Note codex spells the flag --ask-for-approval, not
// --approval (Lane 0 spike finding).
func WithApproval(policy string) Option { return func(r *Runner) { r.approval = policy } }

// WithModel pins the codex --model selection. Empty means "use codex default".
func WithModel(model string) Option { return func(r *Runner) { r.model = model } }

// WithEffort pins codex's model_reasoning_effort config override. Empty means
// "use codex default" and emits no override.
func WithEffort(effort string) Option { return func(r *Runner) { r.effort = effort } }

// WithLoginShell wraps codex argv through the configured login shell.
func WithLoginShell(shell string) Option { return func(r *Runner) { r.loginShell = shell } }

// WithDangerouslyBypassApprovalsAndSandbox toggles passing
// `--dangerously-bypass-approvals-and-sandbox` to the codex CLI. Wired in
// production from the BOSS_PLUGIN_dangerously_bypass_approvals_and_sandbox
// env var (set by bossd's plugin host from Settings.Plugins[codex].Config) —
// see runnerOptsFromEnv in main.go.
//
// Codex rejects the bypass flag combined with --sandbox or
// --ask-for-approval, so when bypass is on, buildArgv (and the interactive
// command in server.go) drop those flags.
func WithDangerouslyBypassApprovalsAndSandbox(v bool) Option {
	return func(r *Runner) { r.dangerouslyBypass = v }
}

// WithCommandFactory overrides the agent command factory (for testing).
// Delegates to agentruntime.WithCommandFactory; preserved as a codex-side
// Option so existing tests don't have to import agentruntime.
func WithCommandFactory(f agentruntime.CommandFactory) Option {
	return func(r *Runner) { r.cmdFactory = f }
}

// NewRunner creates a codex process runner backed by agentruntime.
//
// Two agentruntime hooks are wired here, both per Lane 0 spike findings:
//
//   - PostExit upgrades a generic non-zero exit into ErrAuthRequired when
//     the log tail contains the codex auth-failure markers, so the
//     daemon can detect "user needs to run `codex login`" without
//     inspecting log files itself.
//
//   - SessionIDFromOutput parses the codex-generated UUID out of the
//     `thread.started` JSONL event on stdout. codex has no --session-id
//     flag (Lane 0 finding); the runner therefore cannot return the
//     caller-provided session-ID hint and must wait briefly for codex
//     to emit its own UUID before returning to bossd.
func NewRunner(logger zerolog.Logger, opts ...Option) *Runner {
	r := &Runner{effort: defaultCodexEffort}
	for _, opt := range opts {
		opt(r)
	}
	var extra []agentruntime.Option
	if r.cmdFactory != nil {
		extra = append(extra, agentruntime.WithCommandFactory(r.cmdFactory))
	}
	r.Runner = agentruntime.NewRunner(logger, agentruntime.Options{
		BinaryName: "codex",
		BuildArgv:  r.buildArgv,
		PostExit: func(orig error, tail []byte) error {
			// Only a non-nil exit can be classified at all.
			if orig == nil {
				return nil
			}
			// Non-execution is checked before either marker match. 127/126 is
			// a structural fact the OS reported about the process, while the
			// auth and usage-cap markers are substring matches on
			// agent-authored log text. A codex that never started cannot have
			// printed its own auth banner, so an auth marker in a 127 tail
			// came from the shell or a wrapper -- and reading it as
			// ErrAuthRequired would bench a working credential for a PATH
			// fault, the expensive direction of this mistake (BOS-1172).
			if err := detectNonExecution(orig); err != nil {
				return err
			}
			// Auth is checked next so it still wins when both an auth and a
			// usage-cap marker appear in the same tail, preserving today's
			// behavior.
			if detectAuthFailure(tail) {
				return ErrAuthRequired
			}
			if err := classifyUsageCap(logger, tail, time.Now()); err != nil {
				return err
			}
			return nil
		},
		SessionIDFromOutput: parseThreadStartedID,
	}, extra...)
	return r
}

// parseThreadStartedID scans early stdout for codex's `thread.started`
// JSONL event and returns the agent-generated thread ID. Returns "" when
// no such event is observed in the buffer; the runner falls back to the
// caller-provided session-ID hint in that case.
//
// The `thread.started` payload shape comes from the Lane 0 spike. Example:
//
//	{"type":"thread.started","thread_id":"019e068c-9ade-7cf0-be8c-3b21977576d0"}
func parseThreadStartedID(data []byte) string {
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("{")) {
			continue
		}
		var entry struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
		}
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		if entry.Type == "thread.started" && entry.ThreadID != "" {
			return entry.ThreadID
		}
	}
	return ""
}

// buildArgv constructs the codex CLI argv for headless runs.
//
// Shape per Lane 0 spike:
//   - `codex exec --json --skip-git-repo-check` for fresh runs
//   - `codex exec resume <UUID> --json --skip-git-repo-check` for resume
//     (resume is a SUBCOMMAND, not a flag)
//   - --sandbox / --ask-for-approval / --model are appended when configured
//
// codex generates its own session UUID and emits it via the `thread.started`
// JSONL event on stdout — there is no --session-id flag. The caller-provided
// session ID hint is therefore ignored at the argv layer; the runner's
// SessionIDFromOutput hook (wired in C.6) reads the codex-generated UUID
// out of stdout and returns it back to bossd.
func (r *Runner) buildArgv(in agentruntime.BuildArgvInput) []string {
	args := []string{"codex", "exec"}
	if in.Resume != nil {
		args = append(args, "resume", *in.Resume)
	}
	args = append(args, "--json", "--skip-git-repo-check")
	if r.dangerouslyBypass {
		// Mutually exclusive with --sandbox / --ask-for-approval (codex
		// errors out when combined), so drop those flags entirely here.
		args = append(args, "--dangerously-bypass-approvals-and-sandbox")
	} else {
		if r.sandbox != "" {
			args = append(args, "--sandbox", r.sandbox)
		}
		if r.approval != "" {
			args = append(args, "--ask-for-approval", r.approval)
		}
	}
	// A per-request model (carried via Options) overrides the plugin-global
	// BOSS_PLUGIN_model env default; empty falls back to the env default.
	if model := resolveCodexModel(in.Options["model"], r.model); model != "" {
		args = append(args, "--model", model)
	}
	if effort := resolveCodexEffort(in.Options["effort"], r.effort); effort != "" {
		args = append(args, "-c", "model_reasoning_effort="+effort)
	}
	// Caller-provided session ID is ignored: codex generates its own.
	_ = in.ProvidedSessionID
	_ = in.SessionID
	return loginshell.Wrap(r.loginShell, loginshell.Flags(r.loginShell), args)
}

// resolveCodexModel implements the per-request-wins, env-fallback rule shared
// by the headless and interactive argv paths: a non-empty request model wins;
// otherwise the plugin-global env default (BOSS_PLUGIN_model) is used; empty
// means "codex CLI default" (no --model flag).
func resolveCodexModel(reqModel, envModel string) string {
	if reqModel != "" {
		return reqModel
	}
	return envModel
}

func resolveCodexEffort(reqEffort, envEffort string) string {
	if reqEffort != "" {
		return reqEffort
	}
	return envEffort
}

// Start spawns the codex CLI subprocess. Delegates to the embedded
// agentruntime.Runner.Start. The returned session ID is the codex-generated
// UUID parsed from `thread.started` (via the SessionIDFromOutput hook wired
// in C.6); falls back to the caller-supplied hint if no UUID was observed
// in time.
func (r *Runner) Start(ctx context.Context, workDir, plan string, resume *string, sessionID, logPath, model, effort string, extraEnv map[string]string) (string, error) {
	return r.StartWithOptions(ctx, workDir, plan, resume, sessionID, logPath, extraEnv, map[string]string{
		"model":  model,
		"effort": effort,
	})
}
