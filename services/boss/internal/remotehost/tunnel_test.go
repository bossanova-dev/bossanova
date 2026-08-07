package remotehost

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

const (
	testDest       = "deploy@bastion.example.com"
	testRemoteSock = "/home/dave/.bossanova/bossd.sock"
)

// fakeRunner is the tunnel's stand-in for the ssh binary: it records every argv
// the supervisor asks for and runs /bin/sh -c "$script" instead, the repo's
// fake-command idiom (plugins/bossd-plugin-claude/runner_test.go). script and
// before are keyed by invocation number so a test can script a restart sequence
// — e.g. "fail twice, then come up".
type fakeRunner struct {
	mu     sync.Mutex
	calls  []recordedCmd
	delays []time.Duration
	script func(call int) string
	before func(call int)
}

func (f *fakeRunner) factory() CommandFactory {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		f.mu.Lock()
		call := len(f.calls)
		f.calls = append(f.calls, recordedCmd{name: name, args: args, ctx: ctx})
		before, script := f.before, f.script
		f.mu.Unlock()

		if before != nil {
			before(call)
		}
		s := "sleep 30"
		if script != nil {
			s = script(call)
		}
		return exec.CommandContext(ctx, "/bin/sh", "-c", s)
	}
}

// sleepFn is the supervisor's injected sleep: it records the requested backoff
// so the growth/cap assertions are deterministic, and yields for a real
// millisecond so a fake that exits instantly cannot spin into a fork bomb.
func (f *fakeRunner) sleepFn(d time.Duration) {
	f.mu.Lock()
	f.delays = append(f.delays, d)
	f.mu.Unlock()
	time.Sleep(time.Millisecond)
}

func (f *fakeRunner) setBefore(fn func(call int)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.before = fn
}

func (f *fakeRunner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeRunner) argv(i int) (string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[i].name, append([]string(nil), f.calls[i].args...)
}

func (f *fakeRunner) recordedDelays() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.delays...)
}

// newTestTunnel builds a tunnel wired to a fake ssh with sub-millisecond
// backoff, so no test in this file ever waits on a real reconnect interval.
func newTestTunnel(t *testing.T, mutate func(*TunnelConfig)) (*Tunnel, *fakeRunner) {
	t.Helper()
	fake := &fakeRunner{}
	cfg := TunnelConfig{
		Options:        Options{Destination: testDest, CommandFactory: fake.factory()},
		RemoteSocket:   testRemoteSock,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     4 * time.Millisecond,
		Logger:         zerolog.Nop(),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	tun, err := NewTunnel(cfg)
	if err != nil {
		t.Fatalf("NewTunnel: %v", err)
	}
	tun.sleep = fake.sleepFn
	t.Cleanup(func() { _ = tun.Close() })
	return tun, fake
}

// listenAt binds a unix socket where the fake ssh would have forwarded one, so
// Healthy's dial probe sees a live endpoint.
func listenAt(t *testing.T, path string) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Errorf("listen on %s: %v", path, err)
		return
	}
	t.Cleanup(func() { _ = ln.Close() })
}

// waitFor polls cond until it holds or the budget expires. Budgets here are
// generous only to absorb CI scheduling; the happy path returns in milliseconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestTunnelReadyWhenForwardComesUp(t *testing.T) {
	tun, fake := newTestTunnel(t, nil)
	fake.setBefore(func(int) { listenAt(t, tun.LocalSocket()) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tun.Start(ctx)

	readyCtx, readyCancel := context.WithTimeout(ctx, 3*time.Second)
	defer readyCancel()
	if err := tun.Ready(readyCtx); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if !tun.Healthy() {
		t.Fatal("Healthy() = false after a successful Ready")
	}
}

func TestTunnelForwardArgv(t *testing.T) {
	tun, fake := newTestTunnel(t, nil)
	fake.setBefore(func(int) { listenAt(t, tun.LocalSocket()) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tun.Start(ctx)
	waitFor(t, "the first ssh invocation", func() bool { return fake.count() >= 1 })

	name, args := fake.argv(0)
	if name != "ssh" {
		t.Fatalf("binary = %q, want ssh", name)
	}
	want := []string{
		"-N",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=5",
		"-o", "ServerAliveCountMax=3",
	}
	// The forward is the multiplexing master so uploads can ride it instead of
	// authenticating again. Built from ControlPath() rather than a literal
	// because it is empty where ssh cannot multiplex, and there the argv must
	// carry none of these three.
	if cp := tun.ControlPath(); cp != "" {
		want = append(want,
			"-o", "ControlMaster=auto",
			"-o", "ControlPath="+cp,
			"-o", "ControlPersist=no",
		)
	}
	want = append(want,
		"-L", tun.LocalSocket()+":"+testRemoteSock,
		testDest,
	)
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("argv = %q, want %q", args, want)
	}
	// The destination is an opaque ssh destination: it must arrive as one
	// unmodified argv element, never split or rewritten.
	if args[len(args)-1] != testDest {
		t.Fatalf("destination = %q, want %q verbatim", args[len(args)-1], testDest)
	}
}

// TestTunnelHardenAuthBatchesLaterConnects: the first connect may prompt (a key
// passphrase, an unknown-host confirmation) because startup still owns the
// terminal, but once the forward is up the TUI takes the screen and a prompt
// would be invisible *and* block cmd.Wait forever. After HardenAuth every
// restart must carry BatchMode=yes so it fails fast and is retried instead.
func TestTunnelHardenAuthBatchesLaterConnects(t *testing.T) {
	tun, fake := newTestTunnel(t, nil)
	// Exit immediately so the supervisor keeps restarting and the argv of a
	// *later* connect can be compared against the first.
	fake.script = func(int) string { return "exit 1" }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tun.Start(ctx)
	waitFor(t, "the first ssh invocation", func() bool { return fake.count() >= 1 })

	_, first := fake.argv(0)
	if containsArg(first, "BatchMode=yes") {
		t.Fatalf("the first connect must stay interactive, got %q", first)
	}

	tun.HardenAuth()
	before := fake.count()
	waitFor(t, "a restart after HardenAuth", func() bool { return fake.count() > before })

	_, later := fake.argv(fake.count() - 1)
	if !containsArg(later, "BatchMode=yes") {
		t.Fatalf("a reconnect after HardenAuth must pass BatchMode=yes, got %q", later)
	}
	// The destination is still the opaque final element, and the forward spec
	// is unchanged — hardening must not reorder or rewrite either.
	if later[len(later)-1] != testDest {
		t.Fatalf("destination = %q, want %q verbatim as the final element", later[len(later)-1], testDest)
	}
	if !containsArg(later, tun.LocalSocket()+":"+testRemoteSock) {
		t.Fatalf("forward spec missing from %q", later)
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func TestTunnelRestartsAfterImmediateExit(t *testing.T) {
	tun, fake := newTestTunnel(t, nil)
	fake.script = func(int) string { return "exit 1" }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tun.Start(ctx)

	waitFor(t, "a restart after the child exited", func() bool { return fake.count() >= 2 })
	if tun.Healthy() {
		t.Fatal("Healthy() = true while the child keeps exiting")
	}
}

func TestTunnelBackoffGrowsAndStaysCapped(t *testing.T) {
	tun, fake := newTestTunnel(t, nil)
	fake.script = func(int) string { return "exit 1" }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tun.Start(ctx)
	waitFor(t, "five backoff intervals", func() bool { return len(fake.recordedDelays()) >= 5 })
	cancel()

	delays := fake.recordedDelays()[:5]
	// Jitter keeps each delay in [base/2, base), so consecutive doublings
	// occupy non-overlapping ranges and strict growth is not flaky.
	for i := 1; i < 3; i++ {
		if delays[i] <= delays[i-1] {
			t.Fatalf("backoff did not grow: %v", delays)
		}
	}
	for i, d := range delays {
		if d >= 4*time.Millisecond {
			t.Fatalf("delay %d = %v exceeds the cap; sequence %v", i, d, delays)
		}
	}
	for _, d := range delays[3:] {
		if d < 2*time.Millisecond {
			t.Fatalf("capped delays fell back below the cap floor: %v", delays)
		}
	}
}

func TestTunnelBackoffResetsAfterAStableRun(t *testing.T) {
	tun, fake := newTestTunnel(t, func(cfg *TunnelConfig) {
		cfg.MaxBackoff = 8 * time.Millisecond
	})
	fake.script = func(int) string { return "exit 1" }
	// Three short-lived children grow the backoff to the cap; the fourth is
	// reported as having stayed up, which must return the next wait to the
	// floor. Uptime comes from a seam rather than a real sleep: process spawn
	// time on a loaded machine is unbounded, so judging "stayed up" by wall
	// clock here would make the test flaky rather than deterministic.
	tun.stableAfter = time.Minute
	var runs int
	tun.since = func(time.Time) time.Duration {
		runs++
		if runs == 4 {
			return time.Hour
		}
		return 0
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tun.Start(ctx)
	waitFor(t, "a backoff interval after the stable run", func() bool { return len(fake.recordedDelays()) >= 4 })
	cancel()

	delays := fake.recordedDelays()
	if delays[3] >= delays[2] {
		t.Fatalf("backoff did not reset after a stable run: %v", delays)
	}
	if delays[3] >= time.Millisecond {
		t.Fatalf("post-reset delay %v is not back at the initial floor: %v", delays[3], delays)
	}
}

func TestTunnelContextCancellationStopsRestartsAndRemovesTempDir(t *testing.T) {
	tun, fake := newTestTunnel(t, nil)
	fake.script = func(int) string { return "exit 1" }

	ctx, cancel := context.WithCancel(context.Background())
	done := tun.Start(ctx)
	waitFor(t, "the first ssh invocation", func() bool { return fake.count() >= 1 })
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not exit after context cancellation")
	}

	after := fake.count()
	time.Sleep(20 * time.Millisecond)
	if got := fake.count(); got != after {
		t.Fatalf("supervisor restarted after cancellation: %d -> %d invocations", after, got)
	}
	if _, err := os.Stat(filepath.Dir(tun.LocalSocket())); !os.IsNotExist(err) {
		t.Fatalf("temp dir survived cancellation: stat err = %v", err)
	}
	if tun.Healthy() {
		t.Fatal("Healthy() = true after cancellation")
	}
}

func TestTunnelForwardFailureSurfacesAsError(t *testing.T) {
	tun, fake := newTestTunnel(t, nil)
	// What OpenSSH prints under -o ExitOnForwardFailure=yes when the local
	// endpoint cannot be bound: the process exits instead of pretending to be
	// a healthy connection.
	fake.script = func(int) string {
		return `echo "unix_listener: cannot bind to path /tmp/x.sock: Address already in use" >&2; exit 255`
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tun.Start(ctx)

	readyCtx, readyCancel := context.WithTimeout(ctx, 3*time.Second)
	defer readyCancel()
	err := tun.Ready(readyCtx)
	if err == nil {
		t.Fatal("Ready returned nil for a forward that never came up")
	}
	if !errors.Is(err, ErrForwardFailed) {
		t.Fatalf("err = %v, want ErrForwardFailed", err)
	}
	if !strings.Contains(err.Error(), testDest) {
		t.Fatalf("err %q does not name the destination %q", err, testDest)
	}
	if tun.Healthy() {
		t.Fatal("Healthy() = true for a failed forward")
	}
	if fake.count() == 0 {
		t.Fatal("no ssh invocation was recorded")
	}
}

func TestTunnelOldOpenSSHIsADistinctError(t *testing.T) {
	tun, fake := newTestTunnel(t, nil)
	// Pre-6.7 OpenSSH cannot parse the unix-socket form of -L at all; without
	// classification the user sees an unexplained dial failure instead.
	fake.script = func(int) string {
		return `echo "Bad local forwarding specification '/tmp/a.sock:/tmp/b.sock'" >&2; exit 255`
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tun.Start(ctx)

	readyCtx, readyCancel := context.WithTimeout(ctx, 3*time.Second)
	defer readyCancel()
	err := tun.Ready(readyCtx)
	if !errors.Is(err, ErrUnsupportedForward) {
		t.Fatalf("err = %v, want ErrUnsupportedForward", err)
	}
	if errors.Is(err, ErrForwardFailed) {
		t.Fatalf("old-OpenSSH error also matched ErrForwardFailed: %v", err)
	}
	if !strings.Contains(err.Error(), "6.7") {
		t.Fatalf("err %q does not state the OpenSSH version requirement", err)
	}
}

func TestTunnelTempDirHoldsOnlyTheSocket(t *testing.T) {
	// The daemon token is fetched over ssh and held in memory; nothing may ever
	// write it (or anything else) beside the forwarded socket.
	tun, fake := newTestTunnel(t, nil)
	fake.setBefore(func(int) { listenAt(t, tun.LocalSocket()) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tun.Start(ctx)
	readyCtx, readyCancel := context.WithTimeout(ctx, 3*time.Second)
	defer readyCancel()
	if err := tun.Ready(readyCtx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	dir := filepath.Dir(tun.LocalSocket())
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(tun.LocalSocket()) {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("temp dir contains %v, want only the socket", names)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("temp dir mode = %o, want 0700", perm)
	}
}

func TestTunnelRemovesAStaleSocketBeforeStarting(t *testing.T) {
	// A killed process leaves the socket file behind; binding over it fails,
	// so a fresh tunnel must clear the path first.
	tun, fake := newTestTunnel(t, nil)
	if err := os.WriteFile(tun.LocalSocket(), []byte("stale"), 0o600); err != nil {
		t.Fatalf("seed stale socket: %v", err)
	}
	fake.setBefore(func(int) { listenAt(t, tun.LocalSocket()) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tun.Start(ctx)
	readyCtx, readyCancel := context.WithTimeout(ctx, 3*time.Second)
	defer readyCancel()
	if err := tun.Ready(readyCtx); err != nil {
		t.Fatalf("Ready over a stale socket file: %v", err)
	}
}

func TestTunnelCloseIsIdempotentAndAwaitsTheSupervisor(t *testing.T) {
	tun, fake := newTestTunnel(t, nil)
	fake.setBefore(func(int) { listenAt(t, tun.LocalSocket()) })

	done := tun.Start(context.Background())
	waitFor(t, "the first ssh invocation", func() bool { return fake.count() >= 1 })

	if err := tun.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-done:
	default:
		t.Fatal("Close returned before the supervisor exited")
	}
	if err := tun.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(tun.LocalSocket())); !os.IsNotExist(err) {
		t.Fatalf("temp dir survived Close: stat err = %v", err)
	}
	if tun.Healthy() {
		t.Fatal("Healthy() = true after Close")
	}
}

func TestTunnelStartAfterCloseDoesNotRun(t *testing.T) {
	tun, fake := newTestTunnel(t, nil)
	if err := tun.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	done := tun.Start(context.Background())
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Start after Close did not return a closed channel")
	}
	if fake.count() != 0 {
		t.Fatalf("ran %d ssh invocations after Close, want 0", fake.count())
	}
}

func TestNewTunnelRejectsIncompleteConfig(t *testing.T) {
	if _, err := NewTunnel(TunnelConfig{RemoteSocket: testRemoteSock}); !errors.Is(err, ErrNoDestination) {
		t.Fatalf("err = %v, want ErrNoDestination", err)
	}
	if _, err := NewTunnel(TunnelConfig{Options: Options{Destination: testDest}}); err == nil {
		t.Fatal("want an error when no remote socket is known")
	}
}

func TestNewTunnelFallsBackToTheOptionsRemoteSocket(t *testing.T) {
	// --host-socket lands on Options.RemoteSocket; a caller that passes the
	// override straight through must not end up forwarding an empty path.
	tun, err := NewTunnel(TunnelConfig{
		Options: Options{Destination: testDest, RemoteSocket: "/custom/run/bossd.sock"},
		Logger:  zerolog.Nop(),
	})
	if err != nil {
		t.Fatalf("NewTunnel: %v", err)
	}
	defer func() { _ = tun.Close() }()
	if got := tun.RemoteSocket(); got != "/custom/run/bossd.sock" {
		t.Fatalf("RemoteSocket() = %q", got)
	}
}

func TestTunnelBackoffDefaults(t *testing.T) {
	tun, err := NewTunnel(TunnelConfig{
		Options:      Options{Destination: testDest},
		RemoteSocket: testRemoteSock,
		Logger:       zerolog.Nop(),
	})
	if err != nil {
		t.Fatalf("NewTunnel: %v", err)
	}
	defer func() { _ = tun.Close() }()
	if tun.initialBackoff != defaultInitialBackoff || tun.maxBackoff != defaultMaxBackoff {
		t.Fatalf("defaults = %v/%v, want %v/%v",
			tun.initialBackoff, tun.maxBackoff, defaultInitialBackoff, defaultMaxBackoff)
	}
}

// TestTunnelControlPathIsInsideTheTunnelDirectory: the multiplexing socket must
// live in the same private 0700 directory as the forward. A control socket
// anywhere world-writable would let another local user pre-create the path and
// have boss hand them a connection to the user's remote account.
func TestTunnelControlPathIsInsideTheTunnelDirectory(t *testing.T) {
	tun, err := NewTunnel(TunnelConfig{
		Options:      Options{Destination: testDest},
		RemoteSocket: testRemoteSock,
		Logger:       zerolog.Nop(),
	})
	if err != nil {
		t.Fatalf("NewTunnel: %v", err)
	}
	defer func() { _ = tun.Close() }()

	cp := tun.ControlPath()
	if !multiplexSupported() {
		if cp != "" {
			t.Fatalf("ControlPath() = %q where ssh cannot multiplex, want empty: "+
				"passing ControlMaster to Win32-OpenSSH fails the invocation", cp)
		}
		return
	}
	if cp == "" {
		t.Fatal("ControlPath() = \"\" where ssh can multiplex; uploads would " +
			"open their own ssh and prompt under BatchMode")
	}
	if dir := filepath.Dir(cp); dir != filepath.Dir(tun.LocalSocket()) {
		t.Fatalf("ControlPath() = %q is in %q, want the tunnel's private directory %q",
			cp, dir, filepath.Dir(tun.LocalSocket()))
	}
	if len(cp) > socketPathLimit {
		t.Fatalf("ControlPath() = %q is %d bytes, over the %d sun_path ceiling",
			cp, len(cp), socketPathLimit)
	}
}

// TestUploadSSHOptionsCarryControlPath: the upload helpers must pass the
// session's control socket so a paste rides the tunnel's already
// authenticated connection. BatchMode stays alongside it — a slave whose
// master is gone silently falls back to its own connection, which is the case
// BatchMode exists to keep from wedging the pane.
func TestUploadSSHOptionsCarryControlPath(t *testing.T) {
	got := uploadSSHOptions(Options{Destination: testDest, ControlPath: "/tmp/ctl.sock"})
	want := []string{"-o", "BatchMode=yes", "-o", "ControlPath=/tmp/ctl.sock"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("uploadSSHOptions = %q, want %q", got, want)
	}
}

// TestUploadSSHOptionsWithoutControlPathIsUnchanged pins the pre-multiplexing
// argv for every caller that has no tunnel (a local attach, or a host where
// ssh cannot multiplex): exactly BatchMode, nothing more.
func TestUploadSSHOptionsWithoutControlPathIsUnchanged(t *testing.T) {
	got := uploadSSHOptions(Options{Destination: testDest})
	if !reflect.DeepEqual(got, batchModeOptions) {
		t.Fatalf("uploadSSHOptions = %q, want %q", got, batchModeOptions)
	}
}

// TestUploadSSHOptionsPerCallControlPath: two attaches to different hosts run
// in one process, so a control socket must never leak from one call's options
// into the next. Handing a session the wrong master would point its uploads at
// another host entirely.
//
// This replaces an earlier "does not mutate batchModeOptions" test that was
// removed for being unfalsifiable: batchModeOptions is a full literal, so
// appending to it reallocates and the corruption it claimed to catch cannot
// happen — the test passed identically against an implementation written the
// unsafe way.
func TestUploadSSHOptionsPerCallControlPath(t *testing.T) {
	first := uploadSSHOptions(Options{Destination: testDest, ControlPath: "/tmp/one.sock"})
	second := uploadSSHOptions(Options{Destination: testDest, ControlPath: "/tmp/two.sock"})

	if !containsArg(first, "ControlPath=/tmp/one.sock") {
		t.Fatalf("first call = %q, want ControlPath=/tmp/one.sock", first)
	}
	if !containsArg(second, "ControlPath=/tmp/two.sock") {
		t.Fatalf("second call = %q, want ControlPath=/tmp/two.sock", second)
	}
	if containsArg(second, "ControlPath=/tmp/one.sock") {
		t.Fatalf("second call = %q carries the first call's control socket", second)
	}
	// The first result must still read the same after the second call, so a
	// retained options slice cannot be rewritten underneath its owner.
	if !containsArg(first, "ControlPath=/tmp/one.sock") || containsArg(first, "ControlPath=/tmp/two.sock") {
		t.Fatalf("first call = %q after a second call, want it unchanged", first)
	}
}

// TestTunnelLastFailureIsReadableAfterTheChildExits pins the reader BOS-724
// added: the supervisor already classifies every ssh exit, but until now only
// this package could read it, so a TUI whose tunnel had died could say the
// connection was lost and never say why. LastFailure is that "why", and it must
// stay nil before the first exit rather than reporting a failure that has not
// happened.
func TestTunnelLastFailureIsReadableAfterTheChildExits(t *testing.T) {
	tun, fake := newTestTunnel(t, nil)
	fake.script = func(int) string { return "exit 1" }

	if err := tun.LastFailure(); err != nil {
		t.Fatalf("LastFailure() = %v before the child ran, want nil", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tun.Start(ctx)
	waitFor(t, "the classified failure of an exited child", func() bool { return tun.LastFailure() != nil })

	got := tun.LastFailure().Error()
	// The classified error, not the raw exec one: it names the destination the
	// user asked for, which is the whole point of surfacing it in a view.
	if !strings.Contains(got, testDest) {
		t.Fatalf("LastFailure() = %q, want it to name the destination %q", got, testDest)
	}
	if !strings.Contains(got, "exited") {
		t.Fatalf("LastFailure() = %q, want it to say the tunnel exited", got)
	}
}
