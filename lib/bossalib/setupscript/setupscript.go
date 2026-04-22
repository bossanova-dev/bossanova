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
	RepoPath     string        // main repo path; exposed as BOSS_REPO_DIR
	WorktreePath string        // worktree path; exposed as BOSS_WORKTREE_DIR
	Output       io.Writer     // stdout + stderr sink; nil → os.Stderr
	Timeout      time.Duration // overall timeout; zero → no additional deadline
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

	cmd, err := s.buildCommand(ctx, opts)
	if err != nil {
		return err
	}
	cmd.Dir = opts.WorktreePath
	cmd.Env = append(os.Environ(),
		"BOSS_REPO_DIR="+opts.RepoPath,
		"BOSS_WORKTREE_DIR="+opts.WorktreePath,
	)
	cmd.Stdout = output
	cmd.Stderr = output

	if s.Type == TypeLegacy && opts.Warn != nil {
		opts.Warn("legacy shell-string setup_script detected — rewritten to .boss/setup.sh; re-run 'boss repo settings' to migrate to the structured form")
	}

	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("timed out after %v", opts.Timeout)
		}
		return err
	}
	return nil
}

// buildCommand constructs the *exec.Cmd for this spec, performing any
// filesystem-scoped validation (path traversal under the worktree, Makefile
// existence) along the way.
func (s Spec) buildCommand(ctx context.Context, opts ExecuteOpts) (*exec.Cmd, error) {
	switch s.Type {
	case TypeMake:
		mfPath := filepath.Join(opts.WorktreePath, "Makefile")
		if _, err := os.Stat(mfPath); err != nil {
			return nil, fmt.Errorf("%w: no Makefile in worktree: %w", ErrInvalidSpec, err)
		}
		return exec.CommandContext(ctx, "make", s.Target), nil

	case TypeScript:
		full, err := resolveInsideWorktree(opts.WorktreePath, s.Path)
		if err != nil {
			return nil, err
		}
		if _, err := os.Stat(full); err != nil {
			return nil, fmt.Errorf("%w: script not found: %w", ErrInvalidSpec, err)
		}
		return exec.CommandContext(ctx, full), nil

	case TypeCommand:
		return exec.CommandContext(ctx, s.Argv[0], s.Argv[1:]...), nil

	case TypeLegacy:
		scriptPath, err := writeLegacyScript(opts.WorktreePath, s.LegacyScript)
		if err != nil {
			return nil, err
		}
		return exec.CommandContext(ctx, scriptPath), nil
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
// <worktree>/.boss/setup.sh with a shebang and mode 0700, then returns the
// absolute path.
func writeLegacyScript(worktreePath, content string) (string, error) {
	dir := filepath.Join(worktreePath, ".boss")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("%w: create .boss dir: %w", ErrInvalidSpec, err)
	}
	path := filepath.Join(dir, "setup.sh")
	body := content
	if !strings.HasPrefix(body, "#!") {
		body = "#!/bin/sh\nset -e\n" + body
	}
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		return "", fmt.Errorf("%w: write legacy script: %w", ErrInvalidSpec, err)
	}
	return path, nil
}
