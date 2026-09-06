package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/recurser/boss/internal/client"
	"github.com/recurser/boss/internal/preflight"
	"github.com/recurser/boss/internal/remotehost"
)

// remoteTokenValue is the canned daemon token the fake ssh returns. Tests assert
// it reaches the client and never reaches an error string.
const remoteTokenValue = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// hostSSHRecorder answers the ssh invocations remotehost makes with canned
// output, so the whole --host startup path is exercised without a second
// machine, a network, or real credentials.
type hostSSHRecorder struct {
	mu    sync.Mutex
	calls [][]string
	reply func(args []string) (stdout, stderr string, exit int)
}

func (r *hostSSHRecorder) factory() remotehost.CommandFactory {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		r.mu.Lock()
		r.calls = append(r.calls, append([]string{name}, args...))
		reply := r.reply
		r.mu.Unlock()

		stdout, stderr, exit := "", "", 0
		if reply != nil {
			stdout, stderr, exit = reply(args)
		}
		script := fmt.Sprintf("printf '%%s' %s; printf '%%s' %s >&2; exit %d",
			shellQuoteForTest(stdout), shellQuoteForTest(stderr), exit)
		return exec.CommandContext(ctx, "/bin/sh", "-c", script)
	}
}

func (r *hostSSHRecorder) recorded() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.calls))
	copy(out, r.calls)
	return out
}

func shellQuoteForTest(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// happySSH replies as a healthy remote: `boss env --json` reports a reachable
// daemon, and the token read returns a valid token.
func happySSH(socket string) func(args []string) (string, string, int) {
	return func(args []string) (string, string, int) {
		if len(args) > 1 && args[1] == "boss" {
			return fmt.Sprintf(`{"daemon":{"socket":%q,"reachable":true}}`, socket), "", 0
		}
		return remoteTokenValue, "", 0
	}
}

type fakeHostTunnel struct {
	socket      string
	controlPath string
	healthy     bool
	failure     error
	closed      atomic.Int32
}

func (f *fakeHostTunnel) LocalSocket() string { return f.socket }
func (f *fakeHostTunnel) ControlPath() string { return f.controlPath }
func (f *fakeHostTunnel) Healthy() bool       { return f.healthy }
func (f *fakeHostTunnel) LastFailure() error  { return f.failure }
func (f *fakeHostTunnel) Close() error        { f.closed.Add(1); return nil }

// restoreHostStubs snapshots every --host seam and restores it after the test,
// mirroring restoreDaemonCommandStubs.
func restoreHostStubs(t *testing.T) {
	t.Helper()

	oldFactory := hostCommandFactory
	oldStarter := hostTunnelStarter
	oldPreflight := showHostPreflight
	oldReconnect := showHostReconnectWait
	oldConn := currentHostConnection()
	t.Cleanup(func() {
		hostCommandFactory = oldFactory
		hostTunnelStarter = oldStarter
		showHostPreflight = oldPreflight
		showHostReconnectWait = oldReconnect
		hostConnMu.Lock()
		hostConn = oldConn
		hostConnMu.Unlock()
	})
}

// hostTestCommand mirrors remoteTestCommand for the --host transport.
func hostTestCommand(t *testing.T) *cobra.Command {
	t.Helper()
	return hostTestCommandWith(t, "user@example.test", "")
}

func hostTestCommandWith(t *testing.T, host, hostSocket string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "boss"}
	cmd.Flags().String("host", host, "")
	cmd.Flags().String("host-socket", hostSocket, "")
	cmd.Flags().String("remote", "", "")
	return cmd
}

func TestHostFlagAccessors(t *testing.T) {
	t.Run("reads both flags", func(t *testing.T) {
		cmd := hostTestCommandWith(t, "alias", "/run/bossd.sock")
		if got := hostDestination(cmd); got != "alias" {
			t.Fatalf("hostDestination = %q, want %q", got, "alias")
		}
		if got := hostSocketOverride(cmd); got != "/run/bossd.sock" {
			t.Fatalf("hostSocketOverride = %q, want %q", got, "/run/bossd.sock")
		}
	})

	t.Run("nil safe and absent-flag safe", func(t *testing.T) {
		if got := hostDestination(nil); got != "" {
			t.Fatalf("hostDestination(nil) = %q, want empty", got)
		}
		if got := hostSocketOverride(&cobra.Command{Use: "boss"}); got != "" {
			t.Fatalf("hostSocketOverride without the flag = %q, want empty", got)
		}
	})
}

func TestRequireSingleTransport(t *testing.T) {
	t.Run("rejects host with remote naming both flags", func(t *testing.T) {
		cmd := hostTestCommandWith(t, "user@example.test", "")
		if err := cmd.Flags().Set("remote", "https://orchestrator.example.test"); err != nil {
			t.Fatalf("set remote: %v", err)
		}
		err := requireSingleTransport(cmd)
		if err == nil {
			t.Fatal("expected --host with --remote to be rejected")
		}
		for _, want := range []string{"--host", "--remote"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q must name %s", err.Error(), want)
			}
		}
	})

	t.Run("allows each flag alone", func(t *testing.T) {
		if err := requireSingleTransport(hostTestCommand(t)); err != nil {
			t.Fatalf("--host alone: %v", err)
		}
		if err := requireSingleTransport(remoteTestCommand(t)); err != nil {
			t.Fatalf("--remote alone: %v", err)
		}
		if err := requireSingleTransport(&cobra.Command{Use: "boss"}); err != nil {
			t.Fatalf("plain local command: %v", err)
		}
	})
}

// TestRootRejectsHostWithRemote pins that the exclusion is wired into the root's
// PersistentPreRunE, so it fires for every subcommand before any dial.
func TestRootRejectsHostWithRemote(t *testing.T) {
	root := rootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--host", "user@example.test", "--remote", "https://orchestrator.example.test", "ls"})

	err := root.Execute()
	if err == nil {
		// Elides the middle of a command line, not prose: ellipsis: literal-dots ok
		t.Fatal("expected boss --host ... --remote ... to fail")
	}
	for _, want := range []string{"--host", "--remote"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must name %s", err.Error(), want)
		}
	}
}

// TestRootHelpFlagStillWorks guards the -h shorthand: cobra auto-registers -h
// for --help and evaluates it before PersistentPreRunE, so neither the new
// flags nor the new exclusion check may break `boss -h`.
func TestRootHelpFlagStillWorks(t *testing.T) {
	for _, arg := range []string{"-h", "--help"} {
		t.Run(arg, func(t *testing.T) {
			root := rootCmd()
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs([]string{arg})

			if err := root.Execute(); err != nil {
				t.Fatalf("boss %s: %v", arg, err)
			}
			rendered := out.String()
			if !strings.Contains(rendered, "Usage:") {
				t.Fatalf("boss %s printed no usage: %q", arg, rendered)
			}
			if !strings.Contains(rendered, "--host") {
				t.Fatalf("boss %s should document --host: %q", arg, rendered)
			}
		})
	}
}

// TestIsTailInvocation pins the argv pre-scan that decides whether the file
// logger is attached. Each value-taking root flag must be skipped, or
// `boss --host <dest> tail` would write to the file it is about to read.
func TestIsTailInvocation(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"bare tail", []string{"tail"}, true},
		{"bare cost", []string{"cost"}, true},
		{"remote value skipped", []string{"--remote", "https://x", "tail"}, true},
		{"host value skipped", []string{"--host", "user@example.test", "tail"}, true},
		{"host value skipped for cost", []string{"--host", "user@example.test", "cost"}, true},
		{"host socket value skipped", []string{"--host-socket", "/run/bossd.sock", "tail"}, true},
		{"equals form", []string{"--host=user@example.test", "tail"}, true},
		{"host value is not the subcommand", []string{"--host", "tail"}, false},
		{"other subcommand", []string{"--host", "user@example.test", "ls"}, false},
		{"no args", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTailInvocation(tc.args); got != tc.want {
				t.Fatalf("isTailInvocation(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

func TestRequireLocalRegistration(t *testing.T) {
	t.Run("rejects remote", func(t *testing.T) {
		err := requireLocalRegistration(remoteTestCommand(t), addRegistration(false))
		if err == nil || !strings.Contains(err.Error(), "--remote") {
			t.Fatalf("error = %v, want a message naming --remote", err)
		}
	})

	// A pasted token has no local subprocess in it, but --remote is refused for
	// every shape anyway: there is no remote *machine* behind a cloud
	// orchestrator that the operator could have minted the token on, so allowing
	// it there is a different design question than the --host one (BOS-847).
	t.Run("rejects remote even for a pasted token", func(t *testing.T) {
		err := requireLocalRegistration(remoteTestCommand(t), addRegistration(true))
		if err == nil || !strings.Contains(err.Error(), "--remote") {
			t.Fatalf("error = %v, want a message naming --remote", err)
		}
	})

	t.Run("rejects host", func(t *testing.T) {
		err := requireLocalRegistration(hostTestCommand(t), addRegistration(false))
		if err == nil {
			t.Fatal("expected boss account add against --host to be refused")
		}
		if !strings.Contains(err.Error(), "--host") {
			t.Fatalf("error %q must name --host", err.Error())
		}
		if !strings.Contains(err.Error(), "this machine") {
			t.Fatalf("error %q must explain the agent CLI runs on this machine", err.Error())
		}
		// The refusal is only useful if it carries the way out with it.
		if !strings.Contains(err.Error(), "--token-stdin") {
			t.Fatalf("error %q must point at the shape that does work under --host", err.Error())
		}
		// ...and the way out has to keep the destination. A bare
		// `boss account add claude --token-stdin` is a command that runs, so an
		// operator copies it — straight onto their local daemon, which is the
		// wrong-machine outcome this guard exists to prevent (BOS-847).
		if !strings.Contains(err.Error(), "boss --host user@example.test account add claude --token-stdin") {
			t.Fatalf("error %q must suggest the escape hatch with --host still on it", err.Error())
		}
	})

	// The whole point of the guard is the local subprocess, and
	// `boss account add claude --token-stdin` has none: it reads a token that
	// already exists and stores it through the tunnelled client, so the
	// credential lands on the remote daemon the operator asked for (BOS-847).
	t.Run("allows host for a pasted token", func(t *testing.T) {
		if err := requireLocalRegistration(hostTestCommand(t), addRegistration(true)); err != nil {
			t.Fatalf("claude --token-stdin under --host: %v", err)
		}
	})

	t.Run("allows local", func(t *testing.T) {
		if err := requireLocalRegistration(&cobra.Command{Use: "boss"}, addRegistration(false)); err != nil {
			t.Fatalf("plain local command: %v", err)
		}
	})
}

// TestAccountAddHandlersApplyTheHostGuardBeforeDialling pins the guard at the
// two call sites rather than only on the helper, because which value each
// handler passes for pastedToken IS the policy: codex has no pasted-token shape,
// so it must pass a constant false and stay refused under --host even when the
// operator supplies --token-stdin (BOS-847).
//
// Both cases return before newClient, so nothing here opens an ssh connection.
func TestAccountAddHandlersApplyTheHostGuardBeforeDialling(t *testing.T) {
	t.Run("claude without a pasted token is refused", func(t *testing.T) {
		cmd := hostTestCommand(t)
		// Registered explicitly so the refusal comes from the flag being false
		// rather than from GetBool failing on a flag that was never declared.
		cmd.Flags().Bool("token-stdin", false, "")
		err := runAccountAddClaude(cmd)
		if err == nil || !strings.Contains(err.Error(), "--host") {
			t.Fatalf("error = %v, want the --host refusal", err)
		}
	})

	t.Run("codex is refused even with --token-stdin", func(t *testing.T) {
		cmd := hostTestCommand(t)
		cmd.Flags().Bool("token-stdin", true, "")
		err := runAccountAddCodex(cmd)
		if err == nil || !strings.Contains(err.Error(), "--host") {
			t.Fatalf("error = %v, want the --host refusal rather than the codex "+
				"--token-stdin message; codex must not opt into the pasted-token exemption", err)
		}
	})
}

// TestNewClientHostBranchNeverStartsALocalDaemon is the acceptance criterion:
// asking for a daemon on another machine must not silently boot one here.
func TestNewClientHostBranchNeverStartsALocalDaemon(t *testing.T) {
	restoreHostStubs(t)
	restoreDaemonCommandStubs(t)
	t.Setenv("BOSS_SOCKET", "")
	daemonEnsureRunning = func(string) error {
		t.Fatal("daemonEnsureRunning called for a --host command")
		return nil
	}

	rec := &hostSSHRecorder{reply: happySSH("/remote/run/bossd.sock")}
	hostCommandFactory = rec.factory()
	tunnel := &fakeHostTunnel{socket: filepath.Join(t.TempDir(), "bossd.sock"), healthy: true}
	hostTunnelStarter = func(context.Context, remotehost.TunnelConfig) (hostTunnel, error) {
		return tunnel, nil
	}

	c, err := newClient(hostTestCommand(t))
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	if c == nil {
		t.Fatal("newClient returned a nil client")
	}
	conn := currentHostConnection()
	if conn == nil || conn.tunnel != hostTunnel(tunnel) {
		t.Fatal("the live tunnel was not registered for shutdown")
	}
	if conn.token != remoteTokenValue {
		t.Fatal("the fetched token was not carried into the connection")
	}
}

// TestNewHostClientUsesTheSameDestinationForEveryLeg pins the invariant that
// discovery, the token read, and the forward all address one remote account.
func TestNewHostClientUsesTheSameDestinationForEveryLeg(t *testing.T) {
	restoreHostStubs(t)
	rec := &hostSSHRecorder{reply: happySSH("/remote/run/bossd.sock")}
	hostCommandFactory = rec.factory()

	var gotCfg remotehost.TunnelConfig
	hostTunnelStarter = func(_ context.Context, cfg remotehost.TunnelConfig) (hostTunnel, error) {
		gotCfg = cfg
		return &fakeHostTunnel{socket: filepath.Join(t.TempDir(), "bossd.sock"), healthy: true}, nil
	}

	const dest = "deploy@build-box"
	if _, err := newHostClient(hostTestCommandWith(t, dest, ""), dest); err != nil {
		t.Fatalf("newHostClient: %v", err)
	}
	calls := rec.recorded()
	if len(calls) != 2 {
		t.Fatalf("ssh calls = %d, want 2 (discovery + token): %v", len(calls), calls)
	}
	for _, call := range calls {
		if call[0] != "ssh" || call[1] != dest {
			t.Fatalf("ssh call %v must address %q verbatim as its first argument", call, dest)
		}
	}
	if gotCfg.Destination != dest {
		t.Fatalf("tunnel destination = %q, want %q", gotCfg.Destination, dest)
	}
	if gotCfg.RemoteSocket != "/remote/run/bossd.sock" {
		t.Fatalf("tunnel remote socket = %q, want the discovered path", gotCfg.RemoteSocket)
	}
}

// TestNewHostClientHonoursTheSocketOverride: --host-socket is the escape hatch
// for remotes where boss is not on the ssh PATH, so it must skip discovery.
func TestNewHostClientHonoursTheSocketOverride(t *testing.T) {
	restoreHostStubs(t)
	rec := &hostSSHRecorder{reply: func(args []string) (string, string, int) {
		if len(args) > 1 && args[1] == "boss" {
			t.Error("discovery ran despite --host-socket")
		}
		return remoteTokenValue, "", 0
	}}
	hostCommandFactory = rec.factory()
	hostTunnelStarter = func(_ context.Context, cfg remotehost.TunnelConfig) (hostTunnel, error) {
		if cfg.RemoteSocket != "/opt/bossd.sock" {
			t.Errorf("tunnel remote socket = %q, want the override", cfg.RemoteSocket)
		}
		return &fakeHostTunnel{socket: filepath.Join(t.TempDir(), "bossd.sock"), healthy: true}, nil
	}

	cmd := hostTestCommandWith(t, "user@example.test", "/opt/bossd.sock")
	if _, err := newHostClient(cmd, "user@example.test"); err != nil {
		t.Fatalf("newHostClient: %v", err)
	}
	if got := len(rec.recorded()); got != 1 {
		t.Fatalf("ssh calls = %d, want 1 (token read only)", got)
	}
}

// TestNewHostClientPreservesRemoteSentinels keeps a missing remote `boss`
// distinguishable from a stopped remote daemon: the remedies differ.
func TestNewHostClientPreservesRemoteSentinels(t *testing.T) {
	cases := []struct {
		name  string
		reply func(args []string) (string, string, int)
		want  error
	}{
		{
			name: "remote boss missing",
			reply: func([]string) (string, string, int) {
				return "", "bash: boss: command not found", 127
			},
			want: remotehost.ErrRemoteBossMissing,
		},
		{
			name: "remote daemon down",
			reply: func([]string) (string, string, int) {
				return `{"daemon":{"socket":"","reachable":false}}`, "", 0
			},
			want: remotehost.ErrRemoteDaemonDown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			restoreHostStubs(t)
			rec := &hostSSHRecorder{reply: tc.reply}
			hostCommandFactory = rec.factory()
			hostTunnelStarter = func(context.Context, remotehost.TunnelConfig) (hostTunnel, error) {
				t.Fatal("the tunnel must not start after a failed discovery")
				return nil, nil
			}

			_, err := newHostClient(hostTestCommand(t), "user@example.test")
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want errors.Is %v", err, tc.want)
			}
			if !strings.Contains(err.Error(), "user@example.test") {
				t.Fatalf("error %q must name the host", err.Error())
			}
		})
	}
}

// TestNewHostClientNeverLeaksTheToken: the daemon token is memory-only, so no
// failure path may splice it into a message the user (or a log) sees.
func TestNewHostClientNeverLeaksTheToken(t *testing.T) {
	restoreHostStubs(t)
	rec := &hostSSHRecorder{reply: happySSH("/remote/run/bossd.sock")}
	hostCommandFactory = rec.factory()
	hostTunnelStarter = func(context.Context, remotehost.TunnelConfig) (hostTunnel, error) {
		return nil, errors.New("ssh: connect to host example.test port 22: Connection refused")
	}

	_, err := newHostClient(hostTestCommand(t), "user@example.test")
	if err == nil {
		t.Fatal("expected a failed tunnel start to surface")
	}
	if strings.Contains(err.Error(), remoteTokenValue) {
		t.Fatal("the daemon token leaked into an error message")
	}
	if currentHostConnection() != nil {
		t.Fatal("a failed startup must not register a connection")
	}
}

// TestShutdownHostTunnelRemovesTheTunnelDirectory uses a real Tunnel so the
// private directory it creates is really cleaned up — a leaked one would
// accumulate a 0700 temp dir per boss invocation.
func TestShutdownHostTunnelRemovesTheTunnelDirectory(t *testing.T) {
	restoreHostStubs(t)
	tunnel, err := remotehost.NewTunnel(remotehost.TunnelConfig{
		Options:      remotehost.Options{Destination: "user@example.test"},
		RemoteSocket: "/remote/run/bossd.sock",
	})
	if err != nil {
		t.Fatalf("NewTunnel: %v", err)
	}
	dir := filepath.Dir(tunnel.LocalSocket())
	if _, statErr := os.Stat(dir); statErr != nil {
		t.Fatalf("tunnel directory missing before shutdown: %v", statErr)
	}

	registerHostConnection(&hostConnection{destination: "user@example.test", tunnel: tunnel, token: remoteTokenValue})
	shutdownHostTunnel()

	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatalf("tunnel directory %s survived shutdown (stat err = %v)", dir, statErr)
	}
	if currentHostConnection() != nil {
		t.Fatal("shutdown must clear the registered connection")
	}
	shutdownHostTunnel() // idempotent: a second shutdown must not panic.
}

// TestRegisterHostConnectionClosesThePrevious documents the double-call
// decision: at most one tunnel is registered, so an earlier ssh child and its
// directory can never be orphaned.
func TestRegisterHostConnectionClosesThePrevious(t *testing.T) {
	restoreHostStubs(t)
	first := &fakeHostTunnel{socket: "/tmp/first.sock"}
	second := &fakeHostTunnel{socket: "/tmp/second.sock"}

	registerHostConnection(&hostConnection{tunnel: first})
	registerHostConnection(&hostConnection{tunnel: second})

	if got := first.closed.Load(); got != 1 {
		t.Fatalf("first tunnel closed %d times, want 1", got)
	}
	if got := second.closed.Load(); got != 0 {
		t.Fatalf("second tunnel closed %d times, want 0", got)
	}
	shutdownHostTunnel()
	if got := second.closed.Load(); got != 1 {
		t.Fatalf("second tunnel closed %d times after shutdown, want 1", got)
	}
}

// TestHostReconnectIssue pins the two on-screen tokens a proof scenario
// asserts. Rewording either breaks that scenario.
func TestHostReconnectIssue(t *testing.T) {
	issue := hostReconnectIssue("user@example.test", "dial unix: connection refused")
	if !strings.Contains(issue.Title, "Reconnecting") {
		t.Fatalf("title %q must contain %q", issue.Title, "Reconnecting")
	}
	if !strings.Contains(issue.Title, "user@example.test") {
		t.Fatalf("title %q must name the host", issue.Title)
	}
	if !strings.Contains(issue.Detail, "dial unix: connection refused") {
		t.Fatalf("detail %q must carry the failure detail", issue.Detail)
	}
	if !strings.Contains(issue.Detail, "user@example.test") {
		t.Fatalf("detail %q must name the host", issue.Detail)
	}
}

// TestHostStartupErrorReachesTheDaemonWait: unlike --remote, a --host failure
// must route into waitForDaemon so the reconnect screen gets its chance.
func TestHostStartupErrorReachesTheDaemonWait(t *testing.T) {
	startupErr := errors.New("dial unix: connection refused")
	waitCalled := false

	_, gotErr := handleClientStartupError(hostTestCommand(t), startupErr, func() (client.BossClient, error) {
		waitCalled = true
		return nil, startupErr
	})
	if !waitCalled {
		t.Fatal("a --host startup error must reach the daemon wait, not bypass it")
	}
	if !errors.Is(gotErr, startupErr) {
		t.Fatalf("error = %v, want %v", gotErr, startupErr)
	}
}

// TestWaitForDaemonHostBranch: a dial failure with a supervised tunnel behind
// it must poll that tunnel and resume, and a setup failure with no tunnel must
// take the static screen and propagate. Startup is the only caller, so the
// second case is the one production takes; a drop after startup is the home
// screen's job (see TestDaemonDownRemediationNamesTheHost).
func TestWaitForDaemonHostBranch(t *testing.T) {
	t.Run("recovers through the reconnect screen", func(t *testing.T) {
		restoreHostStubs(t)
		tunnel := &fakeHostTunnel{
			socket:  filepath.Join(t.TempDir(), "bossd.sock"),
			healthy: true,
			failure: errors.New("remotehost: ssh tunnel to user@example.test exited: signal: terminated"),
		}
		registerHostConnection(&hostConnection{
			destination: "user@example.test",
			tunnel:      tunnel,
			token:       remoteTokenValue,
		})

		var gotHost, gotDetail string
		var gotHealthy bool
		showHostReconnectWait = func(host, detail string, healthy func() bool) error {
			gotHost, gotDetail, gotHealthy = host, detail, healthy()
			return nil
		}
		showHostPreflight = func(preflight.Issue) error {
			t.Fatal("the static screen must not be used while a tunnel is live")
			return nil
		}

		c, err := waitForDaemon(hostTestCommand(t), errors.New("dial unix: connection refused"))
		if err != nil {
			t.Fatalf("waitForDaemon: %v", err)
		}
		if c == nil {
			t.Fatal("waitForDaemon returned a nil client after recovery")
		}
		if gotHost != "user@example.test" {
			t.Fatalf("reconnect screen host = %q", gotHost)
		}
		// BOS-724: the screen reports the supervisor's own diagnosis, not the
		// dial error against the forwarded socket — that error only ever names
		// a local temp path the user cannot act on.
		if !strings.Contains(gotDetail, "signal: terminated") {
			t.Fatalf("reconnect detail = %q, want the tunnel's classified failure", gotDetail)
		}
		if strings.Contains(gotDetail, "dial unix") {
			t.Fatalf("reconnect detail = %q must never carry the raw forwarded-socket dial error", gotDetail)
		}
		if !gotHealthy {
			t.Fatal("the reconnect screen must poll the live tunnel's Healthy")
		}
	})

	t.Run("propagates a setup failure with no tunnel", func(t *testing.T) {
		restoreHostStubs(t)
		hostConnMu.Lock()
		hostConn = nil
		hostConnMu.Unlock()

		shown := false
		showHostPreflight = func(preflight.Issue) error { shown = true; return nil }
		showHostReconnectWait = func(string, string, func() bool) error {
			t.Fatal("no tunnel means nothing to reconnect to")
			return nil
		}

		origErr := errors.New("ssh: could not resolve hostname")
		_, err := waitForDaemon(hostTestCommand(t), origErr)
		if !errors.Is(err, origErr) {
			t.Fatalf("error = %v, want the original %v", err, origErr)
		}
		if !shown {
			t.Fatal("the static preflight screen should explain the failure")
		}
	})
}

// hostDaemonSubcommand builds a `boss <path...>` tree carrying --host on the
// root, so the subtree guard can be asserted on the real CommandPath without
// executing anything.
func hostDaemonSubcommand(t *testing.T, host string, path ...string) *cobra.Command {
	t.Helper()
	root := &cobra.Command{Use: "boss"}
	root.Flags().String("host", host, "")
	root.Flags().String("host-socket", "", "")
	root.Flags().String("remote", "", "")
	cmd := root
	for _, name := range path {
		child := &cobra.Command{Use: name}
		cmd.AddCommand(child)
		cmd = child
	}
	return cmd
}

// TestRequireLocalDaemonTarget: the `boss daemon` subtree installs, starts,
// stops, and uninstalls *this* machine's bossd through local subprocesses, so
// under --host it would act on the wrong daemon entirely. It must be refused,
// and only that subtree.
func TestRequireLocalDaemonTarget(t *testing.T) {
	t.Run("rejects every daemon subcommand under host", func(t *testing.T) {
		for _, sub := range []string{"install", "doctor", "uninstall", "status", "start", "stop", "restart", "rotate-token"} {
			t.Run(sub, func(t *testing.T) {
				err := requireLocalDaemonTarget(hostDaemonSubcommand(t, "user@example.test", "daemon", sub))
				if err == nil {
					// Elides the middle of a command line, not prose: ellipsis: literal-dots ok
					t.Fatalf("boss --host ... daemon %s must be refused", sub)
				}
				for _, want := range []string{"--host", "user@example.test", "ssh user@example.test boss daemon"} {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("error %q must contain %q", err.Error(), want)
					}
				}
			})
		}
	})

	t.Run("rejects the bare daemon command", func(t *testing.T) {
		if err := requireLocalDaemonTarget(hostDaemonSubcommand(t, "alias", "daemon")); err == nil {
			t.Fatal("boss --host alias daemon must be refused")
		}
	})

	t.Run("allows the daemon subtree without host", func(t *testing.T) {
		for _, sub := range []string{"status", "stop", "restart"} {
			if err := requireLocalDaemonTarget(hostDaemonSubcommand(t, "", "daemon", sub)); err != nil {
				t.Fatalf("local boss daemon %s: %v", sub, err)
			}
		}
	})

	t.Run("allows other commands under host", func(t *testing.T) {
		// Only the daemon subtree is local-only; --host exists so everything
		// else reaches the remote daemon, and a guard that caught them all
		// would defeat the flag.
		for _, path := range [][]string{{"ls"}, {"session", "new"}, {"repo", "ls"}} {
			cmd := hostDaemonSubcommand(t, "user@example.test", path...)
			if err := requireLocalDaemonTarget(cmd); err != nil {
				// Elides the middle of a command line, not prose: ellipsis: literal-dots ok
				t.Fatalf("boss --host ... %s: %v", cmd.CommandPath(), err)
			}
		}
		// A command whose name merely starts with "daemon" is not the subtree.
		if err := requireLocalDaemonTarget(hostDaemonSubcommand(t, "user@example.test", "daemonize")); err != nil {
			t.Fatalf("boss daemonize is not the daemon subtree: %v", err)
		}
	})

	t.Run("nil safe", func(t *testing.T) {
		if err := requireLocalDaemonTarget(nil); err != nil {
			t.Fatalf("nil command: %v", err)
		}
	})
}

// TestRootRejectsDaemonSubtreeAgainstHost pins that the guard is wired into the
// root's PersistentPreRunE, so it fires before RunE — that is, before any local
// launchd/systemd subprocess runs.
func TestRootRejectsDaemonSubtreeAgainstHost(t *testing.T) {
	for _, sub := range []string{"status", "stop", "uninstall"} {
		t.Run(sub, func(t *testing.T) {
			root := rootCmd()
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs([]string{"--host", "user@example.test", "daemon", sub})

			err := root.Execute()
			if err == nil {
				// Elides the middle of a command line, not prose: ellipsis: literal-dots ok
				t.Fatalf("boss --host ... daemon %s must fail", sub)
			}
			if !strings.Contains(err.Error(), "--host user@example.test") {
				t.Fatalf("error %q must name the destination", err.Error())
			}
		})
	}
}

// TestSupervisedTunnelIsUnhealthyOnceTheSupervisorExits is the BOS-724 half of
// the done-channel criterion: a supervisor goroutine that has unwound is a
// tunnel that will never redial, and before this wrapper existed nothing
// noticed — the underlying Tunnel keeps answering from its own state. The
// wrapped tunnel reports healthy while the supervisor runs and unhealthy the
// moment it stops, whatever the inner tunnel still claims.
func TestSupervisedTunnelIsUnhealthyOnceTheSupervisorExits(t *testing.T) {
	inner := &fakeHostTunnel{socket: "/tmp/forwarded.sock", healthy: true}
	done := make(chan struct{})
	tunnel := &supervisedTunnel{hostTunnel: inner, done: done}

	if !tunnel.Healthy() {
		t.Fatal("Healthy() = false while the supervisor is running and the forward is up")
	}
	close(done)
	if tunnel.Healthy() {
		t.Fatal("Healthy() = true after the supervisor exited; an exited supervisor must be observable as unhealthy")
	}
	// The rest of the interface still delegates, so the wrapper is a status
	// overlay rather than a second implementation.
	if got := tunnel.LocalSocket(); got != inner.socket {
		t.Fatalf("LocalSocket() = %q, want the wrapped tunnel's %q", got, inner.socket)
	}
	if err := tunnel.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := inner.closed.Load(); got != 1 {
		t.Fatalf("inner tunnel closed %d times, want 1", got)
	}
}

// TestStartHostTunnelConsumesTheSupervisorDoneChannel pins the contract
// Tunnel.Start documents ("callers must consume it"): startHostTunnel discarded
// it, so nothing could tell a live supervisor from a dead one. The channel it
// keeps must be the supervisor's own — it closes when the supervisor unwinds,
// and the tunnel reports unhealthy from then on.
func TestStartHostTunnelConsumesTheSupervisorDoneChannel(t *testing.T) {
	restoreHostStubs(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The fake ssh binds the forward itself (in Go, since /bin/sh cannot) and
	// then idles, so Ready sees a live socket without a second machine.
	factory := func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		if local := localSocketFromForwardSpec(args); local != "" {
			if ln, err := net.Listen("unix", local); err == nil {
				go func() { <-ctx.Done(); _ = ln.Close() }()
			}
		}
		return exec.CommandContext(ctx, "/bin/sh", "-c", "sleep 30")
	}

	tunnel, err := startHostTunnel(ctx, remotehost.TunnelConfig{
		Options:      remotehost.Options{Destination: "user@example.test", CommandFactory: factory},
		RemoteSocket: "/remote/run/bossd.sock",
	})
	if err != nil {
		t.Fatalf("startHostTunnel: %v", err)
	}
	t.Cleanup(func() { _ = tunnel.Close() })

	supervised, ok := tunnel.(*supervisedTunnel)
	if !ok {
		t.Fatalf("startHostTunnel returned %T; it must keep Start's done channel, not discard it", tunnel)
	}
	if supervised.done == nil {
		t.Fatal("the supervisor's done channel was discarded")
	}
	if !supervised.Healthy() {
		t.Fatal("Healthy() = false with the forward up and the supervisor running")
	}

	cancel()
	select {
	case <-supervised.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the retained channel never closed, so it is not the supervisor's own")
	}
	if supervised.Healthy() {
		t.Fatal("Healthy() = true after the supervisor unwound")
	}
}

// localSocketFromForwardSpec pulls the local side out of ssh's `-L local:remote`
// argument so a fake ssh can bind the forward the real one would have created.
func localSocketFromForwardSpec(args []string) string {
	for i, arg := range args {
		if arg != "-L" || i+1 >= len(args) {
			continue
		}
		if local, _, found := strings.Cut(args[i+1], ":"); found {
			return local
		}
	}
	return ""
}

// TestHostReconnectDetailNeverLeaksTheDialError is acceptance criterion 2 at the
// cmd boundary: under --host every RPC dials the forwarded socket, so the raw
// failure reads "dial unix /var/folders/…/bossd.sock: connect: no such file or
// directory" — a local temp path the user cannot act on, and the exact string
// that reached a user verbatim. The screen shows the supervisor's classified
// reason instead, or a fixed sentence when there is not one yet.
func TestHostReconnectDetailNeverLeaksTheDialError(t *testing.T) {
	t.Run("reports the classified failure", func(t *testing.T) {
		got := hostReconnectDetail("remotehost: ssh tunnel to user@example.test exited: signal: killed")
		if !strings.Contains(got, "signal: killed") {
			t.Fatalf("detail = %q, want the classified reason", got)
		}
	})

	t.Run("falls back to a fixed sentence", func(t *testing.T) {
		for _, reason := range []string{"", "   "} {
			got := hostReconnectDetail(reason)
			if strings.TrimSpace(got) == "" {
				t.Fatalf("detail for %q is empty; the screen would show a blank line", reason)
			}
			if strings.Contains(got, "dial unix") {
				t.Fatalf("detail = %q must never carry the forwarded-socket dial error", got)
			}
		}
	})
}

// TestWaitForHostDaemonDetailCarriesNoDialErrorOrToken covers both hygiene
// criteria on the one string the wait screen renders: the raw dial error is
// gone (AC2) and the daemon token, which lives beside the tunnel in
// hostConnection, is nowhere near it (AC5).
func TestWaitForHostDaemonDetailCarriesNoDialErrorOrToken(t *testing.T) {
	const rawDialError = "unavailable: dial unix /var/folders/x/T/boss-host-4081179615/bossd.sock: connect: no such file or directory"

	cases := []struct {
		name    string
		failure error
		want    string
	}{
		{
			name:    "with a classified failure",
			failure: errors.New("remotehost: ssh tunnel to user@example.test exited: signal: terminated"),
			want:    "signal: terminated",
		},
		{
			name: "with no failure recorded yet",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			restoreHostStubs(t)
			tunnel := &fakeHostTunnel{
				socket:  filepath.Join(t.TempDir(), "bossd.sock"),
				healthy: true,
				failure: tc.failure,
			}
			registerHostConnection(&hostConnection{
				destination: "user@example.test",
				tunnel:      tunnel,
				token:       remoteTokenValue,
			})

			var gotDetail string
			showHostReconnectWait = func(_, detail string, _ func() bool) error {
				gotDetail = detail
				return nil
			}

			if _, err := waitForHostDaemon("user@example.test", errors.New(rawDialError)); err != nil {
				t.Fatalf("waitForHostDaemon: %v", err)
			}
			if strings.Contains(gotDetail, "dial unix") {
				t.Fatalf("detail = %q leaks the forwarded-socket dial error", gotDetail)
			}
			if strings.Contains(gotDetail, "bossd.sock") {
				t.Fatalf("detail = %q names the forwarded socket path", gotDetail)
			}
			if strings.Contains(gotDetail, remoteTokenValue) {
				t.Fatalf("detail = %q leaks the daemon token", gotDetail)
			}
			if strings.TrimSpace(gotDetail) == "" {
				t.Fatal("the wait screen was given an empty detail")
			}
			if tc.want != "" && !strings.Contains(gotDetail, tc.want) {
				t.Fatalf("detail = %q, want it to contain %q", gotDetail, tc.want)
			}
		})
	}
}

// TestTunnelFailureReasonIsSafeToRender covers what classifyExit actually hands
// back: it splices the ssh child's stderr into the classified error, redacting
// only token-shaped runs. Everything else is content the *remote* host chooses —
// a MOTD, a banner, a multi-line kex dump — and BOS-724 is what turned that
// value into text rendered behind the alt screen. A stray ESC repaints the
// frame, a \n breaks the panel, and an unbounded value pushes the action bar out
// of view, which is the one affordance the reconnect state exists to keep.
func TestTunnelFailureReasonIsSafeToRender(t *testing.T) {
	restoreHostStubs(t)

	for _, tc := range []struct {
		name    string
		failure error
		assert  func(t *testing.T, got string)
	}{
		{
			name:    "an escape sequence never reaches the frame",
			failure: errors.New("remotehost: ssh tunnel exited: \x1b[2J\x1b[1;31mbanner\x07"),
			assert: func(t *testing.T, got string) {
				if strings.ContainsRune(got, 0x1b) || strings.ContainsRune(got, 0x07) {
					t.Fatalf("reason = %q still carries a control byte", got)
				}
				if !strings.Contains(got, "banner") {
					t.Fatalf("reason = %q dropped the readable text with the escapes", got)
				}
			},
		},
		{
			name:    "a multi-line stderr is flattened to one line",
			failure: errors.New("remotehost: ssh tunnel exited\nbanner line one\r\n\tbanner line two"),
			assert: func(t *testing.T, got string) {
				if strings.ContainsAny(got, "\n\r\t") {
					t.Fatalf("reason = %q is still multi-line", got)
				}
				if strings.Contains(got, "  ") {
					t.Fatalf("reason = %q renders as a corridor of spaces", got)
				}
				if !strings.Contains(got, "banner line one banner line two") {
					t.Fatalf("reason = %q lost the flattened detail", got)
				}
			},
		},
		{
			name:    "an unbounded banner cannot push the action bar off screen",
			failure: errors.New("remotehost: " + strings.Repeat("motd ", 400)),
			assert: func(t *testing.T, got string) {
				if len(got) > hostTunnelReasonLimit+len("…") {
					t.Fatalf("reason is %d bytes, want at most %d", len(got), hostTunnelReasonLimit+len("…"))
				}
				if !strings.HasSuffix(got, "…") {
					t.Fatalf("reason = %q was truncated without saying so", got)
				}
			},
		},
		{
			name: "a multi-byte rune is never cut in half",
			// Deliberately a 3-byte rune. A 2-byte one divides the limit
			// exactly, so the naive slice would land on a boundary anyway and
			// the case would pass with the rune-boundary repair deleted —
			// a guard that cannot fail is not one.
			failure: errors.New(strings.Repeat("→", hostTunnelReasonLimit)),
			assert: func(t *testing.T, got string) {
				if !utf8.ValidString(got) {
					t.Fatalf("reason = %q is not valid UTF-8", got)
				}
			},
		},
		{
			name:    "an ordinary one-line classification is untouched",
			failure: errors.New("remotehost: ssh tunnel to user@example.test exited: signal: terminated"),
			assert: func(t *testing.T, got string) {
				if got != "remotehost: ssh tunnel to user@example.test exited: signal: terminated" {
					t.Fatalf("reason = %q, want the classification verbatim", got)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.assert(t, tunnelFailureReason(&fakeHostTunnel{failure: tc.failure}))
		})
	}
}

// TestHostSupervisorRunningIsNotTunnelHealth is the case that distinguishes the
// two facts, and the one a faked seam cannot express.
//
// Healthy() means "the forward is usable right now": runOnce clears running the
// instant cmd.Wait returns, so it is false for the WHOLE backoff window of an
// ordinary drop — exactly the window in which supervise is alive and about to
// redial, and exactly the window in which a failing RPC brings the user to the
// reconnect screen. Feeding that to the view layer told every user of a routine
// blip that the supervisor had stopped and they must restart boss, which is the
// inverse of what this ticket ships. The seam therefore reads the done channel
// and nothing else.
func TestHostSupervisorRunningIsNotTunnelHealth(t *testing.T) {
	restoreHostStubs(t)

	done := make(chan struct{})
	// Mid-backoff: the forward is down, the supervisor is very much alive.
	inner := &fakeHostTunnel{socket: "/tmp/forwarded.sock", healthy: false}
	supervised := &supervisedTunnel{hostTunnel: inner, done: done}
	registerHostConnection(&hostConnection{destination: "user@example.test", tunnel: supervised, token: remoteTokenValue})

	if !hostSupervisorRunning() {
		t.Fatal("hostSupervisorRunning() = false while the tunnel was merely mid-backoff; the reconnect screen would tell the user to restart boss on every blip")
	}
	if supervised.Healthy() {
		t.Fatal("Healthy() = true with the forward down; the two facts must stay distinct")
	}

	close(done)
	if hostSupervisorRunning() {
		t.Fatal("hostSupervisorRunning() = true after the supervisor unwound")
	}
	if supervised.Healthy() {
		t.Fatal("Healthy() = true after the supervisor unwound")
	}
}

// TestHostSupervisorRunningReadsTheRegistry pins the liveness seam's defaults.
// Anything that cannot answer must read as running: neither "--host was never
// used" nor "this transport has no done channel" is evidence a supervisor died,
// and a false claim would tell a user to restart boss over nothing.
func TestHostSupervisorRunningReadsTheRegistry(t *testing.T) {
	restoreHostStubs(t)

	hostConnMu.Lock()
	hostConn = nil
	hostConnMu.Unlock()
	if !hostSupervisorRunning() {
		t.Fatal("hostSupervisorRunning() = false with no transport, want true")
	}

	// A bare tunnel cannot answer, so it must not be read as a dead supervisor.
	registerHostConnection(&hostConnection{
		destination: "user@example.test",
		tunnel:      &fakeHostTunnel{socket: "/tmp/forwarded.sock", healthy: false},
		token:       remoteTokenValue,
	})
	if !hostSupervisorRunning() {
		t.Fatal("hostSupervisorRunning() = false for a transport that cannot answer, want true")
	}
}

// TestHostTunnelFailureReasonReadsTheRegistry pins the seam the view layer is
// given: the reason is read from the live connection at render time (so a
// re-registered or torn-down transport is reported honestly) and is derived
// from tunnel errors alone, never from the token held beside it.
func TestHostTunnelFailureReasonReadsTheRegistry(t *testing.T) {
	restoreHostStubs(t)

	hostConnMu.Lock()
	hostConn = nil
	hostConnMu.Unlock()
	if got := hostTunnelFailureReason(); got != "" {
		t.Fatalf("hostTunnelFailureReason() = %q with no transport, want empty", got)
	}

	tunnel := &fakeHostTunnel{socket: "/tmp/forwarded.sock"}
	registerHostConnection(&hostConnection{
		destination: "user@example.test",
		tunnel:      tunnel,
		token:       remoteTokenValue,
	})
	if got := hostTunnelFailureReason(); got != "" {
		t.Fatalf("hostTunnelFailureReason() = %q before any failure, want empty", got)
	}

	tunnel.failure = errors.New("remotehost: ssh tunnel to user@example.test exited: signal: hangup")
	got := hostTunnelFailureReason()
	if !strings.Contains(got, "signal: hangup") {
		t.Fatalf("hostTunnelFailureReason() = %q, want the classified failure", got)
	}
	if strings.Contains(got, remoteTokenValue) {
		t.Fatalf("hostTunnelFailureReason() = %q leaks the daemon token", got)
	}
}
