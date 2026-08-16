// Package gatecmd executes a "gate command" before a cron job fires.
// Exit 0 means the job may proceed; any other outcome (non-zero exit,
// launch failure, timeout) blocks the job.
//
// Blocking is not one thing. A gate that ran and decided "no work" and a gate
// that never ran at all both stop the fire, but they mean opposite things to an
// operator: the first is a healthy skip, the second is a broken deployment
// reporting a quiet backlog (BOS-880/BOS-881). Run therefore returns a typed
// Result whose FailureKind separates the two, so a caller branches on the
// classification instead of string-matching an error message.
package gatecmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// DefaultTimeout is the gate timeout when Options.Timeout <= 0.
const DefaultTimeout = 60 * time.Second

// Shell exit codes that, by near-universal POSIX convention, mean the shell
// itself could not run what it was asked to run. `sh -c` launches fine even
// when the inner command is missing, so Go sees a plain *exec.ExitError and
// these codes are the only signal separating "the interpreter is gone" from a
// gate that ran and exited non-zero on purpose.
const (
	shellExitNotExecutable = 126 // found, but not executable
	shellExitNotFound      = 127 // command not found
)

// FailureKind classifies why a gate did not pass. It is the typed replacement
// for parsing Result.Err, and the input to CouldNotRun.
type FailureKind int

const (
	// FailureNone is the zero value, used when the gate exited 0.
	FailureNone FailureKind = iota
	// FailureEmptyCommand means Options.Command was blank after trimming —
	// a misconfiguration, so nothing was ever evaluated.
	FailureEmptyCommand
	// FailureTimeout means the gate exceeded its timeout and was killed. The
	// gate's verdict is unknown, not negative.
	FailureTimeout
	// FailureCommandNotFound means the command could not be located: either
	// the direct-exec branch failed lookup, or the shell exited 127.
	FailureCommandNotFound
	// FailureNotExecutable means the command was found but could not be
	// executed (permission, non-executable file, bad interpreter): either a
	// permission error on the direct-exec branch, or shell exit 126.
	FailureNotExecutable
	// FailureLaunch means the process could not be started for some reason
	// that is neither a lookup nor a permission problem — a resource limit
	// (EMFILE, ENOMEM) or a bad executable format (ENOEXEC). A missing
	// working directory is NOT one of these: os/exec reports a chdir failure
	// as fs.ErrNotExist, so it narrows to FailureCommandNotFound above.
	FailureLaunch
	// FailureSignaled means the gate started but was killed by a signal
	// before it could exit — the OOM killer, an operator kill, a segfaulting
	// interpreter, or cancellation of the caller's context (daemon shutdown).
	// os/exec reports this as an *exec.ExitError whose ExitCode is -1, i.e. no
	// exit status was ever observed, so the gate chose nothing.
	FailureSignaled
	// FailureNonZeroExit means the gate RAN and chose to exit non-zero. This
	// is the only failure that represents an actual gate decision.
	FailureNonZeroExit
)

// String implements fmt.Stringer. The strings are stable identifiers suitable
// for structured log fields.
func (k FailureKind) String() string {
	switch k {
	case FailureNone:
		return "none"
	case FailureEmptyCommand:
		return "empty_command"
	case FailureTimeout:
		return "timeout"
	case FailureCommandNotFound:
		return "command_not_found"
	case FailureNotExecutable:
		return "not_executable"
	case FailureLaunch:
		return "launch_failure"
	case FailureSignaled:
		return "signaled"
	case FailureNonZeroExit:
		return "non_zero_exit"
	default:
		return "unknown"
	}
}

// CouldNotRun reports whether the gate never produced a verdict — the command
// was missing, unrunnable, timed out, or was never configured. Only
// FailureNonZeroExit is a real gate decision; everything else means the gate
// condition is UNKNOWN and the fire is being blocked defensively.
//
// The 126/127 mapping is a convention, not a guarantee: a gate script is free
// to exit 127 for its own reasons and will be reported as broken. That is the
// deliberate trade — the convention is near-universal, the failure it catches
// is silent and expensive, and the misclassification is loud and immediately
// visible. Gate authors signalling "no work" should use exit 1.
func (k FailureKind) CouldNotRun() bool {
	switch k {
	case FailureEmptyCommand, FailureTimeout, FailureCommandNotFound,
		FailureNotExecutable, FailureLaunch, FailureSignaled:
		return true
	default:
		return false
	}
}

// Result is the outcome of a gate run.
type Result struct {
	// Passed is true iff the process exited 0.
	Passed bool
	// Failure classifies why the gate did not pass; FailureNone when Passed.
	Failure FailureKind
	// ExitCode is the process exit status when one was observed, and -1
	// whenever none was: an empty command, a launch failure, a timeout kill,
	// or any other signal death. -1 therefore always means "no exit status",
	// and is never paired with FailureNonZeroExit.
	ExitCode int
	// Err is descriptive and non-nil whenever Passed is false.
	Err error
}

// CouldNotRun reports whether the gate never produced a verdict. See
// FailureKind.CouldNotRun.
func (r Result) CouldNotRun() bool { return r.Failure.CouldNotRun() }

// Options configures a gate-command run.
type Options struct {
	Command      string // the gate command (trimmed defensively)
	RepoPath     string // cwd for the command; also REPO_DIR / WORKTREE_DIR / BOSS_WORKTREE_DIR
	LinearAPIKey string
	SentryAPIKey string
	SentryOrg    string
	// ProofAnthropicAPIKey is the keyring-resolved proof model credential
	// exposed to the gate as PROOF_ANTHROPIC_API_KEY; empty means omitted.
	ProofAnthropicAPIKey string
	// ExtraEnv is an allowlisted overlay applied to the gate process after its
	// inherited environment. It is intended for daemon-resolved credentials;
	// values are never logged by this package.
	ExtraEnv map[string]string
	// UnsetEnv names inherited variables to omit from the gate process. An
	// ExtraEnv value for the same name wins, which keeps the composed
	// environment deterministic and free of duplicate keys.
	UnsetEnv []string
	Timeout  time.Duration // <=0 → use DefaultTimeout
	Output   io.Writer     // combined stdout+stderr sink; nil → io.Discard
}

// Run executes the gate command and reports whether the job may proceed.
// Result.Passed is true iff the process exited 0; otherwise Result.Failure
// classifies why, Result.ExitCode carries the observed status (-1 when none),
// and Result.Err is descriptive. Callers must branch on Result.CouldNotRun
// rather than inspecting Result.Err's text.
func Run(ctx context.Context, o Options) Result {
	cmd := strings.TrimSpace(o.Command)
	if cmd == "" {
		return Result{Failure: FailureEmptyCommand, ExitCode: -1, Err: errors.New("empty gate command")}
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
	c.Env = commandEnv(o)
	c.Stdout = output
	c.Stderr = output

	if runErr := c.Run(); runErr != nil {
		return classify(ctx, runErr, timeout)
	}
	return Result{Passed: true, Failure: FailureNone}
}

// classify turns a failed *exec.Cmd run into a Result. The order is load
// bearing: a timeout kill also surfaces as an *exec.ExitError, so the context
// deadline must be tested before the exit status is trusted.
func classify(ctx context.Context, runErr error, timeout time.Duration) Result {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return Result{
			Failure:  FailureTimeout,
			ExitCode: -1,
			Err:      fmt.Errorf("gate command timed out after %v", timeout),
		}
	}

	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) {
		// The process never started: os/exec reports lookup and spawn problems
		// as something other than an ExitError. This is only reachable on the
		// direct-exec branch — `sh -c` itself is virtually always launchable.
		return Result{
			Failure:  launchFailureKind(runErr),
			ExitCode: -1,
			Err:      fmt.Errorf("gate command could not be launched: %w", runErr),
		}
	}

	code := exitErr.ExitCode()
	if code < 0 {
		// The process exited without an exit STATUS, which os/exec reports as
		// -1: it was killed by a signal (OOM killer, an operator kill, a
		// segfaulting interpreter) or the caller's context was cancelled — the
		// deadline case having already been taken above. It chose nothing, so
		// it is a could-not-run, not the "gate ran and said no" that the
		// default arm below would otherwise assert from a code that is not an
		// exit status at all.
		return Result{
			Failure:  FailureSignaled,
			ExitCode: -1,
			Err:      fmt.Errorf("gate command was killed before it exited: %w", runErr),
		}
	}
	switch code {
	case shellExitNotFound:
		return Result{
			Failure:  FailureCommandNotFound,
			ExitCode: code,
			Err:      fmt.Errorf("gate command not found (exit %d): %w", code, runErr),
		}
	case shellExitNotExecutable:
		return Result{
			Failure:  FailureNotExecutable,
			ExitCode: code,
			Err:      fmt.Errorf("gate command is not executable (exit %d): %w", code, runErr),
		}
	default:
		return Result{
			Failure:  FailureNonZeroExit,
			ExitCode: code,
			Err:      fmt.Errorf("gate command failed: %w", runErr),
		}
	}
}

// launchFailureKind narrows a spawn error to the most specific kind available,
// mirroring the shell's 127/126 split so both branches of buildCommand report
// the same vocabulary.
func launchFailureKind(runErr error) FailureKind {
	switch {
	case errors.Is(runErr, exec.ErrNotFound), errors.Is(runErr, fs.ErrNotExist):
		return FailureCommandNotFound
	case errors.Is(runErr, fs.ErrPermission):
		return FailureNotExecutable
	default:
		return FailureLaunch
	}
}

// commandEnv returns the gate environment with inherited values filtered before
// daemon-owned values are appended. os/exec keeps the last duplicate, but
// removing duplicates here makes override and removal behavior explicit.
func commandEnv(o Options) []string {
	overlay := map[string]string{
		"REPO_DIR":          o.RepoPath,
		"WORKTREE_DIR":      o.RepoPath,
		"BOSS_WORKTREE_DIR": o.RepoPath,
		"LINEAR_API_KEY":    o.LinearAPIKey,
		"SENTRY_API_KEY":    o.SentryAPIKey,
		"SENTRY_ORG":        o.SentryOrg,
	}
	if o.ProofAnthropicAPIKey != "" {
		overlay["PROOF_ANTHROPIC_API_KEY"] = o.ProofAnthropicAPIKey
	}
	for key, value := range o.ExtraEnv {
		overlay[key] = value
	}

	unset := make(map[string]struct{}, len(o.UnsetEnv))
	for _, key := range o.UnsetEnv {
		unset[key] = struct{}{}
	}

	env := make([]string, 0, len(os.Environ())+len(overlay))
	for _, entry := range os.Environ() {
		key, _, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		if _, overridden := overlay[key]; overridden {
			continue
		}
		if _, removed := unset[key]; removed {
			continue
		}
		env = append(env, entry)
	}

	keys := make([]string, 0, len(overlay))
	for key := range overlay {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		env = append(env, key+"="+overlay[key])
	}
	return env
}

// buildCommand constructs the *exec.Cmd for the given (pre-trimmed) command.
// Commands starting with '/', './', or '../' are split on whitespace and run
// directly without a shell. Everything else is run via `sh -c`.
func buildCommand(ctx context.Context, cmd string) *exec.Cmd {
	if strings.HasPrefix(cmd, "/") || strings.HasPrefix(cmd, "./") || strings.HasPrefix(cmd, "../") {
		argv := strings.Fields(cmd)
		// #nosec G204 -- executes an operator config-supplied gate command (sibling branch intentionally uses sh -c); operator-trust boundary, not attacker-controlled
		// owner=@recurser review-by=2027-01-18 issue=BOS-28
		return exec.CommandContext(ctx, argv[0], argv[1:]...)
	}
	return exec.CommandContext(ctx, "sh", "-c", cmd)
}
