package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/recurser/boss/internal/daemon"
	"github.com/recurser/boss/internal/termreset"
	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/daemonbin"
	"github.com/recurser/bossalib/daemonstate"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
)

func TestRunDaemonDoctorReportsStagedInstall(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("staged LaunchAgent diagnostics are macOS-specific")
	}
	home, sourcePath, stagedPath := prepareDaemonDoctorInstall(t)
	writeDaemonDoctorPlist(t, home, stagedPath)

	writeDaemonDoctorState(t, stagedPath, true, []daemonstate.TCCProbeResult{
		{Path: filepath.Join(home, "Documents"), Status: daemonstate.TCCProbeStatusOK},
	})

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	if err := runDaemonDoctor(cmd); err != nil {
		t.Fatalf("runDaemonDoctor: %v", err)
	}

	for _, want := range []string{
		"source bossd",
		sourcePath,
		"staged bossd",
		stagedPath,
		"up to date",
		"ProgramArguments",
		"running executable",
		"protected root",
	} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("output missing %q:\n%s", want, output.String())
		}
	}
}

func TestRunDaemonDoctorReportsBlockedRootRemediation(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("protected-root diagnostics are macOS-specific")
	}
	home, _, stagedPath := prepareDaemonDoctorInstall(t)
	writeDaemonDoctorPlist(t, home, stagedPath)
	if err := os.MkdirAll(filepath.Join(home, "Documents"), 0o755); err != nil {
		t.Fatalf("mkdir readable Documents: %v", err)
	}
	writeDaemonDoctorState(t, stagedPath, true, []daemonstate.TCCProbeResult{
		{Path: filepath.Join(home, "Documents"), Status: daemonstate.TCCProbeStatusDenied, Diagnostic: "operation not permitted"},
		{Path: filepath.Join(home, "Desktop"), Status: daemonstate.TCCProbeStatusBlocked, Diagnostic: "probe timed out"},
	})

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	if err := runDaemonDoctor(cmd); !errors.Is(err, errDaemonDoctorUnhealthy) {
		t.Fatalf("runDaemonDoctor error = %v, want unhealthy error", err)
	}

	for _, want := range []string{"denied", "operation not permitted", "blocked", "probe timed out", "System Settings", stagedPath, "not a Homebrew"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("output missing %q:\n%s", want, output.String())
		}
	}
}

func TestRunDaemonDoctorReportsErrorRootAsUnhealthyWithoutPermissionRemediation(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("protected-root diagnostics are macOS-specific")
	}
	home, _, stagedPath := prepareDaemonDoctorInstall(t)
	writeDaemonDoctorPlist(t, home, stagedPath)
	writeDaemonDoctorState(t, stagedPath, true, []daemonstate.TCCProbeResult{
		{Path: filepath.Join(home, "Documents"), Status: daemonstate.TCCProbeStatusError, Diagnostic: "too many open files"},
	})

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	if err := runDaemonDoctor(cmd); !errors.Is(err, errDaemonDoctorUnhealthy) {
		t.Fatalf("runDaemonDoctor error = %v, want unhealthy error", err)
	}
	if !strings.Contains(output.String(), "error") || !strings.Contains(output.String(), "too many open files") || !strings.Contains(output.String(), "run 'boss daemon restart'") {
		t.Fatalf("doctor did not report unhealthy probe error:\n%s", output.String())
	}
	if strings.Contains(output.String(), "System Settings") {
		t.Fatalf("doctor offered permission remediation for probe error:\n%s", output.String())
	}
}

func TestRunDaemonDoctorReportsProbeStateUnavailableWithoutPersistedResults(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("protected-root diagnostics are macOS-specific")
	}
	home, _, stagedPath := prepareDaemonDoctorInstall(t)
	writeDaemonDoctorPlist(t, home, stagedPath)
	documents := filepath.Join(home, "Documents")
	if err := os.MkdirAll(documents, 0o755); err != nil {
		t.Fatalf("mkdir readable Documents: %v", err)
	}
	writeDaemonDoctorState(t, stagedPath, false, nil)

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	if err := runDaemonDoctor(cmd); err != nil {
		t.Fatalf("runDaemonDoctor: %v", err)
	}
	if !strings.Contains(output.String(), "unavailable") {
		t.Fatalf("output missing unavailable probe status:\n%s", output.String())
	}
	if strings.Contains(output.String(), "protected root "+documents+": ok") {
		t.Fatalf("doctor substituted CLI filesystem readability for bossd state:\n%s", output.String())
	}
}

func TestRunDaemonDoctorReportsCompletedZeroRootProbe(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("protected-root diagnostics are macOS-specific")
	}
	home, _, stagedPath := prepareDaemonDoctorInstall(t)
	writeDaemonDoctorPlist(t, home, stagedPath)
	writeDaemonDoctorState(t, stagedPath, true, nil)

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	if err := runDaemonDoctor(cmd); err != nil {
		t.Fatalf("runDaemonDoctor: %v", err)
	}
	if !strings.Contains(output.String(), "no protected roots") {
		t.Fatalf("output missing completed zero-root status:\n%s", output.String())
	}
	if strings.Contains(output.String(), "not yet recorded") {
		t.Fatalf("completed zero-root probe reported unavailable:\n%s", output.String())
	}
}

func TestRunDaemonDoctorRendersLegacyPersistedProbeResults(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("protected-root diagnostics are macOS-specific")
	}
	home, _, stagedPath := prepareDaemonDoctorInstall(t)
	writeDaemonDoctorPlist(t, home, stagedPath)
	writeDaemonDoctorState(t, stagedPath, false, []daemonstate.TCCProbeResult{
		{Path: filepath.Join(home, "Documents"), Status: daemonstate.TCCProbeStatusDenied},
	})

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	if err := runDaemonDoctor(cmd); !errors.Is(err, errDaemonDoctorUnhealthy) {
		t.Fatalf("runDaemonDoctor error = %v, want unhealthy error", err)
	}
	if !strings.Contains(output.String(), "denied") || !strings.Contains(output.String(), "System Settings") {
		t.Fatalf("doctor did not render legacy denied probe state:\n%s", output.String())
	}
	if strings.Contains(output.String(), "not yet recorded") {
		t.Fatalf("doctor hid legacy probe state as unavailable:\n%s", output.String())
	}
}

func TestRunDaemonDoctorReadsConfiguredDaemonAppDataState(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("protected-root diagnostics are macOS-specific")
	}
	home, _, stagedPath := prepareDaemonDoctorInstall(t)
	writeDaemonDoctorPlist(t, home, stagedPath)
	configuredAppDataDir := filepath.Join(home, "configured-daemon-data")
	settingsPath := filepath.Join(home, "settings.json")
	settings := config.DefaultSettings()
	settings.AppDataDir = configuredAppDataDir
	if err := config.SaveTo(settingsPath, settings); err != nil {
		t.Fatalf("save configured settings: %v", err)
	}
	t.Setenv("BOSS_SETTINGS_PATH", settingsPath)
	if err := daemonstate.Write(configuredAppDataDir, daemonstate.Metadata{
		PID:               4242,
		ExecutablePath:    stagedPath,
		TCCProbeCompleted: true,
		TCCProbeResults: []daemonstate.TCCProbeResult{
			{Path: filepath.Join(home, "Documents"), Status: daemonstate.TCCProbeStatusDenied},
		},
	}); err != nil {
		t.Fatalf("write configured daemon state: %v", err)
	}
	stubDaemonDoctorProcess(t, nil)

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	if err := runDaemonDoctor(cmd); !errors.Is(err, errDaemonDoctorUnhealthy) {
		t.Fatalf("runDaemonDoctor error = %v, want unhealthy error", err)
	}
	if !strings.Contains(output.String(), "denied") || !strings.Contains(output.String(), "running executable") {
		t.Fatalf("doctor did not read configured daemon state:\n%s", output.String())
	}
	if !strings.Contains(output.String(), "staged bossd: "+stagedPath+" — up to date") || strings.Contains(output.String(), "FAIL staged bossd") {
		t.Fatalf("doctor did not retain the default stable staging path:\n%s", output.String())
	}
}

func TestRunDaemonDoctorMissingOrEmptyPlistWithOKProbeHasNoPermissionGuidance(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("LaunchAgent diagnostics are macOS-specific")
	}

	for _, tt := range []struct {
		name              string
		writePlist        func(t *testing.T, home, stagedPath string)
		want              string
		wantRemediation   string
		unwantRemediation string
	}{
		{
			name:              "missing plist",
			writePlist:        func(*testing.T, string, string) {},
			want:              "FAIL LaunchAgent plist",
			wantRemediation:   "boss daemon install",
			unwantRemediation: "boss daemon restart",
		},
		{
			name: "empty ProgramArguments",
			writePlist: func(t *testing.T, home, _ string) {
				writeDaemonDoctorEmptyArgumentsPlist(t, home)
			},
			want:              "FAIL LaunchAgent ProgramArguments",
			wantRemediation:   "boss daemon restart",
			unwantRemediation: "boss daemon install",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home, _, stagedPath := prepareDaemonDoctorInstall(t)
			tt.writePlist(t, home, stagedPath)
			writeDaemonDoctorState(t, stagedPath, true, []daemonstate.TCCProbeResult{
				{Path: filepath.Join(home, "Documents"), Status: daemonstate.TCCProbeStatusOK},
			})

			var output bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&output)
			if err := runDaemonDoctor(cmd); !errors.Is(err, errDaemonDoctorUnhealthy) {
				t.Fatalf("runDaemonDoctor error = %v, want unhealthy error", err)
			}
			if !strings.Contains(output.String(), tt.want) || !strings.Contains(output.String(), tt.wantRemediation) {
				t.Fatalf("output missing structural remediation:\n%s", output.String())
			}
			if strings.Contains(output.String(), tt.unwantRemediation) {
				t.Fatalf("output contains incorrect structural remediation:\n%s", output.String())
			}
			if strings.Contains(output.String(), "System Settings") || strings.Contains(output.String(), "Files and Folders") {
				t.Fatalf("structural plist fault incorrectly suggested permission remediation:\n%s", output.String())
			}
		})
	}
}

func TestRunDaemonDoctorStaleInstallDoesNotSuggestPermissionGrant(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("stable daemon staging is a macOS install behavior")
	}
	home, _, stagedPath := prepareDaemonDoctorInstall(t)
	writeDaemonDoctorPlist(t, home, stagedPath)
	if err := os.WriteFile(stagedPath, []byte("stale bossd fixture\n"), 0o755); err != nil {
		t.Fatalf("make staged bossd stale: %v", err)
	}
	writeDaemonDoctorState(t, stagedPath, true, []daemonstate.TCCProbeResult{
		{Path: filepath.Join(home, "Documents"), Status: daemonstate.TCCProbeStatusOK},
	})

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	if err := runDaemonDoctor(cmd); !errors.Is(err, errDaemonDoctorUnhealthy) {
		t.Fatalf("runDaemonDoctor error = %v, want unhealthy error", err)
	}
	if !strings.Contains(output.String(), "stale") || !strings.Contains(output.String(), "boss daemon restart") {
		t.Fatalf("output missing structural remediation:\n%s", output.String())
	}
	if strings.Contains(output.String(), "System Settings") || strings.Contains(output.String(), "Files and Folders") {
		t.Fatalf("stale install incorrectly suggested permission remediation:\n%s", output.String())
	}
}

func TestRunDaemonDoctorFailsCellarLaunchAgentPath(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("LaunchAgent diagnostics are macOS-specific")
	}
	home, _, stagedPath := prepareDaemonDoctorInstall(t)
	cellarPath := filepath.Join(home, "Cellar", "bossanova", "1.90.0", "bin", "bossd")
	writeDaemonDoctorPlist(t, home, cellarPath)
	writeDaemonDoctorState(t, stagedPath, true, []daemonstate.TCCProbeResult{
		{Path: filepath.Join(home, "Documents"), Status: daemonstate.TCCProbeStatusOK},
	})

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	if err := runDaemonDoctor(cmd); !errors.Is(err, errDaemonDoctorUnhealthy) {
		t.Fatalf("runDaemonDoctor error = %v, want unhealthy error", err)
	}

	for _, want := range []string{
		"FAIL",
		cellarPath,
		stagedPath,
		"boss daemon restart",
	} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("output missing %q:\n%s", want, output.String())
		}
	}
	if strings.Contains(output.String(), "System Settings") {
		t.Fatalf("structural plist fault incorrectly suggested permission remediation:\n%s", output.String())
	}
}

func TestRunDaemonDoctorFailsNonStagedLaunchAgentPathWithoutPermissionGuidance(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("LaunchAgent diagnostics are macOS-specific")
	}
	home, _, stagedPath := prepareDaemonDoctorInstall(t)
	nonStagedPath := filepath.Join(home, "custom", "bin", "bossd")
	writeDaemonDoctorPlist(t, home, nonStagedPath)
	writeDaemonDoctorState(t, stagedPath, true, []daemonstate.TCCProbeResult{
		{Path: filepath.Join(home, "Documents"), Status: daemonstate.TCCProbeStatusOK},
	})

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	if err := runDaemonDoctor(cmd); !errors.Is(err, errDaemonDoctorUnhealthy) {
		t.Fatalf("runDaemonDoctor error = %v, want unhealthy error", err)
	}

	for _, want := range []string{
		"FAIL",
		nonStagedPath,
		stagedPath,
		"boss daemon restart",
	} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("output missing %q:\n%s", want, output.String())
		}
	}
	if strings.Contains(output.String(), "System Settings") {
		t.Fatalf("structural plist fault incorrectly suggested permission remediation:\n%s", output.String())
	}
}

// TestRunDaemonDoctorReportsRunningImageBehindStagedFile is the exact BOS-864
// state that used to be reported as fully healthy: the staged file is current,
// but the live process started before that file was written, so it is
// executing different bytes than the path it names.
func TestRunDaemonDoctorReportsRunningImageBehindStagedFile(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("stable daemon staging is a macOS install behavior")
	}
	home, _, stagedPath := prepareDaemonDoctorInstall(t)
	writeDaemonDoctorPlist(t, home, stagedPath)
	writeDaemonDoctorStateStartedAt(t, stagedPath, time.Now().Add(-time.Hour))

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	if err := runDaemonDoctor(cmd); !errors.Is(err, errDaemonDoctorUnhealthy) {
		t.Fatalf("runDaemonDoctor error = %v, want unhealthy error", err)
	}

	// The two facts must be visibly separate: the file is fine, the process is not.
	if !strings.Contains(output.String(), "staged bossd: "+stagedPath+" — up to date") {
		t.Fatalf("staged-file verdict changed; it must stay a separate, correct line:\n%s", output.String())
	}
	for _, want := range []string{
		"running executable: " + stagedPath,
		"stale: the process started",
		"but the staged binary was written",
		"run 'boss daemon restart'",
	} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("output missing %q:\n%s", want, output.String())
		}
	}
	// A running-image mismatch is a restart problem, not a grant problem.
	if strings.Contains(output.String(), "System Settings") || strings.Contains(output.String(), "Files and Folders") {
		t.Fatalf("running-image mismatch incorrectly suggested permission remediation:\n%s", output.String())
	}
	if strings.Contains(output.String(), "run 'boss daemon start'") {
		t.Fatalf("a live daemon should not be offered the start remediation:\n%s", output.String())
	}
}

func TestRunDaemonDoctorReportsRunningImageUpToDate(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("stable daemon staging is a macOS install behavior")
	}
	home, _, stagedPath := prepareDaemonDoctorInstall(t)
	writeDaemonDoctorPlist(t, home, stagedPath)
	staged := time.Now().Add(-time.Hour)
	if err := os.Chtimes(stagedPath, staged, staged); err != nil {
		t.Fatalf("backdate staged binary: %v", err)
	}
	writeDaemonDoctorStateStartedAt(t, stagedPath, time.Now())

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	if err := runDaemonDoctor(cmd); err != nil {
		t.Fatalf("runDaemonDoctor: %v", err)
	}
	if !strings.Contains(output.String(), "running executable: "+stagedPath) || !strings.Contains(output.String(), "up to date (started") {
		t.Fatalf("output missing healthy running-image verdict:\n%s", output.String())
	}
	if strings.Contains(output.String(), "Remediation:") || strings.Contains(output.String(), "boss daemon restart") {
		t.Fatalf("healthy daemon offered remediation:\n%s", output.String())
	}
}

// TestRunDaemonDoctorReportsDeadRecordedPID covers the second diagnostic
// defect: doctor printed a `running executable:` line for a PID it never
// probed, and a stale record never set unhealthy at all.
func TestRunDaemonDoctorReportsDeadRecordedPID(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("stable daemon staging is a macOS install behavior")
	}
	home, _, stagedPath := prepareDaemonDoctorInstall(t)
	writeDaemonDoctorPlist(t, home, stagedPath)
	writeDaemonDoctorStateStartedAt(t, stagedPath, time.Now())
	stubDaemonDoctorProcess(t, syscall.ESRCH)

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	if err := runDaemonDoctor(cmd); !errors.Is(err, errDaemonDoctorUnhealthy) {
		t.Fatalf("runDaemonDoctor error = %v, want unhealthy error", err)
	}
	for _, want := range []string{"not running", "stale", "run 'boss daemon start'"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("output missing %q:\n%s", want, output.String())
		}
	}
	if strings.Contains(output.String(), "run 'boss daemon restart'") {
		t.Fatalf("a dead daemon must not be told to restart:\n%s", output.String())
	}
}

func TestRunDaemonDoctorReportsMissingStateRecordAsUnknown(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("stable daemon staging is a macOS install behavior")
	}
	home, _, stagedPath := prepareDaemonDoctorInstall(t)
	writeDaemonDoctorPlist(t, home, stagedPath)

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	if err := runDaemonDoctor(cmd); err != nil {
		t.Fatalf("runDaemonDoctor: %v", err)
	}
	if !strings.Contains(output.String(), "running bossd: unknown") {
		t.Fatalf("output missing explicit unknown running verdict:\n%s", output.String())
	}
	if strings.Contains(output.String(), "running executable:") {
		t.Fatalf("doctor fabricated a running-executable line without a state record:\n%s", output.String())
	}
}

// TestRunDaemonDoctorReportsUnknownStartTimeAsUnknown pins that an
// undeterminable running-image comparison is never rendered as healthy — and
// never as unhealthy either, so doctor does not gain a second
// unknown-as-broken check.
func TestRunDaemonDoctorReportsUnknownStartTimeAsUnknown(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("stable daemon staging is a macOS install behavior")
	}
	home, _, stagedPath := prepareDaemonDoctorInstall(t)
	writeDaemonDoctorPlist(t, home, stagedPath)
	writeDaemonDoctorStateStartedAt(t, stagedPath, time.Time{})

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	if err := runDaemonDoctor(cmd); err != nil {
		t.Fatalf("runDaemonDoctor: %v", err)
	}
	if !strings.Contains(output.String(), "running executable: "+stagedPath) ||
		!strings.Contains(output.String(), "— unknown (daemon start time unknown)") {
		t.Fatalf("output missing explicit unknown running-image verdict:\n%s", output.String())
	}
	if strings.Contains(output.String(), "up to date (started") {
		t.Fatalf("an undeterminable running image was reported as healthy:\n%s", output.String())
	}
}

func TestRunDaemonDoctorReportsMacOSChecksNotApplicable(t *testing.T) {
	previous := daemonDoctorGOOS
	daemonDoctorGOOS = "linux"
	t.Cleanup(func() { daemonDoctorGOOS = previous })
	// This test asserts the platform early return, not supervision, and writes no
	// state record — so an unstubbed supervision check would read the developer's
	// REAL service manager and REAL bossd state, and fail on any machine whose
	// daemon happens to be running detached. Disabling the probe is the accurate
	// way to say "not under test here".
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
	// BOS-1183: launchd spawn history is macOS-only, so the Linux path must not
	// merely omit the LINE — it must never ask. A probe that runs and prints
	// nothing still shells out to a service manager this platform does not have.
	spawnCalls := stubDaemonDoctorSpawnHistory(t, daemon.SpawnHistory{State: daemon.SpawnStateUnsupported}, nil)

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	if err := runDaemonDoctor(cmd); err != nil {
		t.Fatalf("runDaemonDoctor: %v", err)
	}
	if !strings.Contains(output.String(), "not applicable") {
		t.Fatalf("output missing not-applicable status:\n%s", output.String())
	}
	// Linux behaviour is unchanged: no staging comparison, no liveness probe,
	// no launchd spawn history and no macOS startup-failure directive.
	for _, unwanted := range []string{
		"staged bossd", "running executable", "running bossd",
		"launchd spawn history", "startup diagnosis",
	} {
		if strings.Contains(output.String(), unwanted) {
			t.Fatalf("non-darwin doctor emitted %q:\n%s", unwanted, output.String())
		}
	}
	if *spawnCalls != 0 {
		t.Fatalf("non-darwin doctor probed launchd %d times, want 0", *spawnCalls)
	}
}

// TestWarnIfDaemonBinaryStaleWritesExactlyOneStderrLine is the BOS-864 headline
// behaviour: a stale staged copy under a Homebrew install warns on every
// subcommand, on stderr, leaving stdout untouched for --json consumers.
func TestWarnIfDaemonBinaryStaleWritesExactlyOneStderrLine(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("stable daemon staging is a macOS install behavior")
	}
	prepareStaleHomebrewInstall(t)

	stdout, stderr, cmd := newStalenessWarningCommand(t, "boss")
	warnIfDaemonBinaryStale(cmd)

	if !strings.Contains(stderr.String(), "bossd is running an older build") ||
		!strings.Contains(stderr.String(), "boss daemon restart") ||
		!strings.Contains(stderr.String(), "boss daemon doctor") {
		t.Fatalf("stderr missing the staleness warning:\n%s", stderr.String())
	}
	if got := strings.Count(strings.TrimSpace(stderr.String()), "\n"); got != 0 {
		t.Fatalf("stderr = %q, want exactly one line", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want untouched", stdout.String())
	}
}

// TestRootCommandEmitsStaleDaemonWarningThroughPersistentPreRun drives a real
// rootCmd() so the PersistentPreRunE call to warnIfDaemonBinaryStale is the
// thing under test, not the helper. Every other test in this file calls the
// helper directly against a synthetic cobra.Command, so deleting that one line
// from rootCmd would leave all of them green while the headline acceptance
// criterion ("every boss subcommand prints exactly one warning line") silently
// became false. This is the test that fails when the wiring goes away.
//
// fix-terminal is the subcommand because it dials no daemon, mutates no global
// state, touches no network, and is not one of the four remedy paths the
// warning deliberately skips. rootCmd's PersistentPreRunE calls
// warnIfDaemonBinaryStale BEFORE the gen-skill / fix-terminal / tail / skills bypass
// returns, so the bypass does not suppress the warning — and fix-terminal
// writes a fixed sequence to cmd.OutOrStdout(), which makes the stdout
// assertion below a real stream separation rather than an empty buffer nobody
// ever wrote to.
func TestRootCommandEmitsStaleDaemonWarningThroughPersistentPreRun(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("stable daemon staging is a macOS install behavior")
	}
	prepareStaleHomebrewInstall(t)

	root := rootCmd()
	var stdout, stderr bytes.Buffer
	root.SetArgs([]string{"fix-terminal"})
	root.SetOut(&stdout)
	root.SetErr(&stderr)

	if err := root.Execute(); err != nil {
		t.Fatalf("boss fix-terminal through the real root: %v", err)
	}

	if !strings.Contains(stderr.String(), daemonStalenessWarningText) {
		t.Fatalf("rootCmd PersistentPreRunE did not emit the staleness warning on stderr:\n%q", stderr.String())
	}
	if got := strings.Count(strings.TrimSpace(stderr.String()), "\n"); got != 0 {
		t.Fatalf("stderr = %q, want exactly one line", stderr.String())
	}
	if got := stdout.String(); got != termreset.AbnormalExitReset {
		t.Fatalf("stdout = %q, want only the fix-terminal reset sequence %q", got, termreset.AbnormalExitReset)
	}
}

func TestWarnIfDaemonBinaryStaleStaysSilentForDevBuilds(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("stable daemon staging is a macOS install behavior")
	}
	// prepareDaemonDoctorInstall resolves the source to a checkout-style
	// <home>/opt/bossanova/bin/bossd, not a Cellar keg.
	_, _, stagedPath := prepareDaemonDoctorInstall(t)
	if err := os.WriteFile(stagedPath, []byte("older staged dev bossd fixture with a different size\n"), 0o755); err != nil {
		t.Fatalf("make staged bossd stale: %v", err)
	}

	// Guard against a vacuous pass: the staged copy really is stale, so the
	// Cellar predicate is the only thing suppressing the warning.
	sourcePath, err := daemon.ResolveBossdPath()
	if err != nil {
		t.Fatalf("ResolveBossdPath: %v", err)
	}
	staleness, err := daemonbin.Inspect(sourcePath, stagedPath, time.Time{})
	if err != nil {
		t.Fatalf("daemonbin.Inspect: %v", err)
	}
	if !staleness.StagedBehindSource {
		t.Fatalf("fixture is not actually stale: %+v", staleness)
	}
	if daemonbin.IsHomebrewCellarBinary(sourcePath) {
		t.Fatalf("dev fixture %q classified as a Homebrew Cellar binary", sourcePath)
	}

	_, stderr, cmd := newStalenessWarningCommand(t, "boss")
	warnIfDaemonBinaryStale(cmd)

	if stderr.Len() != 0 {
		t.Fatalf("dev build warned about staleness: %q", stderr.String())
	}
}

func TestWarnIfDaemonBinaryStaleIsSuppressible(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("stable daemon staging is a macOS install behavior")
	}
	prepareStaleHomebrewInstall(t)
	t.Setenv(skipDaemonStalenessWarningEnv, "1")

	_, stderr, cmd := newStalenessWarningCommand(t, "boss")
	warnIfDaemonBinaryStale(cmd)

	if stderr.Len() != 0 {
		t.Fatalf("suppression variable ignored: %q", stderr.String())
	}
}

func TestWarnIfDaemonBinaryStaleIsDarwinOnly(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("stable daemon staging is a macOS install behavior")
	}
	prepareStaleHomebrewInstall(t)
	previous := daemonStalenessGOOS
	daemonStalenessGOOS = "linux"
	t.Cleanup(func() { daemonStalenessGOOS = previous })

	_, stderr, cmd := newStalenessWarningCommand(t, "boss")
	warnIfDaemonBinaryStale(cmd)

	if stderr.Len() != 0 {
		t.Fatalf("non-darwin emitted a staleness warning: %q", stderr.String())
	}
}

func TestWarnIfDaemonBinaryStaleSkipsRemedyCommands(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("stable daemon staging is a macOS install behavior")
	}
	prepareStaleHomebrewInstall(t)

	for _, path := range [][]string{
		{"boss", "daemon", "doctor"},
		{"boss", "daemon", "restart"},
		{"boss", "daemon", "start"},
		{"boss", "upgrade"},
	} {
		_, stderr, cmd := newStalenessWarningCommand(t, path...)
		warnIfDaemonBinaryStale(cmd)
		if stderr.Len() != 0 {
			t.Errorf("%q emitted a staleness warning: %q", cmd.CommandPath(), stderr.String())
		}
	}

	// The remedy list is a prefix match, not a substring match: a command that
	// merely starts with the same words must still warn.
	_, stderr, cmd := newStalenessWarningCommand(t, "boss", "daemon", "status")
	warnIfDaemonBinaryStale(cmd)
	if stderr.Len() == 0 {
		t.Fatalf("%q suppressed the staleness warning", cmd.CommandPath())
	}
}

func TestWarnIfDaemonBinaryStaleSwallowsUnresolvableInputs(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("stable daemon staging is a macOS install behavior")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("PATH", "")

	_, stderr, cmd := newStalenessWarningCommand(t, "boss")
	warnIfDaemonBinaryStale(cmd)

	if stderr.Len() != 0 {
		t.Fatalf("unresolvable source produced output: %q", stderr.String())
	}
}

func TestRunDaemonStatusReportsRunningImage(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("stable daemon staging is a macOS install behavior")
	}
	restoreDaemonCommandStubs(t)
	stagedPath := prepareStaleHomebrewInstall(t)
	writeDaemonDoctorStateStartedAt(t, stagedPath, time.Now().Add(-time.Hour))

	daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: true, Running: true, PID: 4242}, nil
	}
	daemonSocketReachable = func(string) bool { return true }

	output := captureStdout(t, func() {
		if err := runDaemonStatus(nil); err != nil {
			t.Fatalf("runDaemonStatus: %v", err)
		}
	})

	if !strings.Contains(output, "standalone executable:") {
		t.Fatalf("status lost the existing executable line:\n%s", output)
	}
	if !strings.Contains(output, "running image: stale") || !strings.Contains(output, "boss daemon restart") {
		t.Fatalf("status missing the running-image distinction:\n%s", output)
	}
}

// prepareStaleHomebrewInstall builds the released layout the warning exists
// for: a Cellar keg holding a newer bossd than the staged copy launchd keeps
// respawning.
func prepareStaleHomebrewInstall(t *testing.T) (stagedPath string) {
	t.Helper()
	home, _, stagedPath := prepareDaemonDoctorInstall(t)
	cellarBinDir := filepath.Join(home, "opt", "homebrew", "Cellar", "bossanova", "1.91.0", "bin")
	if err := os.MkdirAll(cellarBinDir, 0o755); err != nil {
		t.Fatalf("mkdir Cellar bin: %v", err)
	}
	sourcePath := filepath.Join(cellarBinDir, "bossd")
	if err := os.WriteFile(sourcePath, []byte("newly installed bossd v1.91.0, a different size entirely\n"), 0o755); err != nil {
		t.Fatalf("write Cellar bossd: %v", err)
	}
	t.Setenv("PATH", cellarBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return stagedPath
}

func newStalenessWarningCommand(t *testing.T, path ...string) (stdout, stderr *bytes.Buffer, leaf *cobra.Command) {
	t.Helper()
	if len(path) == 0 {
		t.Fatal("newStalenessWarningCommand needs at least a root name")
	}
	stdout, stderr = &bytes.Buffer{}, &bytes.Buffer{}
	leaf = &cobra.Command{Use: path[0]}
	for _, use := range path[1:] {
		child := &cobra.Command{Use: use}
		leaf.AddCommand(child)
		leaf = child
	}
	leaf.SetOut(stdout)
	leaf.SetErr(stderr)
	return stdout, stderr, leaf
}

func TestDaemonCommandRegistersDoctor(t *testing.T) {
	command, _, err := daemonCmd().Find([]string{"doctor"})
	if err != nil {
		t.Fatalf("find doctor command: %v", err)
	}
	if command.Use != "doctor" {
		t.Fatalf("doctor command Use = %q, want doctor", command.Use)
	}
}

func TestRunDaemonInstallReportsStagedAndSourcePaths(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("stable daemon staging is a macOS install behavior")
	}

	_, sourcePath, stagedPath := prepareDaemonDoctorPaths(t)
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	if err := runDaemonInstall(cmd); err != nil {
		t.Fatalf("runDaemonInstall: %v", err)
	}

	for _, want := range []string{stagedPath, "staged from " + sourcePath} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("output missing %q:\n%s", want, output.String())
		}
	}
}

func TestDaemonInstallBossdLineDoesNotCallUnchangedPathStaged(t *testing.T) {
	line := daemonInstallBossdLine("/usr/local/bin/bossd", "/usr/local/bin/bossd")
	if !strings.Contains(line, "/usr/local/bin/bossd") {
		t.Fatalf("install line missing bossd path: %q", line)
	}
	if strings.Contains(line, "staged from") {
		t.Fatalf("unchanged install path incorrectly described as staged: %q", line)
	}
}

func prepareDaemonDoctorInstall(t *testing.T) (home, sourcePath, stagedPath string) {
	t.Helper()
	home, sourcePath, stagedPath = prepareDaemonDoctorPaths(t)
	if err := os.MkdirAll(filepath.Dir(stagedPath), 0o700); err != nil {
		t.Fatalf("mkdir staged dir: %v", err)
	}
	if err := os.WriteFile(stagedPath, []byte("bossd fixture\n"), 0o755); err != nil {
		t.Fatalf("write staged bossd: %v", err)
	}
	return home, sourcePath, stagedPath
}

func prepareDaemonDoctorPaths(t *testing.T) (home, sourcePath, stagedPath string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	settingsPath := filepath.Join(home, "settings.json")
	if err := config.SaveTo(settingsPath, config.DefaultSettings()); err != nil {
		t.Fatalf("save default settings: %v", err)
	}
	t.Setenv("BOSS_SETTINGS_PATH", settingsPath)

	sourceDir := filepath.Join(home, "opt", "bossanova", "bin")
	sourcePath = filepath.Join(sourceDir, "bossd")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir source dir: %v", err)
	}
	if err := os.WriteFile(sourcePath, []byte("bossd fixture\n"), 0o755); err != nil {
		t.Fatalf("write source bossd: %v", err)
	}
	t.Setenv("PATH", sourceDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// BOS-1183: doctor now reads launchd's spawn history. Left unstubbed the
	// probe shells out to the DEVELOPER's real launchd domain, so any doctor
	// test would report whatever that machine's bossd happens to be doing.
	// Every fixture pins the ordinary running shape — runs = 1 with
	// "(never exited)" — and a test that wants a different one re-stubs after
	// calling the fixture, which wins.
	stubDaemonDoctorSpawnHistory(t, daemon.SpawnHistory{
		State:        daemon.SpawnStateHealthy,
		Target:       daemonDoctorSpawnHistoryTarget,
		Runs:         1,
		RunsKnown:    true,
		NeverExited:  true,
		ServiceState: "running",
	}, nil)

	// Pinned for the same reason and in the same place: doctor now treats a
	// socket it knows is unreachable as a FAIL, and every fixture here points
	// HOME at a fresh temp dir whose socket path nothing is listening on. Left
	// unpinned, every doctor test would carry that unrelated failure. The
	// ordinary healthy shape is "serving", and a test that wants the daemon
	// down re-stubs after calling the fixture, which wins.
	previousReachable := daemonSocketReachable
	daemonSocketReachable = func(string) bool { return true }
	t.Cleanup(func() { daemonSocketReachable = previousReachable })

	appDataDir, err := config.DefaultAppDataDir()
	if err != nil {
		t.Fatalf("config.DefaultAppDataDir: %v", err)
	}
	return home, sourcePath, daemonbin.StagedPath(appDataDir)
}

func writeDaemonDoctorPlist(t *testing.T, home, programArgument string) {
	t.Helper()
	plistPath := filepath.Join(home, "Library", "LaunchAgents", "com.bossanova.bossd.plist")
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o700); err != nil {
		t.Fatalf("mkdir LaunchAgents: %v", err)
	}
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>ProgramArguments</key><array><string>` + programArgument + `</string></array>
</dict></plist>`
	if err := os.WriteFile(plistPath, []byte(plist), 0o600); err != nil {
		t.Fatalf("write plist: %v", err)
	}
}

func writeDaemonDoctorEmptyArgumentsPlist(t *testing.T, home string) {
	t.Helper()
	plistPath := filepath.Join(home, "Library", "LaunchAgents", "com.bossanova.bossd.plist")
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o700); err != nil {
		t.Fatalf("mkdir LaunchAgents: %v", err)
	}
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>ProgramArguments</key><array></array>
</dict></plist>`
	if err := os.WriteFile(plistPath, []byte(plist), 0o600); err != nil {
		t.Fatalf("write plist: %v", err)
	}
}

func writeDaemonDoctorState(t *testing.T, executablePath string, probeCompleted bool, results []daemonstate.TCCProbeResult) {
	t.Helper()
	appDataDir, err := config.DefaultAppDataDir()
	if err != nil {
		t.Fatalf("config.DefaultAppDataDir: %v", err)
	}
	if err := daemonstate.Write(appDataDir, daemonstate.Metadata{
		PID:               4242,
		ExecutablePath:    executablePath,
		TCCProbeCompleted: probeCompleted,
		TCCProbeResults:   results,
	}); err != nil {
		t.Fatalf("daemonstate.Write: %v", err)
	}
	// Doctor now probes the recorded PID (BOS-864). Picking a real PID would be
	// flaky, so every state fixture pins the liveness answer.
	stubDaemonDoctorProcess(t, nil)
	stubDaemonDoctorSupervision(t)
}

// writeDaemonDoctorStateStartedAt writes a healthy-probe state record whose
// StartedAt drives doctor's running-image comparison.
func writeDaemonDoctorStateStartedAt(t *testing.T, executablePath string, startedAt time.Time) {
	t.Helper()
	appDataDir, err := config.DefaultAppDataDir()
	if err != nil {
		t.Fatalf("config.DefaultAppDataDir: %v", err)
	}
	if err := daemonstate.Write(appDataDir, daemonstate.Metadata{
		PID:               4242,
		ExecutablePath:    executablePath,
		StartedAt:         startedAt,
		TCCProbeCompleted: true,
	}); err != nil {
		t.Fatalf("daemonstate.Write: %v", err)
	}
	stubDaemonDoctorProcess(t, nil)
	stubDaemonDoctorSupervision(t)
}

// stubDaemonDoctorSupervision pins the SERVICE-manager view the same way, and
// for the same reason, that stubDaemonDoctorProcess pins process liveness.
//
// The supervision check compares what the service manager owns against the
// recorded PID. Left unstubbed it reads the developer's real launchd/systemd,
// so an unrelated doctor test would pass or fail depending on whether the
// engineer running it happens to have bossd loaded — and would FAIL on any
// machine where it is not, since the fixtures write a live recorded PID.
//
// A test that wants to exercise supervision overrides daemonGetStatus after
// calling the fixture, which wins.
func stubDaemonDoctorSupervision(t *testing.T) {
	t.Helper()
	previous := daemonGetStatus
	daemonGetStatus = func() (*daemon.Status, error) {
		// Matches the PID the state fixtures write, so supervision reads "ok"
		// and perturbs no unrelated assertion.
		return &daemon.Status{Installed: true, Running: true, PID: 4242}, nil
	}
	t.Cleanup(func() { daemonGetStatus = previous })
}

// stubDaemonDoctorProcess makes doctor's signal-0 liveness probe deterministic.
// A nil signalErr means the recorded PID is alive; syscall.ESRCH means it is
// gone.
func stubDaemonDoctorProcess(t *testing.T, signalErr error) {
	t.Helper()
	previous := findDaemonProcess
	findDaemonProcess = func(int) (processSignaler, error) {
		return fakeProcess{err: signalErr}, nil
	}
	t.Cleanup(func() { findDaemonProcess = previous })
}

// TestRunDaemonDoctorReportsServicePath covers BOS-880: doctor must report the
// PATH the SERVICE will use, and resolve node/claude under that PATH rather
// than under the caller's own — resolving under an interactive shell is
// exactly the check that passes while the daemon is broken.
func TestRunDaemonDoctorReportsServicePath(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("staged LaunchAgent diagnostics are macOS-specific")
	}
	home, _, stagedPath := prepareDaemonDoctorInstall(t)
	writeDaemonDoctorPlist(t, home, stagedPath)
	writeDaemonDoctorState(t, stagedPath, true, nil)

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	_ = runDaemonDoctor(cmd)

	for _, want := range []string{"service PATH", "node:", "claude:"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("output missing %q:\n%s", want, output.String())
		}
	}
	if !strings.Contains(output.String(), daemon.ServiceEnvPath()) {
		t.Errorf("output does not report the effective service PATH:\n%s", output.String())
	}
}

func TestDaemonDoctorToolLineFound(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "node")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write stub binary: %v", err)
	}

	line, ok := daemonDoctorToolLine(dir, "node")
	if !ok {
		t.Error("daemonDoctorToolLine ok = false, want true")
	}
	if !strings.Contains(line, binary) {
		t.Errorf("line = %q, want it to name %q", line, binary)
	}
}

func TestDaemonDoctorToolLineMissing(t *testing.T) {
	line, ok := daemonDoctorToolLine(t.TempDir(), "node")
	if ok {
		t.Error("daemonDoctorToolLine ok = true, want false")
	}
	if !strings.Contains(line, "not found") {
		t.Errorf("line = %q, want it to say the tool was not found", line)
	}
}

// TestRunDaemonDoctorServicePathMissingToolIsNotFatal keeps the new report
// diagnostic: a machine without `claude` installed is not an unhealthy daemon,
// so the missing tool must not flip doctor's exit status on its own.
func TestRunDaemonDoctorServicePathMissingToolIsNotFatal(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("staged LaunchAgent diagnostics are macOS-specific")
	}
	home, _, stagedPath := prepareDaemonDoctorInstall(t)
	writeDaemonDoctorPlist(t, home, stagedPath)
	writeDaemonDoctorState(t, stagedPath, true, nil)

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	err := runDaemonDoctor(cmd)

	// The fixture is otherwise healthy; only tool resolution may be missing.
	if err != nil && strings.Contains(err.Error(), "node") {
		t.Errorf("a missing tool made doctor fail: %v", err)
	}
}

// TestRunDaemonDoctorReportsServicePathOnNonDarwin: the Linux systemd unit now
// carries an explicit PATH too, so the report must precede the macOS-only early
// return rather than sit behind it.
func TestRunDaemonDoctorReportsServicePathOnNonDarwin(t *testing.T) {
	previous := daemonDoctorGOOS
	daemonDoctorGOOS = "linux"
	t.Cleanup(func() { daemonDoctorGOOS = previous })
	// Asserts the service-PATH line, not supervision — see the sibling test.
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	if err := runDaemonDoctor(cmd); err != nil {
		t.Fatalf("runDaemonDoctor: %v", err)
	}

	for _, want := range []string{"service PATH", "node:", "claude:", "not applicable on linux"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("output missing %q:\n%s", want, output.String())
		}
	}
}

// writeDaemonDoctorPlistWithPath writes a plist carrying an EnvironmentVariables
// PATH, so doctor can be exercised against a pre-change install.
func writeDaemonDoctorPlistWithPath(t *testing.T, home, programArgument, servicePath string) {
	t.Helper()
	plistPath := filepath.Join(home, "Library", "LaunchAgents", "com.bossanova.bossd.plist")
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o700); err != nil {
		t.Fatalf("mkdir LaunchAgents: %v", err)
	}
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>ProgramArguments</key><array><string>` + programArgument + `</string></array>
<key>EnvironmentVariables</key><dict><key>PATH</key><string>` + servicePath + `</string><key>LC_CTYPE</key><string>UTF-8</string></dict>
</dict></plist>`
	if err := os.WriteFile(plistPath, []byte(plist), 0o600); err != nil {
		t.Fatalf("write plist: %v", err)
	}
}

// TestRunDaemonDoctorFlagsStaleServicePath is the state BOS-880 leaves every
// existing install in until its first restart after the upgrade: the plist on
// disk still carries the old hardcoded PATH while the next render would write
// the repaired one. Doctor must say so rather than report the computed PATH as
// though the daemon already had it.
func TestRunDaemonDoctorFlagsStaleServicePath(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("staged LaunchAgent diagnostics are macOS-specific")
	}
	home, _, stagedPath := prepareDaemonDoctorInstall(t)
	const preChangePath = "/usr/local/bin:/usr/bin:/bin:/opt/homebrew/bin"
	writeDaemonDoctorPlistWithPath(t, home, stagedPath, preChangePath)
	writeDaemonDoctorState(t, stagedPath, true, nil)

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	err := runDaemonDoctor(cmd)

	got := output.String()
	if !strings.Contains(got, "service PATH (installed): "+preChangePath) {
		t.Errorf("output does not report the INSTALLED pre-change PATH:\n%s", got)
	}
	if !strings.Contains(got, "service PATH (next restart): ") {
		t.Errorf("output does not report the next-restart PATH:\n%s", got)
	}
	if !strings.Contains(got, "run 'boss daemon restart'") {
		t.Errorf("output does not point at the restart remedy:\n%s", got)
	}
	if err == nil {
		t.Error("a stale service PATH must make doctor report unhealthy")
	}
}

// TestRunDaemonDoctorMatchingServicePathIsNotStale: once the install has been
// restarted the two PATHs agree, and doctor must go quiet rather than nag.
func TestRunDaemonDoctorMatchingServicePathIsNotStale(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("staged LaunchAgent diagnostics are macOS-specific")
	}
	home, _, stagedPath := prepareDaemonDoctorInstall(t)
	writeDaemonDoctorPlistWithPath(t, home, stagedPath, daemon.ServiceEnvPath())
	writeDaemonDoctorState(t, stagedPath, true, nil)

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	_ = runDaemonDoctor(cmd)

	got := output.String()
	if strings.Contains(got, "next restart") {
		t.Errorf("a matching PATH must not be reported as stale:\n%s", got)
	}
	if strings.Contains(got, "service PATH: stale") {
		t.Errorf("a matching PATH must not be reported as stale:\n%s", got)
	}
}

// TestRunDaemonDoctorStaleServicePathIsUnhealthyOnNonDarwin: the service-PATH
// check is not macOS-specific, so its verdict must survive the early return
// that skips the macOS-only checks. Returning nil there regardless would make
// stale detection a no-op on exactly the platform the PATH line is new on.
func TestRunDaemonDoctorStaleServicePathIsUnhealthyOnNonDarwin(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the fixture writes a macOS plist to drive the installed-PATH read")
	}
	home, _, stagedPath := prepareDaemonDoctorInstall(t)
	writeDaemonDoctorPlistWithPath(t, home, stagedPath, "/usr/local/bin:/usr/bin:/bin:/opt/homebrew/bin")

	previous := daemonDoctorGOOS
	daemonDoctorGOOS = "linux"
	t.Cleanup(func() { daemonDoctorGOOS = previous })

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	err := runDaemonDoctor(cmd)

	if err == nil {
		t.Errorf("a stale service PATH must be unhealthy on non-darwin too:\n%s", output.String())
	}
	if !strings.Contains(output.String(), "run 'boss daemon restart'") {
		t.Errorf("output does not offer the restart remedy:\n%s", output.String())
	}
}

// --- BOS-944: the live daemon-auth check -------------------------------------
//
// Every test below injects through the daemonAuthStateProbe package var rather
// than standing a daemon up, for the same reason findDaemonProcess and
// daemonDoctorGOOS are seams: the rendering is what is under test, and a real
// daemon would make the answer depend on the developer's own login state.

// stubDaemonAuthStateProbe pins the live-auth probe's answer and returns a
// pointer to the number of times doctor asked for it. The count matters: the
// probe must run exactly once per doctor invocation, and it must run at all —
// a check that silently stops calling its probe still prints a clean section.
func stubDaemonAuthStateProbe(t *testing.T, resp *pb.GetAuthStateResponse, probeErr error) *int {
	t.Helper()
	calls := 0
	previous := daemonAuthStateProbe
	daemonAuthStateProbe = func(context.Context) (*pb.GetAuthStateResponse, error) {
		calls++
		return resp, probeErr
	}
	t.Cleanup(func() { daemonAuthStateProbe = previous })
	return &calls
}

// newDaemonDoctorAuthEnv isolates HOME (so no LaunchAgent plist is readable and
// the service-PATH check cannot report stale) and forces the non-darwin path,
// so the only verdict left in the run is the auth one. That keeps these tests
// runnable on every platform instead of skipping on the CI runner.
func newDaemonDoctorAuthEnv(t *testing.T) {
	t.Helper()
	prepareDaemonDoctorPaths(t)
	previous := daemonDoctorGOOS
	daemonDoctorGOOS = "linux"
	t.Cleanup(func() { daemonDoctorGOOS = previous })
}

func runDaemonDoctorCapturing(t *testing.T) (string, error) {
	t.Helper()
	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	err := runDaemonDoctor(cmd)
	return output.String(), err
}

func daemonDoctorAgo(d time.Duration) *timestamppb.Timestamp {
	return timestamppb.New(time.Now().Add(-d))
}

// TestRunDaemonDoctorFailsWedgedDaemonAuth is the headline BOS-944 behaviour:
// a daemon whose upstream authentication has been failing for over an hour must
// come out of the doctor as a FAIL naming the duration and the enumerated
// reason, with `boss login` as the remedy.
func TestRunDaemonDoctorFailsWedgedDaemonAuth(t *testing.T) {
	newDaemonDoctorAuthEnv(t)
	stubDaemonAuthStateProbe(t, &pb.GetAuthStateResponse{
		UpstreamConfigured: true,
		NeedsLogin:         true,
		ReloginReason:      "refresh_outcome_unknown",
		AuthFailingSince:   daemonDoctorAgo(70 * time.Minute),
	}, nil)

	got, err := runDaemonDoctorCapturing(t)

	if !errors.Is(err, errDaemonDoctorUnhealthy) {
		t.Fatalf("a wedged daemon must be unhealthy, got err=%v:\n%s", err, got)
	}
	for _, want := range []string{
		"FAIL daemon auth: upstream authentication has been failing for 1h",
		"(reason: refresh_outcome_unknown)",
		// `never` and `unknown` are different sentences: the daemon has been
		// up and has never once registered, which is not the same as a
		// timestamp we could not read.
		"last successful registration: never",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	remediation := strings.Index(got, "\nRemediation:")
	login := strings.Index(got, "run 'boss login'")
	if remediation < 0 || login < remediation {
		t.Errorf("the login remedy must appear under Remediation:\n%s", got)
	}
}

func TestRunDaemonDoctorReportsSignedInDaemonAuth(t *testing.T) {
	newDaemonDoctorAuthEnv(t)
	stubDaemonAuthStateProbe(t, &pb.GetAuthStateResponse{
		UpstreamConfigured: true,
		UpstreamConnected:  true,
		LastRegisteredAt:   daemonDoctorAgo(2 * time.Minute),
	}, nil)

	got, err := runDaemonDoctorCapturing(t)

	if err != nil {
		t.Fatalf("a healthy daemon must not be unhealthy: %v\n%s", err, got)
	}
	for _, want := range []string{
		"daemon auth: signed in — last successful registration: 2m",
		"(stream connected: true)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"FAIL daemon auth", "boss login"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("a healthy daemon emitted %q:\n%s", unwanted, got)
		}
	}
}

// TestRunDaemonDoctorFailsSignedInButUnregisteredDaemon pins the actual BOS-942
// incident shape: the daemon does NOT think it needs a login, so nothing in the
// system prompts for one, while its re-registration has failed for 45 minutes.
// A suite that only exercised needs_login=true would have stayed green through
// the entire outage.
func TestRunDaemonDoctorFailsSignedInButUnregisteredDaemon(t *testing.T) {
	newDaemonDoctorAuthEnv(t)
	stubDaemonAuthStateProbe(t, &pb.GetAuthStateResponse{
		UpstreamConfigured: true,
		NeedsLogin:         false,
		UpstreamConnected:  false,
		AuthFailingSince:   daemonDoctorAgo(45 * time.Minute),
	}, nil)

	got, err := runDaemonDoctorCapturing(t)

	if !errors.Is(err, errDaemonDoctorUnhealthy) {
		t.Fatalf("the BOS-942 shape must be unhealthy, got err=%v:\n%s", err, got)
	}
	for _, want := range []string{
		"FAIL daemon auth: upstream authentication has been failing for 45m",
		"(reason: not reported)",
		"the daemon has not flagged itself as needing a login",
		"run 'boss login'",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

func TestRunDaemonDoctorReportsAuthStateUnknownWhenDaemonUnreachable(t *testing.T) {
	newDaemonDoctorAuthEnv(t)
	stubDaemonAuthStateProbe(t, nil, errors.New("dial unix /tmp/bossd.sock: connect: connection refused"))

	got, err := runDaemonDoctorCapturing(t)

	if err != nil {
		t.Fatalf("an unreachable daemon is diagnosed by the install/start checks, not this one: %v\n%s", err, got)
	}
	if !strings.Contains(got, "daemon auth: unknown — could not reach the daemon") {
		t.Errorf("output missing the unreachable verdict:\n%s", got)
	}
	for _, unwanted := range []string{"FAIL daemon auth", "boss login"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("could-not-evaluate was reported as %q:\n%s", unwanted, got)
		}
	}
}

// TestRunDaemonDoctorReportsAuthStateUnknownOnOlderDaemon covers the normal
// state for the minutes between a CLI upgrade and the daemon restart that
// follows it. It asserts BOTH texts in one test on purpose: the two unknowns
// have different remedies, and collapsing them into one string is exactly the
// edit this pins against.
func TestRunDaemonDoctorReportsAuthStateUnknownOnOlderDaemon(t *testing.T) {
	newDaemonDoctorAuthEnv(t)
	stubDaemonAuthStateProbe(t, nil, connect.NewError(connect.CodeUnimplemented, errors.New("unknown method GetAuthState")))

	older, err := runDaemonDoctorCapturing(t)
	if err != nil {
		t.Fatalf("an older daemon must not be reported unhealthy: %v\n%s", err, older)
	}
	for _, want := range []string{
		"daemon auth: unknown — this daemon predates the check",
		"run 'boss daemon restart'",
	} {
		if !strings.Contains(older, want) {
			t.Errorf("output missing %q:\n%s", want, older)
		}
	}
	if strings.Contains(older, "could not reach the daemon") {
		t.Errorf("an older daemon was reported as unreachable:\n%s", older)
	}

	stubDaemonAuthStateProbe(t, nil, errors.New("connection refused"))
	unreachable, err := runDaemonDoctorCapturing(t)
	if err != nil {
		t.Fatalf("an unreachable daemon must not be reported unhealthy: %v\n%s", err, unreachable)
	}
	if !strings.Contains(unreachable, "could not reach the daemon") {
		t.Errorf("output missing the unreachable verdict:\n%s", unreachable)
	}
	if strings.Contains(unreachable, "predates the check") {
		t.Errorf("an unreachable daemon was reported as an old one:\n%s", unreachable)
	}
}

// TestRunDaemonDoctorReportsDeliberateSignOutWithoutFailing is the
// teardown-vs-failure discriminator. `boss logout` leaves needs_login=true on a
// perfectly healthy daemon; reporting that as FAIL would fire this check on
// every deliberate sign-out and teach operators to ignore it.
func TestRunDaemonDoctorReportsDeliberateSignOutWithoutFailing(t *testing.T) {
	newDaemonDoctorAuthEnv(t)
	stubDaemonAuthStateProbe(t, &pb.GetAuthStateResponse{
		UpstreamConfigured: true,
		NeedsLogin:         true,
	}, nil)

	got, err := runDaemonDoctorCapturing(t)

	if err != nil {
		t.Fatalf("a deliberate sign-out is not an unhealthy daemon: %v\n%s", err, got)
	}
	if !strings.Contains(got, "daemon auth: signed out — run 'boss login' to sign in again") {
		t.Errorf("output missing the signed-out line:\n%s", got)
	}
	if strings.Contains(got, "FAIL daemon auth") {
		t.Errorf("a deliberate sign-out was reported as a failure:\n%s", got)
	}
	// With no enumerated reason there is nothing to put in parentheses, and an
	// empty "(reason: )" reads as a bug in the daemon rather than an absence.
	if strings.Contains(got, "(reason:") {
		t.Errorf("an empty relogin reason was rendered:\n%s", got)
	}
	if strings.Contains(got, "\nRemediation:") {
		t.Errorf("a healthy sign-out opened the remediation block:\n%s", got)
	}
}

func TestRunDaemonDoctorReportsLocalOnlyDaemonAuthAsHealthy(t *testing.T) {
	newDaemonDoctorAuthEnv(t)
	stubDaemonAuthStateProbe(t, &pb.GetAuthStateResponse{UpstreamConfigured: false}, nil)

	got, err := runDaemonDoctorCapturing(t)

	if err != nil {
		t.Fatalf("a local-only daemon is a supported configuration: %v\n%s", err, got)
	}
	if !strings.Contains(got, "daemon auth: not configured (local-only daemon)") {
		t.Errorf("output missing the local-only line:\n%s", got)
	}
	if strings.Contains(got, "FAIL") {
		t.Errorf("a local-only daemon was reported as failing:\n%s", got)
	}
}

// TestRunDaemonDoctorAuthCheckRunsOnNonDarwin: an upstream credential wedge has
// nothing to do with launchd, so the check must run ahead of the macOS-only
// early return and its verdict must survive it. Mirrors
// TestRunDaemonDoctorStaleServicePathIsUnhealthyOnNonDarwin.
func TestRunDaemonDoctorAuthCheckRunsOnNonDarwin(t *testing.T) {
	newDaemonDoctorAuthEnv(t)
	calls := stubDaemonAuthStateProbe(t, &pb.GetAuthStateResponse{
		UpstreamConfigured: true,
		ReloginReason:      "refresh_token_rejected",
		AuthFailingSince:   daemonDoctorAgo(30 * time.Minute),
	}, nil)

	got, err := runDaemonDoctorCapturing(t)

	if !strings.Contains(got, "not applicable on linux") {
		t.Fatalf("the fixture did not take the non-darwin path:\n%s", got)
	}
	if *calls != 1 {
		t.Errorf("auth probe called %d times on the non-darwin path, want 1", *calls)
	}
	// The LINE itself, not just its consequences. A verdict that reached the
	// exit status and the remediation block while the section stayed silent
	// would satisfy every other assertion here and still leave an operator
	// staring at a failing command with nothing on screen explaining why.
	for _, want := range []string{
		"FAIL daemon auth: upstream authentication has been failing for 30m",
		"(reason: refresh_token_rejected)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the non-darwin path dropped %q from the auth line:\n%s", want, got)
		}
	}
	if !errors.Is(err, errDaemonDoctorUnhealthy) {
		t.Errorf("a wedged daemon must stay unhealthy through the early return, got err=%v:\n%s", err, got)
	}
	if !strings.Contains(got, "run 'boss login'") {
		t.Errorf("the non-darwin early return dropped the login remedy:\n%s", got)
	}
}

// TestRunDaemonDoctorWedgedAuthAloneDoesNotSuggestRestart: a restart cannot fix
// dead credentials — they are still dead afterwards — so an auth wedge must not
// be folded into the flag that prints the restart remedy.
func TestRunDaemonDoctorWedgedAuthAloneDoesNotSuggestRestart(t *testing.T) {
	if runtime.GOOS == "darwin" {
		// Exercise the full macOS path, where the non-auth unhealthy flag that
		// owns the restart remedy actually exists.
		home, _, stagedPath := prepareDaemonDoctorInstall(t)
		writeDaemonDoctorPlist(t, home, stagedPath)
		writeDaemonDoctorState(t, stagedPath, true, nil)
	} else {
		newDaemonDoctorAuthEnv(t)
	}
	stubDaemonAuthStateProbe(t, &pb.GetAuthStateResponse{
		UpstreamConfigured: true,
		ReloginReason:      "refresh_outcome_unknown",
		AuthFailingSince:   daemonDoctorAgo(90 * time.Minute),
	}, nil)

	got, err := runDaemonDoctorCapturing(t)

	if !errors.Is(err, errDaemonDoctorUnhealthy) {
		t.Fatalf("a wedged daemon must be unhealthy, got err=%v:\n%s", err, got)
	}
	if !strings.Contains(got, "run 'boss login'") {
		t.Errorf("output missing the login remedy:\n%s", got)
	}
	if strings.Contains(got, "run 'boss daemon restart'") {
		t.Errorf("an auth wedge offered a restart, which cannot fix it:\n%s", got)
	}
}

// TestRunDaemonDoctorAuthProbeDoesNotStartDaemon: doctor is a diagnostic. If
// the auth probe travelled through the autostart path it would launch the very
// daemon whose absence it was about to report, and the "could not reach the
// daemon" branch would become unreachable.
func TestRunDaemonDoctorAuthProbeDoesNotStartDaemon(t *testing.T) {
	newDaemonDoctorAuthEnv(t)
	previous := daemonEnsureRunning
	daemonEnsureRunning = func(string) error {
		t.Error("daemon doctor started the daemon it was diagnosing")
		return nil
	}
	t.Cleanup(func() { daemonEnsureRunning = previous })

	calls := stubDaemonAuthStateProbe(t, nil, errors.New("connection refused"))

	if _, err := runDaemonDoctorCapturing(t); err != nil {
		t.Fatalf("runDaemonDoctor: %v", err)
	}
	if *calls != 1 {
		t.Errorf("auth probe called %d times, want 1", *calls)
	}
}

// TestRunDaemonDoctorAuthVerdictDiscriminator walks all four combinations of
// (relogin_reason present/absent) x (auth_failing_since present/absent) with
// needs_login=true throughout, because that is where the two states that must
// NOT be conflated live.
//
// A reason with no failure clock is the case this table exists for: it used to
// print the benign signed-out line and exit zero, so a daemon whose refresh
// token had been rejected looked exactly like one somebody had deliberately
// logged out of.
func TestRunDaemonDoctorAuthVerdictDiscriminator(t *testing.T) {
	cases := []struct {
		name         string
		reason       string
		failingSince *timestamppb.Timestamp
		wantFail     bool
		wantLines    []string
		unwanted     []string
	}{
		{
			name:         "reason and clock",
			reason:       "refresh_token_rejected",
			failingSince: daemonDoctorAgo(20 * time.Minute),
			wantFail:     true,
			wantLines: []string{
				"FAIL daemon auth: upstream authentication has been failing for 20m",
				"(reason: refresh_token_rejected)",
			},
			unwanted: []string{"signed out"},
		},
		{
			name:         "clock only",
			failingSince: daemonDoctorAgo(20 * time.Minute),
			wantFail:     true,
			wantLines: []string{
				"FAIL daemon auth: upstream authentication has been failing for 20m",
				"(reason: not reported)",
			},
			unwanted: []string{"signed out"},
		},
		{
			name:     "reason only",
			reason:   "refresh_token_rejected",
			wantFail: true,
			wantLines: []string{
				"FAIL daemon auth: upstream authentication has been failing for an unknown duration",
				"(reason: refresh_token_rejected)",
			},
			// The whole point: an enumerated reason is a fault, never the
			// reassuring line, and never a zero exit.
			unwanted: []string{"signed out"},
		},
		{
			name:      "neither — the deliberate logout",
			wantFail:  false,
			wantLines: []string{"daemon auth: signed out — run 'boss login' to sign in again"},
			unwanted:  []string{"FAIL daemon auth", "\nRemediation:"},
		},
		{
			// The discriminator branches on the RAW reason, not the
			// rendered one, and this is the only case that can tell the
			// difference: a reason made entirely of runes the sanitizer
			// drops renders as the empty string, so a discriminator reading
			// the sanitized value would fall through to the benign
			// signed-out line — a daemon whose credentials were rejected
			// reported as one somebody logged out of. The clock is
			// deliberately absent so the `failingSince != nil` disjunct
			// cannot carry the case.
			name:     "reason only, and every rune of it is unprintable",
			reason:   "\n\u202E\u2028",
			wantFail: true,
			wantLines: []string{
				"FAIL daemon auth: upstream authentication has been failing for an unknown duration",
				"(reason: not reported)",
			},
			unwanted: []string{"signed out"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			newDaemonDoctorAuthEnv(t)
			stubDaemonAuthStateProbe(t, &pb.GetAuthStateResponse{
				UpstreamConfigured: true,
				NeedsLogin:         true,
				ReloginReason:      tc.reason,
				AuthFailingSince:   tc.failingSince,
			}, nil)

			got, err := runDaemonDoctorCapturing(t)

			if tc.wantFail && !errors.Is(err, errDaemonDoctorUnhealthy) {
				t.Fatalf("want unhealthy, got err=%v:\n%s", err, got)
			}
			if !tc.wantFail && err != nil {
				t.Fatalf("want healthy, got err=%v:\n%s", err, got)
			}
			for _, want := range tc.wantLines {
				if !strings.Contains(got, want) {
					t.Errorf("output missing %q:\n%s", want, got)
				}
			}
			for _, unwanted := range tc.unwanted {
				if strings.Contains(got, unwanted) {
					t.Errorf("output contains %q:\n%s", unwanted, got)
				}
			}
			if tc.wantFail {
				remediation := strings.Index(got, "\nRemediation:")
				login := strings.Index(got, "run 'boss login'")
				if remediation < 0 || login < remediation {
					t.Errorf("the login remedy must appear under Remediation:\n%s", got)
				}
			}
		})
	}
}

// TestRunDaemonDoctorBoundsDaemonSuppliedStrings: the reason and the transport
// error come from another process and land verbatim in an operator's terminal.
// Both are bounded and stripped of control characters, so a multi-line or
// screen-length value cannot scroll the verdict away or repaint the terminal
// around it. Sanitising only ever removes material — AC #15's negative
// assertion is about what must never reach this output in the first place.
func TestRunDaemonDoctorBoundsDaemonSuppliedStrings(t *testing.T) {
	t.Run("relogin reason", func(t *testing.T) {
		newDaemonDoctorAuthEnv(t)
		stubDaemonAuthStateProbe(t, &pb.GetAuthStateResponse{
			UpstreamConfigured: true,
			ReloginReason:      "refresh_token_rejected\nHTTP/1.1 401\r\n\tbody: " + strings.Repeat("x", 400),
			AuthFailingSince:   daemonDoctorAgo(time.Minute),
		}, nil)

		got, _ := runDaemonDoctorCapturing(t)

		authLine := ""
		for _, line := range strings.Split(got, "\n") {
			if strings.Contains(line, "FAIL daemon auth") {
				authLine = line
			}
		}
		if authLine == "" {
			t.Fatalf("no FAIL auth line to inspect:\n%s", got)
		}
		if strings.Contains(authLine, "\r") {
			t.Errorf("the rendered reason kept a carriage return: %q", authLine)
		}
		if strings.Contains(got, strings.Repeat("x", daemonDoctorMaxFieldLen+1)) {
			t.Errorf("the reason was not truncated:\n%s", got)
		}
		if !strings.Contains(authLine, "refresh_token_rejected") {
			t.Errorf("sanitising dropped the enumerated reason itself: %q", authLine)
		}
	})

	t.Run("transport error", func(t *testing.T) {
		newDaemonDoctorAuthEnv(t)
		stubDaemonAuthStateProbe(t, nil, errors.New("dial unix: connection refused\n"+strings.Repeat("y", 400)))

		got, _ := runDaemonDoctorCapturing(t)

		unknown := ""
		for _, line := range strings.Split(got, "\n") {
			if strings.Contains(line, "daemon auth: unknown") {
				unknown = line
			}
		}
		if unknown == "" {
			t.Fatalf("no unknown auth line to inspect:\n%s", got)
		}
		if strings.Contains(got, strings.Repeat("y", daemonDoctorMaxFieldLen+1)) {
			t.Errorf("the transport error was not truncated:\n%s", got)
		}
		if !strings.Contains(unknown, "connection refused") {
			t.Errorf("sanitising dropped the cause: %q", unknown)
		}
	})
}

// TestSanitizeDaemonDoctorField pins the transformation itself: control
// characters collapse to single spaces, and the result is bounded.
func TestSanitizeDaemonDoctorField(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty stays empty", in: "", want: ""},
		{name: "plain reason is untouched", in: "refresh_token_rejected", want: "refresh_token_rejected"},
		{name: "newlines collapse", in: "a\nb", want: "a b"},
		{name: "runs collapse to one space", in: "a\r\n\t   b", want: "a b"},
		{name: "escape sequences are stripped", in: "a\x1b[31mred", want: "a [31mred"},
		{name: "surrounding whitespace is trimmed", in: "\n  reason  \n", want: "reason"},
		// The classes that reorder or hide a terminal line WITHOUT being
		// control characters (Cc). unicode.IsControl does not see any of
		// these, so they are what distinguishes the printable-rune predicate
		// from the control-character one it replaced.
		{name: "bidi override is removed", in: "a\u202Eb", want: "a b"},
		{name: "zero-width joiner is removed", in: "a\u200Db", want: "a b"},
		{name: "line separator is removed", in: "a\u2028b", want: "a b"},
		{name: "non-breaking space collapses", in: "a\u00A0b", want: "a b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeDaemonDoctorField(tc.in); got != tc.want {
				t.Errorf("sanitizeDaemonDoctorField(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	t.Run("length is bounded", func(t *testing.T) {
		got := sanitizeDaemonDoctorField(strings.Repeat("z", 500))
		if len([]rune(got)) != daemonDoctorMaxFieldLen+1 {
			t.Fatalf("truncated length = %d runes, want %d plus the ellipsis", len([]rune(got)), daemonDoctorMaxFieldLen)
		}
		if !strings.HasSuffix(got, "…") {
			t.Errorf("truncation is unmarked, so a cut value reads as the whole value: %q", got)
		}
	})
}

// TestReportDaemonSupervisionFlagsADetachedDaemon is the 2026-09-03 incident as
// a unit test. bossd was alive at its recorded PID, its executable matched the
// LaunchAgent, its staged binary was current — and it was not launchd's. Every
// other check in this file passed; doctor exited 0 while the daemon could not
// authenticate to GitHub and logged to /dev/null.
func TestReportDaemonSupervisionFlagsADetachedDaemon(t *testing.T) {
	previous := daemonGetStatus
	t.Cleanup(func() { daemonGetStatus = previous })
	daemonGetStatus = func() (*daemon.Status, error) {
		// Installed, but the service manager is not running it.
		return &daemon.Status{Installed: true, Running: false}, nil
	}
	stubDaemonDoctorProcess(t, nil) // the recorded PID IS alive

	var out strings.Builder
	unhealthy, remediation := reportDaemonSupervision(&out, daemonstate.Metadata{PID: 14350}, nil)

	if !unhealthy || !remediation {
		t.Fatalf("unhealthy=%v remediation=%v, want both true\n%s", unhealthy, remediation, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "FAIL") {
		t.Fatalf("no FAIL in output:\n%s", got)
	}
	// The line has to name the consequences, not just the condition: the whole
	// point is that an operator reading it understands why gh broke.
	for _, fragment := range []string{"14350", "keychain", "unauthenticated"} {
		if !strings.Contains(got, fragment) {
			t.Fatalf("output does not mention %q:\n%s", fragment, got)
		}
	}
}

// TestReportDaemonSupervisionFlagsAPIDMismatch is the other half: the service
// manager owns one process and the state record names another.
func TestReportDaemonSupervisionFlagsAPIDMismatch(t *testing.T) {
	previous := daemonGetStatus
	t.Cleanup(func() { daemonGetStatus = previous })
	daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: true, Running: true, PID: 999}, nil
	}
	stubDaemonDoctorProcess(t, nil)

	var out strings.Builder
	unhealthy, remediation := reportDaemonSupervision(&out, daemonstate.Metadata{PID: 14350}, nil)
	if !unhealthy || !remediation {
		t.Fatalf("unhealthy=%v remediation=%v, want both true\n%s", unhealthy, remediation, out.String())
	}
	if !strings.Contains(out.String(), "999") || !strings.Contains(out.String(), "14350") {
		t.Fatalf("output does not name both PIDs:\n%s", out.String())
	}
}

// TestReportDaemonSupervisionPassesWhenSupervised is the anti-vacuity guard: a
// check that only ever fails would satisfy the tests above while being useless.
func TestReportDaemonSupervisionPassesWhenSupervised(t *testing.T) {
	previous := daemonGetStatus
	t.Cleanup(func() { daemonGetStatus = previous })
	daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: true, Running: true, PID: 14350}, nil
	}
	stubDaemonDoctorProcess(t, nil)

	var out strings.Builder
	unhealthy, remediation := reportDaemonSupervision(&out, daemonstate.Metadata{PID: 14350}, nil)
	if unhealthy || remediation {
		t.Fatalf("unhealthy=%v remediation=%v, want both false\n%s", unhealthy, remediation, out.String())
	}
	if !strings.Contains(out.String(), "ok") {
		t.Fatalf("output does not report ok:\n%s", out.String())
	}
}

// TestReportDaemonSupervisionIsUnknownNotUnhealthyOnInconclusiveInput pins the
// fail-safe. This check runs on developer machines and in CI, where the service
// view is meaningless (BOSS_DAEMON_SKIP_LAUNCHCTL) or there is no state record
// at all. A false FAIL there teaches operators to ignore the line that matters
// on a real host, so every inconclusive input must read "unknown" and return
// healthy.
func TestReportDaemonSupervisionIsUnknownNotUnhealthyOnInconclusiveInput(t *testing.T) {
	previous := daemonGetStatus
	t.Cleanup(func() { daemonGetStatus = previous })

	tests := []struct {
		name        string
		status      *daemon.Status
		statusErr   error
		metadata    daemonstate.Metadata
		metadataErr error
		signalErr   error
	}{
		{name: "status unavailable", statusErr: errors.New("launchctl exploded"), metadata: daemonstate.Metadata{PID: 1}},
		{name: "no state record", status: &daemon.Status{Installed: true}, metadataErr: errors.New("no such file")},
		{name: "not installed", status: &daemon.Status{Installed: false}, metadata: daemonstate.Metadata{PID: 1}},
		{name: "no recorded PID", status: &daemon.Status{Installed: true}, metadata: daemonstate.Metadata{PID: 0}},
		{name: "recorded PID is gone", status: &daemon.Status{Installed: true}, metadata: daemonstate.Metadata{PID: 1}, signalErr: syscall.ESRCH},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			daemonGetStatus = func() (*daemon.Status, error) { return tt.status, tt.statusErr }
			stubDaemonDoctorProcess(t, tt.signalErr)

			var out strings.Builder
			unhealthy, remediation := reportDaemonSupervision(&out, tt.metadata, tt.metadataErr)
			if unhealthy || remediation {
				t.Fatalf("unhealthy=%v remediation=%v, want both false\n%s", unhealthy, remediation, out.String())
			}
			if !strings.Contains(out.String(), "unknown") {
				t.Fatalf("output does not say unknown:\n%s", out.String())
			}
		})
	}
}

// TestReportDaemonSupervisionIsUnknownWhenProbingIsDisabled pins the
// BOSS_DAEMON_SKIP_LAUNCHCTL guard. Under that variable platformGetStatus
// returns Installed=true, Running=false WITHOUT asking the service manager —
// byte-identical to the detached-daemon shape this check flags. Interpreting it
// would make every test harness and CI run fail, which is how a diagnostic gets
// ignored on the host where it is telling the truth.
func TestReportDaemonSupervisionIsUnknownWhenProbingIsDisabled(t *testing.T) {
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")

	previous := daemonGetStatus
	t.Cleanup(func() { daemonGetStatus = previous })
	// The shape platformGetStatus really returns under the skip.
	daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: true, Running: false}, nil
	}
	stubDaemonDoctorProcess(t, nil)

	var out strings.Builder
	unhealthy, remediation := reportDaemonSupervision(&out, daemonstate.Metadata{PID: 14350}, nil)
	if unhealthy || remediation {
		t.Fatalf("unhealthy=%v remediation=%v, want both false\n%s", unhealthy, remediation, out.String())
	}
	if !strings.Contains(out.String(), "unknown") {
		t.Fatalf("output does not say unknown:\n%s", out.String())
	}
}

// TestReportDaemonSupervisionRefusesToCertifyWithoutAServicePID. A service
// manager can report running without yielding a PID — systemd when the MainPID
// read fails, launchd when its output will not parse. Falling through to "ok"
// there would emit a false healthy verdict from the very check added to detect
// an ownership mismatch.
func TestReportDaemonSupervisionRefusesToCertifyWithoutAServicePID(t *testing.T) {
	previous := daemonGetStatus
	t.Cleanup(func() { daemonGetStatus = previous })
	daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: true, Running: true, PID: 0}, nil
	}
	stubDaemonDoctorProcess(t, nil)

	var out strings.Builder
	unhealthy, remediation := reportDaemonSupervision(&out, daemonstate.Metadata{PID: 14350}, nil)
	if unhealthy || remediation {
		t.Fatalf("unhealthy=%v remediation=%v, want both false\n%s", unhealthy, remediation, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "unknown") {
		t.Fatalf("output does not say unknown:\n%s", got)
	}
	if strings.Contains(got, "ok (") {
		t.Fatalf("output certified ownership without a service PID:\n%s", got)
	}
}

// stubDaemonDoctorSpawnHistory pins launchd's spawn-history probe, the same way
// and for the same reason stubDaemonDoctorProcess pins process liveness: left
// unstubbed the probe shells out to the DEVELOPER's real launchd domain, so an
// unrelated doctor test would pass or fail depending on whether the engineer
// running it happens to have bossd loaded. BOS-1183.
//
// It returns a call counter, because "the check silently stopped asking" and
// "the check asked and got a clean answer" print the same nothing.
func stubDaemonDoctorSpawnHistory(t *testing.T, history daemon.SpawnHistory, err error) *int {
	t.Helper()
	calls := 0
	previous := daemonGetSpawnHistory
	daemonGetSpawnHistory = func() (daemon.SpawnHistory, error) {
		calls++
		return history, err
	}
	t.Cleanup(func() { daemonGetSpawnHistory = previous })
	return &calls
}

// daemonDoctorSpawnHistoryTarget is the shape launchd prints for a per-user
// GUI job: the domain is the part of it that was wrong during the incident.
const daemonDoctorSpawnHistoryTarget = "gui/501/com.bossanova.bossd"

// daemonDoctorLine returns the single output line beginning with prefix, so an
// assertion about ONE check's verdict cannot be satisfied by a string that
// happens to appear on some other line of the report.
func daemonDoctorLine(t *testing.T, got, prefix string) string {
	t.Helper()
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	t.Fatalf("output has no %q line:\n%s", prefix, got)
	return ""
}

// TestRunDaemonDoctorReportsNeverSpawnedJobWithConsoleRemediation is the
// headline BOS-1183 behaviour and the reason this whole check exists.
//
// On 2026-09-06 doctor was clean on every adjacent condition — staged binary up
// to date, protected roots ok — while the LaunchAgent sat registered in a GUI
// domain launchd would never spawn anything in, because fast user switching had
// backgrounded the Aqua session. `launchctl list` exits 0 for that job, so
// nothing doctor read could see it, and the diagnosis took an hour outside the
// tooling.
//
// The negative assertion is the important one. R3 says the remedy must NOT be
// `boss daemon start`: that command succeeds by producing an UNSUPERVISED
// bossd, which is BOS-1183's third reported failure. The fixture deliberately
// records a DEAD PID so startRemediation is genuinely set — without that, the
// assertion would pass for the trivial reason that nothing had asked for the
// start remedy in the first place.
func TestRunDaemonDoctorReportsNeverSpawnedJobWithConsoleRemediation(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd spawn history is macOS-specific")
	}
	home, _, stagedPath := prepareDaemonDoctorInstall(t)
	writeDaemonDoctorPlist(t, home, stagedPath)
	writeDaemonDoctorStateStartedAt(t, stagedPath, time.Now())
	stubDaemonDoctorProcess(t, syscall.ESRCH)
	calls := stubDaemonDoctorSpawnHistory(t, daemon.SpawnHistory{
		State:        daemon.SpawnStateNeverSpawned,
		Target:       daemonDoctorSpawnHistoryTarget,
		Runs:         0,
		RunsKnown:    true,
		NeverExited:  true,
		ServiceState: "not running",
	}, nil)

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	err := runDaemonDoctor(cmd)
	got := output.String()

	if !errors.Is(err, errDaemonDoctorUnhealthy) {
		t.Fatalf("a job launchd never spawned must be unhealthy, got err=%v:\n%s", err, got)
	}
	if *calls != 1 {
		t.Errorf("spawn-history probe called %d times, want 1", *calls)
	}
	spawnLine := daemonDoctorLine(t, got, "launchd spawn history:")
	for _, want := range []string{
		"FAIL",
		"never attempted to spawn",
		"runs = 0",
		daemonDoctorSpawnHistoryTarget,
		"never a bossd crash",
	} {
		if !strings.Contains(spawnLine, want) {
			t.Errorf("spawn-history line missing %q:\n%s", want, spawnLine)
		}
	}
	for _, want := range []string{"foreground console", "stat -f %Su /dev/console", "fast user switching"} {
		if !strings.Contains(got, want) {
			t.Errorf("remediation missing %q:\n%s", want, got)
		}
	}
	// R3, stated as an executable assertion.
	if strings.Contains(got, "run 'boss daemon start'") {
		t.Errorf("doctor offered 'boss daemon start' for a domain that will never spawn the job — that command succeeds by producing an unsupervised daemon:\n%s", got)
	}
}

// TestRunDaemonDoctorReportsCrashLoopingJobDistinctlyFromNeverSpawned pins R2:
// launchd-never-tried and bossd-started-and-failed have DISJOINT remedies, so
// the two must not read the same. A crash loop is bossd's own fault, and the
// console requirement has nothing to do with it.
func TestRunDaemonDoctorReportsCrashLoopingJobDistinctlyFromNeverSpawned(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd spawn history is macOS-specific")
	}
	home, _, stagedPath := prepareDaemonDoctorInstall(t)
	writeDaemonDoctorPlist(t, home, stagedPath)
	writeDaemonDoctorStateStartedAt(t, stagedPath, time.Now())
	stubDaemonDoctorSpawnHistory(t, daemon.SpawnHistory{
		State:             daemon.SpawnStateFailing,
		Target:            daemonDoctorSpawnHistoryTarget,
		Runs:              47,
		RunsKnown:         true,
		LastExitCode:      1,
		LastExitCodeKnown: true,
	}, nil)
	// Explicit, and load-bearing: this is the SERVED crash-loop shape. launchd
	// spawned bossd, bossd died, the operator recovered with a detached
	// `boss daemon start`, and launchd's crash record stays on the job forever.
	// The assertions below require that doctor not send that operator to
	// foreground a second bossd over a socket that already answers.
	daemonSocketReachable = func(string) bool { return true }

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	err := runDaemonDoctor(cmd)
	got := output.String()

	if !errors.Is(err, errDaemonDoctorUnhealthy) {
		t.Fatalf("a crash-looping job must be unhealthy, got err=%v:\n%s", err, got)
	}
	spawnLine := daemonDoctorLine(t, got, "launchd spawn history:")
	for _, want := range []string{"FAIL", "47", "exit", "1", stagedPath} {
		if !strings.Contains(spawnLine, want) {
			t.Errorf("spawn-history line missing %q:\n%s", want, spawnLine)
		}
	}
	for _, unwanted := range []string{"foreground console", "/dev/console", "fast user switching"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("a bossd crash loop was given the launchd-domain remedy (%q):\n%s", unwanted, got)
		}
	}
	_, remediation, found := strings.Cut(got, "Remediation:")
	if !found {
		t.Fatalf("output has no Remediation section:\n%s", got)
	}
	if strings.Contains(remediation, "in the foreground") {
		t.Errorf("a SERVED daemon was told to foreground a second bossd — the duplicate the isSocketReachable guards exist to prevent:\n%s", remediation)
	}
	if !strings.Contains(remediation, "run 'boss daemon restart'") {
		t.Errorf("a serving but crash-marked job must fall through to the restart remedy:\n%s", remediation)
	}
}

// TestRunDaemonDoctorReportsUnparseableSpawnHistoryAsUnknown: `launchctl print`
// is a human-readable dump with no format contract, so a macOS release that
// renames a key must make this check say "I don't know" — never healthy, and
// never a FAIL either, because a false FAIL on developer machines and in CI is
// how the one line that matters on a real host gets ignored.
func TestRunDaemonDoctorReportsUnparseableSpawnHistoryAsUnknown(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd spawn history is macOS-specific")
	}
	const reason = "launchctl print output carried neither a `runs` nor a `last exit code` line"
	home, _, stagedPath := prepareDaemonDoctorInstall(t)
	writeDaemonDoctorPlist(t, home, stagedPath)
	writeDaemonDoctorStateStartedAt(t, stagedPath, time.Now())
	stubDaemonDoctorSpawnHistory(t, daemon.SpawnHistory{
		State:  daemon.SpawnStateUnknown,
		Target: daemonDoctorSpawnHistoryTarget,
		Reason: reason,
	}, nil)

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	err := runDaemonDoctor(cmd)
	got := output.String()

	if err != nil {
		t.Fatalf("an unreadable spawn history must not fail the doctor on its own: %v\n%s", err, got)
	}
	spawnLine := daemonDoctorLine(t, got, "launchd spawn history:")
	if !strings.Contains(spawnLine, "unknown") || !strings.Contains(spawnLine, reason) {
		t.Errorf("spawn-history line did not report unknown with its reason:\n%s", spawnLine)
	}
	if strings.Contains(spawnLine, "ok") || strings.Contains(spawnLine, "FAIL") {
		t.Errorf("an unreadable spawn history was given a determinate verdict:\n%s", spawnLine)
	}
}

// TestRunDaemonDoctorReportsHealthySpawnHistory pins the shape that must NOT
// fire: runs > 0 with "(never exited)" is an ordinary daemon that is up right
// now, and only runs = 0 with that same text is the incident.
func TestRunDaemonDoctorReportsHealthySpawnHistory(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd spawn history is macOS-specific")
	}
	restoreDaemonCommandStubs(t)
	home, _, stagedPath := prepareDaemonDoctorInstall(t)
	writeDaemonDoctorPlist(t, home, stagedPath)
	writeDaemonDoctorStateStartedAt(t, stagedPath, time.Now())
	stubDaemonDoctorSpawnHistory(t, daemon.SpawnHistory{
		State:        daemon.SpawnStateHealthy,
		Target:       daemonDoctorSpawnHistoryTarget,
		Runs:         1,
		RunsKnown:    true,
		NeverExited:  true,
		ServiceState: "running",
	}, nil)
	daemonSocketReachable = func(string) bool { return true }

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	if err := runDaemonDoctor(cmd); err != nil {
		t.Fatalf("runDaemonDoctor: %v\n%s", err, output.String())
	}
	got := output.String()
	spawnLine := daemonDoctorLine(t, got, "launchd spawn history:")
	if !strings.Contains(spawnLine, "ok") || !strings.Contains(spawnLine, "1") {
		t.Errorf("spawn-history line missing the healthy verdict and its run count:\n%s", spawnLine)
	}
	if strings.Contains(got, "Remediation:") || strings.Contains(got, "/dev/console") {
		t.Errorf("a healthy spawn history produced remediation:\n%s", got)
	}
}

// TestRunDaemonDoctorDirectsOperatorToTheStagedBinaryWhenNotServing covers the
// second invisible failure of the same incident: bossd exited inside a fail-loud
// migration BEFORE binding the socket, and bossd.stderr.log was 0 bytes because
// launchd — which writes that file through its own redirect — had never run the
// binary. Neither status nor doctor said anything at all.
func TestRunDaemonDoctorDirectsOperatorToTheStagedBinaryWhenNotServing(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd spawn history is macOS-specific")
	}
	restoreDaemonCommandStubs(t)
	home, _, stagedPath := prepareDaemonDoctorInstall(t)
	writeDaemonDoctorPlist(t, home, stagedPath)
	writeDaemonDoctorStateStartedAt(t, stagedPath, time.Now())
	daemonSocketReachable = func(string) bool { return false }

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	_ = runDaemonDoctor(cmd)
	got := output.String()

	directive := daemonDoctorLine(t, got, "startup diagnosis:")
	if !strings.Contains(directive, "before the socket binds") {
		t.Errorf("directive does not name the failure window:\n%s", directive)
	}
	if !strings.Contains(got, "bossd.stderr.log") {
		t.Errorf("directive does not explain why the launchd log is empty:\n%s", got)
	}
	if !strings.Contains(got, stagedPath) {
		t.Errorf("directive does not name the staged bossd to run:\n%s", got)
	}
}

// TestRunDaemonDoctorOmitsStartupDirectiveWhenServing: the directive is a
// diagnostic aid, not a permanent line. A healthy machine that prints it has
// taught its operator to skip the section.
func TestRunDaemonDoctorOmitsStartupDirectiveWhenServing(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd spawn history is macOS-specific")
	}
	restoreDaemonCommandStubs(t)
	home, _, stagedPath := prepareDaemonDoctorInstall(t)
	writeDaemonDoctorPlist(t, home, stagedPath)
	writeDaemonDoctorStateStartedAt(t, stagedPath, time.Now())
	daemonSocketReachable = func(string) bool { return true }

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	if err := runDaemonDoctor(cmd); err != nil {
		t.Fatalf("runDaemonDoctor: %v\n%s", err, output.String())
	}
	if strings.Contains(output.String(), "startup diagnosis:") {
		t.Fatalf("a serving daemon printed the startup-failure directive:\n%s", output.String())
	}
}

// TestRunDaemonDoctorOmitsStartupDirectiveForANeverSpawnedJobThatIsServing is
// the state this branch itself creates, and the one the directive must not fire
// in. The operator took the detached-fallback recovery, so a bossd is up and
// the socket answers — while launchd's runs stays 0 forever, because launchd
// never spawned the job and never will in that domain.
//
// Deriving "not serving" from the spawn history sent exactly that operator to
// foreground a SECOND bossd over a socket that is already served: the duplicate
// the three isSocketReachable guards in platformEnsureRunning exist to prevent.
// The domain FAIL and its console remedy still stand; only the directive is
// wrong here.
func TestRunDaemonDoctorOmitsStartupDirectiveForANeverSpawnedJobThatIsServing(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd spawn history is macOS-specific")
	}
	home, _, stagedPath := prepareDaemonDoctorInstall(t)
	writeDaemonDoctorPlist(t, home, stagedPath)
	writeDaemonDoctorStateStartedAt(t, stagedPath, time.Now())
	stubDaemonDoctorSpawnHistory(t, daemon.SpawnHistory{
		State:        daemon.SpawnStateNeverSpawned,
		Target:       daemonDoctorSpawnHistoryTarget,
		Runs:         0,
		RunsKnown:    true,
		NeverExited:  true,
		ServiceState: "not running",
	}, nil)
	daemonSocketReachable = func(string) bool { return true }

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	err := runDaemonDoctor(cmd)
	got := output.String()

	if !errors.Is(err, errDaemonDoctorUnhealthy) {
		t.Fatalf("a job launchd never spawned is still unhealthy when something else serves the socket, got err=%v:\n%s", err, got)
	}
	if strings.Contains(got, "startup diagnosis:") {
		t.Errorf("doctor told an operator to foreground a second bossd over a socket that is already served:\n%s", got)
	}
	if !strings.Contains(got, "foreground console") {
		t.Errorf("the never-spawned domain remedy was dropped along with the directive:\n%s", got)
	}
}

// TestRunDaemonDoctorFailsAnUnreachableSocket pins the other half of the same
// rule. The directive is recovery instruction, so a run that prints it must not
// simultaneously report health: doctor exiting 0 with no Remediation section is
// the R1 contradiction ("no surface may assert the daemon is running when its
// socket is unreachable") reproduced inside doctor's own output.
func TestRunDaemonDoctorFailsAnUnreachableSocket(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd spawn history is macOS-specific")
	}
	home, _, stagedPath := prepareDaemonDoctorInstall(t)
	writeDaemonDoctorPlist(t, home, stagedPath)
	writeDaemonDoctorStateStartedAt(t, stagedPath, time.Now())
	daemonSocketReachable = func(string) bool { return false }

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	err := runDaemonDoctor(cmd)
	got := output.String()

	if !errors.Is(err, errDaemonDoctorUnhealthy) {
		t.Fatalf("a daemon that is not serving must be unhealthy, got err=%v:\n%s", err, got)
	}
	socketLine := daemonDoctorLine(t, got, "FAIL daemon socket:")
	if !strings.Contains(socketLine, "not serving") {
		t.Errorf("socket verdict does not say the daemon is not serving:\n%s", socketLine)
	}
	if !strings.Contains(got, "Remediation:") {
		t.Errorf("doctor printed the startup directive with no Remediation section:\n%s", got)
	}
	if !strings.Contains(got, "run 'boss daemon start'") {
		t.Errorf("a daemon that is not serving was given no way to start it:\n%s", got)
	}
	if !strings.Contains(got, "startup diagnosis:") {
		t.Errorf("the directive is missing from the one state it exists for:\n%s", got)
	}
}

// TestRunDaemonDoctorReportsSocketReachableWhenServing is the negative of the
// above: the FAIL and its exit status must not fire for a daemon that answers.
func TestRunDaemonDoctorReportsSocketReachableWhenServing(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd spawn history is macOS-specific")
	}
	home, _, stagedPath := prepareDaemonDoctorInstall(t)
	writeDaemonDoctorPlist(t, home, stagedPath)
	writeDaemonDoctorStateStartedAt(t, stagedPath, time.Now())
	daemonSocketReachable = func(string) bool { return true }

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	if err := runDaemonDoctor(cmd); err != nil {
		t.Fatalf("runDaemonDoctor: %v\n%s", err, output.String())
	}
	got := output.String()
	socketLine := daemonDoctorLine(t, got, "daemon socket:")
	if !strings.Contains(socketLine, "reachable") || strings.Contains(socketLine, "not reachable") {
		t.Errorf("a serving daemon did not report a reachable socket:\n%s", socketLine)
	}
}

// TestRunDaemonDoctorRemediatesACrashLoopWithTheForegroundRun pins the remedy
// the crash-loop verdict actually needs. `boss daemon restart` re-runs a binary
// that starts and dies, and `boss daemon start` finds the job already loaded;
// neither shows the operator the error. Naming the foreground run ABOVE the
// Remediation header, as the first cut did, strands the one actionable command
// outside the block an operator reads for it.
func TestRunDaemonDoctorRemediatesACrashLoopWithTheForegroundRun(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd spawn history is macOS-specific")
	}
	home, _, stagedPath := prepareDaemonDoctorInstall(t)
	writeDaemonDoctorPlist(t, home, stagedPath)
	writeDaemonDoctorStateStartedAt(t, stagedPath, time.Now())
	stubDaemonDoctorSpawnHistory(t, daemon.SpawnHistory{
		State:             daemon.SpawnStateFailing,
		Target:            daemonDoctorSpawnHistoryTarget,
		Runs:              47,
		RunsKnown:         true,
		LastExitCode:      1,
		LastExitCodeKnown: true,
	}, nil)
	daemonSocketReachable = func(string) bool { return false }

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	err := runDaemonDoctor(cmd)
	got := output.String()

	if !errors.Is(err, errDaemonDoctorUnhealthy) {
		t.Fatalf("a crash-looping job must be unhealthy, got err=%v:\n%s", err, got)
	}
	_, remediation, found := strings.Cut(got, "Remediation:")
	if !found {
		t.Fatalf("output has no Remediation section:\n%s", got)
	}
	if !strings.Contains(remediation, "foreground") || !strings.Contains(remediation, stagedPath) {
		t.Errorf("the crash-loop remediation does not name the foreground run of the staged binary:\n%s", remediation)
	}
	for _, unwanted := range []string{"run 'boss daemon restart'", "run 'boss daemon start'"} {
		if strings.Contains(remediation, unwanted) {
			t.Errorf("a binary that starts and dies was offered %q, which reproduces the crash:\n%s", unwanted, remediation)
		}
	}
}

// TestRunDaemonDoctorReportsSpawnProbeExecutionFailureAsUnknown covers the
// other half of the probe's error discipline: a non-nil error means launchctl
// could not be EXECUTED, which is still "we could not tell" and must never
// become a FAIL — a machine with no launchctl on PATH is not a broken daemon.
func TestRunDaemonDoctorReportsSpawnProbeExecutionFailureAsUnknown(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd spawn history is macOS-specific")
	}
	home, _, stagedPath := prepareDaemonDoctorInstall(t)
	writeDaemonDoctorPlist(t, home, stagedPath)
	writeDaemonDoctorStateStartedAt(t, stagedPath, time.Now())
	stubDaemonDoctorSpawnHistory(t, daemon.SpawnHistory{State: daemon.SpawnStateUnknown},
		errors.New("exec: \"launchctl\": executable file not found in $PATH"))

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	if err := runDaemonDoctor(cmd); err != nil {
		t.Fatalf("an unrunnable probe must not fail the doctor: %v\n%s", err, output.String())
	}
	spawnLine := daemonDoctorLine(t, output.String(), "launchd spawn history:")
	if !strings.Contains(spawnLine, "unknown") || !strings.Contains(spawnLine, "executable file not found") {
		t.Errorf("spawn-history line did not report the execution failure as unknown:\n%s", spawnLine)
	}
}
