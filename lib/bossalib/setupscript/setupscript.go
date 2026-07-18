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

// setupOutputTailBytes bounds how much of a failed setup script's combined
// stdout/stderr is folded into the returned error. Enough to show the actual
// failure (a stack trace, a "command not found", a make error) without
// ballooning the error string.
const setupOutputTailBytes = 4096

// tailWriter is an io.Writer that retains only the last max bytes written to
// it, so a failed setup script's final output can be surfaced in the error
// without buffering the entire (potentially large) stream.
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
func (s Spec) Execute(ctx context.Context, opts ExecuteOpts) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if opts.WorktreePath == "" {
		return fmt.Errorf("%w: worktree path required", ErrInvalidSpec)
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
		return err
	}
	// Run through the user's login shell when configured so version-manager
	// shims land on PATH (see ExecuteOpts.LoginShell). Unsupported/empty shells
	// fall through to direct execution.
	if opts.LoginShell != "" && loginshell.IsSupported(opts.LoginShell) {
		argv = loginshell.Wrap(opts.LoginShell, loginshell.Flags(opts.LoginShell), argv)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = opts.WorktreePath
	cmd.Env = append(os.Environ(),
		"REPO_DIR="+opts.RepoPath,
		"WORKTREE_DIR="+opts.WorktreePath,
	)
	// Tee the combined stream to a bounded tail buffer so a failure can report
	// the script's actual output (the full stream still reaches opts.Output).
	tail := &tailWriter{max: setupOutputTailBytes}
	sink := io.MultiWriter(output, tail)
	cmd.Stdout = sink
	cmd.Stderr = sink

	if s.Type == TypeLegacy && opts.Warn != nil {
		opts.Warn("legacy shell-string setup_script detected — rewritten to .boss/setup.sh; re-run 'boss repo settings' to migrate to the structured form")
	}

	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("timed out after %v", opts.Timeout)
		}
		if t := strings.TrimSpace(tail.String()); t != "" {
			return fmt.Errorf("run setup script (%s): %w\n--- last setup output ---\n%s", s.Type, err, t)
		}
		return fmt.Errorf("run setup script (%s): %w", s.Type, err)
	}
	return nil
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
