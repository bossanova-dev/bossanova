package views

import (
	"fmt"
	"strings"
)

// This file owns the single switch that tells the TUI it is driving a daemon on
// another machine.
//
// Boss reaches a remote daemon through an ssh-forwarded socket (`boss --host`),
// so every request-response RPC answers about the remote host while anything the
// TUI does with `exec` or the filesystem still happens locally. The two are easy
// to confuse, and the confusions are silent: a worktree path from the daemon
// names a directory that does not exist here, and a `tmux has-session` run here
// answers about the wrong machine. Every site that touches the local machine
// consults isRemoteHost/remoteHostDestination rather than re-deriving the
// context from a flag, so there is exactly one thing to set in a test and
// exactly one thing to grep for when adding a new local-machine call.

// hostDestination is the --host ssh destination when boss is driving a daemon
// on another machine through a tunnel, and "" for the ordinary local daemon. It
// decides what a lost connection tells the user to do (`boss daemon restart`
// restarts the *local* daemon, which is never the one a --host session lost),
// how boss attaches to a chat pane, and which local-filesystem affordances are
// offered at all.
var hostDestination string

// SetHostDestination records the --host ssh destination so a dropped tunnel
// renders as "Reconnecting to <destination>" with ssh remediation instead of
// local-daemon advice, and so attach reaches the remote machine's tmux server.
// Called once during --host startup, before the TUI runs; "" leaves the ordinary
// local behaviour in place.
func SetHostDestination(destination string) { hostDestination = destination }

// hostControlPath is the multiplexing socket mastered by the --host tunnel, or
// "" when there is none (local daemon, Windows, or a temp path too long to
// bind). Uploads pass it to ssh so a paste rides the tunnel's already
// authenticated connection — see remotehost.Tunnel.ControlPath.
//
// It is separate from hostDestination rather than folded into it because the
// two answer different questions and can disagree: a remote session always has
// a destination, and may still have no control path at all.
var hostControlPath string

// SetHostControlPath records the tunnel's multiplexing socket so paste uploads
// authenticate against nothing instead of prompting under BatchMode, which is
// what makes pasting work on password or uncached-passphrase auth. Called once
// during --host startup, before the TUI runs; "" leaves uploads opening their
// own ssh connection, exactly as they did before multiplexing existed.
func SetHostControlPath(controlPath string) { hostControlPath = controlPath }

// remoteHostDestination returns the ssh destination of the remote daemon boss is
// driving, or "" when the daemon is local. Callers that need to build an ssh
// invocation use this; callers that only need to know whether they are remote
// use isRemoteHost.
func remoteHostDestination() string { return hostDestination }

// remoteHostControlPath returns the tunnel's multiplexing socket, or "" when
// uploads must open their own ssh connection.
func remoteHostControlPath() string { return hostControlPath }

// isRemoteHost reports whether boss is driving a daemon on another machine, in
// which case the session's worktree, transcripts, and tmux server all live over
// there and no local-filesystem shortcut may be taken on their behalf.
func isRemoteHost() bool { return hostDestination != "" }

// hostTunnelReason reads the ssh supervisor's last classified failure, or nil
// when boss is local or nothing registered a reader. It is a function rather
// than a value because the answer changes while the TUI is on screen: the
// supervisor keeps redialling, and each attempt reclassifies why it failed.
//
// It is set from cmd/host.go alongside SetHostDestination and reads only tunnel
// errors — the daemon token lives beside the tunnel and must never reach a
// rendered string.
var hostTunnelReason func() string

// SetHostTunnelReason registers the reader for why the --host ssh tunnel is
// down, so a lost-connection screen can report the supervisor's own diagnosis
// instead of only the fact of the outage. Passing nil (or never calling it)
// leaves every affordance working without a reason line.
func SetHostTunnelReason(reason func() string) { hostTunnelReason = reason }

// hostSupervisorRunning reads whether the goroutine redialling the --host tunnel
// is still running, or nil when boss is local or nothing registered a reader. It
// is the second half of what cmd/host.go learned by consuming Start's done
// channel: the reason seam says *why* the last redial failed, this one says
// whether a redial is still coming at all.
//
// It must NOT be fed a "is the forward usable right now" answer. That is false
// for the whole backoff window of an ordinary drop — precisely when something is
// still redialling — so wiring it here would tell the user to restart boss on
// every blip. cmd/host.go reads only the done channel for exactly this reason.
var hostSupervisorRunning func() bool

// SetHostSupervisorRunning registers the reader for whether the --host tunnel is
// still supervised, so a lost-connection screen does not promise an automatic
// recovery that nothing is working towards. Passing nil (or never calling it)
// leaves every affordance reading as it did.
func SetHostSupervisorRunning(running func() bool) { hostSupervisorRunning = running }

// remoteHostSupervisorStopped reports whether the supervisor that redials the
// tunnel has unwound, in which case no reconnect is coming.
//
// It answers false when no reader is registered. Claiming "nobody is redialling"
// is a strictly worse error than staying quiet about it: the screen would tell a
// user to restart boss while the supervisor was in fact mid-backoff and about to
// recover on its own.
func remoteHostSupervisorStopped() bool {
	if hostSupervisorRunning == nil {
		return false
	}
	return !hostSupervisorRunning()
}

// remoteHostFailureReason returns the supervisor's last classified failure as
// display text, or "" when there is none.
func remoteHostFailureReason() string {
	if hostTunnelReason == nil {
		return ""
	}
	return strings.TrimSpace(hostTunnelReason())
}

// hostDialAffordance returns what a view must render in place of err, and
// whether err is a dropped --host tunnel rather than something the remote
// daemon said.
//
// Under --host every RPC goes through the ssh-forwarded unix socket, so a
// dropped tunnel fails each one with `dial unix /var/folders/…/bossd.sock:
// connect: no such file or directory` — a local temp path the user cannot act
// on, from a machine that is not the one that stopped answering. Rendering it
// verbatim is what made a dead tunnel look like a frozen session (BOS-724).
//
// Views reach this through rpcErrorMessage rather than calling it directly, so
// there is one thing to set in a test and one thing to grep for when a new error
// surface is added.
func hostDialAffordance(err error) (string, bool) {
	if !isRemoteHost() || !isForwardedSocketDial(err) {
		return "", false
	}
	return hostDownRemediation(remoteHostDestination()), true
}

// rpcErrorMessage is what a view renders in place of a failed request: the
// reconnecting affordance when a --host tunnel has dropped, and the error itself
// in every other case, local runs included.
//
// Acceptance criterion 2 is worded for *any* --host view, and there are fifteen
// of them — every list, form, and settings screen reports a daemon RPC failure
// the same way. Fixing only the three the ticket named by hand would leave the
// other twelve rendering `dial unix …` the moment a user opened Repos or Trash,
// so the substitution lives here and each call site only asks for a message.
func rpcErrorMessage(err error) string {
	if affordance, ok := hostDialAffordance(err); ok {
		return affordance
	}
	return fmt.Sprintf("Error: %v", err)
}

// rpcStatusDetail is the single-line form of the same substitution, for the
// transient status lines an action failure writes ("Delete failed: …").
//
// A status line is one line inside a live screen, so it cannot carry
// hostDownRemediation's multi-line ssh block; it says the same thing in a
// sentence. Without it, converting only the full-screen error views left every
// action failure under --host reading "Delete failed: unavailable: dial unix
// /var/folders/…/bossd.sock: connect: no such file or directory" — the same
// string on a different surface.
func rpcStatusDetail(err error) string {
	if !isRemoteHost() || !isForwardedSocketDial(err) {
		return fmt.Sprintf("%v", err)
	}
	// Kept in step with the full-screen view: one surface must not promise a
	// reconnect while the other says nothing is redialling.
	detail := "the connection to " + remoteHostDestination() + " dropped; boss is reconnecting"
	if remoteHostSupervisorStopped() {
		detail = "the connection to " + remoteHostDestination() + " dropped and boss is not redialling; restart boss"
	}
	if reason := remoteHostFailureReason(); reason != "" {
		detail += " (last tunnel failure: " + reason + ")"
	}
	return detail
}

// rpcStatusMessage prefixes rpcStatusDetail with the operation that failed, so
// the user still learns *what* did not happen as well as why.
func rpcStatusMessage(prefix string, err error) string {
	return prefix + ": " + rpcStatusDetail(err)
}

// isForwardedSocketDial reports whether err is a failure to reach the forwarded
// socket rather than an answer from the daemon behind it. The match is
// deliberately narrow: an error the remote daemon actually returned ("session
// not found") is the most useful thing on screen and must not be replaced by a
// reconnecting screen that is not true.
func isForwardedSocketDial(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "dial unix")
}
