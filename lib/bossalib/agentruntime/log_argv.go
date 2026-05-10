package agentruntime

import "strings"

// LogTeeArgv builds an argv that runs `inner` with stdout+stderr also
// written to logPath, while preserving the inner exit code via
// `set -o pipefail`. Use for interactive (tmux-hosted) launches that need
// human-pane visibility AND log persistence; do NOT use for headless runs
// — those should write the log directly via Runner.
//
// When logPath is empty the function short-circuits and returns `inner`
// unchanged: empty path means "no bossd-side log persistence requested",
// and the previous tee-of-empty-string bash wrapping (`tee ”`) made tee
// fail with "tee: : No such file or directory" the instant tmux launched
// the pane — killing the agent process, tearing down the tmux session,
// and bouncing the boss attach UI back to the chat list. Callers that
// genuinely have no log path (e.g. user-attached interactive spawns where
// the operator reads the tmux pane directly) should be safe by default.
//
// The shell is `bash`, not `sh`: pipefail is a bash extension and is not
// part of POSIX sh. On Linux distributions where /bin/sh is dash (Debian,
// Ubuntu) or ash (Alpine), `sh -o pipefail` aborts immediately with
// "Illegal option -o pipefail", which would break interactive agent
// sessions. bash is a hard dependency of the daemon's interactive launch
// path; CI verifies cross-platform builds for darwin/linux on amd64+arm64.
func LogTeeArgv(inner []string, logPath string) []string {
	if logPath == "" {
		return inner
	}
	cmd := "set -o pipefail; " + joinShell(inner) + " 2>&1 | tee " + shellQuote(logPath)
	return []string{"bash", "-c", cmd}
}

func joinShell(argv []string) string {
	parts := make([]string, len(argv))
	for i, s := range argv {
		parts[i] = shellQuote(s)
	}
	return strings.Join(parts, " ")
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if isShellSafe(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func isShellSafe(s string) bool {
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == '/' || c == '.' || c == '=' || c == ':':
		default:
			return false
		}
	}
	return true
}
