//go:build darwin

package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestGeneratePlist(t *testing.T) {
	plist, err := generatePlist("/usr/local/bin/bossd")
	if err != nil {
		t.Fatalf("generatePlist: %v", err)
	}

	checks := []string{
		"<string>com.bossanova.bossd</string>",
		"<string>/usr/local/bin/bossd</string>",
		"<key>RunAtLoad</key>",
		"<true/>",
		"<key>KeepAlive</key>",
		"bossd.stdout.log",
		"bossd.stderr.log",
		// BOS-457: raise the FD limit so setup scripts bossd spawns don't
		// inherit macOS's low default (256) and die with EMFILE.
		"<key>SoftResourceLimits</key>",
		"<key>HardResourceLimits</key>",
		"<key>NumberOfFiles</key>",
		"<integer>65536</integer>",
	}

	for _, check := range checks {
		if !strings.Contains(plist, check) {
			t.Errorf("plist missing %q", check)
		}
	}
}

func TestGenerateMcpPlist(t *testing.T) {
	plist, err := generateMcpPlist("/usr/local/bin/mcp", 8765)
	if err != nil {
		t.Fatalf("generateMcpPlist: %v", err)
	}

	checks := []string{
		"<string>com.bossanova.mcp</string>",
		"<string>/usr/local/bin/mcp</string>",
		"<string>--http</string>",
		"<string>127.0.0.1:8765</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
		"<true/>",
		"mcp.stdout.log",
		"mcp.stderr.log",
	}
	for _, check := range checks {
		if !strings.Contains(plist, check) {
			t.Errorf("plist missing %q", check)
		}
	}

	// Acceptance criterion: the MCP plist PATH must include the agent-runner
	// shim dirs that the bossd plist omits.
	if !strings.Contains(plist, "/.nodenv/shims") {
		t.Error("MCP plist PATH missing ~/.nodenv/shims")
	}
	if !strings.Contains(plist, "/.local/bin") {
		t.Error("MCP plist PATH missing ~/.local/bin")
	}

	// BOS-457: the FD-limit raise is scoped to bossd only; the MCP server does
	// not spawn FD-hungry setup scripts, so its plist must not carry the keys.
	if strings.Contains(plist, "NumberOfFiles") {
		t.Error("MCP plist should not contain NumberOfFiles (bossd-only FD raise)")
	}
}

func TestMcpServicePath(t *testing.T) {
	path, err := mcpServicePath()
	if err != nil {
		t.Fatalf("mcpServicePath: %v", err)
	}
	if !strings.HasSuffix(path, "Library/LaunchAgents/com.bossanova.mcp.plist") {
		t.Errorf("unexpected mcp service path: %s", path)
	}
}

func TestServicePath(t *testing.T) {
	path, err := platformServicePath()
	if err != nil {
		t.Fatalf("platformServicePath: %v", err)
	}

	if !strings.HasSuffix(path, "Library/LaunchAgents/com.bossanova.bossd.plist") {
		t.Errorf("unexpected service path: %s", path)
	}
}

// fakeExitError fabricates a genuine *exec.ExitError with the given exit
// code, so errors.As in bootoutLaunchdService sees the real type launchctl
// invocations produce rather than a hand-rolled stand-in.
func fakeExitError(t *testing.T, code int) *exec.ExitError {
	t.Helper()
	err := exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	var exitErr *exec.ExitError
	if err == nil {
		t.Fatalf("`sh -c exit %d` unexpectedly succeeded", code)
	}
	if ee, ok := err.(*exec.ExitError); ok {
		exitErr = ee
	} else {
		t.Fatalf("expected *exec.ExitError from `sh -c exit %d`, got %v (%T)", code, err, err)
	}
	return exitErr
}

// assertBootoutArgs asserts the recorded runLaunchctl args are exactly the
// label-form bootout target (BOS-627: never a plist path).
func assertBootoutArgs(t *testing.T, args []string, label string) {
	t.Helper()
	uid := strconv.Itoa(os.Getuid())
	want := []string{"bootout", "gui/" + uid + "/" + label}
	if len(args) != len(want) || args[0] != want[0] || args[1] != want[1] {
		t.Fatalf("bootout args = %v, want %v", args, want)
	}
	for _, a := range args {
		if strings.Contains(a, ".plist") {
			t.Errorf("bootout arg %q must not reference a plist path (BOS-627: label form only)", a)
		}
	}
}

// TestBootoutLaunchdService exercises bootoutLaunchdService directly (with an
// injected stillRunning probe) for both the MCP label and the daemon label,
// per the measured launchctl exit codes in BOS-627.
func TestBootoutLaunchdService(t *testing.T) {
	origRunLaunchctl := runLaunchctl
	origTimeout := bootoutVerifyTimeout
	t.Cleanup(func() {
		runLaunchctl = origRunLaunchctl
		bootoutVerifyTimeout = origTimeout
	})

	for _, label := range []string{McpLabel, Label} {
		label := label

		t.Run(label+"/exit_0_is_nil", func(t *testing.T) {
			var gotArgs []string
			runLaunchctl = func(args ...string) ([]byte, error) {
				gotArgs = args
				return []byte(""), nil
			}
			if err := bootoutLaunchdService(label, func() bool {
				t.Fatal("stillRunning must not be consulted on exit 0")
				return true
			}); err != nil {
				t.Fatalf("bootoutLaunchdService: %v", err)
			}
			assertBootoutArgs(t, gotArgs, label)
		})

		t.Run(label+"/exit_3_no_such_process_is_nil", func(t *testing.T) {
			var gotArgs []string
			runLaunchctl = func(args ...string) ([]byte, error) {
				gotArgs = args
				return []byte("Boot-out failed: 3: No such process"), fakeExitError(t, 3)
			}
			if err := bootoutLaunchdService(label, func() bool {
				t.Fatal("stillRunning must not be consulted on exit 3")
				return true
			}); err != nil {
				t.Fatalf("bootoutLaunchdService: %v", err)
			}
			assertBootoutArgs(t, gotArgs, label)
		})

		t.Run(label+"/exit_113_is_nil", func(t *testing.T) {
			runLaunchctl = func(args ...string) ([]byte, error) {
				return []byte("Could not find service"), fakeExitError(t, 113)
			}
			if err := bootoutLaunchdService(label, func() bool {
				t.Fatal("stillRunning must not be consulted on exit 113")
				return true
			}); err != nil {
				t.Fatalf("bootoutLaunchdService: %v", err)
			}
		})

		// This is the reporter's regression: exit 5 ("Input/output error") is
		// what launchctl bootout actually returns for an already-stopped
		// service on this build. Treating every non-{0,3,113} exit as a hard
		// failure (the old exit-113-only check) meant `boss mcp stop` against
		// an already-stopped service always errored. Verifying stillRunning()
		// lets it report success instead.
		t.Run(label+"/exit_5_not_running_is_the_reporters_regression", func(t *testing.T) {
			bootoutVerifyTimeout = 50 * time.Millisecond
			var gotArgs []string
			runLaunchctl = func(args ...string) ([]byte, error) {
				gotArgs = args
				return []byte("Boot-out failed: 5: Input/output error"), fakeExitError(t, 5)
			}
			if err := bootoutLaunchdService(label, func() bool { return false }); err != nil {
				t.Fatalf("bootoutLaunchdService: %v", err)
			}
			assertBootoutArgs(t, gotArgs, label)
		})

		t.Run(label+"/exit_5_still_running_is_an_error", func(t *testing.T) {
			bootoutVerifyTimeout = 50 * time.Millisecond
			runLaunchctl = func(args ...string) ([]byte, error) {
				return []byte("Boot-out failed: 5: Input/output error"), fakeExitError(t, 5)
			}
			err := bootoutLaunchdService(label, func() bool { return true })
			if err == nil {
				t.Fatal("expected an error when stillRunning stays true after bootout exit 5")
			}
			if !strings.Contains(err.Error(), "Boot-out failed: 5: Input/output error") {
				t.Errorf("error %q does not contain the launchctl output verbatim", err)
			}
		})

		// M7: a nil stillRunning probe leaves bootoutLaunchdService with no way
		// to verify the job actually stopped, so it must fail closed --
		// surfacing the launchctl error rather than reporting success for an
		// exit code (5, a generic EIO) that can also mean the job is still up.
		t.Run(label+"/nil_stillRunning_fails_closed", func(t *testing.T) {
			runLaunchctl = func(args ...string) ([]byte, error) {
				return []byte("Boot-out failed: 5: Input/output error"), fakeExitError(t, 5)
			}
			err := bootoutLaunchdService(label, nil)
			if err == nil {
				t.Fatal("expected an error when stillRunning is nil and bootout did not exit 0/3/113")
			}
			if !strings.Contains(err.Error(), "Boot-out failed: 5: Input/output error") {
				t.Errorf("error %q does not contain the launchctl output verbatim", err)
			}
		})
	}
}

// TestStillRunningProbesFailClosed covers the probe-error path of
// platformMcpStop/platformStop's verification callbacks: a status read that
// itself failed is "cannot tell", not "stopped". Reporting "not running" there
// would let bootoutLaunchdService turn an unverifiable end state into a silent
// success after a bootout that really failed -- the same fail-open hazard the
// nil-probe guard closes.
//
// The error is induced by pointing HOME at a tree where Library/LaunchAgents is
// a regular FILE, so os.Stat of the plist beneath it returns ENOTDIR rather than
// ENOENT (ENOENT is the ordinary not-installed case and must stay a nil error).
func TestStillRunningProbesFailClosed(t *testing.T) {
	origRunLaunchctl := runLaunchctl
	origTimeout := bootoutVerifyTimeout
	t.Cleanup(func() {
		runLaunchctl = origRunLaunchctl
		bootoutVerifyTimeout = origTimeout
	})

	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "Library"), 0o700); err != nil {
		t.Fatalf("mkdir Library: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, "Library", "LaunchAgents"), []byte("not a dir"), 0o600); err != nil {
		t.Fatalf("write LaunchAgents-as-file: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")

	// Sanity: the induced failure really is a non-ENOENT status error, so the
	// assertions below are not vacuous.
	if _, err := platformMcpGetStatus(); err == nil {
		t.Fatal("platformMcpGetStatus: want a non-nil error when LaunchAgents is not a directory")
	}
	if _, err := platformGetStatus(); err == nil {
		t.Fatal("platformGetStatus: want a non-nil error when LaunchAgents is not a directory")
	}

	if !mcpStillRunningProbe() {
		t.Error("mcpStillRunningProbe() = false on a probe error, want true (fail closed)")
	}
	if !bossdStillRunningProbe() {
		t.Error("bossdStillRunningProbe() = false on a probe error, want true (fail closed)")
	}

	// End to end: a non-{0,3,113} bootout whose verification cannot be read
	// must surface the launchctl error, not report success.
	bootoutVerifyTimeout = 20 * time.Millisecond
	runLaunchctl = func(_ ...string) ([]byte, error) {
		return []byte("Boot-out failed: 5: Input/output error"), fakeExitError(t, 5)
	}
	for name, stop := range map[string]func() error{
		"platformMcpStop": platformMcpStop,
		"platformStop":    platformStop,
	} {
		err := stop()
		if err == nil {
			t.Errorf("%s() = nil after bootout exit 5 with an unreadable status probe, want an error", name)
			continue
		}
		if !strings.Contains(err.Error(), "Boot-out failed: 5: Input/output error") {
			t.Errorf("%s() error %q does not carry the launchctl output", name, err)
		}
	}
}

// TestPlatformMcpStopRealProbeAlreadyStopped is the counterpart boundary to
// TestStillRunningProbesFailClosed: with the REAL mcpStillRunningProbe (not an
// injected `return false`) and no plist installed, a bootout exit 5 must still
// resolve to success. That is acceptance criterion 1's neighbourhood, and it is
// the path a future edit to mcpStillRunningProbe could most easily break --
// making `boss mcp stop` error again when nothing is running, which is the
// original BOS-627 bug.
func TestPlatformMcpStopRealProbeAlreadyStopped(t *testing.T) {
	origRunLaunchctl := runLaunchctl
	origTimeout := bootoutVerifyTimeout
	t.Cleanup(func() {
		runLaunchctl = origRunLaunchctl
		bootoutVerifyTimeout = origTimeout
	})

	// A real (empty) LaunchAgents directory: os.Stat of the plist beneath it
	// returns ENOENT, which platformMcpGetStatus must report as a nil error
	// with Installed=false -- not as a probe failure.
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "Library", "LaunchAgents"), 0o700); err != nil {
		t.Fatalf("mkdir LaunchAgents: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")

	st, err := platformMcpGetStatus()
	if err != nil {
		t.Fatalf("platformMcpGetStatus: want nil error for an absent plist, got %v", err)
	}
	if st.Installed {
		t.Fatalf("platformMcpGetStatus: Installed = true, want false for an absent plist")
	}
	if mcpStillRunningProbe() {
		t.Fatal("mcpStillRunningProbe() = true with no plist installed, want false")
	}

	bootoutVerifyTimeout = 20 * time.Millisecond
	runLaunchctl = func(_ ...string) ([]byte, error) {
		return []byte("Boot-out failed: 5: Input/output error"), fakeExitError(t, 5)
	}
	if err := platformMcpStop(); err != nil {
		t.Errorf("platformMcpStop() = %v, want nil (bootout exit 5 with nothing running is the already-stopped case)", err)
	}
}

// TestPlatformMcpStopWiring exercises platformMcpStop/platformStop
// themselves (rather than bootoutLaunchdService directly) so the label
// selection and the skipLaunchctl() short-circuit are covered end to end.
func TestPlatformMcpStopWiring(t *testing.T) {
	origRunLaunchctl := runLaunchctl
	origTimeout := bootoutVerifyTimeout
	t.Cleanup(func() {
		runLaunchctl = origRunLaunchctl
		bootoutVerifyTimeout = origTimeout
	})

	t.Run("skip_launchctl_short_circuits_both", func(t *testing.T) {
		t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
		runLaunchctl = func(args ...string) ([]byte, error) {
			t.Fatal("runLaunchctl must not be called when BOSS_DAEMON_SKIP_LAUNCHCTL is set")
			return nil, nil
		}
		if err := platformMcpStop(); err != nil {
			t.Fatalf("platformMcpStop: %v", err)
		}
		if err := platformStop(); err != nil {
			t.Fatalf("platformStop: %v", err)
		}
	})

	t.Run("wiring_targets_the_correct_label", func(t *testing.T) {
		t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
		var gotArgs []string
		runLaunchctl = func(args ...string) ([]byte, error) {
			gotArgs = args
			return []byte(""), nil
		}

		if err := platformMcpStop(); err != nil {
			t.Fatalf("platformMcpStop: %v", err)
		}
		assertBootoutArgs(t, gotArgs, McpLabel)

		if err := platformStop(); err != nil {
			t.Fatalf("platformStop: %v", err)
		}
		assertBootoutArgs(t, gotArgs, Label)
	})
}
