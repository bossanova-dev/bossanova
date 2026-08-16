package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/recurser/boss/internal/daemon"
	"github.com/recurser/boss/internal/termreset"
	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/daemonbin"
	"github.com/recurser/bossalib/daemonstate"
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

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	if err := runDaemonDoctor(cmd); err != nil {
		t.Fatalf("runDaemonDoctor: %v", err)
	}
	if !strings.Contains(output.String(), "not applicable") {
		t.Fatalf("output missing not-applicable status:\n%s", output.String())
	}
	// Linux behaviour is unchanged: no staging comparison and no liveness probe.
	for _, unwanted := range []string{"staged bossd", "running executable", "running bossd"} {
		if strings.Contains(output.String(), unwanted) {
			t.Fatalf("non-darwin doctor emitted %q:\n%s", unwanted, output.String())
		}
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
// warnIfDaemonBinaryStale BEFORE the gen-skill / fix-terminal / skills bypass
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
