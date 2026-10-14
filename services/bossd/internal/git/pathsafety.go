package git

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrInvalidBranchName is returned when a branch name is one git itself would
// refuse (`git check-ref-format --branch`), or one bossd refuses before asking
// git. A branch name becomes a path component of the worktree directory, so it
// must be validated before any filesystem work derives a path from it.
var ErrInvalidBranchName = errors.New("invalid branch name")

// ErrPathOutsideBase is returned when a cleanup target does not resolve to a
// path strictly inside the directory it is allowed to delete under.
var ErrPathOutsideBase = errors.New("path is not inside the worktree base")

// ValidateBranchName reports whether name is a branch git will accept for
// `git worktree add -b`. It delegates the ref rules to git so bossd accepts
// exactly what git does, after rejecting the shapes git would treat as options
// (a leading `-`; check-ref-format takes no `--` separator) or expand rather
// than validate (`@{…}` reflog syntax).
func ValidateBranchName(ctx context.Context, name string) error {
	switch {
	case name == "":
		return fmt.Errorf("%w: empty", ErrInvalidBranchName)
	case strings.HasPrefix(name, "-"):
		return fmt.Errorf("%w: %q starts with '-'", ErrInvalidBranchName, name)
	case strings.Contains(name, "@{"):
		return fmt.Errorf("%w: %q contains '@{'", ErrInvalidBranchName, name)
	}
	// Run from the filesystem root, not the daemon's cwd or $TMPDIR: git exits
	// 128 for a cwd it cannot read exactly as it does for an invalid name, and
	// a missing $TMPDIR fails the chdir, so either would refuse every name.
	// The root always exists, and check-ref-format --branch needs no repository
	// once `@{` is rejected above.
	out, err := runGit(ctx, string(filepath.Separator), "check-ref-format", "--branch", name)
	if err != nil {
		return classifyBranchProbeError(name, err)
	}
	if out != name {
		return fmt.Errorf("%w: %q normalizes to %q", ErrInvalidBranchName, name, out)
	}
	return nil
}

// classifyBranchProbeError maps a failed `git check-ref-format` run to a
// verdict. A probe killed by a signal (Exited() is false), stopped by its
// context (runGit returns the context error, not an *exec.ExitError), or never
// started says nothing about the name, so it surfaces as itself rather than as
// "invalid". A probe that exited non-zero is read as a rejection of the name;
// git also exits 128 for its own fatal errors (e.g. an unparsable global
// config), which this cannot tell apart without matching localized stderr, so
// such an environment fault reads as an invalid name — a refusal either way.
func classifyBranchProbeError(name string, err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.Exited() {
		return fmt.Errorf("%w: %q", ErrInvalidBranchName, name)
	}
	return fmt.Errorf("validate branch name: %w", err)
}

// ensureWithin returns nil only when target lies strictly inside base — never
// base itself — both lexically and after resolving symlinks in the longest
// existing prefix of each path. The resolved comparison closes the
// symlinked-ancestor case: a directory inside base replaced by a symlink to
// somewhere else would otherwise pass the lexical check while a recursive
// delete followed the link out of the base.
func ensureWithin(base, target string) error {
	absBase, err := filepath.Abs(base)
	if err != nil {
		return fmt.Errorf("resolve base %q: %w", base, err)
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return fmt.Errorf("resolve target %q: %w", target, err)
	}
	if !strictlyUnder(absBase, absTarget) {
		return fmt.Errorf("%w: %q is not under %q", ErrPathOutsideBase, absTarget, absBase)
	}
	realBase, err := resolveExistingPrefix(absBase)
	if err != nil {
		return fmt.Errorf("resolve base %q: %w", absBase, err)
	}
	realTarget, err := resolveExistingPrefix(absTarget)
	if err != nil {
		return fmt.Errorf("resolve target %q: %w", absTarget, err)
	}
	if !strictlyUnder(realBase, realTarget) {
		return fmt.Errorf("%w: %q resolves to %q, outside %q", ErrPathOutsideBase, absTarget, realTarget, realBase)
	}
	return nil
}

// strictlyUnder reports whether the absolute, clean path target is a strict
// descendant of base.
func strictlyUnder(base, target string) bool {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolveExistingPrefix resolves symlinks in the longest existing prefix of the
// absolute path p and re-appends the components that do not exist yet (which
// cannot be symlinks). A component that exists but cannot be resolved — a
// dangling symlink — is an error, so the caller refuses rather than guesses.
func resolveExistingPrefix(p string) (string, error) {
	cur := p
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		if _, lerr := os.Lstat(cur); lerr == nil {
			return "", fmt.Errorf("%q exists but does not resolve: %w", cur, err)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err
		}
		missing = append(missing, filepath.Base(cur))
		cur = parent
	}
}
