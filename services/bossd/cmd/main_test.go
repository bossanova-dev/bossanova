package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	"go.uber.org/goleak"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/daemonstate"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/telemetry"
	"github.com/recurser/bossd/internal/chatupload"
	"github.com/recurser/bossd/internal/server"
)

type sendChatMessageStub struct {
	response *connect.Response[bossanovav1.SendChatMessageResponse]
	err      error
}

func (s sendChatMessageStub) SendChatMessage(context.Context, *connect.Request[bossanovav1.SendChatMessageRequest]) (*connect.Response[bossanovav1.SendChatMessageResponse], error) {
	return s.response, s.err
}

func TestDeliverChatMessageRejectsUndeliveredResponses(t *testing.T) {
	tests := []struct {
		name string
		stub sendChatMessageStub
	}{
		{name: "rpc error", stub: sendChatMessageStub{err: errors.New("unavailable")}},
		{name: "nil response", stub: sendChatMessageStub{}},
		{name: "nil message", stub: sendChatMessageStub{response: connect.NewResponse[bossanovav1.SendChatMessageResponse](nil)}},
		{name: "undelivered", stub: sendChatMessageStub{response: connect.NewResponse(&bossanovav1.SendChatMessageResponse{Delivered: false})}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := deliverChatMessage(context.Background(), tt.stub, "chat-1", "message"); err == nil {
				t.Fatal("deliverChatMessage() error = nil, want error")
			}
		})
	}
}

// TestDeliverChatMessageErrorNamesDeliveryState guards the diagnosis that
// BOS-598 moved. An unconfirmed submit used to arrive here as a CodeInternal
// error whose message the rpc-error branch wrapped and logged; it now arrives as
// a SUCCESSFUL rpc carrying delivered=false. All three workers sharing this
// primitive (callback delivery, transient resume, broadcast delivery) log only
// what it returns, so collapsing that to a bare "not delivered" would erase the
// reason for exactly the case that is hardest to diagnose — and would hide that
// the capped-backoff retry they schedule can double-deliver a message the pane
// may already be running.
func TestDeliverChatMessageErrorNamesDeliveryState(t *testing.T) {
	tests := []struct {
		name     string
		response *bossanovav1.SendChatMessageResponse
		want     []string
		// wantOnce are substrings the rendered reason must state exactly once.
		// The server's notice_text already ends with the resend guidance, so a
		// headline that repeats it prints the same sentence twice in one log line.
		wantOnce []string
	}{
		{
			name: "unconfirmed carries the state and the notice",
			response: &bossanovav1.SendChatMessageResponse{
				Delivered:     false,
				DeliveryState: bossanovav1.SendChatMessageResponse_DELIVERY_STATE_UNCONFIRMED,
				NoticeText:    "message delivery could not be confirmed: capture-pane failed",
			},
			want:     []string{"unconfirmed", "may already have been submitted", "capture-pane failed"},
			wantOnce: []string{"may already have been submitted"},
		},
		{
			name: "unconfirmed states the server's own guidance once",
			response: &bossanovav1.SendChatMessageResponse{
				Delivered:     false,
				DeliveryState: bossanovav1.SendChatMessageResponse_DELIVERY_STATE_UNCONFIRMED,
				NoticeText: "message delivery could not be confirmed on tmux session \"boss-1\": capture-pane failed; " +
					"the message may already have been submitted; check the pane before resending",
			},
			want:     []string{"unconfirmed", "check the pane before resending"},
			wantOnce: []string{"may already have been submitted"},
		},
		{
			name: "not submitted names the payload's known state",
			response: &bossanovav1.SendChatMessageResponse{
				Delivered:     false,
				DeliveryState: bossanovav1.SendChatMessageResponse_DELIVERY_STATE_NOT_SUBMITTED,
			},
			want: []string{"not submitted"},
		},
		{
			name:     "unset state keeps the generic reason",
			response: &bossanovav1.SendChatMessageResponse{Delivered: false},
			want:     []string{"not delivered"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := sendChatMessageStub{response: connect.NewResponse(tt.response)}
			err := deliverChatMessage(context.Background(), stub, "chat-1", "message")
			if err == nil {
				t.Fatal("deliverChatMessage() error = nil, want error")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("deliverChatMessage() error = %q, want it to mention %q", err, want)
				}
			}
			for _, once := range tt.wantOnce {
				if got := strings.Count(err.Error(), once); got != 1 {
					t.Errorf("deliverChatMessage() error = %q, mentions %q %d times, want exactly once", err, once, got)
				}
			}
		})
	}
}

// TestRun_GracefulShutdown_NoGoroutineLeak boots the full daemon with an
// isolated DB + socket, sends a synthetic SIGTERM, and asserts run() returns
// within 10s without leaking goroutines. This is the end-to-end guard for
// the shutdownWG discipline added in the Sprint 2 FL1 work: if any daemon
// goroutine is spawned without being tracked (or respects a ctx that doesn't
// fire during shutdown), goleak will catch it here.
//
// The hung-plugin Ping scenario is covered at the plugin-host level by
// plugin.TestStopNoGoroutineLeak — Kill() fires SIGTERM/SIGKILL regardless
// of Ping state, and the pingAll loop now snapshots under the lock and
// times out each ping outside it.
func TestRun_GracefulShutdown_NoGoroutineLeak(t *testing.T) {
	// lumberjack's mill goroutine (log rotation worker) has no public stop hook;
	// Close() shuts the file but not the goroutine. It's a known upstream quirk
	// (natefinch/lumberjack#56) and benign — the goroutine dies when the process
	// exits. Ignore it so this test still catches leaks we own.
	defer goleak.VerifyNone(t,
		goleak.IgnoreCurrent(),
		goleak.IgnoreAnyFunction("gopkg.in/natefinch/lumberjack%2ev2.(*Logger).millRun"),
	)

	// Use a short tempdir under /tmp so the unix socket path stays under
	// the 104-byte sun_path limit on macOS. t.TempDir() on darwin expands
	// into /private/var/folders/... which can push us past the limit once
	// we append the Library/Application Support/... suffix.
	baseDir, err := os.MkdirTemp("/tmp", "bossdtest-")
	if err != nil {
		t.Fatalf("mkdir base: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(baseDir) })

	// Isolate $HOME so config.Load, skilldata, and other HOME-relative
	// lookups don't touch the developer's real bossd state.
	t.Setenv("HOME", baseDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(baseDir, ".config"))
	t.Setenv("BOSS_SETTINGS_PATH", filepath.Join(baseDir, "settings.json"))
	// Opt out of the cloud orchestrator: avoids real network I/O during
	// the test (which would otherwise leak an http2 readLoop goroutine
	// and hit the real keychain, popping the "allow access to Bossanova
	// keychain" prompt on every developer run). Local-only mode is a
	// first-class production path, so this still exercises the same
	// shutdown code — just without the upstream Manager.
	t.Setenv("BOSSD_ORCHESTRATOR_URL", "")

	dbPath := filepath.Join(baseDir, "bossd.db")
	socketPath := filepath.Join(baseDir, "bossd.sock")

	stopSig := make(chan os.Signal, 1)
	ready := make(chan struct{})

	done := make(chan error, 1)
	go func() {
		done <- run(runOpts{
			stopSig:    stopSig,
			dbPath:     dbPath,
			socketPath: socketPath,
			plugins:    []config.PluginConfig{}, // disable discovery
			onReady:    func() { close(ready) },
		})
	}()

	// Wait for the daemon's startup to reach the ready point (all
	// goroutines launched, server listening).
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("run exited before ready: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("daemon did not reach ready state within 15s")
	}

	// Give the server goroutine a moment to actually bind the socket
	// before we shut down, so Shutdown has something to stop.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socketPath); err == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	stopSig <- syscall.SIGTERM

	// Shutdown must complete within the daemon's own 10s hard upper
	// bound, plus a small slack for the select-race and any last
	// defer unwinding.
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return within 15s of SIGTERM")
	}
}

func TestRun_GracefulShutdown_CancelsStartupStrandedCronRecovery(t *testing.T) {
	baseDir, err := os.MkdirTemp("/tmp", "bossdtest-")
	if err != nil {
		t.Fatalf("mkdir base: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(baseDir) })
	t.Setenv("HOME", baseDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(baseDir, ".config"))
	t.Setenv("BOSS_SETTINGS_PATH", filepath.Join(baseDir, "settings.json"))
	t.Setenv("BOSSD_ORCHESTRATOR_URL", "")

	stopSig := make(chan os.Signal, 1)
	ready := make(chan struct{})
	recoveryStarted := make(chan struct{})
	recoveryStopped := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- run(runOpts{
			stopSig:    stopSig,
			dbPath:     filepath.Join(baseDir, "bossd.db"),
			socketPath: filepath.Join(baseDir, "bossd.sock"),
			plugins:    []config.PluginConfig{},
			onReady:    func() { close(ready) },
			startupStrandedCronRecovery: func(ctx context.Context) (int, error) {
				close(recoveryStarted)
				<-ctx.Done()
				close(recoveryStopped)
				return 0, ctx.Err()
			},
		})
	}()

	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("run exited before ready: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("daemon did not reach ready state")
	}
	select {
	case <-recoveryStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("startup stranded-cron recovery did not start")
	}

	stopSig <- syscall.SIGTERM
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return after SIGTERM")
	}
	select {
	case <-recoveryStopped:
	default:
		t.Fatal("startup stranded-cron recovery was not cancelled before shutdown completed")
	}
}

// TestRun_StartupBootstrapReapPrecedesCronRecovery pins the ORDER of the two
// startup recovery passes (BOS-717). Both select pre-agent rows
// (CreatingWorktree/StartingAgent) and both claim one with a conditional state
// transition, so running them concurrently is not corrupting but IS a coin flip:
// whichever won decided whether a half-created session was reclaimed as a failed
// bootstrap (Blocked + worktree/branch cleanup) or finalized as a completed cron
// run — and finalize is the wrong answer for a row whose worktree may never have
// been created. The bootstrap reaper must therefore complete first; rows it
// declines still fall through to the cron sweep, so ordering narrows nothing.
//
// Run them as two goroutines again and this fails: the cron seam observes
// reapDone still false.
func TestRun_StartupBootstrapReapPrecedesCronRecovery(t *testing.T) {
	baseDir, err := os.MkdirTemp("/tmp", "bossdtest-")
	if err != nil {
		t.Fatalf("mkdir base: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(baseDir) })
	t.Setenv("HOME", baseDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(baseDir, ".config"))
	t.Setenv("BOSS_SETTINGS_PATH", filepath.Join(baseDir, "settings.json"))
	t.Setenv("BOSSD_ORCHESTRATOR_URL", "")

	stopSig := make(chan os.Signal, 1)
	ready := make(chan struct{})
	cronRan := make(chan struct{})
	done := make(chan error, 1)

	var mu sync.Mutex
	reapDone := false
	reapFirst := false

	go func() {
		done <- run(runOpts{
			stopSig:    stopSig,
			dbPath:     filepath.Join(baseDir, "bossd.db"),
			socketPath: filepath.Join(baseDir, "bossd.sock"),
			plugins:    []config.PluginConfig{},
			onReady:    func() { close(ready) },
			startupStrandedBootstrapReap: func(context.Context) (int, error) {
				// Hold the pass open briefly: if the two ran concurrently the
				// cron seam would observe reapDone false during this window.
				time.Sleep(150 * time.Millisecond)
				mu.Lock()
				reapDone = true
				mu.Unlock()
				return 0, nil
			},
			startupStrandedCronRecovery: func(context.Context) (int, error) {
				mu.Lock()
				reapFirst = reapDone
				mu.Unlock()
				close(cronRan)
				return 0, nil
			},
		})
	}()

	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("run exited before ready: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("daemon did not reach ready state")
	}
	select {
	case <-cronRan:
	case <-time.After(10 * time.Second):
		t.Fatal("startup stranded-cron recovery did not run")
	}

	mu.Lock()
	ordered := reapFirst
	mu.Unlock()
	if !ordered {
		t.Fatal("stranded-cron recovery ran before the stranded-bootstrap reap finished; " +
			"the two startup passes must be sequential, reaper first")
	}

	stopSig <- syscall.SIGTERM
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return after SIGTERM")
	}
}

func TestDaemonDistinctIDUsesHyphenatedSharedHelper(t *testing.T) {
	got := daemonDistinctIDFromHostname("host-value")
	want := telemetry.DaemonDistinctID("host-value")
	if got != want {
		t.Fatalf("daemonDistinctIDFromHostname() = %q, want %q", got, want)
	}
	if strings.Contains(got, ":") {
		t.Fatalf("daemon distinct ID %q contains colon", got)
	}
}

func TestDaemonDistinctIDFallbackIsHyphenated(t *testing.T) {
	if got := daemonDistinctIDFromHostname(""); got != "daemon-unknown" {
		t.Fatalf("daemonDistinctIDFromHostname empty = %q, want daemon-unknown", got)
	}
}

func TestRunUsesSettingsPathProfileForDBAndSocket(t *testing.T) {
	baseDir, err := os.MkdirTemp("/tmp", "bossd-profile-")
	if err != nil {
		t.Fatalf("mkdir base: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(baseDir) })

	settingsPath := filepath.Join(baseDir, "settings.json")
	appDataDir := filepath.Join(baseDir, "data")
	socketPath := filepath.Join(baseDir, "profile.sock")
	t.Setenv("BOSS_SETTINGS_PATH", settingsPath)
	t.Setenv("BOSSD_ORCHESTRATOR_URL", "")

	settings := config.DefaultSettings()
	settings.AppDataDir = appDataDir
	settings.SocketPath = socketPath
	if err := config.SaveTo(settingsPath, settings); err != nil {
		t.Fatalf("SaveTo() returned error: %v", err)
	}

	stopSig := make(chan os.Signal, 1)
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- run(runOpts{
			stopSig: stopSig,
			plugins: []config.PluginConfig{},
			onReady: func() { close(ready) },
		})
	}()

	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("run exited before ready: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("daemon did not reach ready state within 15s")
	}

	if _, err := os.Stat(filepath.Join(appDataDir, "bossd.db")); err != nil {
		t.Fatalf("settings DB path was not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(appDataDir, server.LockFileName)); err != nil {
		t.Fatalf("settings lock file was not created: %v", err)
	}
	metadata, err := daemonstate.Read(appDataDir)
	if err != nil {
		t.Fatalf("daemon metadata was not written: %v", err)
	}
	if metadata.PID != os.Getpid() {
		t.Fatalf("metadata PID = %d, want %d", metadata.PID, os.Getpid())
	}
	if metadata.SettingsPath != settingsPath {
		t.Fatalf("metadata settings path = %q, want %q", metadata.SettingsPath, settingsPath)
	}
	if metadata.SocketPath != socketPath {
		t.Fatalf("metadata socket path = %q, want %q", metadata.SocketPath, socketPath)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(socketPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("settings socket path was not created at %s", socketPath)
		}
		time.Sleep(25 * time.Millisecond)
	}

	stopSig <- syscall.SIGTERM
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return within 15s of SIGTERM")
	}
	if _, err := daemonstate.Read(appDataDir); !os.IsNotExist(err) {
		t.Fatalf("daemon metadata after shutdown error = %v, want not exist", err)
	}
}

func TestCleanupDaemonShutdownStateKeepsReplacementMetadata(t *testing.T) {
	appDataDir := t.TempDir()
	if err := daemonstate.Write(appDataDir, daemonstate.Metadata{PID: 1}); err != nil {
		t.Fatalf("write old daemon metadata: %v", err)
	}

	cleanupDaemonShutdownState(closeFunc(func() error {
		return daemonstate.Write(appDataDir, daemonstate.Metadata{PID: 2})
	}), true, appDataDir)

	metadata, err := daemonstate.Read(appDataDir)
	if err != nil {
		t.Fatalf("read replacement daemon metadata: %v", err)
	}
	if metadata.PID != 2 {
		t.Fatalf("replacement metadata PID = %d, want 2", metadata.PID)
	}
}

type closeFunc func() error

func (f closeFunc) Close() error { return f() }

func TestRunLogsSecurityRejectionForUntrustedConfiguredPlugin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission hardening is a no-op on Windows")
	}

	baseDir, err := os.MkdirTemp("/tmp", "bossd-security-")
	if err != nil {
		t.Fatalf("mkdir base: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(baseDir) })

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cleanWD := filepath.Join(baseDir, "cwd")
	if err := os.Mkdir(cleanWD, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cleanWD); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	settingsPath := filepath.Join(baseDir, "settings.json")
	appDataDir := filepath.Join(baseDir, "data")
	socketPath := filepath.Join(baseDir, "bossd.sock")
	stateDir := filepath.Join(baseDir, "state")
	t.Setenv("HOME", baseDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(baseDir, ".config"))
	t.Setenv("XDG_STATE_HOME", stateDir)
	t.Setenv("BOSS_SETTINGS_PATH", settingsPath)
	t.Setenv("BOSSD_ORCHESTRATOR_URL", "")

	pluginDir := filepath.Join(baseDir, "plugins")
	if err := os.Mkdir(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pluginPath := filepath.Join(pluginDir, "bossd-plugin-claude")
	if err := os.WriteFile(pluginPath, []byte("#!/bin/sh\nexit 0\n"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(pluginPath, 0o777); err != nil {
		t.Fatal(err)
	}

	settings := config.DefaultSettings()
	settings.AppDataDir = appDataDir
	settings.SocketPath = socketPath
	settings.Plugins = []config.PluginConfig{{Name: "claude", Path: pluginPath, Enabled: true}}
	if err := config.SaveTo(settingsPath, settings); err != nil {
		t.Fatalf("SaveTo() returned error: %v", err)
	}

	stopSig := make(chan os.Signal, 1)
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- run(runOpts{
			stopSig: stopSig,
			onReady: func() { close(ready) },
		})
	}()

	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("run exited before ready: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("daemon did not reach ready state within 15s")
	}

	stopSig <- syscall.SIGTERM
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return within 15s of SIGTERM")
	}

	logPath := filepath.Join(stateDir, "bossanova", "logs", "bossd.log")
	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read bossd log: %v", err)
	}
	logText := string(body)
	if !strings.Contains(logText, "SECURITY: refusing to load unverified plugin binary before exec") {
		t.Fatalf("bossd log missing SECURITY rejection: %s", logText)
	}
	if !strings.Contains(logText, "group/world-writable") {
		t.Fatalf("bossd log missing group/world-writable reason: %s", logText)
	}
}

// TestClampInt32 is the BOS-413 boundary table-test for the package-local
// gosec-G115 clamp helper: normal values pass through; out-of-range int inputs
// clamp to the int32 extremes instead of wrapping. int32 range is
// [-2147483648, 2147483647].
func TestClampInt32(t *testing.T) {
	const (
		maxI32 = 2147483647
		minI32 = -2147483648
	)
	tests := []struct {
		name string
		in   int
		want int32
	}{
		{"normal", 42, 42},
		{"zero", 0, 0},
		{"max", maxI32, maxI32},
		{"min", minI32, minI32},
		{"clampsHigh", maxI32 + 1, maxI32},
		{"clampsLow", minI32 - 1, minI32},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clampInt32(tt.in); got != tt.want {
				t.Errorf("clampInt32(%d) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

// TestChatUploadSenderMapsUnconfirmedToTheSentinel pins the one mapping the
// BOS-661 retention decision rests on.
//
// chatupload.Finish KEEPS an uploaded file when — and only when — the sender
// wraps chatupload.ErrDeliveryUnconfirmed, because an unconfirmed submit may
// already be running in the agent's pane and deleting the file would strand a
// live prompt on a missing path. Every other undelivered state is a definite
// non-delivery and the file is removed. If a future edit returned a bare error
// for DELIVERY_STATE_UNCONFIRMED, the manager would silently start deleting
// files underneath prompts that had already been submitted, and nothing else
// in the tree would notice.
func TestChatUploadSenderMapsUnconfirmedToTheSentinel(t *testing.T) {
	tests := []struct {
		name            string
		stub            sendChatMessageStub
		wantErr         bool
		wantUnconfirmed bool
	}{
		{
			name: "delivered",
			stub: sendChatMessageStub{response: connect.NewResponse(&bossanovav1.SendChatMessageResponse{
				Delivered: true,
			})},
		},
		{
			name: "unconfirmed keeps the bytes",
			stub: sendChatMessageStub{response: connect.NewResponse(&bossanovav1.SendChatMessageResponse{
				Delivered:     false,
				DeliveryState: bossanovav1.SendChatMessageResponse_DELIVERY_STATE_UNCONFIRMED,
				NoticeText:    "no live composer was drawn",
			})},
			wantErr:         true,
			wantUnconfirmed: true,
		},
		{
			name: "definite non-delivery does not match the sentinel",
			stub: sendChatMessageStub{response: connect.NewResponse(&bossanovav1.SendChatMessageResponse{
				Delivered:     false,
				DeliveryState: bossanovav1.SendChatMessageResponse_DELIVERY_STATE_NOT_SUBMITTED,
			})},
			wantErr: true,
		},
		{
			name:    "rpc error",
			stub:    sendChatMessageStub{err: errors.New("unavailable")},
			wantErr: true,
		},
		{
			name:    "nil response",
			stub:    sendChatMessageStub{},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sender := &chatUploadSender{}
			sender.setServer(tt.stub)
			err := sender.SendChatMessage(context.Background(), "chat-1", "message", true)
			if tt.wantErr != (err != nil) {
				t.Fatalf("SendChatMessage() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got := errors.Is(err, chatupload.ErrDeliveryUnconfirmed); got != tt.wantUnconfirmed {
				t.Fatalf("errors.Is(err, ErrDeliveryUnconfirmed) = %v, want %v (err = %v)", got, tt.wantUnconfirmed, err)
			}
		})
	}

	// An unwired server is a definite non-delivery, not an unconfirmed one.
	unwired := &chatUploadSender{}
	err := unwired.SendChatMessage(context.Background(), "chat-1", "message", true)
	if err == nil {
		t.Fatal("SendChatMessage() with no server error = nil, want error")
	}
	if errors.Is(err, chatupload.ErrDeliveryUnconfirmed) {
		t.Fatal("an unwired sender must not report an unconfirmed delivery; the file would be retained for 24h")
	}
}
