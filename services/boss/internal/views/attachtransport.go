package views

// This file owns everything about *reaching* a chat pane: the command boss
// hands the terminal to, and the liveness probe that decides whether handing it
// over is worth doing. Split from attach.go, which is the Bubble Tea model, so
// the ssh-transport concern reads as one thing and attach.go stays a view.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// attachCommand is the process boss hands the terminal to for a chat pane: the
// argv to run, and the working directory to run it in.
type attachCommand struct {
	argv []string
	dir  string
}

// buildAttachCommand returns the command that puts the user inside a chat's tmux
// session.
//
// Locally that is tmux itself, run in the session's worktree — unchanged. When
// boss is driving a daemon on another machine the tmux server is over there, so
// boss runs the same tmux command *there*, over ssh:
//
//   - -t forces a TTY. Without it ssh runs the remote command with no pty and
//     tmux refuses to attach ("open terminal failed").
//   - tmux -u declares the terminal UTF-8 to the tmux CLIENT, which is the end
//     that decides this — and under --host that client runs on the far side of
//     an ssh connection, exactly the environment where BOS-658 traced
//     box-drawing characters rendering as underscores to a client whose locale
//     disagreed with the terminal.
//   - the remote tmux runs through the remote LOGIN shell, via
//     remoteTmuxCommand. Without that, an attach to a host whose tmux is not on
//     sshd's reduced PATH fails with `tmux: command not found`.
//   - the session name is shell-quoted for the remote form only, and quoted
//     twice over because remoteTmuxCommand introduces a second shell parse —
//     see its doc. Unlike a local exec, ssh concatenates its trailing arguments
//     and hands the result to the remote user's shell, so a separate local argv
//     element is not a separate remote word. Today's names are `boss-<8>-<8>`
//     from the daemon's ChatSessionName and quote to themselves, but the value
//     arrives over the wire from another machine and this is the seam where it
//     would stop being data. The local form is left unquoted: exec.Command
//     passes argv straight through with no shell to defend against.
//   - dir stays EMPTY for the remote case. The worktree path came from the
//     remote daemon and names a directory on that machine; using it as a local
//     exec.Cmd.Dir makes the process fail to start with a confusing chdir
//     error. The working directory that matters belongs to the remote tmux
//     pane, which the remote daemon already established when it created the
//     session.
//
// No BatchMode here, unlike the liveness probe: this command owns the terminal,
// so an ssh key passphrase prompt is something the user can actually answer.
func buildAttachCommand(destination, tmuxName, worktreePath string) attachCommand {
	if destination == "" {
		return attachCommand{argv: []string{"tmux", "attach", "-t", tmuxName}, dir: worktreePath}
	}
	return attachCommand{argv: []string{
		"ssh", "-t",
		// -e none disables ssh's own escape character. With a pty allocated, ssh
		// scans every line the user types for a leading "~": `~.` silently kills
		// the pane, `~^Z` suspends it, `~C`/`~#` splice ssh's UI over the agent's,
		// and `~~` collapses to one tilde — which quietly corrupts any pasted line
		// starting with one, including a markdown `~~~` fence and the plan
		// prefillClaudeInput pastes down this same pty. The user detaches with
		// Ctrl+X (bosspty, local stdin) or tmux's own prefix, so nothing here
		// needs ssh's escapes and everything here is hurt by them.
		"-e", "none",
		// The same keepalives internal/remotehost's tunnel sets, for the reason
		// it documents: a half-open TCP connection (laptop suspended, wifi
		// switched) leaves ssh looking alive while nothing flows, and without
		// these the drop only surfaces when the OS gives up minutes later. That
		// matters more here than for the tunnel: a hung attach leaves the pane
		// frozen with no eject, and bosspty's manager hands the same wedged
		// process back on every later attach because its done channel never
		// closes. 5s x 3 misses ejects the user in ~15s instead.
		"-o", fmt.Sprintf("ServerAliveInterval=%d", attachServerAliveInterval),
		"-o", fmt.Sprintf("ServerAliveCountMax=%d", attachServerAliveCountMax),
		destination, remoteTmuxCommand("tmux -u attach -t " + shellQuote(tmuxName)),
	}}
}

// remoteTmuxCommand renders a tmux invocation as the single remote command
// string ssh sends, routed through the remote user's LOGIN shell.
//
// `ssh host tmux …` runs the command in a NON-login shell. A remote whose tmux
// is not on that reduced PATH then fails every attach with `tmux: command not
// found` — canonically Homebrew's /opt/homebrew/bin on Apple Silicon, which
// ~/.zprofile exports and which zsh does NOT read for a non-interactive shell.
// `$SHELL -l` gives the command the environment an interactive login gets,
// which is the one tmux was installed into and the one bossd's own session
// already runs in.
//
// Spelled with a bare "$SHELL" deliberately. The defensive-looking POSIX
// default-value form ${SHELL:-/bin/sh} is a syntax ERROR in fish — `${ is not a
// valid variable in fish`, exit 127 — and fish is a real remote login shell, so
// that spelling breaks a host that works today while guarding against nothing:
// sshd sets SHELL from the passwd entry.
//
// The result is deliberately ONE argv element. ssh concatenates its trailing
// arguments and hands the string to the remote shell, so `-lc`'s operand has to
// survive as a single word — which is also what keeps a session name carrying
// shell syntax as data across BOTH parses: the outer shell unwraps this
// quoting, and the inner `$SHELL -lc` unwraps the layer buildAttachCommand and
// tmuxProbeCommandLine already applied to the name.
func remoteTmuxCommand(inner string) string {
	return `exec "$SHELL" -lc ` + shellQuote(inner)
}

// attachServerAliveInterval/attachServerAliveCountMax mirror the values
// internal/remotehost's tunnel uses, so the two ssh connections a --host session
// holds notice a dead link on the same schedule.
const (
	attachServerAliveInterval = 5
	attachServerAliveCountMax = 3
)

// buildAttachReproArgv is the copy-pasteable form of the attach command, for the
// error screen.
//
// It keeps everything that decides WHERE the command runs — the transport, the
// destination, the tmux invocation and its session — and drops the tuning
// options buildAttachCommand adds, because none of them can cause the failure
// this diagnostic exists to explain (a missing terminfo entry) and all of them
// cost line width. The full form runs ~140 columns, which soft-wraps out of the
// fixed-width block whose entire job is to be copied verbatim; this is ~90 and
// still reproduces the failure on the right machine.
//
// The LOCAL form leaves the name unquoted: that argv is never executed, only
// handed to shellJoin, which quotes every element for display, and quoting twice
// would print a session name with literal quotes in it.
//
// The REMOTE form must carry remoteTmuxCommand's wrapper verbatim, quoting and
// all. It is not decoration: the wrapper is what makes tmux resolve on a host
// whose tmux is off sshd's PATH, so a plain `ssh <dest> tmux …` here would print
// a command that fails where the attach it claims to reproduce succeeded — and
// on exactly the misconfigured host most likely to be reading this screen.
// shellJoin then quotes the whole wrapper as one element, which is correct: the
// rendered line pastes back into the user's shell as the command that ran.
//
// The result must stay a subsequence of buildAttachCommand's argv — that is what
// makes it a reproduction rather than a second, drifting spelling of the attach
// — and a test asserts it.
func buildAttachReproArgv(destination, tmuxName string) []string {
	if destination == "" {
		return []string{"tmux", "attach", "-t", tmuxName}
	}
	return []string{
		"ssh", "-t",
		destination, remoteTmuxCommand("tmux -u attach -t " + shellQuote(tmuxName)),
	}
}

// tmuxProbeTimeout bounds the remote liveness probe. The probe runs on the
// detach path with a live TUI waiting on it, so an ssh that hangs (an
// unreachable host that never RSTs, a stalled TCP connect) must not hang boss —
// after the timeout the probe reports tmuxProbeUnreachable, which is explicitly
// NOT "the session is gone".
const tmuxProbeTimeout = 8 * time.Second

// tmuxProbeWaitDelay bounds cmd.Wait once the probe's ssh is gone. Mirrors
// internal/remotehost's waitDelay, for the same reason it exists there: Wait
// otherwise blocks until every process inheriting the stderr pipe closes it,
// which a grandchild of a killed ssh may not do promptly.
const tmuxProbeWaitDelay = 2 * time.Second

// tmuxNoSuchSessionExitCode is the only status a remote probe can return that
// is an ANSWER rather than a failure to ask: `tmux has-session` exits 0 when the
// session is there and 1 when a reachable server holds no session by that name,
// and nothing else. Every other status belongs to something between boss and
// that server — 255 is OpenSSH's own (name resolution, connection refused,
// host-key mismatch, a credential BatchMode cannot obtain), 127 and 126 are the
// remote shell failing to find or run tmux at all (remoteTmuxCommand routes
// through the login shell so a Homebrew tmux resolves, but a host that has no
// tmux, or whose login shell cannot run the wrapper, still lands here), and -1
// is a locally signal-killed ssh. None
// of them learned anything about the session, so the classifier enumerates this
// one value and treats everything else as unreachable.
const tmuxNoSuchSessionExitCode = 1

// newTmuxProbeCommand builds the process the liveness probe runs. A package var
// so tests can assert the argv of both the local and the remote form without an
// ssh binary or a running tmux server. Mirrors the injected command factory in
// services/bossd/internal/tmux.
var newTmuxProbeCommand = exec.CommandContext

// tmuxProbeResult is what a liveness probe learned about a tmux session. It has
// three values rather than two because, remotely, "no" and "I could not ask" are
// different answers with opposite consequences: the caller that would show
// errChatSessionGone must not tell a user their live remote chat is gone (and
// invite them to start a duplicate) merely because ssh could not connect.
type tmuxProbeResult int

const (
	// tmuxProbeGone: a tmux server answered and has no session by that name.
	tmuxProbeGone tmuxProbeResult = iota
	// tmuxProbeAlive: the session exists.
	tmuxProbeAlive
	// tmuxProbeUnreachable: nothing answered about the tmux server — ssh failed
	// on its own account, or the bound expired. Remote-only by construction.
	tmuxProbeUnreachable
)

// probeTmuxSession asks whether a tmux session by the given name is still
// running. Used by the attach flow to distinguish a Ctrl+X / Ctrl+] detach
// (session still alive on the tmux server, claude still running inside) from
// a real claude exit (session torn down by tmux when its only window's
// command exited), and to decide whether a resume-attach should show
// errChatSessionGone instead of handing the terminal to a doomed tmux attach.
//
// Under --host it must answer about the REMOTE tmux server: a local probe would
// report every remote chat as gone.
//
// The local classification is deliberately unchanged: `tmux has-session` either
// answers or the tmux binary is missing, and both have always meant "gone", so
// a local probe never returns tmuxProbeUnreachable.
func probeTmuxSession(name string) tmuxProbeResult {
	if name == "" {
		return tmuxProbeGone
	}
	destination := remoteHostDestination()
	argv, timeout := tmuxProbeCommandLine(destination, name)
	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := newTmuxProbeCommand(ctx, argv[0], argv[1:]...)
	var stderr bytes.Buffer
	if destination != "" {
		// Remote only, so the local probe keeps the exact process shape it
		// always had: a nil Stderr goes to the null device, with no pipe and no
		// copying goroutine for a WaitDelay to have to bound.
		//
		// Captured rather than inherited because the probe runs behind a live
		// TUI — ssh's complaints must not land on the frame — and because they
		// are the only thing that distinguishes "tmux is not on the remote
		// PATH" from a real answer on a shell that reports a status of its own
		// choosing. Set over anything an injected factory supplied: the
		// classification below reads this buffer and nothing else.
		cmd.Stderr = &stderr
		// Capturing stderr is what makes WaitDelay necessary. os/exec gives a
		// non-*os.File Stderr a pipe and a copying goroutine, and Run then
		// blocks until every process holding the write end closes it — which a
		// grandchild of the ssh the context just killed (a ProxyCommand, a
		// multiplexer) need not do. Without this the 8 s bound is aspirational,
		// and this probe runs synchronously on the Update loop and inside the
		// tea.Exec callback, so an unbounded Run wedges the whole TUI. Same
		// hazard, same remedy, as internal/remotehost's tunnel.
		cmd.WaitDelay = tmuxProbeWaitDelay
	}
	return classifyTmuxProbe(destination, cmd.Run(), ctx.Err(), stderr.Bytes())
}

// classifyTmuxProbe maps a probe's outcome to a result. Split out from
// probeTmuxSession so every branch — including the expired bound, which a test
// cannot reach through the real 8 s timeout — is assertable as a pure function.
//
// Only a remote probe that reached tmux and was told "no such session" may
// answer tmuxProbeGone. Everything else about a remote run — ssh's own failure,
// an expired bound, a shell that could not find or run tmux — is unreachable,
// because none of it is evidence about the session.
func classifyTmuxProbe(destination string, runErr, ctxErr error, stderr []byte) tmuxProbeResult {
	if runErr == nil {
		return tmuxProbeAlive
	}
	if destination == "" {
		return tmuxProbeGone
	}
	if ctxErr != nil {
		// The bound expired: the host is not answering, which says nothing at
		// all about the session.
		return tmuxProbeUnreachable
	}
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) {
		// ssh could not be started at all.
		return tmuxProbeUnreachable
	}
	// Enumerate the ONE status that is an answer, not the many that are not.
	// Defaulting to gone reads every unanticipated status as "your chat is
	// gone" — including ExitCode() == -1, which is what a locally
	// signal-killed ssh reports (SIGHUP on a closing terminal, SIGTERM, the
	// OOM killer), none of which learned anything about the remote session.
	// tmux has-session exits 0 or 1 and nothing else, so this loses no real
	// answer.
	if exitErr.ExitCode() == tmuxNoSuchSessionExitCode && !remoteCommandMissing(stderr) {
		return tmuxProbeGone
	}
	return tmuxProbeUnreachable
}

// remoteCommandMissing reports whether the remote shell said it could not find
// tmux, for the shells and ssh wrappers that report a status other than 127.
// Follows internal/remotehost's isCommandNotFound, but requires the phrase to
// name tmux: `ssh host cmd` still sources ~/.bashrc, and a guarded pyenv/nvm
// init that prints "bash: line 1: pyenv: command not found" would otherwise make
// a genuinely dead session read as unreachable and stop the resume guard from
// ever firing for that user. The three shell spellings below are the whole
// vocabulary; anything else is caught by the 127/126 exit codes, which no
// rc-file noise can forge.
func remoteCommandMissing(stderr []byte) bool {
	msg := string(stderr)
	for _, phrase := range []string{
		"tmux: command not found", // bash
		"command not found: tmux", // zsh
		"tmux: not found",         // dash / ash
	} {
		if strings.Contains(msg, phrase) {
			return true
		}
	}
	return false
}

// tmuxSessionAlive reports whether the session is positively known to be
// running. Unreachable counts as not-alive here: the detach path uses it to
// decide whether the user is still holding a live chat, and an unanswerable
// probe is no evidence that they are.
func tmuxSessionAlive(name string) bool {
	return probeTmuxSession(name) == tmuxProbeAlive
}

// detachedAfterExec decides whether the pane the user just left is a detach
// (the chat is still running, drop back to the caller's view) rather than an
// exit. Two signals, and only the first is sufficient alone:
//
//   - intercepted: bosspty saw Ctrl+X / Ctrl+]. This is read from the LOCAL
//     stdin filter, so it is transport-independent — an ssh between the pty and
//     tmux changes nothing — and it short-circuits, which is what keeps an
//     ordinary --host detach free of a remote round trip. Under --host that
//     matters twice over: no probe means nothing to fail, so the detach lands
//     back in the local TUI while the remote tmux session keeps running.
//   - the pane exited cleanly AND the session is still there. The chat survives
//     even when bosspty did not see the keystroke (the user typed tmux's own
//     detach prefix, or used its menu), so this is the fallback — and only the
//     fallback.
//
// A live session is NOT sufficient on its own, which is what this function used
// to assume. bossd creates the tmux SESSION; the tmux CLIENT is the only part
// that dies when a --host attach fails during startup, so the session is
// genuinely alive in both cases and the verdict then short-circuits the entire
// error path in the agentFinishedMsg handler — swallowing the launch diagnostic
// written for exactly that failure. A live session proves the work survived,
// not that the user was ever attached (BOS-728).
//
// exitErr is the conjunct that separates them, chosen over the alternatives
// because it is exact rather than approximate and needs no new state: a tmux
// client that detaches (its own prefix, or its menu) exits 0, and ssh
// propagates the remote status verbatim, so a clean exit is positive evidence
// the client ran far enough to be detached FROM. A client that never started
// exits non-zero. The two rejected candidates: bosspty exposes no
// "pane was entered" signal (PTYCommand.ProcessExited is true for a detaching
// client and a failing one alike), and an elapsed-time threshold cannot
// separate "ssh connect + instant failure" from "ssh connect + human reaction"
// on a slow link.
//
// The ordering also biases correctly. Non-nil exitErr short-circuits ahead of
// the probe, so a failed pane pays no remote round trip either; and the
// genuinely ambiguous case — a clean exit with a live session — still reads as
// a detach, because holding an error screen after an ordinary detach is a worse
// outcome than the bug this conjunct fixes.
//
// Split out of the tea.Exec callback purely so the short-circuit is assertable:
// inside the closure there is no way to prove a remote probe did not run.
func detachedAfterExec(intercepted bool, exitErr error, tmuxName string) bool {
	return intercepted || (exitErr == nil && tmuxSessionAlive(tmuxName))
}

// tmuxProbeCommandLine returns the liveness probe for a tmux server, plus the
// timeout it should be bounded by (0 = none).
//
// The local form is left exactly as it was — an unbounded local `tmux
// has-session` against a unix socket answers immediately or not at all, and
// bounding it would change local behaviour for no benefit. The remote form
// crosses a network, so it is both bounded and run with BatchMode=yes: a
// credential prompt behind a live TUI has no visible terminal to prompt on, so
// failing fast is the only honest outcome.
func tmuxProbeCommandLine(destination, name string) (argv []string, timeout time.Duration) {
	if destination == "" {
		return []string{"tmux", "has-session", "-t", name}, 0
	}
	// Routed through the login shell exactly like the attach: if the two
	// disagreed about how tmux resolves, the probe would report a live session
	// unreachable (or vice versa) purely because it looked somewhere else.
	return []string{
		"ssh", "-o", "BatchMode=yes",
		destination, remoteTmuxCommand("tmux has-session -t " + shellQuote(name)),
	}, tmuxProbeTimeout
}
