package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"github.com/recurser/boss/internal/client"
	"github.com/recurser/boss/internal/preflight"
	"github.com/recurser/boss/internal/remotehost"
	"github.com/recurser/boss/internal/views"
)

// hostDiscoverTimeout bounds the two one-shot ssh calls (socket discovery and
// the token read). It is generous because a first connection can pay for agent
// or ProxyJump round-trips, but finite so a wedged ssh cannot hang boss.
const hostDiscoverTimeout = 60 * time.Second

// hostTunnelReadyTimeout bounds how long the forwarded socket has to accept a
// dial before startup gives up. A var so tests need not wait it out.
var hostTunnelReadyTimeout = 60 * time.Second

// hostTunnel is the slice of *remotehost.Tunnel the cmd layer depends on. It is
// an interface so the startup path can be tested without spawning ssh.
type hostTunnel interface {
	LocalSocket() string
	ControlPath() string
	Healthy() bool
	// LastFailure is the supervisor's own diagnosis of the most recent ssh
	// exit, or nil. It is what a dropped-tunnel screen reports instead of the
	// dial error against the forwarded socket (BOS-724).
	LastFailure() error
	Close() error
}

// supervisedTunnel pairs a tunnel with the done channel Tunnel.Start returns, so
// a supervisor goroutine that has exited is observable rather than silently
// ignored. Without it the underlying Tunnel keeps answering from its own state:
// nothing distinguishes "redialling" from "nobody is redialling any more", which
// is exactly the silence BOS-724 was filed for.
type supervisedTunnel struct {
	hostTunnel
	done <-chan struct{}
}

// Healthy reports false as soon as the supervisor unwinds, whatever the wrapped
// tunnel still claims: with no supervisor there is no redial coming, so a poll
// loop that kept waiting would wait forever.
func (s *supervisedTunnel) Healthy() bool {
	return s.supervisorRunning() && s.hostTunnel.Healthy()
}

// supervisorRunning reports only whether the goroutine that redials is still
// there. It is deliberately NOT Healthy(): Healthy means "the forward is usable
// right now", which is false for the whole backoff window of an ordinary drop —
// the window in which something is very much still redialling. A screen that
// conflated the two would tell the user to restart boss on every blip, which is
// the inverse of what this ticket is for.
func (s *supervisedTunnel) supervisorRunning() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// supervisorAware is implemented by a transport that knows whether its own
// supervisor is still running. Kept off hostTunnel because the wrapped
// *remotehost.Tunnel cannot answer it — only the wrapper holding Start's done
// channel can.
type supervisorAware interface {
	supervisorRunning() bool
}

// hostConnection is the live --host transport: the supervised ssh tunnel plus
// the remote daemon's auth token. The token is held in memory only — it is
// never written to disk, never logged, and never spliced into an error.
type hostConnection struct {
	destination string
	tunnel      hostTunnel
	token       string
}

// The process holds at most one --host transport; run() tears it down on exit
// so both the TUI and a one-shot CLI command clean up their ssh child and its
// private directory.
var (
	hostConnMu sync.Mutex
	hostConn   *hostConnection
)

// hostCommandFactory overrides how remotehost spawns ssh. nil means the real
// exec; tests inject a fake so no test needs a second machine.
var hostCommandFactory remotehost.CommandFactory

// hostTunnelStarter builds, starts, and waits for the forward. A seam because
// the real one spawns ssh and binds a unix socket.
var hostTunnelStarter = startHostTunnel

// The two blocking screens, behind vars so the startup decision can be asserted
// without driving a real terminal.
var (
	showHostPreflight     = views.RunPreflight
	showHostReconnectWait = runHostReconnectWait
)

// requireSingleTransport rejects --host together with --remote. They select
// different daemons entirely — an ssh tunnel to another machine's bossd versus
// the cloud orchestrator — so silently preferring one would drive the wrong
// machine. It runs at the top of the root's PersistentPreRunE, before any dial.
func requireSingleTransport(cmd *cobra.Command) error {
	if hostDestination(cmd) != "" && remoteURL(cmd) != "" {
		return errors.New("--host and --remote select different daemons and cannot be combined; pass only one")
	}
	return nil
}

// newHostClient drives a bossd on another machine: it discovers that daemon's
// socket, reads its auth token over the same ssh destination, forwards the
// socket to a local one, and returns an ordinary local client pointed at the
// forward.
//
// It deliberately never calls daemonEnsureRunning. The user asked for the
// daemon on `host`; booting a local one here would silently drive the wrong
// machine and mask the real failure.
func newHostClient(cmd *cobra.Command, host string) (client.BossClient, error) {
	opts := remotehost.Options{
		Destination:    host,
		RemoteSocket:   hostSocketOverride(cmd),
		CommandFactory: hostCommandFactory,
	}

	// The tunnel must outlive discovery, so only the two one-shot ssh calls get
	// the bounded context; the supervisor gets the command's own.
	baseCtx := hostCommandContext(cmd)
	discoverCtx, cancel := context.WithTimeout(baseCtx, hostDiscoverTimeout)
	defer cancel()

	remoteSocket, err := remotehost.DiscoverSocket(discoverCtx, opts)
	if err != nil {
		return nil, hostError(host, err)
	}
	token, err := remotehost.FetchToken(discoverCtx, opts, remoteSocket)
	if err != nil {
		return nil, hostError(host, err)
	}

	tunnel, err := hostTunnelStarter(baseCtx, remotehost.TunnelConfig{
		Options:      opts,
		RemoteSocket: remoteSocket,
		Logger:       log.Logger,
	})
	if err != nil {
		return nil, hostError(host, err)
	}
	registerHostConnection(&hostConnection{destination: host, tunnel: tunnel, token: token})
	// A tunnel that drops *after* startup surfaces on the home screen's
	// daemon-down view, not the startup wait, so the home screen needs the
	// destination to say "Reconnecting to <host>" instead of telling the user
	// to restart the local daemon it is not talking to.
	views.SetHostDestination(host)
	// Paste uploads run behind a live attached pane and so must set BatchMode,
	// which turns a credential prompt into an immediate failure. Handing them
	// the tunnel's control socket lets them ride the connection the user has
	// already authenticated, so a user on password or uncached-passphrase auth
	// can paste at all. Empty when multiplexing is unavailable, which leaves
	// uploads opening their own ssh exactly as before.
	views.SetHostControlPath(tunnel.ControlPath())
	// …and the reason it dropped, so that screen can report the supervisor's
	// diagnosis rather than only the fact of the outage.
	views.SetHostTunnelReason(hostTunnelFailureReason)
	// …and whether anything is still redialling, which is what consuming Start's
	// done channel is *for*: a screen that promises an automatic recovery after
	// the supervisor has unwound is telling the user to keep waiting for a
	// reconnect nobody is attempting.
	views.SetHostSupervisorRunning(hostSupervisorRunning)
	return client.NewLocalWithToken(tunnel.LocalSocket(), token), nil
}

// hostError prefixes a remotehost failure with the flag and destination that
// caused it while preserving the sentinel, so a stopped remote daemon stays
// distinguishable from a missing remote `boss`.
func hostError(host string, err error) error {
	return fmt.Errorf("--host %s: %w", host, err)
}

// hostCommandContext returns the command's context, which lives for the whole
// process — the tunnel supervisor must not be cancelled when one RPC ends.
func hostCommandContext(cmd *cobra.Command) context.Context {
	if cmd == nil {
		return context.Background()
	}
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

// startHostTunnel is the production hostTunnelStarter: supervise the forward
// and block until the local socket accepts a dial.
func startHostTunnel(ctx context.Context, cfg remotehost.TunnelConfig) (hostTunnel, error) {
	tunnel, err := remotehost.NewTunnel(cfg)
	if err != nil {
		return nil, err
	}
	// Start's doc is explicit that callers must consume this channel, and the
	// repo convention says the same of every safego.Go: a closed done is the
	// free, precise signal that the supervisor is gone.
	done := tunnel.Start(ctx)

	readyCtx, cancel := context.WithTimeout(ctx, hostTunnelReadyTimeout)
	defer cancel()
	if err := tunnel.Ready(readyCtx); err != nil {
		// NewTunnel already created the private directory, so a forward that
		// never came up must still be closed or it leaves one behind.
		_ = tunnel.Close()
		return nil, err
	}
	// The forward is up, so any interactive ssh auth has already been answered
	// and the TUI is about to take the terminal. From here a reconnect that
	// stopped at a prompt would block forever behind the alt screen, so it must
	// fail fast and be retried instead.
	tunnel.HardenAuth()
	return &supervisedTunnel{hostTunnel: tunnel, done: done}, nil
}

// registerHostConnection records the live transport so the deferred shutdown in
// run() can tear it down.
//
// A second --host client in one process (no command does this today, but one
// could) closes the first rather than replacing it silently: at most one tunnel
// is ever registered, so an earlier ssh child and its directory cannot be
// orphaned for the life of the process.
func registerHostConnection(conn *hostConnection) {
	hostConnMu.Lock()
	previous := hostConn
	hostConn = conn
	hostConnMu.Unlock()

	if previous != nil && previous.tunnel != nil {
		_ = previous.tunnel.Close()
	}
}

// currentHostConnection returns the live --host transport, or nil.
func currentHostConnection() *hostConnection {
	hostConnMu.Lock()
	defer hostConnMu.Unlock()
	return hostConn
}

// hostTunnelFailureReason returns why the live --host tunnel last went down, or
// "" when there is no transport or it has not failed. It reads the registry at
// call time rather than closing over one tunnel, so a re-registered transport
// (or a torn-down one) is reported honestly.
//
// It is derived from tunnel errors only. hostConnection also holds the daemon
// token, which is memory-only and must never reach a log line or a rendered
// string — see TestHostTunnelFailureReasonReadsTheRegistry.
func hostTunnelFailureReason() string {
	return tunnelFailureReason(currentHostConnection().activeTunnel())
}

// hostSupervisorRunning reports whether the goroutine redialling the live --host
// tunnel is still running. Like hostTunnelFailureReason it reads the registry at
// call time, so a re-registered transport is answered for rather than a captured
// one.
//
// It answers true for anything that cannot say otherwise — no transport, or a
// tunnel with no done channel behind it. The view layer asks this only to decide
// whether to STOP promising a reconnect, and neither "--host was never used" nor
// "this transport cannot tell" is evidence that a supervisor died.
func hostSupervisorRunning() bool {
	aware, ok := currentHostConnection().activeTunnel().(supervisorAware)
	if !ok {
		return true
	}
	return aware.supervisorRunning()
}

// activeTunnel is the nil-safe accessor for the supervised tunnel; a nil
// receiver means --host was never used, which is the ordinary local case.
func (c *hostConnection) activeTunnel() hostTunnel {
	if c == nil {
		return nil
	}
	return c.tunnel
}

// tunnelFailureReason is the single boundary where the supervisor's classified
// error becomes display text, so it is where the value is made safe to render.
//
// classifyExit splices the ssh child's stderr into that error, redacting only
// token-shaped runs — everything else is content the *remote* host chooses, of
// any length: a MOTD, a banner, a multi-line kex dump. Before BOS-724 that only
// reached a Debug line and a startup error printed before the TUI took the
// terminal. It is now concatenated into a lipgloss panel behind the alt screen,
// where a stray ESC or \r repaints or scrolls the frame and an unbounded value
// pushes the action bar out of view. The e2e seed already refuses exactly these
// two shapes for exactly this reason (hostwait_e2e.go resolveE2EHostReconnectReason);
// production sanitizes rather than refuses, because a mangled reason is still
// better evidence than none.
func tunnelFailureReason(tunnel hostTunnel) string {
	if tunnel == nil {
		return ""
	}
	err := tunnel.LastFailure()
	if err == nil {
		return ""
	}
	return sanitizeTunnelReason(err.Error())
}

// hostTunnelReasonLimit caps the rendered failure reason. A classified ssh exit
// is one line; anything longer is not a reason, and a long enough one would push
// the action bar off the screen — the very affordance the reconnect state exists
// to keep. The e2e seed derives its own cap from this one so a captured frame
// and a live one cannot disagree about how much fits.
const hostTunnelReasonLimit = 200

// sanitizeTunnelReason flattens a classified failure to one control-free line
// of bounded length. Newlines and tabs become spaces so the reason stays a
// single line; every other control rune (ESC above all) is dropped rather than
// substituted, since nothing it could stand for is worth a byte on screen.
func sanitizeTunnelReason(reason string) string {
	var b strings.Builder
	b.Grow(len(reason))
	for _, r := range reason {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteRune(' ')
		case unicode.IsControl(r):
		default:
			b.WriteRune(r)
		}
	}
	// Collapse the runs the substitution above can create, so a multi-line
	// stderr does not render as a corridor of spaces.
	flattened := strings.Join(strings.Fields(b.String()), " ")
	if len(flattened) > hostTunnelReasonLimit {
		// Truncate on a rune boundary: a half-written multi-byte rune renders
		// as a replacement glyph in the middle of the reason.
		flattened = strings.ToValidUTF8(flattened[:hostTunnelReasonLimit], "") + "…"
	}
	return flattened
}

// shutdownHostTunnel closes the live --host tunnel and removes its private
// directory. Idempotent, and a no-op when --host was not used.
func shutdownHostTunnel() {
	hostConnMu.Lock()
	conn := hostConn
	hostConn = nil
	hostConnMu.Unlock()

	if conn == nil || conn.tunnel == nil {
		return
	}
	if err := conn.tunnel.Close(); err != nil {
		// File-only logging: this cannot corrupt the TUI, and a failed teardown
		// is not worth failing an otherwise successful command over.
		log.Warn().Err(err).Str("destination", conn.destination).
			Msg("boss: could not close the --host ssh tunnel")
	}
}

// waitForHostDaemon handles a dial failure against a --host daemon.
//
// waitForDaemon runs only when the *startup* dial fails, and a --host transport
// is registered only once its tunnel is up, so that path reaches here with no
// tunnel: the failure happened during setup, where no amount of polling helps,
// so the static screen explains it and the original error propagates.
//
// A drop *after* startup does not come through here at all — the client is
// already built, so it surfaces on the home screen, whose daemon-down
// remediation names the host and its ssh checks (views.SetHostDestination).
//
// The live-tunnel branch is the correct answer whenever a dial fails with a
// supervised tunnel behind it, and is what the e2e reconnect seed drives; it is
// not the mid-session path.
func waitForHostDaemon(host string, origErr error) (client.BossClient, error) {
	conn := currentHostConnection()
	if conn == nil || conn.tunnel == nil {
		if err := showHostPreflight(*preflight.DaemonIssue(origErr)); err != nil {
			return nil, err
		}
		return nil, origErr
	}
	// origErr is deliberately dropped here. Under --host every RPC dials the
	// forwarded socket, so a drop reads "dial unix /var/folders/…/bossd.sock:
	// connect: no such file or directory" — a local temp path the user cannot
	// act on, and the string that reached a user verbatim in BOS-724. The
	// supervisor's classified failure is the honest answer.
	if err := showHostReconnectWait(host, hostReconnectDetail(tunnelFailureReason(conn.tunnel)), conn.tunnel.Healthy); err != nil {
		return nil, err
	}
	return client.NewLocalWithToken(conn.tunnel.LocalSocket(), conn.token), nil
}

// hostReconnectFallbackDetail is what the reconnect screen says when the
// supervisor has not classified a failure yet — the first drop can be noticed by
// a failing RPC before the child has exited. Fixed and non-leaky by
// construction: it names neither the forwarded socket nor anything local.
const hostReconnectFallbackDetail = "The connection to the remote daemon was lost."

// hostReconnectDetail turns the supervisor's last classified failure into the
// line above the reconnect remediation, so a user who hits this can read *why*
// the tunnel went down without attaching a debugger (BOS-724).
func hostReconnectDetail(reason string) string {
	if strings.TrimSpace(reason) == "" {
		return hostReconnectFallbackDetail
	}
	return "Last tunnel failure: " + reason
}

// hostReconnectIssue builds the wait screen shown while the ssh tunnel to host
// is down. "Reconnecting" and the host string are on-screen evidence a proof
// scenario asserts, so do not reword them casually.
func hostReconnectIssue(host, detail string) preflight.Issue {
	return preflight.Issue{
		Title: "Reconnecting to " + host,
		Detail: detail + "\n\n" +
			"The ssh tunnel to " + host + " dropped. Boss keeps redialling and resumes automatically.\n\n" +
			"If it does not come back:\n\n" +
			"  ssh " + host + "                    # check the connection itself\n" +
			"  ssh " + host + " boss daemon status # check bossd on that host",
	}
}

// runHostReconnectWait shows the auto-recovering wait screen for a --host
// tunnel. It goes through views.RunDaemonWait so the [q]uit action bar and the
// automatic-reconnect footer are the same ones the local daemon wait shows.
func runHostReconnectWait(host, detail string, healthy func() bool) error {
	return views.RunDaemonWait(hostReconnectIssue(host, detail), healthy)
}
