// Package preflight runs startup checks for required external software so
// the boss TUI can fail with a friendly blocking message instead of a stack
// trace once a feature that needs the missing tool is exercised.
package preflight

import (
	"fmt"
	"os/exec"
	"runtime"
)

// Issue describes a failed preflight check. Title is a short headline shown
// in bold; Detail is the multi-line explanation including install hints.
type Issue struct {
	Title  string
	Detail string
}

// CheckTmux verifies that the tmux binary is on PATH. Boss uses tmux to
// host agent sessions, so without it neither the daemon nor the attach
// flow can function.
func CheckTmux() *Issue {
	if _, err := exec.LookPath("tmux"); err == nil {
		return nil
	}
	return &Issue{
		Title:  "tmux is not installed",
		Detail: "Boss uses tmux to host agent sessions. Install it and restart boss:\n\n" + tmuxInstallHint(),
	}
}

func tmuxInstallHint() string {
	switch runtime.GOOS {
	case "darwin":
		return "  brew install tmux"
	case "linux":
		return "  sudo apt install tmux        # Debian/Ubuntu\n" +
			"  sudo dnf install tmux        # Fedora\n" +
			"  sudo pacman -S tmux          # Arch"
	default:
		return fmt.Sprintf("  See https://github.com/tmux/tmux/wiki/Installing for %s", runtime.GOOS)
	}
}

// DaemonIssue wraps a daemon-connection error in an Issue suitable for
// display on the same blocking screen used for missing-software failures.
func DaemonIssue(err error) *Issue {
	return &Issue{
		Title: "Cannot connect to the bossd daemon",
		Detail: fmt.Sprintf("%v\n\n", err) +
			"Try one of:\n\n" +
			"  boss daemon install   # set up automatic startup\n" +
			"  bossd                 # start it manually in another terminal",
	}
}
