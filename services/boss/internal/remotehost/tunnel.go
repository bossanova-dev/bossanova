package remotehost

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/safego"
)

const (
	// serverAliveInterval/serverAliveCountMax are deliberately small: a
	// half-open TCP connection (laptop suspended, wifi switched) leaves ssh
	// looking alive while nothing flows, and without keepalives the failure
	// only surfaces when the OS gives up minutes later — that is the
	// "Operation timed out" in the original report. 5s x 3 misses means the
	// supervisor sees the drop in ~15s and starts redialling.
	serverAliveInterval = 5
	serverAliveCountMax = 3

	// defaultInitialBackoff/defaultMaxBackoff bound reconnect attempts: fast
	// enough that a blip costs one in-flight RPC, capped so a genuinely dead
	// host is not retried in a battery-burning loop.
	defaultInitialBackoff = 500 * time.Millisecond
	defaultMaxBackoff     = 30 * time.Second

	// defaultStableUptime is how long a child must survive before the run
	// counts as "stayed up" and the backoff returns to its floor. It is longer
	// than a connection failure takes to report, so a host that instantly
	// refuses can never reset the ramp, and short enough that a genuine
	// hours-long session does not carry an old ramp into its next blip.
	defaultStableUptime = 30 * time.Second

	// readyPollInterval is how often Ready re-probes; dialTimeout bounds each
	// probe so a wedged socket cannot block a TUI poll loop.
	readyPollInterval = 10 * time.Millisecond
	dialTimeout       = 250 * time.Millisecond

	// closeWaitTimeout bounds how long Close waits for the supervisor to
	// unwind, so a stuck child cannot hang process shutdown.
	closeWaitTimeout = 5 * time.Second

	// waitDelay bounds cmd.Wait once the child is gone: Wait otherwise blocks
	// until every process inheriting a captured output pipe closes it, which a
	// grandchild of a killed ssh may not do promptly. Shared with the upload
	// helpers (upload.go), which capture stdout as well as stderr.
	waitDelay = 2 * time.Second

	// socketPathLimit is the practical sun_path ceiling (104 on darwin, 108 on
	// linux); a longer path fails to bind with a bewildering EINVAL.
	socketPathLimit = 100
)

// Sentinel errors for the two ssh exits that are not worth retrying blindly.
var (
	// ErrForwardFailed means ssh exited because it could not establish the
	// forward (`-o ExitOnForwardFailure=yes`). Distinguishing it from a plain
	// connection failure is the whole point of that option: without it ssh
	// would sit there connected but useless, looking healthy.
	ErrForwardFailed = errors.New("remotehost: ssh could not establish the socket forward")
	// ErrUnsupportedForward means the local or remote ssh predates OpenSSH 6.7
	// and cannot forward unix sockets at all. Classified separately so the user
	// sees a version requirement instead of an unexplained dial failure.
	ErrUnsupportedForward = errors.New("remotehost: ssh does not support unix-socket forwarding")
)

// TunnelConfig configures the supervised forward.
type TunnelConfig struct {
	Options // Destination, the --host-socket override, and the command factory.

	// RemoteSocket is the daemon socket path on the remote host, as returned by
	// DiscoverSocket. It falls back to Options.RemoteSocket when empty so a
	// caller may pass the --host-socket override through untouched.
	RemoteSocket string
	// InitialBackoff is the first reconnect wait; zero means a sane default.
	InitialBackoff time.Duration
	// MaxBackoff caps the reconnect wait; zero means a sane default.
	MaxBackoff time.Duration
	// Logger receives supervision events. It must never be given the daemon
	// token — this file never touches it.
	Logger zerolog.Logger
}

// Tunnel supervises `ssh -N -L <local.sock>:<remote.sock> <destination>`,
// restarting it with capped exponential backoff so a dropped connection only
// fails an in-flight RPC — the next poll succeeds once the redial lands.
//
// The destination is handed to ssh verbatim (see the package doc); the forward
// is the third invocation that must carry the identical string.
type Tunnel struct {
	factory        CommandFactory
	destination    string
	remoteSocket   string
	localSocket    string
	controlPath    string
	dir            string
	initialBackoff time.Duration
	maxBackoff     time.Duration
	stableAfter    time.Duration
	log            zerolog.Logger

	// sleep replaces the backoff wait in tests so the reconnect ladder can be
	// asserted without spending real seconds. nil means the real, ctx-aware wait.
	sleep func(time.Duration)
	// since measures how long a child stayed up. It is a seam because process
	// spawn time is not bounded on a loaded machine, so a test that judged
	// "stayed up" by real elapsed time would be flaky by construction.
	since func(time.Time) time.Duration

	mu        sync.Mutex
	running   bool
	closed    bool
	started   bool
	hardened  bool
	lastErr   error
	lastFatal bool
	cancel    context.CancelFunc
	done      <-chan struct{}

	cleanupOnce sync.Once
	cleanupErr  error
}

// NewTunnel validates the config and creates the private 0700 directory that
// will hold the forwarded socket — and nothing else, since the daemon token is
// held in memory and must never reach local disk.
func NewTunnel(cfg TunnelConfig) (*Tunnel, error) {
	if strings.TrimSpace(cfg.Destination) == "" {
		return nil, ErrNoDestination
	}
	remote := cfg.RemoteSocket
	if remote == "" {
		remote = cfg.Options.RemoteSocket
	}
	if remote == "" {
		return nil, fmt.Errorf("remotehost: no remote socket path for %s", cfg.Destination)
	}

	// MkdirTemp creates the directory 0700 and, being random-named, is not a
	// path another local user can pre-create and hijack.
	dir, err := os.MkdirTemp("", "boss-host-")
	if err != nil {
		return nil, fmt.Errorf("remotehost: could not create the tunnel directory: %w", err)
	}
	local := filepath.Join(dir, "bossd.sock")
	if len(local) > socketPathLimit {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("remotehost: local socket path %q is too long for a unix socket (%d > %d); set TMPDIR to something shorter",
			local, len(local), socketPathLimit)
	}

	// The multiplexing socket lives in the same private 0700 directory. It is
	// bound by ssh as a unix socket, so it is subject to the same sun_path
	// ceiling as the forward — checked on its own rather than inferred from the
	// forward's check, which a later rename of either file would silently
	// invalidate. An over-long path degrades to no multiplexing rather than
	// failing the tunnel: the forward is what the session needs, and uploads
	// keep the BatchMode behaviour they had before multiplexing existed.
	control := filepath.Join(dir, "ctl.sock")
	if !multiplexSupported() || len(control) > socketPathLimit {
		control = ""
	}

	t := &Tunnel{
		factory:        cfg.commandFactory(),
		destination:    cfg.Destination,
		remoteSocket:   remote,
		localSocket:    local,
		controlPath:    control,
		dir:            dir,
		initialBackoff: cfg.InitialBackoff,
		maxBackoff:     cfg.MaxBackoff,
		stableAfter:    defaultStableUptime,
		log:            cfg.Logger,
	}
	if t.initialBackoff <= 0 {
		t.initialBackoff = defaultInitialBackoff
	}
	if t.maxBackoff <= 0 {
		t.maxBackoff = defaultMaxBackoff
	}
	if t.maxBackoff < t.initialBackoff {
		t.maxBackoff = t.initialBackoff
	}
	return t, nil
}

// multiplexSupported reports whether ssh on this platform can multiplex over a
// control socket. Win32-OpenSSH does not implement ControlMaster at all, and
// passing it there is not merely inert — ssh rejects the unsupported option and
// the invocation fails, which for the forward would mean no session at all.
//
// It is a function rather than a build-tagged constant so a single test binary
// can cover both branches by pinning the value it compares against.
func multiplexSupported() bool { return runtime.GOOS != "windows" }

// LocalSocket returns the forwarded local socket path — what the boss client
// dials in place of the remote daemon's own socket.
func (t *Tunnel) LocalSocket() string { return t.localSocket }

// RemoteSocket returns the daemon socket path on the far side.
func (t *Tunnel) RemoteSocket() string { return t.remoteSocket }

// ControlPath returns the multiplexing socket this tunnel masters, or "" when
// multiplexing is unavailable (Windows, or a temp path too long to bind).
//
// Callers pass it to the upload helpers so a paste rides this already
// authenticated connection instead of opening its own. That is what makes
// pasting work for a user on password or uncached-passphrase auth: those
// helpers run behind a live attached pane and so must set BatchMode, which
// turns any credential prompt into an immediate failure — see batchModeOptions.
// Riding the master means the slave authenticates against nothing, so there is
// no prompt for BatchMode to refuse.
//
// It is "" until Start has brought a child up, and empty again during a
// reconnect: a slave given a ControlPath with no live master falls back to its
// own connection, where BatchMode still applies. So an upload landing inside a
// backoff window fails exactly as it did before multiplexing existed.
func (t *Tunnel) ControlPath() string { return t.controlPath }

// Start supervises the forward until ctx is cancelled or Close is called, and
// returns safego.Go's done channel. Callers must consume it (Close awaits it);
// starting twice returns the original channel.
func (t *Tunnel) Start(ctx context.Context) <-chan struct{} {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	if t.started {
		done := t.done
		t.mu.Unlock()
		return done
	}
	runCtx, cancel := context.WithCancel(ctx)
	t.started = true
	t.cancel = cancel
	done := safego.Go(t.log, func() { t.supervise(runCtx) })
	t.done = done
	t.mu.Unlock()
	return done
}

// Ready blocks until the forwarded socket accepts a dial, ctx expires, or the
// child fails in a way retrying cannot fix (a rejected forward, or an ssh too
// old to forward unix sockets). Returning that recorded failure is what keeps a
// broken forward from looking like a slow one.
func (t *Tunnel) Ready(ctx context.Context) error {
	ticker := time.NewTicker(readyPollInterval)
	defer ticker.Stop()
	for {
		if t.Healthy() {
			return nil
		}
		if err := t.fatalFailure(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			if last := t.LastFailure(); last != nil {
				return fmt.Errorf("remotehost: ssh tunnel to %s did not come up: %w (last failure: %v)",
					t.destination, ctx.Err(), last)
			}
			return fmt.Errorf("remotehost: ssh tunnel to %s did not come up: %w", t.destination, ctx.Err())
		case <-ticker.C:
		}
	}
}

// Healthy reports whether the forward is currently usable: a live child *and* a
// socket that accepts a connection. The process alone is not evidence — that is
// exactly the false-healthy state ExitOnForwardFailure exists to prevent.
func (t *Tunnel) Healthy() bool {
	t.mu.Lock()
	running, closed := t.running, t.closed
	t.mu.Unlock()
	if closed || !running {
		return false
	}
	conn, err := net.DialTimeout("unix", t.localSocket, dialTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// Close stops supervising, kills the child, waits (bounded) for the supervisor
// to unwind, and removes the private directory. It is idempotent.
func (t *Tunnel) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	cancel, done := t.cancel, t.done
	t.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	var waitErr error
	if done != nil {
		select {
		case <-done:
		case <-time.After(closeWaitTimeout):
			waitErr = fmt.Errorf("remotehost: tunnel supervisor for %s did not stop within %s",
				t.destination, closeWaitTimeout)
		}
	}
	if err := t.cleanup(); err != nil {
		return err
	}
	return waitErr
}

// supervise runs the forward, restarting it with jittered, capped backoff until
// ctx is done. It owns directory cleanup on exit so a cancelled context (not
// only an explicit Close) leaves nothing behind.
func (t *Tunnel) supervise(ctx context.Context) {
	defer func() {
		if err := t.cleanup(); err != nil {
			t.log.Warn().Err(err).Msg("remotehost: could not remove the tunnel directory")
		}
	}()

	backoff := t.initialBackoff
	for {
		if ctx.Err() != nil {
			return
		}
		startedAt := time.Now()
		err := t.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		uptime := t.uptime(startedAt)
		t.recordFailure(err)
		t.log.Debug().Err(err).Str("destination", t.destination).
			Dur("uptime", uptime).Msg("remotehost: ssh tunnel exited; reconnecting")

		// A forward that stayed up was a real connection, not a failing host:
		// its next blip should reconnect promptly rather than inherit the ramp.
		if uptime >= t.stableAfter {
			backoff = t.initialBackoff
		}
		t.wait(ctx, jittered(backoff))
		if backoff *= 2; backoff > t.maxBackoff {
			backoff = t.maxBackoff
		}
	}
}

// runOnce starts one ssh child and blocks until it exits, returning a
// classified error. Stderr is captured (not inherited) so a forward rejection
// can be turned into an actionable message instead of scrolling past the TUI.
func (t *Tunnel) runOnce(ctx context.Context) error {
	// A socket file left by a killed process makes bind fail, so clear the
	// path on every start, not just on exit.
	if err := t.removeSocket(); err != nil {
		return err
	}

	// #nosec G204 -- ssh with a fixed option set and the user's own --host
	// destination; argv exec with no shell involved. owner=@recurser review-by=2027-02-07 issue=BOS-713
	cmd := t.factory(ctx, "ssh", t.argv()...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = waitDelay

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("remotehost: could not start ssh for %s: %w", t.destination, err)
	}
	t.setRunning(true)
	err := cmd.Wait()
	t.setRunning(false)
	return classifyExit(t.destination, err, stderr.Bytes())
}

// HardenAuth stops ssh prompting for credentials on every later reconnect.
//
// The first connect deliberately allows an interactive prompt (a key
// passphrase, an unknown-host confirmation): it happens during startup while
// the user still owns the terminal, and refusing there would break anyone
// without an agent-loaded key. Once the forward is up the TUI takes the
// screen, and a supervised restart that prompted would write into the alt
// screen and then block in cmd.Wait indefinitely — ssh reads passphrases from
// /dev/tty, which no timeout on this side covers. After this call such a
// restart fails immediately instead, and is retried with the usual backoff,
// which the reconnect screen can show and the user can quit.
//
// Call it once the forward is known good; it is not undone.
func (t *Tunnel) HardenAuth() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.hardened = true
}

// argv is the forward invocation. The destination is the final element and is
// passed through byte-identically — see the package doc.
func (t *Tunnel) argv() []string {
	args := []string{
		// -N: forwarding only, no remote command or shell.
		"-N",
		// A forward that cannot be established must exit, so the supervisor
		// sees a failure rather than a connected-but-useless session.
		"-o", "ExitOnForwardFailure=yes",
		"-o", fmt.Sprintf("ServerAliveInterval=%d", serverAliveInterval),
		"-o", fmt.Sprintf("ServerAliveCountMax=%d", serverAliveCountMax),
	}

	if t.controlPath != "" {
		// This process is the multiplexing master for the session, so an upload
		// helper can ride it rather than authenticating again — see ControlPath.
		// `auto` rather than `yes` so a stale socket from a previous run is
		// reused-or-replaced instead of aborting the forward, which is the one
		// thing on this argv the session cannot do without.
		//
		// ControlPersist=no is set explicitly to override any value in the
		// user's ssh_config: a persisting master would outlive this -N child and
		// keep a connection to the user's host open after boss exited, and it
		// would survive into the next tunnel with a socket this process no
		// longer owns. Tying the master's life to this child keeps "the tunnel
		// is up" and "the master is up" the same fact.
		args = append(args,
			"-o", "ControlMaster=auto",
			"-o", "ControlPath="+t.controlPath,
			"-o", "ControlPersist=no",
		)
	}

	t.mu.Lock()
	hardened := t.hardened
	t.mu.Unlock()
	if hardened {
		// BatchMode turns a credential prompt into an immediate failure, which
		// is what a reconnect behind a live TUI needs — see HardenAuth.
		args = append(args, "-o", "BatchMode=yes")
	}

	return append(args,
		"-L", t.localSocket+":"+t.remoteSocket,
		t.destination,
	)
}

// wait sleeps for d, returning early when ctx is done so a cancelled tunnel
// does not linger for a full backoff interval.
func (t *Tunnel) wait(ctx context.Context, d time.Duration) {
	if t.sleep != nil {
		t.sleep(d)
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func (t *Tunnel) uptime(startedAt time.Time) time.Duration {
	if t.since != nil {
		return t.since(startedAt)
	}
	return time.Since(startedAt)
}

func (t *Tunnel) setRunning(running bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.running = running
}

func (t *Tunnel) recordFailure(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastErr = err
	t.lastFatal = errors.Is(err, ErrForwardFailed) || errors.Is(err, ErrUnsupportedForward)
}

// fatalFailure returns the last failure only when retrying cannot fix it, so
// Ready surfaces a rejected forward immediately while a refused connection
// keeps its chance to recover.
func (t *Tunnel) fatalFailure() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.lastFatal {
		return t.lastErr
	}
	return nil
}

// LastFailure returns the classified error from the most recent ssh exit, or
// nil while the child has never exited. It is the supervisor's own diagnosis —
// the same value recordFailure stores and the Debug line reports — exported so
// a caller behind a live TUI can say *why* the tunnel is down instead of
// rendering the forwarded socket's dial error, which only ever describes the
// local temp file the user cannot act on (BOS-724).
//
// It never carries the daemon token: nothing in this file touches it, and the
// two stderr splices in classifyExit go through redactTokens.
func (t *Tunnel) LastFailure() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastErr
}

func (t *Tunnel) removeSocket() error {
	if err := os.Remove(t.localSocket); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remotehost: could not clear the stale local socket %s: %w", t.localSocket, err)
	}
	return nil
}

// cleanup removes the private directory exactly once, whether the supervisor
// unwound on its own or Close tore it down.
func (t *Tunnel) cleanup() error {
	t.cleanupOnce.Do(func() {
		if err := os.RemoveAll(t.dir); err != nil {
			t.cleanupErr = fmt.Errorf("remotehost: could not remove the tunnel directory %s: %w", t.dir, err)
		}
	})
	return t.cleanupErr
}

// classifyExit turns an ssh exit into an error the user can act on. The stderr
// substrings are OpenSSH's own wording; matching text is enough here and avoids
// shelling out to `ssh -V` just to explain a failure.
func classifyExit(destination string, err error, stderr []byte) error {
	// Redacted like every other stderr splice (see redactTokens). This child is
	// `ssh -N`, which opens no session channel, so no login shell, rc file, or
	// ForceCommand wrapper runs and it never reads the token file — but these
	// two branches interpolate detail directly rather than through
	// stderrDetail, and an invariant with a structural exception is one an
	// unrelated future edit can quietly widen.
	detail := redactTokens(strings.TrimSpace(string(stderr)))
	lower := strings.ToLower(detail)
	switch {
	case mentionsUnsupportedForward(lower):
		return fmt.Errorf("%w: forwarding a unix socket with -L needs OpenSSH 6.7 or newer on both ends; %s reported: %s",
			ErrUnsupportedForward, destination, detail)
	case mentionsForwardFailure(lower):
		return fmt.Errorf("%w to %s: %s", ErrForwardFailed, destination, detail)
	case err != nil:
		return fmt.Errorf("remotehost: ssh tunnel to %s exited: %w%s", destination, err, stderrDetail(stderr))
	default:
		return fmt.Errorf("remotehost: ssh tunnel to %s exited cleanly%s", destination, stderrDetail(stderr))
	}
}

// mentionsUnsupportedForward matches the pre-6.7 refusal to even parse the
// unix-socket form of -L. Checked before the generic forward failure because an
// old ssh also prints "bad forwarding specification".
func mentionsUnsupportedForward(lower string) bool {
	needles := []string{
		"bad local forwarding specification",
		"bad forwarding specification",
		"unix domain socket forwarding",
		"unix domain sockets are not supported",
	}
	return containsAny(lower, needles)
}

func mentionsForwardFailure(lower string) bool {
	needles := []string{
		"unix_listener",
		"could not request local forwarding",
		"forwarding request failed",
		"address already in use",
		"cannot bind to path",
		"cannot listen to port",
		"bind: ",
	}
	return containsAny(lower, needles)
}

func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// jittered spreads a backoff over [d/2, d) so several boss processes losing the
// same network do not redial the host in lockstep.
func jittered(d time.Duration) time.Duration {
	half := d / 2
	if half <= 0 {
		return d
	}
	// #nosec G404 -- reconnect jitter; timing spread needs no crypto-grade randomness and gates no security decision
	// owner=@recurser review-by=2027-02-07 issue=BOS-713
	return half + time.Duration(rand.Int63n(int64(half)))
}
