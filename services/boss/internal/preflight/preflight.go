// Package preflight runs startup checks for required external software so
// the boss TUI can fail with a friendly blocking message instead of a stack
// trace once a feature that needs the missing tool is exercised.
package preflight

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
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

// CheckShellTools verifies that bash and tee are on PATH. The daemon
// wraps log-tailed agent launches with `bash -c "set -o pipefail;
// <inner> 2>&1 | tee <log>"` so cron and repair runs persist their
// transcript to disk for the daemon to follow. A missing tee or bash
// kills the tmux pane the instant it starts and the boss attach UI
// just flashes back to the chat list — surface the dependency up
// front so the user sees a real error instead of an unexplained flash.
func CheckShellTools() *Issue {
	missing := make([]string, 0, 2)
	for _, bin := range []string{"bash", "tee"} {
		if _, err := exec.LookPath(bin); err != nil {
			missing = append(missing, bin)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	title := missing[0] + " is not installed"
	if len(missing) > 1 {
		title = "bash and tee are not installed"
	}
	return &Issue{
		Title: title,
		Detail: "Boss runs log-tailed agent launches through `bash | tee` so cron " +
			"and repair runs persist their tmux pane output to disk. Install " +
			"the missing tool(s) and restart boss:\n\n" + shellToolsInstallHint(missing),
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

func shellToolsInstallHint(missing []string) string {
	pkgs := map[string]struct {
		darwin string
		debian string
		fedora string
		arch   string
	}{
		"bash": {darwin: "bash", debian: "bash", fedora: "bash", arch: "bash"},
		"tee":  {darwin: "coreutils", debian: "coreutils", fedora: "coreutils", arch: "coreutils"},
	}
	seen := map[string]bool{}
	var darwinPkgs, debianPkgs, fedoraPkgs, archPkgs []string
	for _, m := range missing {
		p, ok := pkgs[m]
		if !ok {
			continue
		}
		if !seen["darwin:"+p.darwin] {
			darwinPkgs = append(darwinPkgs, p.darwin)
			seen["darwin:"+p.darwin] = true
		}
		if !seen["debian:"+p.debian] {
			debianPkgs = append(debianPkgs, p.debian)
			seen["debian:"+p.debian] = true
		}
		if !seen["fedora:"+p.fedora] {
			fedoraPkgs = append(fedoraPkgs, p.fedora)
			seen["fedora:"+p.fedora] = true
		}
		if !seen["arch:"+p.arch] {
			archPkgs = append(archPkgs, p.arch)
			seen["arch:"+p.arch] = true
		}
	}
	switch runtime.GOOS {
	case "darwin":
		return fmt.Sprintf("  brew install %s", strings.Join(darwinPkgs, " "))
	case "linux":
		return fmt.Sprintf("  sudo apt install %s        # Debian/Ubuntu\n", strings.Join(debianPkgs, " ")) +
			fmt.Sprintf("  sudo dnf install %s        # Fedora\n", strings.Join(fedoraPkgs, " ")) +
			fmt.Sprintf("  sudo pacman -S %s          # Arch", strings.Join(archPkgs, " "))
	default:
		return fmt.Sprintf("  Install the missing tools (%s) for %s.", strings.Join(missing, ", "), runtime.GOOS)
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
