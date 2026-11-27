package main

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/adapter/cloudrpc"
	daemonrpc "github.com/recurser/boss/internal/adapter/daemonrpc"
	daemon "github.com/recurser/boss/internal/daemonlife"
	"github.com/spf13/cobra"
)

func TestFirstLaunchDaemonRegistration_DaemonLifecycle_AC271401(t *testing.T) {
	oldEnsureRunning := daemonEnsureRunning
	daemonEnsureRunning = func(socketPath string) error {
		t.Fatalf("daemonEnsureRunning(%q) must never be called during first-launch registration", socketPath)
		return nil
	}
	defer func() { daemonEnsureRunning = oldEnsureRunning }()

	const (
		testSocketPath = "/tmp/boss-first-launch-test.sock"
		testBossdPath  = "/opt/homebrew/bin/bossd"
	)

	newFixture := func(t *testing.T) (firstLaunchRegistration, *bytes.Buffer, *int, *[]struct {
		path  string
		force bool
	}, *int, *bool, *[]time.Duration) {
		t.Helper()
		clock := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
		var out bytes.Buffer
		resolveCalls := 0
		var installCalls []struct {
			path  string
			force bool
		}
		uninstallCalls := 0
		registered := false
		var sleeps []time.Duration

		reg := firstLaunchRegistration{
			goos:              "darwin",
			getenv:            func(string) string { return "" },
			settingsProfile:   func() (string, error) { return "", nil },
			socketPath:        func() (string, error) { return testSocketPath, nil },
			socketReachable:   func(string) bool { return false },
			serviceRegistered: func() (bool, error) { return registered, nil },
			supervision: func() daemon.SupervisionModeStatus {
				return daemon.SupervisionModeStatus{Mode: daemon.SupervisionModeLaunchAgent}
			},
			resolveBossd: func() (string, error) {
				resolveCalls++
				return testBossdPath, nil
			},
			install: func(path string, force bool) error {
				installCalls = append(installCalls, struct {
					path  string
					force bool
				}{path: path, force: force})
				registered = true
				return nil
			},
			uninstall: func() error {
				uninstallCalls++
				registered = false
				return nil
			},
			startWait: daemon.LifecycleStartupTimeout,
			pollEvery: daemon.LifecyclePollInterval,
			now:       func() time.Time { return clock },
			sleep: func(d time.Duration) {
				sleeps = append(sleeps, d)
				clock = clock.Add(d)
			},
			out: &out,
		}
		return reg, &out, &resolveCalls, &installCalls, &uninstallCalls, &registered, &sleeps
	}

	assertSingleLine := func(t *testing.T, got string) string {
		t.Helper()
		if !strings.HasSuffix(got, "\n") {
			t.Fatalf("output %q must end with a newline", got)
		}
		trimmed := strings.TrimSuffix(got, "\n")
		if trimmed == "" || strings.Contains(trimmed, "\n") {
			t.Fatalf("output %q must be exactly one non-empty line", got)
		}
		return trimmed
	}

	t.Run("register/socket-becomes-reachable", func(t *testing.T) {
		reg, out, resolveCalls, installCalls, uninstallCalls, _, sleeps := newFixture(t)
		probes := 0
		reg.socketReachable = func(sock string) bool {
			if sock != testSocketPath {
				t.Fatalf("socketReachable called with %q, want %q", sock, testSocketPath)
			}
			probes++
			return probes >= 4
		}

		if err := reg.run(); err != nil {
			t.Fatalf("run() = %v, want nil", err)
		}
		if *resolveCalls != 1 {
			t.Fatalf("resolveBossd calls = %d, want 1", *resolveCalls)
		}
		if len(*installCalls) != 1 || (*installCalls)[0].path != testBossdPath || (*installCalls)[0].force {
			t.Fatalf("install calls = %+v, want [{path:%q force:false}]", *installCalls, testBossdPath)
		}
		if *uninstallCalls != 0 {
			t.Fatalf("uninstall calls = %d, want 0", *uninstallCalls)
		}
		line := assertSingleLine(t, out.String())
		if !strings.Contains(line, "BOSS_NO_AUTO_DAEMON_INSTALL") {
			t.Fatalf("registration line %q must name BOSS_NO_AUTO_DAEMON_INSTALL", line)
		}
		if len(*sleeps) != 2 {
			t.Fatalf("sleep calls = %d, want 2", len(*sleeps))
		}
		for i, d := range *sleeps {
			if d != daemon.LifecyclePollInterval {
				t.Fatalf("sleep[%d] = %v, want %v", i, d, daemon.LifecyclePollInterval)
			}
		}
	})

	t.Run("register/timeout-returns-err-service-installed-not-serving", func(t *testing.T) {
		reg, out, resolveCalls, installCalls, uninstallCalls, _, sleeps := newFixture(t)

		err := reg.run()
		if !errors.Is(err, daemon.ErrServiceInstalledNotServing) {
			t.Fatalf("run() error = %v, want wrapping ErrServiceInstalledNotServing", err)
		}
		if !strings.Contains(err.Error(), testSocketPath) {
			t.Fatalf("timeout error %q must name socket path %q", err.Error(), testSocketPath)
		}
		if *resolveCalls != 1 {
			t.Fatalf("resolveBossd calls = %d, want 1", *resolveCalls)
		}
		if len(*installCalls) != 1 || (*installCalls)[0].path != testBossdPath || (*installCalls)[0].force {
			t.Fatalf("install calls = %+v, want [{path:%q force:false}]", *installCalls, testBossdPath)
		}
		if *uninstallCalls != 0 {
			t.Fatalf("uninstall calls = %d, want 0", *uninstallCalls)
		}
		line := assertSingleLine(t, out.String())
		if !strings.Contains(line, "BOSS_NO_AUTO_DAEMON_INSTALL") {
			t.Fatalf("registration line %q must name BOSS_NO_AUTO_DAEMON_INSTALL", line)
		}
		if len(*sleeps) == 0 {
			t.Fatal("expected polling sleeps before timeout")
		}
	})

	skipCases := []struct {
		name   string
		mutate func(*firstLaunchRegistration)
	}{
		{
			name:   "skip/non-darwin",
			mutate: func(r *firstLaunchRegistration) { r.goos = "linux" },
		},
		{
			name: "skip/opted-out",
			mutate: func(r *firstLaunchRegistration) {
				r.getenv = func(k string) string {
					if k == "BOSS_NO_AUTO_DAEMON_INSTALL" {
						return "1"
					}
					return ""
				}
			},
		},
		{
			name: "skip/non-default-profile",
			mutate: func(r *firstLaunchRegistration) {
				r.settingsProfile = func() (string, error) { return "/tmp/custom/settings.json", nil }
			},
		},
		{
			name: "skip/profile-error",
			mutate: func(r *firstLaunchRegistration) {
				r.settingsProfile = func() (string, error) { return "", errors.New("invalid settings path") }
			},
		},
		{
			name: "skip/socket-path-error",
			mutate: func(r *firstLaunchRegistration) {
				r.socketPath = func() (string, error) { return "", errors.New("no home dir") }
			},
		},
		{
			name: "skip/socket-reachable",
			mutate: func(r *firstLaunchRegistration) {
				r.socketReachable = func(string) bool { return true }
			},
		},
		{
			name: "skip/service-registered",
			mutate: func(r *firstLaunchRegistration) {
				r.serviceRegistered = func() (bool, error) { return true, nil }
			},
		},
		{
			name: "skip/service-registered-error",
			mutate: func(r *firstLaunchRegistration) {
				r.serviceRegistered = func() (bool, error) { return false, errors.New("probe failed") }
			},
		},
		{
			name: "skip/supervision-error",
			mutate: func(r *firstLaunchRegistration) {
				r.supervision = func() daemon.SupervisionModeStatus {
					return daemon.SupervisionModeStatus{Err: errors.New("unknown supervision mode")}
				}
			},
		},
		{
			name: "skip/non-launch-agent-mode",
			mutate: func(r *firstLaunchRegistration) {
				r.supervision = func() daemon.SupervisionModeStatus {
					return daemon.SupervisionModeStatus{Mode: daemon.SupervisionModeUnattended}
				}
			},
		},
	}

	for _, tc := range skipCases {
		t.Run(tc.name, func(t *testing.T) {
			reg, out, _, installCalls, uninstallCalls, _, _ := newFixture(t)
			tc.mutate(&reg)

			if err := reg.run(); err != nil {
				t.Fatalf("run() = %v, want nil", err)
			}
			if len(*installCalls) != 0 {
				t.Fatalf("install called %d times, want 0", len(*installCalls))
			}
			if *uninstallCalls != 0 {
				t.Fatalf("uninstall called %d times, want 0", *uninstallCalls)
			}
			if out.Len() != 0 {
				t.Fatalf("output = %q, want empty", out.String())
			}
		})
	}

	warnCases := []struct {
		name            string
		wantSubstrings  []string
		forbidSubstring string
		wantInstall     int
		wantUninstall   int
		wantRegistered  bool
		mutate          func(*firstLaunchRegistration, *bool)
	}{
		{
			name:           "warn/resolve-bossd-error",
			wantSubstrings: []string{"boss daemon install", "bossd binary missing"},
			wantInstall:    0,
			wantUninstall:  0,
			mutate: func(r *firstLaunchRegistration, _ *bool) {
				r.resolveBossd = func() (string, error) { return "", errors.New("bossd binary missing") }
			},
		},
		{
			name:            "warn/install-error",
			wantSubstrings:  []string{"boss daemon install", "launchctl load refused"},
			forbidSubstring: "--force",
			wantInstall:     1,
			wantUninstall:   1,
			wantRegistered:  false,
			mutate: func(r *firstLaunchRegistration, _ *bool) {
				orig := r.install
				r.install = func(path string, force bool) error {
					_ = orig(path, force)
					return errors.New("launchctl load refused")
				}
			},
		},
		{
			name:            "warn/install-error-before-plist",
			wantSubstrings:  []string{"boss daemon install", "stage bossd failed"},
			forbidSubstring: "--force",
			wantInstall:     1,
			wantUninstall:   0,
			wantRegistered:  false,
			mutate: func(r *firstLaunchRegistration, registered *bool) {
				orig := r.install
				r.install = func(path string, force bool) error {
					_ = orig(path, force)
					*registered = false
					return errors.New("stage bossd failed")
				}
			},
		},
		{
			name:           "warn/install-error-already-exists",
			wantSubstrings: []string{"boss daemon install", "plist already exists"},
			wantInstall:    1,
			wantUninstall:  0,
			wantRegistered: true,
			mutate: func(r *firstLaunchRegistration, _ *bool) {
				orig := r.install
				r.install = func(path string, force bool) error {
					_ = orig(path, force)
					return errors.New("plist already exists at /tmp/com.bossanova.bossd.plist (use --force to overwrite)")
				}
			},
		},
		{
			name:           "warn/install-error-cleanup-failed",
			wantSubstrings: []string{"boss daemon install --force", "launchctl load refused", "remove plist: permission denied"},
			wantInstall:    1,
			wantUninstall:  1,
			wantRegistered: true,
			mutate: func(r *firstLaunchRegistration, registered *bool) {
				origInstall := r.install
				r.install = func(path string, force bool) error {
					_ = origInstall(path, force)
					return errors.New("launchctl load refused")
				}
				origUninstall := r.uninstall
				r.uninstall = func() error {
					_ = origUninstall()
					*registered = true
					return errors.New("remove plist: permission denied")
				}
			},
		},
	}

	for _, tc := range warnCases {
		t.Run(tc.name, func(t *testing.T) {
			reg, out, _, installCalls, uninstallCalls, registered, _ := newFixture(t)
			tc.mutate(&reg, registered)

			if err := reg.run(); err != nil {
				t.Fatalf("run() = %v, want nil", err)
			}
			if len(*installCalls) != tc.wantInstall {
				t.Fatalf("install calls = %d, want %d", len(*installCalls), tc.wantInstall)
			}
			if *uninstallCalls != tc.wantUninstall {
				t.Fatalf("uninstall calls = %d, want %d", *uninstallCalls, tc.wantUninstall)
			}
			if *registered != tc.wantRegistered {
				t.Fatalf("registered after run = %v, want %v", *registered, tc.wantRegistered)
			}
			line := assertSingleLine(t, out.String())
			for _, want := range tc.wantSubstrings {
				if !strings.Contains(line, want) {
					t.Fatalf("warning line %q must contain %q", line, want)
				}
			}
			if tc.forbidSubstring != "" && strings.Contains(line, tc.forbidSubstring) {
				t.Fatalf("warning line %q must not contain %q", line, tc.forbidSubstring)
			}
		})
	}
}

func TestNewFirstLaunchRegistration(t *testing.T) {
	var buf bytes.Buffer
	reg := newFirstLaunchRegistration(&buf)

	if reg.goos != runtime.GOOS {
		t.Fatalf("goos = %q, want %q", reg.goos, runtime.GOOS)
	}
	if reg.startWait != daemon.LifecycleStartupTimeout {
		t.Fatalf("startWait = %v, want %v", reg.startWait, daemon.LifecycleStartupTimeout)
	}
	if reg.pollEvery != daemon.LifecyclePollInterval {
		t.Fatalf("pollEvery = %v, want %v", reg.pollEvery, daemon.LifecyclePollInterval)
	}
	if reg.out != &buf {
		t.Fatalf("out = %v, want provided buffer", reg.out)
	}

	fnFields := []struct {
		name string
		fn   any
	}{
		{"getenv", reg.getenv},
		{"settingsProfile", reg.settingsProfile},
		{"socketPath", reg.socketPath},
		{"socketReachable", reg.socketReachable},
		{"serviceRegistered", reg.serviceRegistered},
		{"supervision", reg.supervision},
		{"resolveBossd", reg.resolveBossd},
		{"install", reg.install},
		{"uninstall", reg.uninstall},
		{"now", reg.now},
		{"sleep", reg.sleep},
	}
	for _, f := range fnFields {
		if reflect.ValueOf(f.fn).IsNil() {
			t.Fatalf("field %s is nil", f.name)
		}
	}
}

func TestNewTUIClientRegisteringDaemon(t *testing.T) {
	oldRegister := registerDaemonOnFirstLaunch
	oldDialLocal := dialTUIDaemonClient
	oldDialRemote := dialTUIRemoteClient
	defer func() {
		registerDaemonOnFirstLaunch = oldRegister
		dialTUIDaemonClient = oldDialLocal
		dialTUIRemoteClient = oldDialRemote
	}()

	t.Run("local/registers-before-dial", func(t *testing.T) {
		t.Setenv("BOSS_SOCKET", "")
		var events []string
		wantClient := new(daemonrpc.LocalClient)

		registerDaemonOnFirstLaunch = func() error {
			events = append(events, "register")
			return nil
		}
		dialTUIDaemonClient = func(*cobra.Command) (*daemonrpc.LocalClient, error) {
			events = append(events, "dial-local")
			return wantClient, nil
		}
		dialTUIRemoteClient = func(*cobra.Command, string) (*cloudrpc.RemoteClient, error) {
			t.Fatal("remote dialer must not be called for local TUI launch")
			return nil, nil
		}

		got, err := newTUIClient(&cobra.Command{Use: "boss"})
		if err != nil {
			t.Fatalf("newTUIClient: %v", err)
		}
		if got != wantClient {
			t.Fatalf("client = %v, want %v", got, wantClient)
		}
		wantEvents := []string{"register", "dial-local"}
		if !reflect.DeepEqual(events, wantEvents) {
			t.Fatalf("events = %v, want %v", events, wantEvents)
		}
	})

	t.Run("local/registration-error-skips-dial", func(t *testing.T) {
		t.Setenv("BOSS_SOCKET", "")
		regErr := fmt.Errorf("%w: did not serve", daemon.ErrServiceInstalledNotServing)

		registerDaemonOnFirstLaunch = func() error {
			return regErr
		}
		dialTUIDaemonClient = func(*cobra.Command) (*daemonrpc.LocalClient, error) {
			t.Fatal("dialer must not be called when registration returns an error")
			return nil, nil
		}
		dialTUIRemoteClient = func(*cobra.Command, string) (*cloudrpc.RemoteClient, error) {
			t.Fatal("remote dialer must not be called for local TUI launch")
			return nil, nil
		}

		got, err := newTUIClient(&cobra.Command{Use: "boss"})
		if !errors.Is(err, daemon.ErrServiceInstalledNotServing) {
			t.Fatalf("error = %v, want wrapping ErrServiceInstalledNotServing", err)
		}
		if got != nil {
			t.Fatalf("client = %v, want nil", got)
		}
	})

	t.Run("skip/remote", func(t *testing.T) {
		t.Setenv("BOSS_SOCKET", "")
		remoteDialed := false
		wantClient := new(cloudrpc.RemoteClient)

		registerDaemonOnFirstLaunch = func() error {
			t.Fatal("registration must not run under --remote")
			return nil
		}
		dialTUIDaemonClient = func(*cobra.Command) (*daemonrpc.LocalClient, error) {
			t.Fatal("local dialer must not run under --remote")
			return nil, nil
		}
		dialTUIRemoteClient = func(*cobra.Command, string) (*cloudrpc.RemoteClient, error) {
			remoteDialed = true
			return wantClient, nil
		}

		got, err := newTUIClient(remoteTestCommand(t))
		if err != nil {
			t.Fatalf("newTUIClient: %v", err)
		}
		if !remoteDialed || got != wantClient {
			t.Fatalf("remoteDialed=%v got=%v", remoteDialed, got)
		}
	})

	t.Run("skip/host", func(t *testing.T) {
		t.Setenv("BOSS_SOCKET", "")
		localDialed := false

		registerDaemonOnFirstLaunch = func() error {
			t.Fatal("registration must not run under --host")
			return nil
		}
		dialTUIDaemonClient = func(*cobra.Command) (*daemonrpc.LocalClient, error) {
			localDialed = true
			return new(daemonrpc.LocalClient), nil
		}
		dialTUIRemoteClient = func(*cobra.Command, string) (*cloudrpc.RemoteClient, error) {
			t.Fatal("remote dialer must not run under --host")
			return nil, nil
		}

		if _, err := newTUIClient(hostTestCommand(t)); err != nil {
			t.Fatalf("newTUIClient: %v", err)
		}
		if !localDialed {
			t.Fatal("expected daemon dialer to run for --host")
		}
	})

	t.Run("skip/boss-socket", func(t *testing.T) {
		t.Setenv("BOSS_SOCKET", "/tmp/explicit.sock")
		localDialed := false

		registerDaemonOnFirstLaunch = func() error {
			t.Fatal("registration must not run when BOSS_SOCKET is set")
			return nil
		}
		dialTUIDaemonClient = func(*cobra.Command) (*daemonrpc.LocalClient, error) {
			localDialed = true
			return new(daemonrpc.LocalClient), nil
		}
		dialTUIRemoteClient = func(*cobra.Command, string) (*cloudrpc.RemoteClient, error) {
			t.Fatal("remote dialer must not run for BOSS_SOCKET")
			return nil, nil
		}

		if _, err := newTUIClient(&cobra.Command{Use: "boss"}); err != nil {
			t.Fatalf("newTUIClient: %v", err)
		}
		if !localDialed {
			t.Fatal("expected daemon dialer to run for explicit BOSS_SOCKET")
		}
	})
}
