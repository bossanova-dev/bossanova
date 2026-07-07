// Package preflight runs startup checks for required external software so
// the boss TUI can fail with a friendly blocking message instead of a stack
// trace once a feature that needs the missing tool is exercised.
package preflight

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/recurser/bossalib/loginshell"
)

const agentResolveTimeout = 5 * time.Second

var safeAgentCommandPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

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

// CheckAgentResolvable verifies the agent CLI resolves through the user's
// login shell in the worktree, mirroring how the daemon launches it.
func CheckAgentResolvable(shell, agent, worktree string) *Issue {
	return checkAgentResolvable(shell, agent, worktree, runShell)
}

// checkAgentResolvable verifies the agent CLI resolves through the user's login
// shell in the worktree, mirroring how the daemon launches it. runShell is
// injected for testability; production passes runShell which execs a
// shell-specific `command -v <agent>` probe with cwd=worktree for supported
// shells.
func checkAgentResolvable(shell, agent, worktree string, run func(shell, worktree, line string) error) *Issue {
	if !safeAgentCommandPattern.MatchString(agent) {
		return &Issue{
			Title: agent + " is not a valid agent command",
			Detail: fmt.Sprintf(
				"Boss cannot check the enabled agent provider %q because its command name "+
					"is not safe to pass to shell preflight. Use only letters, numbers, dot, "+
					"underscore, and hyphen, starting with a letter or number.",
				agent),
		}
	}
	if shell == "" {
		return nil
	}
	if !loginshell.IsSupported(shell) {
		return nil
	}
	// `command -v` probes the same login shell + worktree the daemon launches
	// through, so it catches a missing PATH/shim binary up front. It assumes the
	// agent resolves as a PATH/shim entry (how every supported agent ships); an
	// agent provided purely as a shell function or alias could diverge from the
	// `exec`-based launch wrap, but none do today.
	if err := run(shell, worktree, "command -v "+agent); err != nil {
		return agentNotFoundIssue(shell, agent, worktree)
	}
	return nil
}

func agentNotFoundIssue(shell, agent, worktree string) *Issue {
	flags := strings.Join(loginshell.Flags(shell), " ")
	line := loginshell.CommandLine(shell, "command -v "+agent)
	return &Issue{
		Title: agent + " was not found for this project",
		Detail: fmt.Sprintf(
			"Boss launches %s through your login shell (%s) in the worktree, but it "+
				"could not be resolved there. If you use a per-project version manager "+
				"(nodenv/asdf/mise) or a project-local install, make sure `%s` works when "+
				"you run it in:\n\n    %s\n\n(checked: %s %s %q)",
			agent, shell, agent, worktree, shell, flags, line),
	}
}

func runShell(shell, worktree, line string) error {
	return runShellWithTimeout(shell, worktree, line, agentResolveTimeout)
}

func runShellWithTimeout(shell, worktree, line string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	err := runShellContext(ctx, shell, worktree, line)
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("shell command timed out after %s: %w", timeout, ctx.Err())
	}
	return err
}

func runShellContext(ctx context.Context, shell, worktree, line string) error {
	args := append(loginshell.Flags(shell), loginshell.CommandLine(shell, line))
	cmd := exec.CommandContext(ctx, shell, args...)
	cmd.Dir = worktree
	return cmd.Run()
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
			daemonRemediationDetail(),
	}
}

func daemonRemediationDetail() string {
	return "Try:\n\n" +
		"  boss daemon restart   # start or restart the daemon\n" +
		"  boss daemon status    # inspect daemon health\n" +
		"  boss daemon install   # set up automatic startup\n" +
		"  bossd                 # start the daemon manually"
}
