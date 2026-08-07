// Package setupscript defines the structured "setup script" contract used
// by worktree creation and resurrection.
//
// Historically the daemon stored a bare shell string and executed it with
// `sh -c`, which let anyone with write access to the setup_script column
// inject arbitrary shell. The Spec type replaces that with a small discrim-
// inated union: make target, script path, or command argv. Each variant is
// executed without a shell, and path inputs are validated against traversal
// before exec.
//
// Bare-string values from the legacy schema are still accepted: Parse wraps
// them in a Spec{Type: TypeLegacy}, and Execute materializes the content to
// <worktree>/.boss/setup.sh before running. A reconfiguration hint is logged
// via the optional Logger.
package setupscript

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/recurser/bossalib/loginshell"
)

// Type discriminates the Spec shape.
type Type string

const (
	// TypeMake runs `make <Target>` in the worktree. Requires a Makefile.
	TypeMake Type = "make"
	// TypeScript runs an executable file at <worktree>/<Path>. No shell.
	TypeScript Type = "script"
	// TypeCommand runs Argv[0] with Argv[1:] as args. No shell.
	TypeCommand Type = "command"
	// TypeLegacy wraps an un-migrated bare shell string. Execute writes it
	// to <worktree>/.boss/setup.sh with a shebang before running.
	TypeLegacy Type = "legacy"
)

// Spec is the structured setup-script contract.
//
// Exactly one of Target/Path/Argv/LegacyScript is meaningful depending on
// Type. JSON (un)marshaling round-trips the active field plus Type.
type Spec struct {
	Type         Type     `json:"type"`
	Target       string   `json:"target,omitempty"`
	Path         string   `json:"path,omitempty"`
	Argv         []string `json:"argv,omitempty"`
	LegacyScript string   `json:"-"` // populated by Parse; never marshaled
}

// ErrInvalidSpec is returned for specs that fail validation.
var ErrInvalidSpec = errors.New("invalid setup_script spec")

// setupOutputTailBytes bounds how much of a setup script's combined
// stdout/stderr is folded into the returned error. Enough to show the actual
// failure (a stack trace, a "command not found", a make error) without
// ballooning the error string. It doubles as the log-volume bound for
// ExecuteOpts.LogOutput, which is written on both the success and the failure
// path, so raising it to enrich error messages also multiplies what a chatty
// setup script writes to the daemon log.
const setupOutputTailBytes = 4096

// setupWaitDelay bounds how long Wait keeps waiting once the script's own
// process is finished — killed after ctx expired, or exited on its own — for
// the stdout/stderr pipe to close. Long enough that a normally-exiting child's
// last writes still land in the tail; short enough that a grandchild which
// survives the script (a spawned daemon, a `pnpm install` service) cannot hold
// a bootstrap open past its deadline. See the WaitDelay comment in Execute.
//
// A var, not a const, only so tests can shorten it: asserting the bound with
// the production value would cost five seconds of wall clock per case.
var setupWaitDelay = 5 * time.Second

// tailWriter is an io.Writer that retains only the last max bytes written to
// it, so a setup script's final output can be surfaced in the error and in the
// LogOutput sink without buffering the entire (potentially large) stream.
type tailWriter struct {
	max int
	buf []byte
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	if len(w.buf) > w.max {
		w.buf = w.buf[len(w.buf)-w.max:]
	}
	return len(p), nil
}

func (w *tailWriter) String() string { return string(w.buf) }

// Parse decodes a stored setup_script column value into a Spec.
//
// If the string starts with `{` it is parsed as JSON. Anything else is
// treated as a legacy shell string — Parse always succeeds for non-JSON
// input; Validate/Execute handle the legacy semantics.
func Parse(stored string) (Spec, error) {
	s := strings.TrimSpace(stored)
	if s == "" {
		return Spec{}, fmt.Errorf("%w: empty", ErrInvalidSpec)
	}
	if !strings.HasPrefix(s, "{") {
		return Spec{Type: TypeLegacy, LegacyScript: stored}, nil
	}
	var spec Spec
	if err := json.Unmarshal([]byte(s), &spec); err != nil {
		return Spec{}, fmt.Errorf("%w: decode json: %w", ErrInvalidSpec, err)
	}
	return spec, nil
}

// Validate returns ErrInvalidSpec when the spec is malformed, ignoring
// anything filesystem-dependent (no worktree required). Filesystem checks
// (path traversal, Makefile existence) are deferred to Execute, since they
// require the worktree path.
func (s Spec) Validate() error {
	switch s.Type {
	case TypeMake:
		if strings.TrimSpace(s.Target) == "" {
			return fmt.Errorf("%w: make target must be non-empty", ErrInvalidSpec)
		}
		if strings.ContainsAny(s.Target, " \t\n\r;|&$`") {
			return fmt.Errorf("%w: make target must not contain shell metacharacters", ErrInvalidSpec)
		}
	case TypeScript:
		if err := validateScriptPath(s.Path); err != nil {
			return err
		}
	case TypeCommand:
		if len(s.Argv) == 0 {
			return fmt.Errorf("%w: command argv must be non-empty", ErrInvalidSpec)
		}
		if strings.TrimSpace(s.Argv[0]) == "" {
			return fmt.Errorf("%w: command argv[0] must be non-empty", ErrInvalidSpec)
		}
	case TypeLegacy:
		if strings.TrimSpace(s.LegacyScript) == "" {
			return fmt.Errorf("%w: legacy script must be non-empty", ErrInvalidSpec)
		}
	default:
		return fmt.Errorf("%w: unknown type %q", ErrInvalidSpec, s.Type)
	}
	return nil
}

// validateScriptPath rejects absolute paths and any path that escapes the
// worktree root via `..` segments. Filesystem existence is checked at
// Execute time, not here, so Validate remains pure.
func validateScriptPath(p string) error {
	if strings.TrimSpace(p) == "" {
		return fmt.Errorf("%w: script path must be non-empty", ErrInvalidSpec)
	}
	if filepath.IsAbs(p) {
		return fmt.Errorf("%w: script path must be relative to the worktree", ErrInvalidSpec)
	}
	clean := filepath.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf(`%w: script path must not escape the worktree via ".."`, ErrInvalidSpec)
	}
	return nil
}

// ExecuteOpts plumbs execution context into Execute.
type ExecuteOpts struct {
	RepoPath     string        // main repo path; exposed as REPO_DIR
	WorktreePath string        // worktree path; exposed as WORKTREE_DIR
	Output       io.Writer     // stdout + stderr sink; nil → os.Stderr
	Timeout      time.Duration // overall timeout; zero → no additional deadline
	// LogOutput is a secondary, diagnostics-only sink for the daemon log. It is
	// additive to Output rather than an alternative: a caller that claims the
	// live stream (the create-session stream does) otherwise leaves the run with
	// no record at all in the log. It receives the bounded output tail once, on
	// completion — not the live stream — so a chatty setup script cannot flood
	// the log. Optional; nil is the historical behaviour.
	LogOutput io.Writer
	// LoginShell, when set to a supported shell (the user's $SHELL captured in
	// settings), runs the setup command THROUGH that login shell — exactly how
	// agent plugins are launched. The daemon's own PATH is the restricted
	// launchd/login environment and omits per-project version-manager shims
	// (nodenv/asdf/mise/…), so a bare `make setup-worktree` can't find `pnpm`
	// and silently skips dependency + git-hook installation. The login shell
	// loads those shims, so tools resolve identically to an interactive run.
	// Empty or unsupported → the command runs directly (legacy behaviour).
	LoginShell string
	// Warn is called exactly once on legacy-script execution with a
	// reconfiguration hint. Optional — nil is fine.
	Warn func(msg string)
}

// Execute runs the spec. Execute re-validates, performs filesystem-scoped
// checks (path traversal after join, Makefile existence), then invokes the
// underlying binary via exec.CommandContext without a shell.
//
// The returned duration measures the script's own run (cmd.Run) and is
// reported on the failure path too — a script that failed after five minutes
// still cost five minutes. It is zero only when the spec never reached exec.
func (s Spec) Execute(ctx context.Context, opts ExecuteOpts) (time.Duration, error) {
	if err := s.Validate(); err != nil {
		return 0, err
	}
	if opts.WorktreePath == "" {
		return 0, fmt.Errorf("%w: worktree path required", ErrInvalidSpec)
	}

	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	output := opts.Output
	if output == nil {
		output = os.Stderr
	}

	argv, err := s.buildArgv(opts)
	if err != nil {
		return 0, err
	}
	// Run through the user's login shell when configured so version-manager
	// shims land on PATH (see ExecuteOpts.LoginShell). Unsupported/empty shells
	// fall through to direct execution.
	if opts.LoginShell != "" && loginshell.IsSupported(opts.LoginShell) {
		argv = loginshell.Wrap(opts.LoginShell, loginshell.Flags(opts.LoginShell), argv)
	}
	// #nosec G204 -- runs the operator-configured setup_script, optionally wrapped in a login shell; local-trust
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = opts.WorktreePath
	cmd.Env = append(os.Environ(),
		"REPO_DIR="+opts.RepoPath,
		"WORKTREE_DIR="+opts.WorktreePath,
	)
	// Tee the combined stream to a bounded tail buffer: it feeds both the
	// failure error and opts.LogOutput (the full stream still reaches
	// opts.Output).
	tail := &tailWriter{max: setupOutputTailBytes}
	sink := io.MultiWriter(output, tail)
	cmd.Stdout = sink
	cmd.Stderr = sink
	// Bound Wait's two sources of unexpected delay: a child that outlives the
	// kill once ctx is done, and a child that has exited while a surviving
	// grandchild (a `pnpm install` service, a spawned daemon) still holds the
	// write end of the pipe. Because Stdout/Stderr are not *os.File, os/exec
	// copies through a pipe, so without this cmd.Run can block on that pipe
	// long past opts.Timeout — leaving the caller's bootstrap deadline unable
	// to bound the setup script it exists to bound (BOS-717).
	//
	// The false-failure this trades against is handled below rather than paid:
	// a script that itself exited 0 and merely left the pipe open reports
	// exec.ErrWaitDelay, which is recognized and not reported as a failure.
	cmd.WaitDelay = setupWaitDelay

	if s.Type == TypeLegacy && opts.Warn != nil {
		opts.Warn("legacy shell-string setup_script detected — rewritten to .boss/setup.sh; re-run 'boss repo settings' to migrate to the structured form")
	}

	started := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(started)

	// The script ran to a successful exit and only its pipe was still held open
	// by a surviving grandchild, so cmd.WaitDelay elapsed and Wait reported
	// exec.ErrWaitDelay. The setup itself succeeded — all that is lost is
	// whatever the background process would have written after the script
	// returned — so this must not be turned into a failed setup. The exit-status
	// check is what keeps it narrow: a script killed at the deadline exits
	// unsuccessfully and reports an ExitError, which still falls through to the
	// timeout handling below.
	//
	// This is deliberately the OPPOSITE of how bossd's runGitWithTimeout treats
	// the same error, and the asymmetry is the point: there, stdout IS the
	// result being parsed, so a read abandoned mid-pipe makes the answer
	// untrustworthy and must be an error. Here the result is the exit status and
	// the output is only diagnostics, so a truncated tail costs nothing a caller
	// relies on.
	if runErr != nil && errors.Is(runErr, exec.ErrWaitDelay) &&
		cmd.ProcessState != nil && cmd.ProcessState.Success() {
		runErr = nil
	}

	// One materialization of the (bounded) tail feeds both consumers below, so
	// the blank-only guard cannot drift between them.
	tailStr := strings.TrimSpace(tail.String())

	// Emit the tail to the log sink once the run is over, on success and failure
	// alike. Writing the tail rather than joining the live MultiWriter is what
	// keeps the log bounded; see ExecuteOpts.LogOutput. The sink is diagnostics,
	// so a write failure is deliberately swallowed — it must not turn a working
	// setup script into a failed one.
	if opts.LogOutput != nil && tailStr != "" {
		_, _ = io.WriteString(opts.LogOutput, tailStr)
	}

	if runErr != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			// Wrap DeadlineExceeded so callers can errors.Is it, and carry the
			// tail: a timed-out install is exactly the case where the last few
			// KB name the step that hung. The deadline may be the caller's ctx
			// rather than opts.Timeout, so only name a limit we actually set.
			limit := "the caller's deadline"
			if opts.Timeout > 0 {
				limit = opts.Timeout.String()
			}
			if tailStr != "" {
				return elapsed, fmt.Errorf("run setup script (%s): timed out after %s: %w\n--- last setup output ---\n%s",
					s.Type, limit, context.DeadlineExceeded, tailStr)
			}
			return elapsed, fmt.Errorf("run setup script (%s): timed out after %s: %w", s.Type, limit, context.DeadlineExceeded)
		}
		if tailStr != "" {
			return elapsed, fmt.Errorf("run setup script (%s): %w\n--- last setup output ---\n%s", s.Type, runErr, tailStr)
		}
		return elapsed, fmt.Errorf("run setup script (%s): %w", s.Type, runErr)
	}
	return elapsed, nil
}

// buildArgv resolves the command argv for this spec, performing any
// filesystem-scoped validation (path traversal under the worktree, Makefile
// existence) along the way. The caller may wrap the argv through a login shell
// before building the *exec.Cmd.
func (s Spec) buildArgv(opts ExecuteOpts) ([]string, error) {
	switch s.Type {
	case TypeMake:
		mfPath := filepath.Join(opts.WorktreePath, "Makefile")
		if _, err := os.Stat(mfPath); err != nil {
			return nil, fmt.Errorf("%w: no Makefile in worktree: %w", ErrInvalidSpec, err)
		}
		return []string{"make", s.Target}, nil

	case TypeScript:
		full, err := resolveInsideWorktree(opts.WorktreePath, s.Path)
		if err != nil {
			return nil, err
		}
		if _, err := os.Stat(full); err != nil {
			return nil, fmt.Errorf("%w: script not found: %w", ErrInvalidSpec, err)
		}
		return []string{full}, nil

	case TypeCommand:
		return append([]string{s.Argv[0]}, s.Argv[1:]...), nil

	case TypeLegacy:
		scriptPath, err := writeLegacyScript(opts.WorktreePath, s.LegacyScript)
		if err != nil {
			return nil, err
		}
		// Invoke via `sh` rather than executing the file directly so the
		// materialized script does not need an execute bit (it is written
		// 0o600). Any shebang line (the prepended `#!/bin/sh`, or one the
		// stored legacy string carried itself) is a no-op comment under `sh`:
		// legacy scripts always run under `sh`, matching the documented
		// historical `sh -c` contract (see the package doc), which likewise
		// ignored embedded shebangs. A legacy string is a bare POSIX-sh
		// command, so this is behaviour-preserving for real values.
		return []string{"sh", scriptPath}, nil
	}
	return nil, fmt.Errorf("%w: unknown type %q", ErrInvalidSpec, s.Type)
}

// resolveInsideWorktree joins the worktree path with a user-supplied
// relative path and verifies the result is still a descendant of the
// worktree. This catches `..` traversal even when Validate's string-level
// check missed a symlink-like trick.
func resolveInsideWorktree(worktree, rel string) (string, error) {
	absWT, err := filepath.Abs(worktree)
	if err != nil {
		return "", fmt.Errorf("%w: resolve worktree: %w", ErrInvalidSpec, err)
	}
	candidate := filepath.Join(absWT, rel)
	clean := filepath.Clean(candidate)
	if clean != absWT && !strings.HasPrefix(clean, absWT+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: script path resolves outside worktree: %s", ErrInvalidSpec, rel)
	}
	return clean, nil
}

// writeLegacyScript materializes a legacy shell string at
// <worktree>/.boss/setup.sh with a shebang and mode 0600, then returns the
// absolute path. The script is run via `sh <path>`, so it needs no exec bit.
func writeLegacyScript(worktreePath, content string) (string, error) {
	dir := filepath.Join(worktreePath, ".boss")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("%w: create .boss dir: %w", ErrInvalidSpec, err)
	}
	path := filepath.Join(dir, "setup.sh")
	body := content
	if !strings.HasPrefix(body, "#!") {
		body = "#!/bin/sh\nset -e\n" + body
	}
	// Write owner-only (0o600, no exec bit): the script is invoked via `sh
	// <path>` (see buildArgv), so it never needs to be executable. Keeping it
	// non-executable satisfies gosec G306 without a follow-up chmod.
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return "", fmt.Errorf("%w: write legacy script: %w", ErrInvalidSpec, err)
	}
	return path, nil
}
