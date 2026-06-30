// Package loginshell wraps command argv for execution through a user's login shell.
package loginshell

import (
	"os"
	"path/filepath"
)

// DefaultFlags are the fallback shell flags for shells that do not need
// shell-specific rc loading. Returns a fresh slice so callers cannot mutate the
// canonical value.
func DefaultFlags() []string {
	return []string{"-l", "-c"}
}

// Flags returns shell-specific flags for running a command body. The agent
// plugins and preflight both use this so the probe matches the real launch path.
//
// zsh and fish run as interactive login shells (-l -i -c). Both commonly load
// per-project version managers (nodenv/rbenv/pyenv/asdf/mise) only in their
// interactive config: fish's default config.fish template wraps user setup in
// `if status is-interactive` (and the canonical nodenv line is
// `status --is-interactive; and source (nodenv init -|psub)`), so a plain
// `-l -c` login shell never puts the agent's shims on PATH and the launch dies
// with exit 127. Adding -i runs that interactive setup. bash instead sources
// ~/.bashrc explicitly via CommandLine, so it does not need -i.
func Flags(shell string) []string {
	switch filepath.Base(shell) {
	case "zsh", "fish":
		return []string{"-l", "-i", "-c"}
	default:
		return DefaultFlags()
	}
}

// CommandLine returns line wrapped with any shell-specific rc loading needed
// before executing the command body.
func CommandLine(shell, line string) string {
	if filepath.Base(shell) == "bash" {
		return `if [ -r "$HOME/.bashrc" ]; then . "$HOME/.bashrc"; fi; ` + line
	}
	return line
}

// Wrap returns argv wrapped for execution through shell with flags. The command
// is passed via the shell's positional args (no re-quoting) and exec'd so the
// agent replaces the shell and owns the PTY. shell=="" or empty argv is a
// passthrough. Unknown shells are also passthrough because their -c argv
// semantics may not support the forms below. fish and POSIX shells expose
// positional args differently, so the two branches build distinct forms (and
// size their slices exactly).
func Wrap(shell string, flags []string, argv []string) []string {
	if len(argv) == 0 {
		return nil
	}
	if shell == "" {
		return argv
	}

	if isFishShell(shell) {
		// fish puts every arg after the -c body into $argv with no $0 label slot,
		// so a label would be exec'd as a command. Form: <shell> <flags> 'exec $argv' <argv...>.
		wrapped := make([]string, 0, 1+len(flags)+1+len(argv))
		wrapped = append(wrapped, shell)
		wrapped = append(wrapped, flags...)
		wrapped = append(wrapped, "exec $argv")
		wrapped = append(wrapped, argv...)
		return wrapped
	}

	if !isPOSIXShell(shell) {
		return argv
	}

	// POSIX/bash fill $0,$1,…; "$@" excludes $0, so pass a throwaway label that
	// becomes $0. Form: <shell> <flags> 'exec "$@"' <label> <argv...>.
	wrapped := make([]string, 0, 1+len(flags)+2+len(argv))
	wrapped = append(wrapped, shell)
	wrapped = append(wrapped, flags...)
	wrapped = append(wrapped, CommandLine(shell, "exec \"$@\""), filepath.Base(shell))
	wrapped = append(wrapped, argv...)
	return wrapped
}

// IsSupported reports whether Wrap knows how to run argv through shell.
func IsSupported(shell string) bool {
	return isFishShell(shell) || isPOSIXShell(shell)
}

func isFishShell(shell string) bool {
	return filepath.Base(shell) == "fish"
}

func isPOSIXShell(shell string) bool {
	switch filepath.Base(shell) {
	case "sh", "bash", "zsh", "ksh", "mksh", "pdksh", "dash", "ash", "yash":
		return true
	default:
		return false
	}
}

// Detect returns the user's configured shell.
func Detect() string {
	return os.Getenv("SHELL")
}
