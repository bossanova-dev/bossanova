package views

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

type unsupportedTerminalError struct {
	termProgram string
}

func (e *unsupportedTerminalError) Error() string {
	if e.termProgram == "" {
		return "no supported terminal detected"
	}
	return fmt.Sprintf("no new-tab handler for %q", e.termProgram)
}

// newTabCmd returns the command that opens a new terminal tab cd'd into cwd,
// chosen by inspecting the environment. Detection order:
//
//  1. $TMUX set                         -> tmux new-window -c <cwd>
//  2. iTerm2 (TERM_PROGRAM / session)   -> osascript ...
//  3. Ghostty (env signals, darwin)     -> open -a Ghostty <cwd>
//  4. none                              -> unsupportedTerminalError
//
// Split from openInNewTab so it can be tested without running anything.
func newTabCmd(env func(string) string, goos string, cwd string) (*exec.Cmd, error) {
	if env("TMUX") != "" {
		return exec.Command("tmux", "new-window", "-c", cwd), nil
	}

	termProgram := env("TERM_PROGRAM")

	if termProgram == "iTerm.app" || env("ITERM_SESSION_ID") != "" {
		return exec.Command("osascript", "-e", buildITermScript(cwd)), nil
	}

	if env("GHOSTTY_RESOURCES_DIR") != "" || strings.EqualFold(termProgram, "ghostty") {
		if goos != "darwin" {
			return nil, &unsupportedTerminalError{termProgram: "ghostty (non-darwin)"}
		}
		// Ghostty's macOS bundle treats a directory argument as "open
		// this path in a new tab of the existing window". Using -n
		// would spawn a second app instance (separate dock icon and
		// window); --args --working-directory= only works for the
		// first launch and is ignored on reopen.
		return exec.Command("open", "-a", "Ghostty", cwd), nil
	}

	return nil, &unsupportedTerminalError{termProgram: termProgram}
}

func buildITermScript(cwd string) string {
	// Single-quote the path at the shell layer so $, backticks, and $(...)
	// in the path are not expanded by the shell that iTerm feeds the
	// text to. The inner escaping handles any ' in the path.
	escaped := escapeForAppleScript(escapeForShellSingleQuote(cwd))
	return fmt.Sprintf(`tell application "iTerm"
	tell current window
		create tab with default profile
		tell current session to write text "cd '%s'"
	end tell
end tell`, escaped)
}

func escapeForAppleScript(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// escapeForShellSingleQuote rewrites s so it can be placed between
// single quotes in a POSIX shell. Each embedded single quote is
// replaced by the four-char sequence [quote, backslash, quote, quote]
// — close the quoted run, emit a backslash-escaped literal quote,
// then reopen a new quoted run.
func escapeForShellSingleQuote(s string) string {
	return strings.ReplaceAll(s, `'`, `'\''`)
}

// openInNewTab opens a new terminal tab cd'd into cwd using whatever
// mechanism fits the current environment.
func openInNewTab(cwd string) error {
	cmd, err := newTabCmd(os.Getenv, runtime.GOOS, cwd)
	if err != nil {
		return err
	}
	return cmd.Run()
}

// hasNewTabSupport reports whether the current environment has a
// detected terminal that openInNewTab knows how to drive. Callers
// use this to hide the [t]erminal action when it would only error.
func hasNewTabSupport() bool {
	_, err := newTabCmd(os.Getenv, runtime.GOOS, "")
	return err == nil
}
