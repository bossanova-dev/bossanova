//go:build linux

package daemon

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossalib/config"
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

// TestGeneratedUnitTimeoutStopSecCoversShutdownBudget pins the Linux half of
// the BOS-888 ceiling chain. systemd SIGKILLs bossd at TimeoutStopSec, and a
// hard kill there skips the deferred database.Close and the socket cleanup. The
// distro default (90s) is generous, but a distro that lowered it would cut the
// failover proxy drain, so the unit pins its own value — which must exceed
// LifecycleShutdownTimeout for the CLI's wait to be what bounds a stuck
// shutdown. Note the reach limit recorded in systemd.go: this covers new
// installs only, because platformRestart does not regenerate the unit.
func TestGeneratedUnitTimeoutStopSecCoversShutdownBudget(t *testing.T) {
	unit, err := generateUnit("/usr/local/bin/bossd")
	if err != nil {
		t.Fatalf("generateUnit: %v", err)
	}

	const key = "TimeoutStopSec="
	var value string
	for _, line := range strings.Split(unit, "\n") {
		if strings.HasPrefix(line, key) {
			value = strings.TrimSpace(strings.TrimPrefix(line, key))
			break
		}
	}
	if value == "" {
		t.Fatalf("unit has no %s line; a distro that lowered DefaultTimeoutStopSec would cut the proxy drain", key)
	}
	secs, err := strconv.Atoi(value)
	if err != nil {
		t.Fatalf("TimeoutStopSec value %q: %v", value, err)
	}

	if got := time.Duration(secs) * time.Second; got <= LifecycleShutdownTimeout {
		t.Fatalf("unit TimeoutStopSec = %v, want > LifecycleShutdownTimeout = %v so the CLI's wait, not systemd's SIGKILL, bounds a stuck shutdown", got, LifecycleShutdownTimeout)
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

// TestGenerateUnitSetsExplicitPath is the Linux half of the BOS-880 parity
// invariant: the bossd unit previously had no Environment=PATH= at all and
// inherited the systemd user manager's PATH.
func TestGenerateUnitSetsExplicitPath(t *testing.T) {
	stubHome(t)
	stubServiceSettings(t, config.Settings{})

	unit, err := generateUnit("/usr/local/bin/bossd")
	if err != nil {
		t.Fatalf("generateUnit: %v", err)
	}

	if !strings.Contains(unit, `Environment="PATH=`) {
		t.Fatalf("bossd unit has no Environment=PATH= line:\n%s", unit)
	}
	for _, want := range []string{"/stub/home/.nodenv/shims", "/stub/home/.local/bin"} {
		if !strings.Contains(unit, want) {
			t.Errorf("bossd unit PATH missing %q:\n%s", want, unit)
		}
	}
}

// TestGenerateUnitAndMcpUnitShareOnePath fails the moment the two units render
// their PATH from different helpers again.
func TestGenerateUnitAndMcpUnitShareOnePath(t *testing.T) {
	stubHome(t)
	stubServiceSettings(t, config.Settings{DaemonPathExtra: []string{"~/.asdf/shims"}})

	unit, err := generateUnit("/usr/local/bin/bossd")
	if err != nil {
		t.Fatalf("generateUnit: %v", err)
	}
	mcpUnit, err := generateMcpUnit("/usr/local/bin/mcp", 8765)
	if err != nil {
		t.Fatalf("generateMcpUnit: %v", err)
	}

	want := `Environment="PATH=` + serviceEnvPath() + `"`
	for name, rendered := range map[string]string{"bossd": unit, "mcp": mcpUnit} {
		if !strings.Contains(rendered, want) {
			t.Errorf("%s unit does not render the shared service PATH:\n%s", name, rendered)
		}
	}
}

func TestGenerateUnitPlacesConfiguredExtraFirst(t *testing.T) {
	stubHome(t)
	stubServiceSettings(t, config.Settings{DaemonPathExtra: []string{"~/.asdf/shims"}})

	unit, err := generateUnit("/usr/local/bin/bossd")
	if err != nil {
		t.Fatalf("generateUnit: %v", err)
	}

	if !strings.Contains(unit, `Environment="PATH=/stub/home/.asdf/shims:`) {
		t.Errorf("configured extra is not at the front of the unit PATH:\n%s", unit)
	}
}

// TestGenerateUnitRejectsNewlineInjection is the sharper half of why sanitizing
// is load-bearing: text/template does not escape, so a newline in an entry
// would otherwise inject an arbitrary directive into the unit file.
func TestGenerateUnitRejectsNewlineInjection(t *testing.T) {
	stubHome(t)
	stubServiceSettings(t, config.Settings{DaemonPathExtra: []string{
		"/opt/evil\nExecStartPre=/bin/rm -rf /",
		"/opt/other\rEnvironment=INJECTED=1",
	}})

	unit, err := generateUnit("/usr/local/bin/bossd")
	if err != nil {
		t.Fatalf("generateUnit: %v", err)
	}

	for _, injected := range []string{"ExecStartPre", "INJECTED", "/opt/evil", "/opt/other"} {
		if strings.Contains(unit, injected) {
			t.Errorf("unit contains injected content %q:\n%s", injected, unit)
		}
	}

	// Every line must still be a directive or a section header — no orphaned
	// fragment left behind by a partially-filtered entry.
	for _, line := range strings.Split(unit, "\n") {
		if line == "" || strings.HasPrefix(line, "[") {
			continue
		}
		if !strings.Contains(line, "=") {
			t.Errorf("unit contains a non-directive line %q:\n%s", line, unit)
		}
	}
}

// TestServiceEnvPathSplitsInheritedPath guards the Linux baseline: the
// inherited PATH is colon-joined, so it must be split into entries or the
// dedupe would compare one opaque string against individual directories.
func TestServiceEnvPathSplitsInheritedPath(t *testing.T) {
	stubHome(t)
	stubServiceSettings(t, config.Settings{DaemonPathExtra: []string{"/usr/bin"}})
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")

	got := serviceEnvPath()

	// /usr/bin was configured as an extra, so it moves to the front and must
	// appear exactly once.
	if !strings.HasPrefix(got, "/usr/bin:") {
		t.Errorf("serviceEnvPath() = %q, want it to start with the configured extra", got)
	}
	if count := strings.Count(got, "/usr/bin:") + strings.Count(got, ":/usr/bin"); count != 1 {
		t.Errorf("serviceEnvPath() = %q, want /usr/bin exactly once (count %d)", got, count)
	}
}

// TestServiceEnvPathDropsHostileInheritedEntries covers the entry class the
// bossd unit newly interpolates: a baseline entry from $PATH is not a
// compile-time literal.
func TestServiceEnvPathDropsHostileInheritedEntries(t *testing.T) {
	stubHome(t)
	stubServiceSettings(t, config.Settings{})
	t.Setenv("PATH", "/usr/bin:/opt/bad\nExecStartPre=/bin/false:/bin")

	got := serviceEnvPath()

	if strings.ContainsAny(got, "\n\r") {
		t.Errorf("serviceEnvPath() = %q, want no newline from the inherited PATH", got)
	}
	if strings.Contains(got, "ExecStartPre") {
		t.Errorf("serviceEnvPath() = %q, want the injected directive dropped", got)
	}
}

// TestGenerateUnitQuotesPathSoSpacesSurvive: a space is legal in a Unix
// directory name but systemd splits an UNQUOTED Environment= line on
// whitespace into separate assignments, which would silently truncate the
// daemon's PATH — the same failure mode this ticket fixes. The value is quoted
// rather than the entry dropped, so such a directory keeps working.
func TestGenerateUnitQuotesPathSoSpacesSurvive(t *testing.T) {
	stubHome(t)
	stubServiceSettings(t, config.Settings{DaemonPathExtra: []string{"/opt/my tools/bin"}})

	unit, err := generateUnit("/usr/local/bin/bossd")
	if err != nil {
		t.Fatalf("generateUnit: %v", err)
	}

	if !strings.Contains(unit, `Environment="PATH=/opt/my tools/bin:`) {
		t.Fatalf("space-bearing entry did not survive as a quoted value:\n%s", unit)
	}

	// The PATH assignment must be exactly one quoted line, closed on that line.
	var pathLine string
	for _, line := range strings.Split(unit, "\n") {
		if strings.HasPrefix(line, `Environment="PATH=`) {
			pathLine = line
			break
		}
	}
	if pathLine == "" {
		t.Fatalf("no quoted PATH line:\n%s", unit)
	}
	if !strings.HasSuffix(pathLine, `"`) {
		t.Errorf("quoted PATH line is not closed on its own line: %q", pathLine)
	}
	if strings.Count(pathLine, `"`) != 2 {
		t.Errorf("quoted PATH line has unbalanced quotes: %q", pathLine)
	}
}

// TestGenerateUnitRejectsBackslash: the quoted systemd value undergoes C-style
// unescaping, so a literal backslash cannot survive the round trip and is
// dropped rather than silently mangled.
func TestGenerateUnitRejectsBackslash(t *testing.T) {
	stubHome(t)
	stubServiceSettings(t, config.Settings{DaemonPathExtra: []string{`/opt/back\slash`}})

	unit, err := generateUnit("/usr/local/bin/bossd")
	if err != nil {
		t.Fatalf("generateUnit: %v", err)
	}

	if strings.Contains(unit, `back\slash`) {
		t.Errorf("backslash-bearing entry reached the unit:\n%s", unit)
	}
}

func TestUnitEnvironmentPath(t *testing.T) {
	cases := []struct {
		name string
		unit string
		want string
		ok   bool
	}{
		{
			name: "quoted form this package writes",
			unit: "[Service]\nEnvironment=\"PATH=/a:/b\"\nEnvironment=LC_CTYPE=C.UTF-8\n",
			want: "/a:/b",
			ok:   true,
		},
		{
			name: "bare form an older install carries",
			unit: "[Service]\nEnvironment=PATH=/a:/b\n",
			want: "/a:/b",
			ok:   true,
		},
		{
			// A single Environment= line may hold several assignments; taking
			// everything after PATH= would swallow the ones that follow.
			name: "several assignments on one line",
			unit: "[Service]\nEnvironment=\"PATH=/a b:/bin\" LC_CTYPE=C.UTF-8\n",
			want: "/a b:/bin",
			ok:   true,
		},
		{
			name: "PATH is not the first assignment on the line",
			unit: "[Service]\nEnvironment=LC_CTYPE=C.UTF-8 \"PATH=/a:/b\"\n",
			want: "/a:/b",
			ok:   true,
		},
		{
			name: "a space-bearing directory survives the quotes",
			unit: "[Service]\nEnvironment=\"PATH=/opt/my tools/bin:/usr/bin\"\n",
			want: "/opt/my tools/bin:/usr/bin",
			ok:   true,
		},
		{
			name: "no PATH assignment",
			unit: "[Service]\nEnvironment=LC_CTYPE=C.UTF-8\nExecStart=/bin/bossd\n",
			want: "",
			ok:   false,
		},
		{
			name: "no Environment line at all",
			unit: "[Service]\nExecStart=/bin/bossd\n",
			want: "",
			ok:   false,
		},
		{
			name: "empty PATH is not a usable value",
			unit: "[Service]\nEnvironment=\"PATH=\"\n",
			want: "",
			ok:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := unitEnvironmentPath(tc.unit)
			if got != tc.want || ok != tc.ok {
				t.Errorf("unitEnvironmentPath() = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestUnitEnvironmentPathRoundTripsGeneratedUnit keeps reader and writer honest
// about each other, so a template change cannot silently break the stale-PATH
// comparison in `boss daemon doctor`.
func TestUnitEnvironmentPathRoundTripsGeneratedUnit(t *testing.T) {
	stubHome(t)
	stubServiceSettings(t, config.Settings{DaemonPathExtra: []string{"/opt/my tools/bin"}})

	unit, err := generateUnit("/usr/local/bin/bossd")
	if err != nil {
		t.Fatalf("generateUnit: %v", err)
	}

	got, ok := unitEnvironmentPath(unit)
	if !ok {
		t.Fatalf("unitEnvironmentPath could not read the unit this package renders:\n%s", unit)
	}
	if want := serviceEnvPath(); got != want {
		t.Errorf("round trip = %q, want %q", got, want)
	}
}

// stubSocketReachableAfter replaces the dial seam so isSocketReachable reports
// "unreachable" for the first failures probes and "reachable" afterwards. A
// net.Pipe end is used rather than a real listener because unix socket paths
// have a length limit that t.TempDir() paths already exceed.
func stubSocketReachableAfter(t *testing.T, failures int) {
	t.Helper()

	calls := 0
	original := dialUnixSocket
	t.Cleanup(func() { dialUnixSocket = original })
	dialUnixSocket = func(string, string, time.Duration) (net.Conn, error) {
		calls++
		if calls > failures {
			conn, _ := net.Pipe()
			return conn, nil
		}
		return nil, errors.New("socket not reachable")
	}
}

// writeFakeBossd installs a runnable stand-in for bossd next to a fake boss
// executable, so ResolveBossdPath finds it and the fallback spawn can really
// start it. It exits immediately: the test's dial stub, not the child, decides
// when the socket becomes reachable.
func writeFakeBossd(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	bossdPath := filepath.Join(dir, "bossd")
	if err := os.WriteFile(bossdPath, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("write fake bossd: %v", err)
	}
	original := executablePath
	t.Cleanup(func() { executablePath = original })
	executablePath = func() (string, error) { return filepath.Join(dir, "boss"), nil }
	return bossdPath
}

func writeBossdUnitFile(t *testing.T, home string) {
	t.Helper()

	dir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir unit dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ServiceName), []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatalf("write unit file: %v", err)
	}
}

// TestPlatformEnsureRunningReportsStartMode pins BOS-1183 on the systemd path.
// The signature now carries a StartMode, and the three success points must
// report which of them ran: nothing started, systemd started it (supervised),
// or the direct spawn started it (unsupervised — no Restart=, gone after a
// reboot). Behaviour is otherwise unchanged, which is what the systemctl-call
// assertions below pin: the branch conditions did not move, and BOS-1181 owns
// any change to which path runs.
func TestPlatformEnsureRunningReportsStartMode(t *testing.T) {
	t.Run("already reachable starts nothing", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		stubSocketReachableAfter(t, 0)
		originalRunSystemctl := runSystemctl
		t.Cleanup(func() { runSystemctl = originalRunSystemctl })
		runSystemctl = func(args ...string) ([]byte, error) {
			t.Fatalf("systemctl invoked for an already-reachable socket: %v", args)
			return nil, nil
		}

		mode, err := platformEnsureRunning(filepath.Join(home, "bossd.sock"))
		if err != nil {
			t.Fatalf("platformEnsureRunning: %v", err)
		}
		if mode != StartModeAlreadyRunning {
			t.Errorf("StartMode = %v, want %v", mode, StartModeAlreadyRunning)
		}
	})

	t.Run("systemd start is supervised", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		// BOSS_DAEMON_SKIP_LAUNCHCTL makes platformGetStatus report the unit
		// installed and not running without asking systemd — exactly the
		// Installed && !Running shape that selects the service-manager branch.
		t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
		writeBossdUnitFile(t, home)
		stubSocketReachableAfter(t, 1)
		var calls [][]string
		originalRunSystemctl := runSystemctl
		t.Cleanup(func() { runSystemctl = originalRunSystemctl })
		runSystemctl = func(args ...string) ([]byte, error) {
			calls = append(calls, args)
			return nil, nil
		}

		mode, err := platformEnsureRunning(filepath.Join(home, "bossd.sock"))
		if err != nil {
			t.Fatalf("platformEnsureRunning: %v", err)
		}
		if mode != StartModeServiceManager {
			t.Errorf("StartMode = %v, want %v", mode, StartModeServiceManager)
		}
		want := [][]string{{"--user", "start", ServiceName}}
		if !reflect.DeepEqual(calls, want) {
			t.Errorf("systemctl calls = %v, want %v", calls, want)
		}
	})

	t.Run("direct spawn is unsupervised", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
		// No unit file: platformGetStatus reports not installed, the
		// service-manager branch is skipped, and the fallback spawn runs.
		writeFakeBossd(t)
		// Two failures: the entry probe and the final pre-spawn guard.
		stubSocketReachableAfter(t, 2)
		originalRunSystemctl := runSystemctl
		t.Cleanup(func() { runSystemctl = originalRunSystemctl })
		runSystemctl = func(args ...string) ([]byte, error) {
			t.Fatalf("systemctl invoked with no unit installed: %v", args)
			return nil, nil
		}

		mode, err := platformEnsureRunning(filepath.Join(home, "bossd.sock"))
		if err != nil {
			t.Fatalf("platformEnsureRunning: %v", err)
		}
		if mode != StartModeDetached {
			t.Errorf("StartMode = %v, want %v — an unsupervised spawn must not look like a supervised start", mode, StartModeDetached)
		}
	})

	t.Run("failure carries no mode", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
		t.Setenv("PATH", t.TempDir())
		original := executablePath
		t.Cleanup(func() { executablePath = original })
		executablePath = func() (string, error) { return filepath.Join(t.TempDir(), "boss"), nil }
		stubSocketReachableAfter(t, 2)

		mode, err := platformEnsureRunning(filepath.Join(home, "bossd.sock"))
		if err == nil {
			t.Fatal("platformEnsureRunning succeeded with no bossd to start")
		}
		if mode != StartModeUnknown {
			t.Errorf("StartMode = %v, want %v — an error must never carry a supervision verdict", mode, StartModeUnknown)
		}
	})
}
