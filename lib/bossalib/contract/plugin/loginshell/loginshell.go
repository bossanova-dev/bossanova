// Package loginshell wraps command argv for execution through a user's login shell.
package loginshell

import (
	"fmt"
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

// WrapReadingFile is Wrap plus one trailing argument read from path at exec
// time: the shell reads the file, removes it, and execs argv with its contents
// appended as a single final argument. The contents never appear in the
// returned argv, so a long input does not travel through tmux's command buffer.
//
// The read goes through `command cat`, which bypasses a user alias or function
// named cat loaded from interactive rc files. Command substitution strips
// trailing newlines from the contents; internal bytes are preserved. A missing
// or unreadable file makes the shell exit non-zero instead of launching argv
// with an empty argument.
//
// ok is false when shell cannot run the form (empty shell, empty argv, empty
// path, or an unsupported shell); callers then deliver the input inline.
func WrapReadingFile(shell string, flags []string, argv []string, path string) (wrapped []string, ok bool) {
	if len(argv) == 0 || path == "" || !IsSupported(shell) {
		return nil, false
	}

	if isFishShell(shell) {
		// fish has no $0 slot: the path is $argv[1], erased before exec.
		const body = `set -l __boss_input_file $argv[1]; set -e argv[1]; ` +
			`set -l __boss_input (command cat -- $__boss_input_file | string collect); or exit 1; ` +
			`command rm -f -- $__boss_input_file; exec $argv $__boss_input`
		wrapped = make([]string, 0, 1+len(flags)+2+len(argv))
		wrapped = append(wrapped, shell)
		wrapped = append(wrapped, flags...)
		wrapped = append(wrapped, body, path)
		wrapped = append(wrapped, argv...)
		return wrapped, true
	}

	// POSIX: label is $0, the path is $1 (shifted off), argv is "$@".
	const body = `__boss_input_file=$1; shift; ` +
		`__boss_input=$(command cat -- "$__boss_input_file") || exit; ` +
		`command rm -f -- "$__boss_input_file"; exec "$@" "$__boss_input"`
	wrapped = make([]string, 0, 1+len(flags)+3+len(argv))
	wrapped = append(wrapped, shell)
	wrapped = append(wrapped, flags...)
	wrapped = append(wrapped, CommandLine(shell, body), filepath.Base(shell), path)
	wrapped = append(wrapped, argv...)
	return wrapped, true
}

// WrapInitialInput returns argv wrapped for shell with input as its final
// argument. When path is non-empty and shell supports WrapReadingFile, input is
// written to path with mode 0600 and read at exec time; otherwise input is
// appended inline and the result is a plain Wrap. An empty input is a plain
// Wrap with no file written.
func WrapInitialInput(shell string, flags []string, argv []string, input, path string) ([]string, error) {
	if input == "" {
		return Wrap(shell, flags, argv), nil
	}
	if wrapped, ok := WrapReadingFile(shell, flags, argv, path); ok {
		if err := writePrivateFile(path, input); err != nil {
			return nil, err
		}
		return wrapped, nil
	}
	inline := make([]string, 0, len(argv)+1)
	inline = append(inline, argv...)
	inline = append(inline, input)
	return Wrap(shell, flags, inline), nil
}

// writePrivateFile writes content to path readable only by its owner. The mode
// is forced after the write because O_CREATE's mode does not apply to a file
// that already exists.
func writePrivateFile(path, content string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("write initial input file: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return fmt.Errorf("write initial input file: %w", err)
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return fmt.Errorf("write initial input file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write initial input file: %w", err)
	}
	return nil
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
