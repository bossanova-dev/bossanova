// Package main manages opencode CLI subprocess lifecycle for coding sessions.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/agentruntime"
	"github.com/recurser/bossalib/loginshell"
)

// Runner wraps agentruntime.Runner with opencode-specific argv building. The
// embedded *agentruntime.Runner exposes the generic process lifecycle methods
// (Start, Stop, IsRunning, ExitError, Subscribe, History) reused verbatim from
// the shared runtime; this wrapper only adds the opencode-specific argv
// construction, the model/permission toggles wired from BOSS_PLUGIN_* env vars,
// and the two agentruntime hooks (auth/usage classification via PostExit,
// session-id discovery via SessionIDFromOutput). Mirrors the codex twin
// (plugins/bossd-plugin-codex/runner.go) file-for-file.
type Runner struct {
	*agentruntime.Runner
	model                      string
	dangerouslySkipPermissions bool
	loginShell                 string
	cliVersion                 string
	cmdFactory                 agentruntime.CommandFactory
}

// Option configures a Runner.
type Option func(*Runner)

// WithModel pins the opencode --model selection (provider/model form, e.g.
// anthropic/claude-sonnet). Empty means "use opencode default" (no --model).
func WithModel(model string) Option { return func(r *Runner) { r.model = model } }

// WithDangerouslySkipPermissions toggles passing --dangerously-skip-permissions
// to the opencode CLI instead of the default --auto. Wired in production from
// the BOSS_PLUGIN_dangerously_skip_permissions env var (set by bossd's plugin
// host from Settings.Plugins[opencode].Config) — see runnerOptsFromEnv in
// main.go.
//
// A permission flag is ALWAYS present on the headless path (bossd runs runners
// unattended in isolated worktrees, so a tool-using run with no permission flag
// would hang forever on an unanswerable prompt). --auto is the documented,
// stable default; --dangerously-skip-permissions works today but is undocumented
// and could be removed without notice, so it is the opt-in escape hatch.
func WithDangerouslySkipPermissions(v bool) Option {
	return func(r *Runner) { r.dangerouslySkipPermissions = v }
}

// WithLoginShell wraps opencode argv through the configured login shell so
// nodenv/asdf shims resolve the opencode binary.
func WithLoginShell(shell string) Option { return func(r *Runner) { r.loginShell = shell } }

// WithCLIVersion pins the installed opencode CLI version string (e.g. "1.17.11"),
// so buildArgv can select a permission flag the installed binary actually
// accepts. Production wires this from a one-shot `opencode --version` probe (see
// probeOpencodeVersion / runnerOptsFromEnv). Empty or unparseable means
// "unknown", which falls back to the universally-accepted
// --dangerously-skip-permissions rather than the version-gated --auto.
func WithCLIVersion(version string) Option {
	return func(r *Runner) { r.cliVersion = strings.TrimSpace(version) }
}

// WithCommandFactory overrides the agent command factory (for testing).
// Delegates to agentruntime.WithCommandFactory; preserved as an opencode-side
// Option so tests don't have to import agentruntime.
func WithCommandFactory(f agentruntime.CommandFactory) Option {
	return func(r *Runner) { r.cmdFactory = f }
}

// NewRunner creates an opencode process runner backed by agentruntime.
//
// Two agentruntime hooks are wired here, mirroring the codex twin:
//
//   - PostExit upgrades a generic non-zero exit into ErrAuthRequired when the
//     log tail carries an opencode auth-failure signal (an `error` event with
//     statusCode 401/403 or an auth-marker message), so the daemon can detect
//     "user needs to run `opencode auth login`" without inspecting logs itself.
//     Auth is checked FIRST so it wins over a co-occurring usage-cap marker,
//     preserving the codex precedence contract.
//
//   - SessionIDFromOutput parses opencode's own session id out of the first
//     JSONL event's top-level `sessionID` on stdout. opencode generates/echoes
//     its own id (the caller-provided hint is ignored at the argv layer), so the
//     runner waits briefly for that first event before returning to bossd.
func NewRunner(logger zerolog.Logger, opts ...Option) *Runner {
	r := &Runner{}
	for _, opt := range opts {
		opt(r)
	}
	var extra []agentruntime.Option
	if r.cmdFactory != nil {
		extra = append(extra, agentruntime.WithCommandFactory(r.cmdFactory))
	}
	r.Runner = agentruntime.NewRunner(logger, agentruntime.Options{
		BinaryName: "opencode",
		BuildArgv:  r.buildArgv,
		PostExit: func(orig error, tail []byte) error {
			return classifyExit(logger, orig, tail, time.Now())
		},
		SessionIDFromOutput: sessionIDFromOutput,
	}, extra...)
	return r
}

// classifyExit is the PostExit body: it upgrades a generic non-zero exit into a
// typed sentinel by inspecting the log tail, and is a named function (rather than
// an inline closure) so the precedence contract below is exercised by tests
// against the code that actually runs, not a reconstruction of it.
//
// Precedence, mirroring the codex twin:
//
//   - Auth is checked FIRST, so ErrAuthRequired wins when both an auth and a
//     usage/rate-limit marker appear in the same tail.
//   - Only a non-nil exit can be a usage-cap / rate-limit: a clean (nil) exit is
//     never reclassified, so a success tail that merely mentions "429" in prose
//     cannot manufacture a cap.
func classifyExit(logger zerolog.Logger, orig error, tail []byte, now time.Time) error {
	if orig == nil {
		return nil
	}
	if detectAuthFailure(tail) {
		return ErrAuthRequired
	}
	if err := classifyUsageCap(logger, tail, now); err != nil {
		return err
	}
	return nil
}

// sessionIDFromOutput scans early stdout for opencode's first JSONL event and
// returns its top-level `sessionID`. Every opencode `--format json` event
// carries a top-level `sessionID` of the form `ses_XXXXXXXXXXXXXXXXXXXX` (also
// duplicated under `part.sessionID`); the canonical session id is the FIRST
// event's value. Returns "" when no such id is observed in the buffer, in which
// case the runner falls back to the caller-provided hint.
//
// The parse is line-scoped and failure-tolerant: a banner before the first JSON
// line, garbled bytes, unknown event `type`s, and a trailing partial line are
// all skipped rather than aborting discovery. We deliberately return the first
// NON-EMPTY `sessionID` without hard-rejecting ids that fail a `ses_` shape
// check, so a format drift in a future opencode release does not silently break
// session tracking; the vendored fixtures show the `ses_` prefix today.
func sessionIDFromOutput(data []byte) string {
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("{")) {
			continue
		}
		var entry struct {
			SessionID string `json:"sessionID"`
		}
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		if entry.SessionID != "" {
			return entry.SessionID
		}
	}
	return ""
}

// buildArgv constructs the opencode CLI argv for headless runs.
//
// Shape (opencode v1.18.3, upstream anomalyco/opencode):
//
//	opencode run --format json --dir <wd> <perm-flag> [--model provider/model] [--session ses_id] [<plan>]
//
// Invariants asserted by table-driven tests:
//   - Exactly ONE permission flag is ALWAYS present: --auto by default, or
//     --dangerously-skip-permissions when configured.
//   - --model is appended only when a model resolves non-empty (per-request wins,
//     env fallback).
//   - --session is appended ONLY on resume, sourced from in.Resume. The
//     caller-supplied session-id hint (in.SessionID / in.ProvidedSessionID) is
//     ignored at the argv layer — opencode echoes its own id, discovered via
//     SessionIDFromOutput.
//   - in.Plan is appended LAST as the positional run message when non-empty.
//     `opencode run [message..]` takes the prompt positionally and has no stdin
//     prompt path (upstream anomalyco/opencode#25508), so the plan MUST ride the
//     argv — agentruntime still pipes it to stdin, but opencode ignores that. It
//     is passed as a single argv element, so a multi-line plan survives the
//     login-shell wrap (loginshell.Wrap forwards argv via "$@"/$argv with no
//     re-quoting) as one unsplit argument.
//   - Argument ordering is fixed (flags, then --model, then --session, then the
//     positional plan) so the golden slices are stable.
func (r *Runner) buildArgv(in agentruntime.BuildArgvInput) []string {
	args := []string{"opencode", "run", "--format", "json", "--dir", in.WorkDir}
	// Permission flag — ALWAYS present, exactly one. --auto is the documented,
	// stable default but only exists on opencode >= 1.18 (BOS-437 live
	// validation: v1.17.11's `opencode run` rejects it with a usage banner and
	// runs nothing). When the escape hatch is set, OR the installed CLI predates
	// --auto (or its version is unknown), fall back to the
	// --dangerously-skip-permissions flag, which is accepted by every version
	// that has shipped and gives the unattended full-auto-approval a headless
	// worktree run needs.
	if r.dangerouslySkipPermissions || !autoPermissionSupported(r.cliVersion) {
		args = append(args, "--dangerously-skip-permissions")
	} else {
		args = append(args, "--auto")
	}
	// A per-request model (carried via Options) overrides the plugin-global
	// BOSS_PLUGIN_model env default; empty falls back to the env default.
	if model := resolveModel(in.Options["model"], r.model); model != "" {
		args = append(args, "--model", model)
	}
	// Resume maps to `--session <id>`, appended only when resuming.
	if in.Resume != nil {
		args = append(args, "--session", *in.Resume)
	}
	// Caller-provided session ID is ignored: opencode echoes its own.
	_ = in.ProvidedSessionID
	_ = in.SessionID
	// Positional run message — LAST, so it is never mistaken for a flag value.
	// opencode reads the prompt from argv, not stdin, so an empty plan would
	// launch a run with no instruction; append only when non-empty.
	if in.Plan != "" {
		args = append(args, in.Plan)
	}
	return loginshell.Wrap(r.loginShell, loginshell.Flags(r.loginShell), args)
}

// autoPermissionFloorMajor / autoPermissionFloorMinor is the earliest opencode
// release whose `opencode run` accepts the --auto permission flag. Versions
// below it only offer --dangerously-skip-permissions.
const (
	autoPermissionFloorMajor = 1
	autoPermissionFloorMinor = 18
)

// autoPermissionSupported reports whether the given opencode CLI version string
// (e.g. "1.18.3", "v1.17.11") accepts the --auto permission flag. It compares
// only major.minor against the 1.18 floor; an empty or unparseable version is
// treated as "unknown" and returns false, so the caller falls back to the
// universally-accepted --dangerously-skip-permissions rather than risk emitting
// a flag the installed binary rejects.
func autoPermissionSupported(version string) bool {
	major, minor, ok := parseMajorMinor(version)
	if !ok {
		return false
	}
	if major != autoPermissionFloorMajor {
		return major > autoPermissionFloorMajor
	}
	return minor >= autoPermissionFloorMinor
}

// parseMajorMinor extracts the leading major and minor integers from a semver-ish
// version string, tolerating a leading "v" and any patch/pre-release suffix.
// Returns ok=false when the two leading components are not both present integers.
func parseMajorMinor(version string) (major, minor int, ok bool) {
	v := strings.TrimSpace(version)
	v = strings.TrimPrefix(v, "v")
	if v == "" {
		return 0, 0, false
	}
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	maj, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, false
	}
	min, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, false
	}
	return maj, min, true
}

// probeOpencodeVersion runs `opencode --version` (through the same login-shell
// wrap the run/db paths use) with a short timeout and returns the reported
// version string. Any failure returns "" so the permission-flag selection falls
// back to the safe default. Mirrors opencodeStore.resolveViaSubprocess.
func probeOpencodeVersion(loginShell string) string {
	argv := []string{"opencode", "--version"}
	if loginShell != "" {
		argv = loginshell.Wrap(loginShell, loginshell.Flags(loginShell), argv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), versionProbeTimeout)
	defer cancel()
	// #nosec G204 G702 -- const `opencode --version` argv wrapped through the login shell; loginShell is trusted daemon config (Settings.Plugins[opencode]), not attacker input; no shell string interpolation; mirrors the run path and the `opencode db path` probe
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).Output()
	if err != nil {
		return ""
	}
	// Take the last non-empty line: a login-shell wrap may emit banner lines
	// before the command's own single-line version output.
	s := strings.TrimSpace(string(out))
	if idx := strings.LastIndexByte(s, '\n'); idx >= 0 {
		s = strings.TrimSpace(s[idx+1:])
	}
	return s
}

const versionProbeTimeout = 5 * time.Second

// resolveModel implements the per-request-wins, env-fallback rule shared by the
// headless argv path: a non-empty request model wins; otherwise the plugin-global
// env default (BOSS_PLUGIN_model) is used; empty means "opencode CLI default"
// (no --model flag). Mirrors codex's resolveCodexModel.
func resolveModel(reqModel, envModel string) string {
	if reqModel != "" {
		return reqModel
	}
	return envModel
}
