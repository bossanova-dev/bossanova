package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

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
}
