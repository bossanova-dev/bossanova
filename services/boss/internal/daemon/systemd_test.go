//go:build linux

package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestGenerateUnit(t *testing.T) {
	unit, err := generateUnit("/usr/local/bin/bossd")
	if err != nil {
		t.Fatalf("generateUnit: %v", err)
	}

	checks := []string{
		"Description=Bossanova Daemon",
		"ExecStart=/usr/local/bin/bossd",
		"Restart=always",
		"RestartSec=5",
		"WantedBy=default.target",
		// BOS-457: raise the FD limit so setup scripts bossd spawns don't die
		// with EMFILE during FD-heavy steps like prisma codegen.
		"LimitNOFILE=65536",
	}

	for _, check := range checks {
		if !strings.Contains(unit, check) {
			t.Errorf("unit file missing %q", check)
		}
	}

	// BOS-457: the FD-limit raise is scoped to bossd only; the MCP server does
	// not spawn FD-hungry setup scripts, so its unit must not carry the key.
	mcpUnit, err := generateMcpUnit("/usr/local/bin/mcp", 8765)
	if err != nil {
		t.Fatalf("generateMcpUnit: %v", err)
	}
	if strings.Contains(mcpUnit, "LimitNOFILE") {
		t.Error("MCP unit should not contain LimitNOFILE (bossd-only FD raise)")
	}
}

func TestSystemdServicePath(t *testing.T) {
	path, err := platformServicePath()
	if err != nil {
		t.Fatalf("platformServicePath: %v", err)
	}

	if !strings.HasSuffix(path, ".config/systemd/user/bossd.service") {
		t.Errorf("unexpected service path: %s", path)
	}
}

// writeMcpUnitFile creates a minimal MCP unit file under home's
// ~/.config/systemd/user, so mcpServicePath()/platformMcpGetStatus() see the
// unit as installed.
func writeMcpUnitFile(t *testing.T, home string) {
	t.Helper()
	dir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir unit dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, McpServiceName), []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatalf("write unit file: %v", err)
	}
}

// fakeSystemctlExitError fabricates a real *exec.ExitError (rather than a
// hand-rolled error type) for tests that need runSystemctl to report a
// non-zero systemctl exit.
func fakeSystemctlExitError(t *testing.T) error {
	t.Helper()
	err := exec.Command("sh", "-c", "exit 1").Run()
	if err == nil {
		t.Fatal("expected `sh -c exit 1` to fail")
	}
	return err
}

// TestPlatformMcpStop covers BOS-627's status-gated, verified-end-state
// contract for platformMcpStop via an injected runSystemctl. It mutates
// package vars (runSystemctl, mcpStopVerifyTimeout) and uses t.Setenv, so it
// must not run in parallel with itself or other tests touching those vars.
func TestPlatformMcpStop(t *testing.T) {
	exitErr := fakeSystemctlExitError(t)

	tests := []struct {
		name string
		// installUnit controls whether a unit file is written before the
		// unit is stopped.
		installUnit bool
		// activeUntil: -1 means is-active always reports "active"; 0 (or any
		// non-negative N) means the Nth-and-later is-active call reports
		// "inactive" instead.
		activeUntil int
		timeout     time.Duration
		wantErr     bool
	}{
		{
			name:        "absent unit returns nil without invoking systemctl",
			installUnit: false,
			timeout:     2 * time.Second,
			wantErr:     false,
		},
		{
			name:        "already-inactive unit after failed stop returns nil",
			installUnit: true,
			activeUntil: 0,
			timeout:     2 * time.Second,
			wantErr:     false,
		},
		{
			name:        "verified-active unit after failed stop returns an error",
			installUnit: true,
			activeUntil: -1,
			timeout:     50 * time.Millisecond,
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
			if tt.installUnit {
				writeMcpUnitFile(t, home)
			}

			origRunSystemctl := runSystemctl
			origTimeout := mcpStopVerifyTimeout
			t.Cleanup(func() {
				runSystemctl = origRunSystemctl
				mcpStopVerifyTimeout = origTimeout
			})
			mcpStopVerifyTimeout = tt.timeout

			var calls [][]string
			isActiveCalls := 0
			runSystemctl = func(args ...string) ([]byte, error) {
				calls = append(calls, append([]string(nil), args...))
				switch {
				case len(args) >= 2 && args[0] == "--user" && args[1] == "stop":
					return []byte("stop output"), exitErr
				case len(args) >= 2 && args[0] == "--user" && args[1] == "is-active":
					isActiveCalls++
					if tt.activeUntil < 0 || isActiveCalls <= tt.activeUntil {
						return []byte("active\n"), nil
					}
					return []byte("inactive\n"), exitErr
				case len(args) >= 2 && args[0] == "--user" && args[1] == "show":
					// platformMcpGetStatus's own installed-and-running probe
					// fetches MainPID; only reached when is-active reported
					// active for the pre-check inside platformMcpStop.
					return []byte("MainPID=1234\n"), nil
				default:
					t.Fatalf("unexpected systemctl invocation: %v", args)
					return nil, nil
				}
			}

			err := platformMcpStop()

			if tt.wantErr && err == nil {
				t.Fatalf("platformMcpStop() = nil, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("platformMcpStop() = %v, want nil", err)
			}
			if tt.wantErr && !strings.Contains(err.Error(), "stop output") {
				t.Errorf("error %q does not contain the systemctl output verbatim", err)
			}

			if !tt.installUnit {
				if len(calls) != 0 {
					t.Errorf("absent unit: expected no systemctl invocations, got %v", calls)
				}
				return
			}

			// platformMcpStop's own installed-check calls platformMcpGetStatus,
			// which independently probes is-active (and show, if running)
			// before the stop attempt — so the stop call is not necessarily
			// calls[0]. Find it explicitly and assert its exact argv.
			var stopCall []string
			for _, c := range calls {
				if len(c) >= 2 && c[0] == "--user" && c[1] == "stop" {
					stopCall = c
					break
				}
			}
			if stopCall == nil {
				t.Fatalf("installed unit: expected a stop invocation, got calls %v", calls)
			}
			want := []string{"--user", "stop", McpServiceName}
			if !reflect.DeepEqual(stopCall, want) {
				t.Errorf("stop invocation = %v, want %v", stopCall, want)
			}
		})
	}
}

// TestMcpUnitStateProbes pins the split between isMcpUnitActive (used by
// platformMcpGetStatus, must not claim "running" it cannot see) and
// mcpUnitStillRunning (used by platformMcpStop's verification, must fail
// CLOSED). The load-bearing row is the unreadable probe: `systemctl is-active`
// exits non-zero for BOTH a stopped unit and a broken probe, so only the state
// word separates them -- and treating "cannot tell" as stopped would report a
// silent false success after a `stop` that really failed.
func TestMcpUnitStateProbes(t *testing.T) {
	exitErr := fakeSystemctlExitError(t)

	tests := []struct {
		name             string
		out              string
		err              error
		wantState        string
		wantActive       bool
		wantStillRunning bool
	}{
		{name: "active", out: "active\n", err: nil, wantState: "active", wantActive: true, wantStillRunning: true},
		{name: "activating", out: "activating\n", err: exitErr, wantState: "activating", wantStillRunning: true},
		{name: "deactivating", out: "deactivating\n", err: exitErr, wantState: "deactivating", wantStillRunning: true},
		{name: "inactive is genuinely stopped", out: "inactive\n", err: exitErr, wantState: "inactive"},
		{name: "failed is genuinely stopped", out: "failed\n", err: exitErr, wantState: "failed"},
		{
			name:             "unreadable probe is not stopped",
			out:              "Failed to connect to bus: No such file or directory\n",
			err:              exitErr,
			wantState:        "",
			wantStillRunning: true,
		},
		{name: "empty output is not stopped", out: "", err: exitErr, wantState: "", wantStillRunning: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			origRunSystemctl := runSystemctl
			t.Cleanup(func() { runSystemctl = origRunSystemctl })
			runSystemctl = func(args ...string) ([]byte, error) {
				want := []string{"--user", "is-active", McpServiceName}
				if !reflect.DeepEqual(args, want) {
					t.Fatalf("systemctl invocation = %v, want %v", args, want)
				}
				return []byte(tt.out), tt.err
			}

			if got := mcpUnitActiveState(); got != tt.wantState {
				t.Errorf("mcpUnitActiveState() = %q, want %q", got, tt.wantState)
			}
			if got := isMcpUnitActive(); got != tt.wantActive {
				t.Errorf("isMcpUnitActive() = %v, want %v", got, tt.wantActive)
			}
			if got := mcpUnitStillRunning(); got != tt.wantStillRunning {
				t.Errorf("mcpUnitStillRunning() = %v, want %v", got, tt.wantStillRunning)
			}
		})
	}
}

// TestPlatformMcpStopUnreadableProbeFailsClosed is the end-to-end half of
// TestMcpUnitStateProbes: a failed `stop` whose verification probe cannot be
// read must surface the systemctl error, not report success.
func TestPlatformMcpStopUnreadableProbeFailsClosed(t *testing.T) {
	exitErr := fakeSystemctlExitError(t)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
	writeMcpUnitFile(t, home)

	origRunSystemctl := runSystemctl
	origTimeout := mcpStopVerifyTimeout
	t.Cleanup(func() {
		runSystemctl = origRunSystemctl
		mcpStopVerifyTimeout = origTimeout
	})
	mcpStopVerifyTimeout = 20 * time.Millisecond

	runSystemctl = func(args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "--user" && args[1] == "stop" {
			return []byte("stop output"), exitErr
		}
		// Every is-active probe fails to reach the user bus.
		return []byte("Failed to connect to bus: No such file or directory"), exitErr
	}

	err := platformMcpStop()
	if err == nil {
		t.Fatal("platformMcpStop() = nil with an unreadable verification probe, want an error")
	}
	if !strings.Contains(err.Error(), "stop output") {
		t.Errorf("error %q does not carry the systemctl output", err)
	}
}

// TestPlatformMcpStopSkipLaunchctl asserts the BOSS_DAEMON_SKIP_LAUNCHCTL
// short-circuit fires before any systemctl invocation, so a mistake that
// removes the skipLaunchctl() check does not silently no-op in this test
// suite while still shelling out in production.
func TestPlatformMcpStopSkipLaunchctl(t *testing.T) {
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")

	origRunSystemctl := runSystemctl
	t.Cleanup(func() { runSystemctl = origRunSystemctl })

	called := false
	runSystemctl = func(args ...string) ([]byte, error) {
		called = true
		return nil, nil
	}

	if err := platformMcpStop(); err != nil {
		t.Fatalf("platformMcpStop() = %v, want nil when BOSS_DAEMON_SKIP_LAUNCHCTL is set", err)
	}
	if called {
		t.Error("platformMcpStop() invoked systemctl despite BOSS_DAEMON_SKIP_LAUNCHCTL being set")
	}
}
